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
	SelectedShowName   string
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
		case state == StateAwaitingShowSelection:
			bot.acceptSearchResult(msg)
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
		shows, err := listShows(bot.DB, msg.From.ID)
		if err != nil {
			bot.reply(chatID, "Error: can't list shows at this time")
			return
		}
		if len(shows) == 0 {
			bot.reply(chatID, "You have no shows yet. Use /add <show> to add one.")
		} else {
			bot.reply(chatID, "Your shows:\n"+strings.Join(shows, "\n"))
		}
	default:
		bot.reply(chatID, "Unknown command: /"+command)
	}
}

func (bot *Bot) reminderLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
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
						"New episode \"%d. %s\" of \"%s\" is coming out today!",
						r.EpisodeNumber, r.EpisodeTitle, r.ShowName,
					),
				)
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
	bot.BotApi.Send(message)
}

func (bot *Bot) handleAddCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	bot.reply(chatID, "Enter show name:")
	bot.setState(msg.From.ID, StateAwaitingShowName)
}

func (bot *Bot) acceptShowName(msg *tgbotapi.Message) {
	query := msg.Text
	chatID := msg.Chat.ID
	userID := msg.From.ID
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
			bot.reply(msg.Chat.ID, "Error searching show: "+query)
			return
		}

		replyText := "Found:\n"
		for i, r := range results[:min(5, len(results))] {
			replyText += fmt.Sprintf("%d. %s (%s)\n", i+1, r.Name, safeString(r.Premiered))
		}

		bot.withUserContext(userID, func(ctx *UserContext) {
			ctx.SearchResults = results
			ctx.State = StateAwaitingShowSelection
		})

		replyText += "Send the number:"
		bot.reply(chatID, replyText)
	}
}

func (bot *Bot) acceptSearchResult(msg *tgbotapi.Message) {
	userID := msg.From.ID
	choice := strings.TrimSpace(msg.Text)
	idx, err := strconv.Atoi(choice)
	if err != nil {
		bot.reply(msg.Chat.ID, "Cancelling show selection.")
		bot.setState(userID, StateNone)
		return
	}

	userCtx := bot.getUserContext(userID)
	if userCtx == nil || len(userCtx.SearchResults) == 0 {
		bot.reply(msg.Chat.ID, "No search results found. Please start over.")
		bot.clearState(userID)
		return
	}

	if idx < 1 || idx > 5 {
		bot.reply(msg.Chat.ID, "Invalid number.")
		return
	}

	showSearchResult := userCtx.SearchResults[idx-1]

	internalID, err := addShow(
		bot.DB, userID, showSearchResult.Name, "tvmaze", showSearchResult.ID,
	)
	if err != nil {
		log.Printf("Error adding show: %s\n", err)
		return
	}

	bot.withUserContext(userID, func(ctx *UserContext) {
		ctx.SelectedInternalID = internalID
		ctx.SelectedProviderID = showSearchResult.ID
		ctx.SelectedShowName = showSearchResult.Name
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

	bot.reply(
		msg.Chat.ID,
		fmt.Sprintf(
			"TV show \"%s\" added. Which season and episode are you on?",
			showSearchResult.Name,
		),
	)
	bot.setState(userID, StateAwaitingSeasonEpisode)
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

	nextEpisode, err := findEpisodeByNumber(
		bot.DB, strconv.Itoa(userCtx.SelectedProviderID), season, number+1,
	)
	if err != nil {
		bot.reply(chatID, "I can't find next episode to remind you :(")
		return
	}

	if !nextEpisode.AiredAtUTC.IsZero() {
		err = createReminder(
			bot.DB, userID, int(userCtx.SelectedInternalID), nextEpisode.ID,
			nextEpisode.AiredAtUTC, chatID,
		)
		if err != nil {
			return
		}
		bot.reply(
			chatID,
			fmt.Sprintf(
				"Reminder created for next episode \"%s\", which is expected to air on %s",
				nextEpisode.Title, nextEpisode.AiredAtUTC.Format("Mon Jan 2, 15:04"),
			))
	}
}

func safeString(s *string) string {
	if s == nil {
		return "N/A"
	}
	return *s
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
