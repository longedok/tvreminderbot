package main

import (
	"context"
	"log"
	"os"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

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
		UserContexts: make(map[int64]*UserContext),
	}
	bot.setCommands()

	go reminderLoop(bot, db, context.Background())

	handler := &Handler{
		Bot: bot,
		DB:  db,
	}
	handler.processUpdatesForever()
}
