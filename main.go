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
)

type Bot struct {
	BotApi        *tgbotapi.BotAPI
	DB            *sql.DB
	States        map[int64]UserState
	SearchResults map[int64][]ShowSearchResult
	mu            sync.Mutex
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
		BotApi:        botApi,
		DB:            db,
		States:        make(map[int64]UserState),
		SearchResults: make(map[int64][]ShowSearchResult),
	}

	updateConfig := tgbotapi.NewUpdate(0)
	updateConfig.Timeout = 30

	updates := botApi.GetUpdatesChan(updateConfig)

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

func (bot *Bot) setState(userID int64, state UserState) {
	bot.mu.Lock()
	defer bot.mu.Unlock()
	bot.States[userID] = state
}

func (bot *Bot) getState(userID int64) UserState {
	bot.mu.Lock()
	defer bot.mu.Unlock()
	return bot.States[userID]
}

func (bot *Bot) clearState(userID int64) {
	bot.mu.Lock()
	defer bot.mu.Unlock()
	delete(bot.States, userID)
	delete(bot.SearchResults, userID)
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

		showIds := make([]int, 0)
		replyText := "Found:\n"
		for i, r := range results[:min(5, len(results))] {
			replyText += fmt.Sprintf("%d. %s (%s)\n", i+1, r.Name, safeString(r.Premiered))
			showIds = append(showIds, r.ID)
		}

		bot.mu.Lock()
		bot.SearchResults[userID] = results
		bot.mu.Unlock()

		replyText += "Send the number:"
		bot.reply(chatID, replyText)
		bot.setState(userID, StateAwaitingShowSelection)
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

	searchResults := bot.SearchResults[userID]

	if idx < 1 || idx > 5 {
		bot.reply(msg.Chat.ID, "Invalid number.")
		return
	}

	showSearchResult := searchResults[idx-1]

	err = addShow(bot.DB, userID, showSearchResult.Name, "tvmaze", showSearchResult.ID)
	if err != nil {
		log.Printf("Error adding show: %s\n", err)
		return
	}

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

	bot.reply(msg.Chat.ID, fmt.Sprintf("TV show \"%s\" added", showSearchResult.Name))
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
