package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type UserState int

const (
	StateNone = iota
	StateAwaitingShowName
	StateAwaitingShowSelection
	StateAwaitingSeasonEpisode
)

type UserContext struct {
	State              UserState
	SearchResults      []ShowSearchResult
	SelectedInternalID int64
	SelectedProviderID int
	SelectedSeason     int
}

type Bot struct {
	BotApi       *tgbotapi.BotAPI
	DB           *sql.DB
	UserContexts map[int64]*UserContext
	mu           sync.Mutex
}

func main() {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN not set")
	}

	botApi, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Fatalf("failted to create bot: %v", err)
	}

	log.Printf("Authorized on account %s", botApi.Self.UserName)

	// Set bot commands for menu
	commands := []tgbotapi.BotCommand{
		{Command: "start", Description: "Start the bot"},
		{Command: "help", Description: "Show help information"},
		{Command: "add", Description: "Add a TV show to track"},
		{Command: "shows", Description: "List your tracked shows"},
	}
	if _, err := botApi.Request(tgbotapi.NewSetMyCommands(commands...)); err != nil {
		log.Printf("Failed to set bot commands: %v", err)
	}

	db, err := openDB()
	if err != nil {
		log.Fatalf("failed to open db: %v", err)
	}
	defer db.Close()

	bot := &Bot{
		BotApi:       botApi,
		DB:           db,
		UserContexts: make(map[int64]*UserContext),
	}

	updateConfig := tgbotapi.NewUpdate(0)
	updateConfig.Timeout = 30

	updates := botApi.GetUpdatesChan(updateConfig)

	go bot.reminderLoop(context.Background())

	for update := range updates {
		if update.CallbackQuery != nil {
			bot.handleCallback(update.CallbackQuery)
			continue
		}

		if update.Message == nil {
			continue
		}

		msg := update.Message
		userID := msg.From.ID
		state := bot.getState(userID)

		switch {
		case msg.IsCommand():
			bot.handleCommand(msg)
			continue
		case state == StateAwaitingShowName:
			bot.acceptShowName(msg)
		case state == StateAwaitingSeasonEpisode:
			bot.acceptSeasonEpisode(msg)
		}
	}
}

func (bot *Bot) handleCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	command := msg.Command()
	// args := strings.TrimSpace(msg.CommandArguments())

	switch command {
	case "start":
		bot.reply(chatID, "Hello! I'm TV Show Reminder.")
	case "help":
		helpText := dedent(`
		Commands:

		/add <show>
		/shows - list your shows
		/help - show this help
		`)
		bot.reply(chatID, helpText)
	case "add":
		bot.handleAddCommand(msg)
	case "shows":
		shows, err := listShowsWithProgress(bot.DB, msg.From.ID)
		if err != nil {
			bot.reply(chatID, "Error: can't list shows at this time")
			return
		}
		if len(shows) == 0 {
			bot.reply(chatID, "You have no shows yet. Use /add <show> to add one.")
		} else {
			var showLines []string
			for _, show := range shows {
				if show.Season.Valid && show.Episode.Valid {
					showLines = append(showLines, fmt.Sprintf("%s (S%dE%d)", show.Name, show.Season.Int32, show.Episode.Int32))
				} else {
					showLines = append(showLines, show.Name)
				}
			}
			bot.reply(chatID, "Your shows:\n"+strings.Join(showLines, "\n"))
		}
	default:
		bot.reply(chatID, "Unknown command: /"+command)
	}
}

func (bot *Bot) handleCallback(cb *tgbotapi.CallbackQuery) {
	data := cb.Data
	parts := strings.Split(data, ":")
	if len(parts) < 1 {
		log.Printf("handleCallback: invalid callback data: %s", data)
		return
	}
	action := parts[0]
	param := ""
	if len(parts) > 1 {
		param = parts[1]
	}

	switch action {
	case "acceptShowName":
		bot.acceptShowNameCallback(cb, param)
	case "selectSeason":
		bot.handleSeasonCallback(cb, param)
	case "selectEpisode":
		bot.handleEpisodeCallback(cb, param)
	case "cancel":
		bot.handleCancelCallback(cb)
	}
}

func (bot *Bot) reminderLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.Println("reminderLoop: tick")
			reminders, err := getDueReminders(bot.DB)
			if err != nil {
				log.Printf("reminderLoop: getDueReminders error: %v", err)
				continue
			}
			if len(reminders) == 0 {
				log.Println("reminderLoop: no reminders due")
			} else {
				log.Printf("reminderLoop: %d reminders due", len(reminders))
			}
			for _, r := range reminders {
				log.Printf(
					"reminderLoop: sending reminder chat=%d show=%q episode=%d title=%q",
					r.ChatID, r.ShowName, r.EpisodeNumber, r.EpisodeTitle,
				)
				bot.reply(
					r.ChatID,
					fmt.Sprintf(
						"Episode #%d \"%s\" of \"%s\" (season %d) is coming out today!",
						r.EpisodeNumber, r.EpisodeTitle, r.ShowName, r.EpisodeSeason,
					),
				)

				if err := markReminderSent(bot.DB, r); err != nil {
					log.Printf("reminderLoop: failed to mark reminder sent: %v", err)
				}
			}
		case <-ctx.Done():
			log.Println("reminderLoop: context cancelled, exiting")
			return
		}
	}
}

func (bot *Bot) withUserContext(userID int64, fn func(*UserContext)) {
	bot.mu.Lock()
	defer bot.mu.Unlock()
	if bot.UserContexts[userID] == nil {
		bot.UserContexts[userID] = &UserContext{}
	}
	fn(bot.UserContexts[userID])
}

func (bot *Bot) getUserContext(userID int64) *UserContext {
	bot.mu.Lock()
	defer bot.mu.Unlock()
	return bot.UserContexts[userID]
}

func (bot *Bot) setState(userID int64, state UserState) {
	bot.withUserContext(userID, func(ctx *UserContext) {
		ctx.State = state
	})
}

func (bot *Bot) getState(userID int64) UserState {
	ctx := bot.getUserContext(userID)
	if ctx == nil {
		return StateNone
	}
	return ctx.State
}

func (bot *Bot) clearState(userID int64) {
	bot.mu.Lock()
	defer bot.mu.Unlock()
	delete(bot.UserContexts, userID)
}

func (bot *Bot) reply(chatID int64, text string) {
	message := tgbotapi.NewMessage(chatID, text)
	message.ReplyMarkup = tgbotapi.ReplyKeyboardRemove{RemoveKeyboard: true}
	bot.BotApi.Send(message)
}

func (bot *Bot) handleAddCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	args := strings.TrimSpace(msg.CommandArguments())
	if args != "" {
		bot.searchAndSelectShow(args, msg.From.ID, chatID)
	} else {
		bot.reply(chatID, "Enter show name:")
		bot.setState(msg.From.ID, StateAwaitingShowName)
	}
}

func (bot *Bot) searchAndSelectShow(query string, userID int64, chatID int64) {
	if query == "" {
		bot.reply(chatID, "Enter show name")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if results, err := SearchShow(ctx, query); err != nil {
		bot.reply(chatID, "Error searching show "+query)
	} else {
		if len(results) == 0 {
			bot.reply(chatID, "No shows found for: "+query)
			return
		}

		// Limit to top 5 results
		max := min(5, len(results))

		var rows [][]tgbotapi.InlineKeyboardButton
		for i := 0; i < max; i++ {
			trimmed := trimString(results[i].Name, 25)
			label := fmt.Sprintf("%d. %s (%s)", i+1, trimmed, safeString(results[i].Premiered))
			cb := fmt.Sprintf("acceptShowName:%d", i+1)
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(label, cb)))
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cancel")))
		inlineMarkup := tgbotapi.NewInlineKeyboardMarkup(rows...)

		bot.withUserContext(userID, func(ctx *UserContext) {
			ctx.SearchResults = results
			ctx.State = StateAwaitingShowSelection
		})

		listText := "Pick the show you want to add:"
		m := tgbotapi.NewMessage(chatID, listText)
		m.ReplyMarkup = inlineMarkup
		bot.BotApi.Send(m)
	}
}

func (bot *Bot) acceptShowName(msg *tgbotapi.Message) {
	query := msg.Text
	chatID := msg.Chat.ID
	userID := msg.From.ID
	bot.searchAndSelectShow(query, userID, chatID)
}

func (bot *Bot) acceptShowNameCallback(cb *tgbotapi.CallbackQuery, param string) {
	log.Printf("callbackAcceptShowName: parameter: %s", param)
	idx, err := strconv.Atoi(param)
	if err != nil {
		log.Printf("callbackAcceptShowName: invalid parameter: %s", param)
		return
	}

	userID := cb.From.ID
	msg := cb.Message

	userCtx := bot.getUserContext(userID)
	log.Printf("acceptShowNameCallback userCtx = %v, userID = %d", userCtx, userID)
	if userCtx == nil || len(userCtx.SearchResults) == 0 {
		bot.reply(msg.Chat.ID, "No search results found. Please start over.")
		bot.clearState(userID)
		return
	}

	showSearchResult := userCtx.SearchResults[idx-1]

	internalID, err := addShow(
		bot.DB, userID, showSearchResult.Name, "tvmaze", showSearchResult.ID,
	)
	if err != nil {
		log.Printf("Error adding show: %s\n", err)
		bot.reply(msg.Chat.ID, "Error adding show, please try again later.")
		return
	}

	bot.withUserContext(userID, func(ctx *UserContext) {
		ctx.SelectedInternalID = internalID
		ctx.SelectedProviderID = showSearchResult.ID
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	episodes, err := FetchEpisodes(ctx, showSearchResult.ID)
	if err != nil {
		bot.reply(msg.Chat.ID, fmt.Sprintf("Episode fetching failed: %s", err))
		return
	}

	for _, episode := range episodes {
		showIdStr := strconv.Itoa(showSearchResult.ID)
		episodeIdStr := strconv.Itoa(episode.ID)
		airstampTime, err := time.Parse(time.RFC3339, episode.Airstamp)
		if err != nil {
			return
		}
		err = upsertEpisode(
			bot.DB, "tvmaze", showIdStr, episodeIdStr, episode.Name, episode.Season,
			episode.Number, episode.Airdate, episode.Airtime, airstampTime)
		if err != nil {
			return
		}
	}

	// Get seasons for the show
	seasons, err := getSeasons(bot.DB, strconv.Itoa(showSearchResult.ID))
	if err != nil {
		bot.reply(msg.Chat.ID, "Error fetching seasons")
		return
	}

	if len(seasons) == 1 {
		// Skip season selection, go directly to episode selection
		bot.withUserContext(userID, func(ctx *UserContext) {
			ctx.SelectedSeason = seasons[0]
			ctx.State = StateAwaitingSeasonEpisode
		})
		// Get episodes for the single season
		episodes, err := getEpisodesBySeason(bot.DB, strconv.Itoa(showSearchResult.ID), seasons[0])
		if err != nil {
			bot.reply(msg.Chat.ID, "Error fetching episodes")
			return
		}
		var rows [][]tgbotapi.InlineKeyboardButton
		for _, episode := range episodes {
			label := fmt.Sprintf("%d. %s", episode.Number, episode.Title)
			cbData := fmt.Sprintf("selectEpisode:%d", episode.Number)
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(label, cbData)))
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cancel")))
		inlineMarkup := tgbotapi.NewInlineKeyboardMarkup(rows...)
		text := fmt.Sprintf("TV show \"%s\" added. Which episode of season %d are you on?", showSearchResult.Name, seasons[0])
		editMsg := tgbotapi.NewEditMessageText(msg.Chat.ID, msg.MessageID, text)
		editMsg.ReplyMarkup = &inlineMarkup
		bot.BotApi.Send(editMsg)
	} else {
		var rows [][]tgbotapi.InlineKeyboardButton
		for _, season := range seasons {
			label := fmt.Sprintf("Season %d", season)
			cb := fmt.Sprintf("selectSeason:%d", season)
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(label, cb)))
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cancel")))
		inlineMarkup := tgbotapi.NewInlineKeyboardMarkup(rows...)
		bot.withUserContext(userID, func(ctx *UserContext) {
			ctx.State = StateAwaitingSeasonEpisode
		})
		text := fmt.Sprintf("TV show \"%s\" added. Which season are you on?", showSearchResult.Name)
		editMsg := tgbotapi.NewEditMessageText(msg.Chat.ID, msg.MessageID, text)
		editMsg.ReplyMarkup = &inlineMarkup
		bot.BotApi.Send(editMsg)
	}
	cb_response := tgbotapi.NewCallback(cb.ID, cb.Data)
	bot.BotApi.Request(cb_response)
}

func (bot *Bot) acceptSeasonEpisode(msg *tgbotapi.Message) {
	text := strings.TrimSpace(msg.Text)
	userID := msg.From.ID
	chatID := msg.Chat.ID
	parts := strings.Split(text, " ")
	if len(parts) != 2 {
		bot.reply(chatID, "Wrong format of the reply, it should be: #season #episode")
		return
	}
	season, err := strconv.Atoi(parts[0])
	if err != nil {
		bot.reply(chatID, "Wrong #season")
		return
	}
	number, err := strconv.Atoi(parts[1])
	if err != nil {
		bot.reply(chatID, "Wrong #episode")
		return
	}

	userCtx := bot.getUserContext(userID)
	if userCtx == nil {
		bot.reply(chatID, "Session expired. Please start over with /add")
		bot.clearState(userID)
		return
	}

	// Find the current episode
	currentEpisode, err := findEpisodeByNumber(
		bot.DB, strconv.Itoa(userCtx.SelectedProviderID), season, number,
	)
	if err != nil {
		bot.reply(chatID, "I can't find the episode you specified")
		return
	}

	// Find the next episode
	nextEpisode, err := findEpisodeByNumber(
		bot.DB, strconv.Itoa(userCtx.SelectedProviderID), season, number+1,
	)
	if err != nil {
		// No next episode found, record the current episode as last watched
		err = updateLastWatchedEpisode(bot.DB, userCtx.SelectedInternalID, currentEpisode.ID)
		if err != nil {
			bot.reply(chatID, "Failed to update progress")
			return
		}
		bot.reply(chatID, fmt.Sprintf("Marked episode \"%s\" as watched", currentEpisode.Title))
		return
	}

	// Check if next episode has an air date and if it's in the future
	if !nextEpisode.AiredAtUTC.IsZero() && nextEpisode.AiredAtUTC.After(time.Now()) {
		// Update last watched to current episode and create reminder for future episode
		err = updateLastWatchedEpisode(bot.DB, userCtx.SelectedInternalID, currentEpisode.ID)
		if err != nil {
			bot.reply(chatID, "Failed to update progress")
			return
		}
		err = createReminder(
			bot.DB, userID, int(userCtx.SelectedInternalID), nextEpisode.ID,
			nextEpisode.AiredAtUTC, chatID,
		)
		if err != nil {
			bot.reply(chatID, "Failed to create reminder")
			return
		}
		bot.reply(
			chatID,
			fmt.Sprintf(
				"Reminder created for next episode \"%s\", which is expected to air on %s",
				nextEpisode.Title, nextEpisode.AiredAtUTC.Format("Mon Jan 2, 15:04"),
			))
	} else {
		// Next episode has already aired or no air date, record current episode as last watched
		err = updateLastWatchedEpisode(bot.DB, userCtx.SelectedInternalID, currentEpisode.ID)
		if err != nil {
			bot.reply(chatID, "Failed to update progress")
			return
		}
		bot.reply(chatID, fmt.Sprintf("Marked episode \"%s\" as watched", currentEpisode.Title))
	}
}

func (bot *Bot) handleSeasonCallback(cb *tgbotapi.CallbackQuery, param string) {
	season, err := strconv.Atoi(param)
	if err != nil {
		log.Printf("handleSeasonCallback: invalid season: %s", param)
		return
	}

	userID := cb.From.ID
	msg := cb.Message

	userCtx := bot.getUserContext(userID)
	if userCtx == nil {
		bot.reply(msg.Chat.ID, "Session expired. Please start over with /add")
		bot.clearState(userID)
		return
	}

	// Store selected season
	bot.withUserContext(userID, func(ctx *UserContext) {
		ctx.SelectedSeason = season
	})

	// Get episodes for the selected season
	episodes, err := getEpisodesBySeason(bot.DB, strconv.Itoa(userCtx.SelectedProviderID), season)
	if err != nil {
		bot.reply(msg.Chat.ID, "Error fetching episodes")
		return
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, episode := range episodes {
		label := fmt.Sprintf("%d. %s", episode.Number, episode.Title)
		cbData := fmt.Sprintf("selectEpisode:%d", episode.Number)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(label, cbData)))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cancel")))
	inlineMarkup := tgbotapi.NewInlineKeyboardMarkup(rows...)

	text := fmt.Sprintf("Which episode of season %d are you on?", season)
	editMsg := tgbotapi.NewEditMessageText(msg.Chat.ID, msg.MessageID, text)
	editMsg.ReplyMarkup = &inlineMarkup
	bot.BotApi.Send(editMsg)

	cb_response := tgbotapi.NewCallback(cb.ID, cb.Data)
	bot.BotApi.Request(cb_response)
}

func (bot *Bot) handleEpisodeCallback(cb *tgbotapi.CallbackQuery, param string) {
	episodeNumber, err := strconv.Atoi(param)
	if err != nil {
		log.Printf("handleEpisodeCallback: invalid episode number: %s", param)
		return
	}

	userID := cb.From.ID
	msg := cb.Message

	userCtx := bot.getUserContext(userID)
	if userCtx == nil {
		bot.reply(msg.Chat.ID, "Session expired. Please start over with /add")
		bot.clearState(userID)
		return
	}

	season := userCtx.SelectedSeason

	var resultText string

	// Find the current episode
	currentEpisode, err := findEpisodeByNumber(
		bot.DB, strconv.Itoa(userCtx.SelectedProviderID), season, episodeNumber,
	)
	if err != nil {
		bot.reply(msg.Chat.ID, "I can't find the episode you specified")
		bot.clearState(userID)
		return
	}

	// Find the next episode
	nextEpisode, err := findEpisodeByNumber(
		bot.DB, strconv.Itoa(userCtx.SelectedProviderID), season, episodeNumber+1,
	)
	if err != nil {
		// No next episode found, record the current episode as last watched
		err = updateLastWatchedEpisode(bot.DB, userCtx.SelectedInternalID, currentEpisode.ID)
		if err != nil {
			resultText = "Failed to update progress"
		} else {
			resultText = fmt.Sprintf("Marked episode \"%s\" as watched", currentEpisode.Title)
		}
	} else {
		// Check if next episode has an air date and if it's in the future
		if !nextEpisode.AiredAtUTC.IsZero() && nextEpisode.AiredAtUTC.After(time.Now()) {
			// Update last watched to current episode and create reminder for future episode
			err = updateLastWatchedEpisode(bot.DB, userCtx.SelectedInternalID, currentEpisode.ID)
			if err != nil {
				resultText = "Failed to update progress"
			} else {
				err = createReminder(
					bot.DB, userID, int(userCtx.SelectedInternalID), nextEpisode.ID,
					nextEpisode.AiredAtUTC, msg.Chat.ID,
				)
				if err != nil {
					resultText = "Failed to create reminder"
				} else {
					resultText = fmt.Sprintf(
						"Reminder created for next episode \"%s\", which is expected to air on %s",
						nextEpisode.Title, nextEpisode.AiredAtUTC.Format("Mon Jan 2, 15:04"),
					)
				}
			}
		} else {
			// Next episode has already aired or no air date, record current episode as last watched
			err = updateLastWatchedEpisode(bot.DB, userCtx.SelectedInternalID, currentEpisode.ID)
			if err != nil {
				resultText = "Failed to update progress"
			} else {
				resultText = fmt.Sprintf("Marked episode \"%s\" as watched", currentEpisode.Title)
			}
		}
	}

	// Edit the message to show the result
	editMsg := tgbotapi.NewEditMessageText(msg.Chat.ID, msg.MessageID, resultText)
	editMsg.ReplyMarkup = nil
	bot.BotApi.Send(editMsg)

	bot.clearState(userID)

	cb_response := tgbotapi.NewCallback(cb.ID, cb.Data)
	bot.BotApi.Request(cb_response)
}

func (bot *Bot) handleCancelCallback(cb *tgbotapi.CallbackQuery) {
	userID := cb.From.ID
	msg := cb.Message

	bot.clearState(userID)
	editMsg := tgbotapi.NewEditMessageText(msg.Chat.ID, msg.MessageID, "Operation cancelled.")
	editMsg.ReplyMarkup = nil
	bot.BotApi.Send(editMsg)

	cb_response := tgbotapi.NewCallback(cb.ID, cb.Data)
	bot.BotApi.Request(cb_response)
}

func (bot *Bot) acceptSeasonEpisodeCallback(cb *tgbotapi.CallbackQuery, param string) {
	// This method is now replaced by handleSeasonCallback and handleEpisodeCallback
}

func safeString(s *string) string {
	if s == nil {
		return "N/A"
	}
	return *s
}

func trimString(s string, maxLen int) string {
	runeCount := utf8.RuneCountInString(s)
	if runeCount <= maxLen {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxLen-3]) + "..."
}

func dedent(s string) string {
	lines := strings.Split(s, "\n")
	minIndent := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		count := len(line) - len(strings.TrimLeft(line, " \t"))
		if minIndent == -1 || count < minIndent {
			minIndent = count
		}
	}
	if minIndent > 0 {
		for i, line := range lines {
			if len(line) >= minIndent {
				lines[i] = line[minIndent:]
			}
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
