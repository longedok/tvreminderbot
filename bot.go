package main

import (
	"database/sql"
	"sync"

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
	ShowsList          []ShowProgress
}

type Bot struct {
	BotApi       *tgbotapi.BotAPI
	DB           *sql.DB
	UserContexts map[int64]*UserContext
	mu           sync.Mutex
}

type ReplyOptions struct {
	ReplyMarkup   interface{} // e.g., *tgbotapi.InlineKeyboardMarkup or *tgbotapi.ReplyKeyboardMarkup
	ParseMode     string      // "HTML", "Markdown", etc.
	EditMessageID int         // message ID to edit; if 0, send new message
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

func (bot *Bot) reply(chatID int64, text string, opts ...ReplyOptions) {
	var opt ReplyOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	if opt.EditMessageID != 0 {
		editMsg := tgbotapi.NewEditMessageText(chatID, opt.EditMessageID, text)
		if opt.ReplyMarkup != nil {
			if markup, ok := opt.ReplyMarkup.(*tgbotapi.InlineKeyboardMarkup); ok {
				editMsg.ReplyMarkup = markup
			}
		}
		if opt.ParseMode != "" {
			editMsg.ParseMode = opt.ParseMode
		}
		bot.BotApi.Send(editMsg)
	} else {
		message := tgbotapi.NewMessage(chatID, text)
		if opt.ReplyMarkup != nil {
			if markup, ok := opt.ReplyMarkup.(*tgbotapi.InlineKeyboardMarkup); ok {
				message.ReplyMarkup = markup
			}
		}
		if opt.ParseMode != "" {
			message.ParseMode = opt.ParseMode
		}
		bot.BotApi.Send(message)
	}
}
