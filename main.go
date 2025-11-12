package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type Handler struct {
	Bot *Bot
	DB  *sql.DB
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
		{Command: "add", Description: "Add a TV show to track"},
		{Command: "shows", Description: "List your tracked shows"},
		{Command: "help", Description: "Show help information"},
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

	handler := &Handler{
		Bot: bot,
		DB:  db,
	}

	updateConfig := tgbotapi.NewUpdate(0)
	updateConfig.Timeout = 30

	updates := botApi.GetUpdatesChan(updateConfig)

	go reminderLoop(bot, db, context.Background())

	for update := range updates {
		if update.CallbackQuery != nil {
			handler.handleCallback(update.CallbackQuery)
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
			handler.handleCommand(msg)
			continue
		case state == StateAwaitingShowName:
			handler.acceptShowName(msg)
		case state == StateAwaitingSeasonEpisode:
			handler.acceptSeasonEpisode(msg)
		}
	}
}

func reminderLoop(bot *Bot, db *sql.DB, ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			reminders, err := getDueReminders(db)
			if err != nil {
				log.Printf("reminderLoop: getDueReminders error: %v", err)
				continue
			}
			if len(reminders) != 0 {
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

				if err := markReminderSent(db, r); err != nil {
					log.Printf("reminderLoop: failed to mark reminder sent: %v", err)
				}
			}
		case <-ctx.Done():
			log.Println("reminderLoop: context cancelled, exiting")
			return
		}
	}
}

func (handler *Handler) handleCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	command := msg.Command()
	// args := strings.TrimSpace(msg.CommandArguments())

	switch command {
	case "start":
		startText := dedent(`
		Hello! I'm a bot that helps you track your TV shows and notify you when new episodes air.

		/add - Add a TV show to track
		/shows - List your tracked shows
		`)
		handler.Bot.reply(chatID, startText)
	case "help":
		helpText := dedent(`
		Commands:

		/add <show>
		/shows - list your shows
		/help - show this help
		`)
		handler.Bot.reply(chatID, helpText)
	case "add":
		handler.handleAddCommand(msg)
	case "shows":
		handler.handleShowsCommand(msg)
	default:
		handler.Bot.reply(chatID, "Unknown command: /"+command)
	}
}

func (handler *Handler) handleCallback(cb *tgbotapi.CallbackQuery) {
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
		handler.acceptShowNameCallback(cb, param)
	case "selectSeason":
		handler.handleSeasonCallback(cb, param)
	case "selectEpisode":
		handler.handleEpisodeCallback(cb, param)
	case "selectShow":
		handler.handleShowCallback(cb, param)
	case "backToShows":
		handler.handleBackToShowsCallback(cb)
	case "toggleNotifications":
		handler.handleToggleNotificationsCallback(cb, param)
	case "markNextWatched":
		handler.handleMarkNextWatchedCallback(cb, param)
	case "cancel":
		handler.handleCancelCallback(cb)
	}
}

func (handler *Handler) handleAddCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	args := strings.TrimSpace(msg.CommandArguments())
	if args != "" {
		handler.searchAndSelectShow(args, msg.From.ID, chatID)
	} else {
		handler.Bot.reply(chatID, "Enter show name:")
		handler.Bot.setState(msg.From.ID, StateAwaitingShowName)
	}
}

func (handler *Handler) handleShowsCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	shows, err := listShowsWithProgress(handler.DB, msg.From.ID)
	if err != nil {
		handler.Bot.reply(chatID, "Error: can't list shows at this time")
		return
	}
	if len(shows) == 0 {
		handler.Bot.reply(chatID, "You have no shows yet. Use /add <show> to add one.")
		return
	}
	handler.Bot.withUserContext(msg.From.ID, func(ctx *UserContext) {
		ctx.ShowsList = shows
	})
	var rows [][]tgbotapi.InlineKeyboardButton
	for i, show := range shows {
		line := show.Name
		if show.NotificationsEnabled && show.NextAirDate.Valid {
			line = "🔔 " + line
		}
		if show.Season.Valid && show.Episode.Valid {
			line += fmt.Sprintf(" (S%02dE%02d)", show.Season.Int32, show.Episode.Int32)
		}
		if show.NextEpisodeSeason.Valid && show.NextEpisodeNumber.Valid {
			if show.NextAirDate.Valid && show.NextAirDate.Time.After(time.Now()) {
				line += fmt.Sprintf(" - Next: %s", show.NextAirDate.Time.Format("Mon Jan 2, 15:04"))
			} else {
				line += " - Next episode available!"
			}
		}
		cbData := fmt.Sprintf("selectShow:%d", i)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(line, cbData)))
	}
	inlineMarkup := tgbotapi.NewInlineKeyboardMarkup(rows...)
	handler.Bot.reply(chatID, "Your shows:", ReplyOptions{ReplyMarkup: &inlineMarkup})
}

func (handler *Handler) searchAndSelectShow(query string, userID int64, chatID int64) {
	if query == "" {
		handler.Bot.reply(chatID, "Enter show name")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if results, err := SearchShow(ctx, query); err != nil {
		handler.Bot.reply(chatID, "Error searching show "+query)
	} else {
		if len(results) == 0 {
			handler.Bot.reply(chatID, "No shows found for: "+query)
			return
		}

		// Limit to top 5 results
		max := min(5, len(results))

		var rows [][]tgbotapi.InlineKeyboardButton
		for i := range max {
			trimmed := trimString(results[i].Name, 25)
			label := fmt.Sprintf("%d. %s (%s)", i+1, trimmed, safeString(results[i].Premiered))
			cb := fmt.Sprintf("acceptShowName:%d", i+1)
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(label, cb)))
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cancel")))
		inlineMarkup := tgbotapi.NewInlineKeyboardMarkup(rows...)

		handler.Bot.withUserContext(userID, func(ctx *UserContext) {
			ctx.SearchResults = results
			ctx.State = StateAwaitingShowSelection
		})

		listText := "Pick the show you want to add:"
		handler.Bot.reply(chatID, listText, ReplyOptions{ReplyMarkup: &inlineMarkup})
	}
}

func (handler *Handler) acceptShowName(msg *tgbotapi.Message) {
	query := msg.Text
	chatID := msg.Chat.ID
	userID := msg.From.ID
	handler.searchAndSelectShow(query, userID, chatID)
}

func (handler *Handler) acceptShowNameCallback(cb *tgbotapi.CallbackQuery, param string) {
	log.Printf("callbackAcceptShowName: parameter: %s", param)
	idx, err := strconv.Atoi(param)
	if err != nil {
		log.Printf("callbackAcceptShowName: invalid parameter: %s", param)
		return
	}

	userID := cb.From.ID
	msg := cb.Message

	userCtx := handler.Bot.getUserContext(userID)
	log.Printf("acceptShowNameCallback userCtx = %v, userID = %d", userCtx, userID)
	if userCtx == nil || len(userCtx.SearchResults) == 0 {
		handler.Bot.reply(msg.Chat.ID, "No search results found. Please start over.")
		handler.Bot.clearState(userID)
		return
	}

	showSearchResult := userCtx.SearchResults[idx-1]

	internalID, err := addShow(
		handler.DB, userID, showSearchResult.Name, "tvmaze", showSearchResult.ID,
	)
	if err != nil {
		log.Printf("Error adding show: %s\n", err)
		handler.Bot.reply(msg.Chat.ID, "Error adding show, please try again later.")
		return
	}

	handler.Bot.withUserContext(userID, func(ctx *UserContext) {
		ctx.SelectedInternalID = internalID
		ctx.SelectedProviderID = showSearchResult.ID
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	episodes, err := FetchEpisodes(ctx, showSearchResult.ID)
	if err != nil {
		handler.Bot.reply(msg.Chat.ID, fmt.Sprintf("Episode fetching failed: %s", err))
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
			handler.DB, "tvmaze", showIdStr, episodeIdStr, episode.Name, episode.Season,
			episode.Number, episode.Airdate, episode.Airtime, airstampTime)
		if err != nil {
			return
		}
	}

	// Get seasons for the show
	seasons, err := getSeasons(handler.DB, strconv.Itoa(showSearchResult.ID))
	if err != nil {
		handler.Bot.reply(msg.Chat.ID, "Error fetching seasons")
		return
	}

	if len(seasons) == 1 {
		// Skip season selection, go directly to episode selection
		handler.Bot.withUserContext(userID, func(ctx *UserContext) {
			ctx.SelectedSeason = seasons[0]
			ctx.State = StateAwaitingSeasonEpisode
		})
		// Get episodes for the single season
		episodes, err := getEpisodesBySeason(handler.DB, strconv.Itoa(showSearchResult.ID), seasons[0])
		if err != nil {
			handler.Bot.reply(msg.Chat.ID, "Error fetching episodes")
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
		handler.Bot.reply(msg.Chat.ID, text, ReplyOptions{ReplyMarkup: &inlineMarkup, EditMessageID: msg.MessageID})
	} else {
		var rows [][]tgbotapi.InlineKeyboardButton
		for _, season := range seasons {
			label := fmt.Sprintf("Season %d", season)
			cb := fmt.Sprintf("selectSeason:%d", season)
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(label, cb)))
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cancel")))
		inlineMarkup := tgbotapi.NewInlineKeyboardMarkup(rows...)
		handler.Bot.withUserContext(userID, func(ctx *UserContext) {
			ctx.State = StateAwaitingSeasonEpisode
		})
		text := fmt.Sprintf("TV show \"%s\" added. Which season are you on?", showSearchResult.Name)
		handler.Bot.reply(msg.Chat.ID, text, ReplyOptions{ReplyMarkup: &inlineMarkup, EditMessageID: msg.MessageID})
	}
	cb_response := tgbotapi.NewCallback(cb.ID, "")
	handler.Bot.BotApi.Request(cb_response)
}

func (handler *Handler) acceptSeasonEpisode(msg *tgbotapi.Message) {
	text := strings.TrimSpace(msg.Text)
	userID := msg.From.ID
	chatID := msg.Chat.ID
	parts := strings.Split(text, " ")
	if len(parts) != 2 {
		handler.Bot.reply(chatID, "Wrong format of the reply, it should be: #season #episode")
		return
	}
	season, err := strconv.Atoi(parts[0])
	if err != nil {
		handler.Bot.reply(chatID, "Wrong #season")
		return
	}
	number, err := strconv.Atoi(parts[1])
	if err != nil {
		handler.Bot.reply(chatID, "Wrong #episode")
		return
	}

	userCtx := handler.Bot.getUserContext(userID)
	if userCtx == nil {
		handler.Bot.reply(chatID, "Session expired. Please start over with /add")
		handler.Bot.clearState(userID)
		return
	}

	// Find the current episode
	currentEpisode, err := findEpisodeByNumber(
		handler.DB, strconv.Itoa(userCtx.SelectedProviderID), season, number,
	)
	if err != nil {
		handler.Bot.reply(chatID, "I can't find the episode you specified")
		return
	}

	// Find the next episode
	nextEpisode, err := findEpisodeByNumber(
		handler.DB, strconv.Itoa(userCtx.SelectedProviderID), season, number+1,
	)
	if err != nil {
		// No next episode found, record the current episode as last watched
		err = updateLastWatchedEpisode(handler.DB, userCtx.SelectedInternalID, currentEpisode.ID)
		if err != nil {
			handler.Bot.reply(chatID, "Failed to update progress")
			return
		}
		// Get show name from database
		var showName string
		err = handler.DB.QueryRow(`SELECT name FROM shows WHERE id = ?`, userCtx.SelectedInternalID).Scan(&showName)
		if err != nil {
			handler.Bot.reply(chatID, "Failed to get show name")
			return
		}
		handler.Bot.reply(chatID, fmt.Sprintf("\"%s\" added. Marked as watched up to S%02dE%02d.", showName, season, number))
		return
	}

	// Check if next episode has an air date and if it's in the future
	if !nextEpisode.AiredAtUTC.IsZero() && nextEpisode.AiredAtUTC.After(time.Now()) {
		// Update last watched to current episode and create reminder for future episode
		err = updateLastWatchedEpisode(handler.DB, userCtx.SelectedInternalID, currentEpisode.ID)
		if err != nil {
			handler.Bot.reply(chatID, "Failed to update progress")
			return
		}
		err = createReminder(
			handler.DB, userID, int(userCtx.SelectedInternalID), nextEpisode.ID,
			nextEpisode.AiredAtUTC, chatID,
		)
		if err != nil {
			handler.Bot.reply(chatID, "Failed to create reminder")
			return
		}
		// Get show name from database
		var showName string
		err = handler.DB.QueryRow(`SELECT name FROM shows WHERE id = ?`, userCtx.SelectedInternalID).Scan(&showName)
		if err != nil {
			handler.Bot.reply(chatID, "Failed to get show name")
			return
		}
		handler.Bot.reply(
			chatID,
			fmt.Sprintf(
				"\"%s\" added. Marked as watched up to S%02dE%02d. Next episode \"%s\" is expected to air on %s. I'll notify you when it airs.",
				showName, season, number, nextEpisode.Title, nextEpisode.AiredAtUTC.Format("Mon Jan 2, 15:04"),
			))
	} else {
		// Next episode has already aired or no air date, record current episode as last watched
		err = updateLastWatchedEpisode(handler.DB, userCtx.SelectedInternalID, currentEpisode.ID)
		if err != nil {
			handler.Bot.reply(chatID, "Failed to update progress")
			return
		}
		// Get show name from database
		var showName string
		err = handler.DB.QueryRow(`SELECT name FROM shows WHERE id = ?`, userCtx.SelectedInternalID).Scan(&showName)
		if err != nil {
			handler.Bot.reply(chatID, "Failed to get show name")
			return
		}
		handler.Bot.reply(chatID, fmt.Sprintf("\"%s\" added. Marked as watched up to S%02dE%02d.", showName, season, number))
	}
}

func (handler *Handler) handleSeasonCallback(cb *tgbotapi.CallbackQuery, param string) {
	season, err := strconv.Atoi(param)
	if err != nil {
		log.Printf("handleSeasonCallback: invalid season: %s", param)
		return
	}

	userID := cb.From.ID
	msg := cb.Message

	userCtx := handler.Bot.getUserContext(userID)
	if userCtx == nil {
		handler.Bot.reply(msg.Chat.ID, "Session expired. Please start over with /add")
		handler.Bot.clearState(userID)
		return
	}

	// Store selected season
	handler.Bot.withUserContext(userID, func(ctx *UserContext) {
		ctx.SelectedSeason = season
	})

	// Get episodes for the selected season
	episodes, err := getEpisodesBySeason(handler.DB, strconv.Itoa(userCtx.SelectedProviderID), season)
	if err != nil {
		handler.Bot.reply(msg.Chat.ID, "Error fetching episodes")
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
	handler.Bot.reply(msg.Chat.ID, text, ReplyOptions{ReplyMarkup: &inlineMarkup, EditMessageID: msg.MessageID})

	cb_response := tgbotapi.NewCallback(cb.ID, "")
	handler.Bot.BotApi.Request(cb_response)
}

func (handler *Handler) handleEpisodeCallback(cb *tgbotapi.CallbackQuery, param string) {
	episodeNumber, err := strconv.Atoi(param)
	if err != nil {
		log.Printf("handleEpisodeCallback: invalid episode number: %s", param)
		return
	}

	userID := cb.From.ID
	msg := cb.Message

	userCtx := handler.Bot.getUserContext(userID)
	if userCtx == nil {
		handler.Bot.reply(msg.Chat.ID, "Session expired. Please start over with /add")
		handler.Bot.clearState(userID)
		return
	}

	season := userCtx.SelectedSeason

	var resultText string

	// Find the current episode
	currentEpisode, err := findEpisodeByNumber(
		handler.DB, strconv.Itoa(userCtx.SelectedProviderID), season, episodeNumber,
	)
	if err != nil {
		handler.Bot.reply(msg.Chat.ID, "I can't find the episode you specified")
		handler.Bot.clearState(userID)
		return
	}

	// Find the next episode
	nextEpisode, err := findEpisodeByNumber(
		handler.DB, strconv.Itoa(userCtx.SelectedProviderID), season, episodeNumber+1,
	)
	if err != nil {
		// No next episode found, record the current episode as last watched
		err = updateLastWatchedEpisode(handler.DB, userCtx.SelectedInternalID, currentEpisode.ID)
		if err != nil {
			resultText = "Failed to update progress"
		} else {
			// Get show name from database
			var showName string
			err = handler.DB.QueryRow(`SELECT name FROM shows WHERE id = ?`, userCtx.SelectedInternalID).Scan(&showName)
			if err != nil {
				resultText = "Failed to get show name"
			} else {
				resultText = fmt.Sprintf("\"%s\" added. Marked as watched up to S%02dE%02d.", showName, season, episodeNumber)
			}
		}
	} else {
		// Check if next episode has an air date and if it's in the future
		if !nextEpisode.AiredAtUTC.IsZero() && nextEpisode.AiredAtUTC.After(time.Now()) {
			// Update last watched to current episode and create reminder for future episode
			err = updateLastWatchedEpisode(handler.DB, userCtx.SelectedInternalID, currentEpisode.ID)
			if err != nil {
				resultText = "Failed to update progress"
			} else {
				err = createReminder(
					handler.DB, userID, int(userCtx.SelectedInternalID), nextEpisode.ID,
					nextEpisode.AiredAtUTC, msg.Chat.ID,
				)
				if err != nil {
					resultText = "Failed to create reminder"
				} else {
					// Get show name from database
					var showName string
					err = handler.DB.QueryRow(`SELECT name FROM shows WHERE id = ?`, userCtx.SelectedInternalID).Scan(&showName)
					if err != nil {
						resultText = "Failed to get show name"
					} else {
						resultText = fmt.Sprintf(
							"\"%s\" added. Marked as watched up to S%02dE%02d. Next episode \"%s\" is expected to air on %s. I'll notify you when it airs.",
							showName, season, episodeNumber, nextEpisode.Title, nextEpisode.AiredAtUTC.Format("Mon Jan 2, 15:04"),
						)
					}
				}
			}
		} else {
			// Next episode has already aired or no air date, record current episode as last watched
			err = updateLastWatchedEpisode(handler.DB, userCtx.SelectedInternalID, currentEpisode.ID)
			if err != nil {
				resultText = "Failed to update progress"
			} else {
				// Get show name from database
				var showName string
				err = handler.DB.QueryRow(`SELECT name FROM shows WHERE id = ?`, userCtx.SelectedInternalID).Scan(&showName)
				if err != nil {
					resultText = "Failed to get show name"
				} else {
					resultText = fmt.Sprintf("\"%s\" added. Marked as watched up to S%02dE%02d.", showName, season, episodeNumber)
				}
			}
		}
	}

	// Edit the message to show the result
	handler.Bot.reply(msg.Chat.ID, resultText, ReplyOptions{EditMessageID: msg.MessageID})

	handler.Bot.clearState(userID)

	cb_response := tgbotapi.NewCallback(cb.ID, "")
	handler.Bot.BotApi.Request(cb_response)
}

func (handler *Handler) handleCancelCallback(cb *tgbotapi.CallbackQuery) {
	userID := cb.From.ID
	msg := cb.Message

	handler.Bot.clearState(userID)
	handler.Bot.reply(msg.Chat.ID, "Operation cancelled.", ReplyOptions{EditMessageID: msg.MessageID})

	cb_response := tgbotapi.NewCallback(cb.ID, "")
	handler.Bot.BotApi.Request(cb_response)
}

func (handler *Handler) handleToggleNotificationsCallback(cb *tgbotapi.CallbackQuery, param string) {
	idx, err := strconv.Atoi(param)
	if err != nil {
		log.Printf("handleToggleNotificationsCallback: invalid parameter: %s", param)
		return
	}

	userID := cb.From.ID
	msg := cb.Message

	userCtx := handler.Bot.getUserContext(userID)
	if userCtx == nil || len(userCtx.ShowsList) == 0 {
		handler.Bot.reply(msg.Chat.ID, "No shows found. Please start over with /shows")
		handler.Bot.clearState(userID)
		return
	}

	if idx < 0 || idx >= len(userCtx.ShowsList) {
		handler.Bot.reply(msg.Chat.ID, "Invalid show selection.")
		return
	}

	show := userCtx.ShowsList[idx]

	// Get the show ID from the database
	var showID int64
	err = handler.DB.QueryRow(`
		SELECT id FROM shows
		WHERE user_id = ? AND name = ?
	`, userID, show.Name).Scan(&showID)
	if err != nil {
		handler.Bot.reply(msg.Chat.ID, "Error toggling notifications")
		return
	}

	err = toggleShowNotifications(handler.DB, showID)
	if err != nil {
		handler.Bot.reply(msg.Chat.ID, "Error toggling notifications")
		return
	}

	// Refresh the shows list
	shows, err := listShowsWithProgress(handler.DB, userID)
	if err != nil {
		handler.Bot.reply(msg.Chat.ID, "Error refreshing shows list")
		return
	}
	userCtx.ShowsList = shows

	// Re-show the show details
	handler.handleShowCallback(cb, param)
}

func (handler *Handler) handleMarkNextWatchedCallback(cb *tgbotapi.CallbackQuery, param string) {
	idx, err := strconv.Atoi(param)
	if err != nil {
		log.Printf("handleMarkNextWatchedCallback: invalid parameter: %s", param)
		return
	}

	userID := cb.From.ID
	msg := cb.Message

	userCtx := handler.Bot.getUserContext(userID)
	if userCtx == nil || len(userCtx.ShowsList) == 0 {
		handler.Bot.reply(msg.Chat.ID, "No shows found. Please start over with /shows")
		handler.Bot.clearState(userID)
		return
	}

	if idx < 0 || idx >= len(userCtx.ShowsList) {
		handler.Bot.reply(msg.Chat.ID, "Invalid show selection.")
		return
	}

	show := userCtx.ShowsList[idx]

	// Get the show ID and provider_show_id from the database
	var showID int64
	var providerShowID string
	err = handler.DB.QueryRow(`
		SELECT id, provider_show_id FROM shows
		WHERE user_id = ? AND name = ?
	`, userID, show.Name).Scan(&showID, &providerShowID)
	if err != nil {
		handler.Bot.reply(msg.Chat.ID, "Error finding show")
		return
	}

	// Find the next episode
	nextEpisode, err := findNextEpisode(handler.DB, providerShowID, show.Season, show.Episode)
	if err != nil {
		handler.Bot.reply(msg.Chat.ID, "No next episode found.")
		return
	}

	// Update last watched to the next episode
	err = updateLastWatchedEpisode(handler.DB, showID, nextEpisode.ID)
	if err != nil {
		handler.Bot.reply(msg.Chat.ID, "Error updating progress")
		return
	}

	// Refresh the shows list
	shows, err := listShowsWithProgress(handler.DB, userID)
	if err != nil {
		handler.Bot.reply(msg.Chat.ID, "Error refreshing shows list")
		return
	}
	userCtx.ShowsList = shows

	// Re-show the show details
	handler.handleShowCallback(cb, param)
}

func (handler *Handler) handleBackToShowsCallback(cb *tgbotapi.CallbackQuery) {
	userID := cb.From.ID
	msg := cb.Message

	userCtx := handler.Bot.getUserContext(userID)
	if userCtx == nil || len(userCtx.ShowsList) == 0 {
		handler.Bot.reply(msg.Chat.ID, "No shows found. Please start over with /shows")
		handler.Bot.clearState(userID)
		return
	}

	shows := userCtx.ShowsList
	var rows [][]tgbotapi.InlineKeyboardButton
	for i, show := range shows {
		line := show.Name
		if show.NotificationsEnabled && show.NextAirDate.Valid {
			line = "🔔 " + line
		}
		if show.Season.Valid && show.Episode.Valid {
			line += fmt.Sprintf(" (S%02dE%02d)", show.Season.Int32, show.Episode.Int32)
		}
		if show.NextEpisodeSeason.Valid && show.NextEpisodeNumber.Valid {
			if show.NextAirDate.Valid && show.NextAirDate.Time.After(time.Now()) {
				line += fmt.Sprintf(" - Next: %s", show.NextAirDate.Time.Format("Mon Jan 2, 15:04"))
			} else {
				line += " - next episode is out"
			}
		}
		cbData := fmt.Sprintf("selectShow:%d", i)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(line, cbData)))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "cancel")))
	inlineMarkup := tgbotapi.NewInlineKeyboardMarkup(rows...)

	handler.Bot.reply(msg.Chat.ID, "Your shows:", ReplyOptions{ReplyMarkup: &inlineMarkup, EditMessageID: msg.MessageID})

	cb_response := tgbotapi.NewCallback(cb.ID, "")
	handler.Bot.BotApi.Request(cb_response)
}

func (handler *Handler) handleShowCallback(cb *tgbotapi.CallbackQuery, param string) {
	idx, err := strconv.Atoi(param)
	if err != nil {
		log.Printf("handleShowCallback: invalid parameter: %s", param)
		return
	}

	userID := cb.From.ID
	msg := cb.Message

	userCtx := handler.Bot.getUserContext(userID)
	if userCtx == nil || len(userCtx.ShowsList) == 0 {
		handler.Bot.reply(msg.Chat.ID, "No shows found. Please start over with /shows.")
		handler.Bot.clearState(userID)
		return
	}

	if idx < 0 || idx >= len(userCtx.ShowsList) {
		handler.Bot.reply(msg.Chat.ID, "Invalid show selection.")
		return
	}

	show := userCtx.ShowsList[idx]

	var infoText string
	infoText += fmt.Sprintf("<b>%s</b>\n\n", show.Name)
	if show.Season.Valid && show.Episode.Valid {
		infoText += fmt.Sprintf("Current episode: S%02dE%02d\n", show.Season.Int32, show.Episode.Int32)
	} else {
		infoText += "Current episode: Not set\n"
	}
	if show.NextAirDate.Valid {
		infoText += fmt.Sprintf(
			"Next episode air date: %s\n",
			show.NextAirDate.Time.Format("Mon Jan 2, 15:04"),
		)
	} else {
		infoText += "Next episode air date: N/A\n"
	}
	notificationsStatus := "Enabled"
	if !show.NotificationsEnabled {
		notificationsStatus = "Disabled"
	}
	infoText += fmt.Sprintf("Notifications: %s\n", notificationsStatus)

	var rows [][]tgbotapi.InlineKeyboardButton
	toggleText := "Disable Notifications"
	if !show.NotificationsEnabled {
		toggleText = "Enable Notifications"
	}
	rows = append(
		rows,
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				toggleText, fmt.Sprintf("toggleNotifications:%d", idx))))
	rows = append(
		rows,
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				"Mark next as watched", fmt.Sprintf("markNextWatched:%d", idx))))
	rows = append(
		rows,
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("<< Back to shows list", "backToShows")))
	inlineMarkup := tgbotapi.NewInlineKeyboardMarkup(rows...)

	handler.Bot.reply(msg.Chat.ID, infoText, ReplyOptions{ReplyMarkup: &inlineMarkup, ParseMode: "HTML", EditMessageID: msg.MessageID})

	cb_response := tgbotapi.NewCallback(cb.ID, "")
	handler.Bot.BotApi.Request(cb_response)
}
