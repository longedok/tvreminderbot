package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type Handler struct {
	Bot *Bot
	DB  *sql.DB
}

func (handler *Handler) processUpdatesForever() {
	updateConfig := tgbotapi.NewUpdate(0)
	updateConfig.Timeout = 30
	updates := handler.Bot.BotApi.GetUpdatesChan(updateConfig)

	for update := range updates {
		if update.CallbackQuery != nil {
			handler.handleCallback(update.CallbackQuery)
			continue
		}

		if update.Message == nil {
			log.Printf("processUpdatesForever: message is nil")
			continue
		}

		msg := update.Message
		userID := msg.From.ID
		state := handler.Bot.getState(userID)

		switch {
		case msg.IsCommand():
			handler.handleCommand(msg)
		case state == StateAwaitingShowName:
			handler.acceptShowName(msg)
		default:
			handler.Bot.reply(msg.Chat.ID, "Unexpected message received, see /help for available commands.")
		}
	}
}

func (handler *Handler) handleCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	command := msg.Command()

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
		handler.Bot.reply(chatID, fmt.Sprintf("Unknown command: /%s. See /help for available commands.", command))
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
		handler.handleShowNameCallback(cb, param)
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

// ADD command flow

func (handler *Handler) handleAddCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	args := strings.TrimSpace(msg.CommandArguments())
	if args == "" {
		handler.Bot.reply(chatID, "Enter show name:")
		handler.Bot.setState(msg.From.ID, StateAwaitingShowName)
	} else {
		handler.searchAndSelectShow(args, msg.From.ID, chatID)
	}
}

func (handler *Handler) acceptShowName(msg *tgbotapi.Message) {
	query := msg.Text
	chatID := msg.Chat.ID
	userID := msg.From.ID
	handler.searchAndSelectShow(query, userID, chatID)
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

		var rows [][][]string
		for i := range max {
			trimmed := trimString(results[i].Name, 25)
			label := fmt.Sprintf("%d. %s (%s)", i+1, trimmed, safeString(results[i].Premiered))
			cb := fmt.Sprintf("acceptShowName:%d", i+1)
			rows = append(rows, [][]string{{label, cb}})
		}
		rows = append(rows, [][]string{{"❌ Cancel", "cancel"}})
		inlineMarkup := makeKeyboardMarkup(rows)

		handler.Bot.withUserContext(userID, func(ctx *UserContext) {
			ctx.SearchResults = results
			ctx.State = StateAwaitingShowSelection
		})

		listText := "Pick the show you want to add:"
		handler.Bot.reply(chatID, listText, ReplyOptions{ReplyMarkup: inlineMarkup})
	}
}

func (handler *Handler) handleShowNameCallback(cb *tgbotapi.CallbackQuery, param string) {
	idx, err := strconv.Atoi(param)
	if err != nil {
		log.Printf("handleShowNameCallback: invalid parameter: %s", param)
		return
	}

	userID := cb.From.ID
	msg := cb.Message
	chatID := msg.Chat.ID

	userCtx := handler.Bot.getUserContext(userID)
	if userCtx == nil || len(userCtx.SearchResults) == 0 {
		handler.Bot.reply(chatID, "No search results found. Please start over with /add.")
		handler.Bot.clearState(userID)
		return
	}

	showSearchResult := userCtx.SearchResults[idx-1]

	internalID, err := addShow(
		handler.DB, userID, showSearchResult.Name, "tvmaze", showSearchResult.ID,
	)
	if err != nil {
		log.Printf("Error adding show: %s\n", err)
		handler.Bot.reply(chatID, "Error adding show, please try again later.")
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
		handler.Bot.reply(chatID, fmt.Sprintf("Episode fetching failed: %s", err))
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

	seasons, err := getSeasons(handler.DB, strconv.Itoa(showSearchResult.ID))
	if err != nil {
		handler.Bot.reply(chatID, "Error fetching seasons")
		return
	}

	if len(seasons) == 1 {
		// Skip season selection, go directly to episode selection
		handler.Bot.withUserContext(userID, func(ctx *UserContext) {
			ctx.SelectedSeason = seasons[0]
			ctx.State = StateAwaitingSeasonEpisode
		})
		episodeKeyboard, err := handler.makeEpisodeKeyboard(strconv.Itoa(showSearchResult.ID), seasons[0])
		if err != nil {
			handler.Bot.reply(chatID, "Error fetching episodes")
			return
		}
		text := fmt.Sprintf(
			"TV show \"%s\" added. Which episode of season %d are you on?",
			showSearchResult.Name, seasons[0],
		)
		handler.Bot.reply(chatID, text, ReplyOptions{ReplyMarkup: episodeKeyboard, EditMessageID: msg.MessageID})
	} else {
		var rows [][][]string
		for _, season := range seasons {
			label := fmt.Sprintf("Season %d", season)
			cbData := fmt.Sprintf("selectSeason:%d", season)

			rows = append(rows, [][]string{{label, cbData}})
		}
		rows = append(rows, [][]string{{"❌ Cancel", "cancel"}})
		inlineMarkup := makeKeyboardMarkup(rows)
		log.Printf("inlineMarkup: %+v", inlineMarkup)
		handler.Bot.withUserContext(userID, func(ctx *UserContext) {
			ctx.State = StateAwaitingSeasonEpisode
		})
		text := fmt.Sprintf("TV show \"%s\" added. Which season are you on?", showSearchResult.Name)
		handler.Bot.reply(chatID, text, ReplyOptions{ReplyMarkup: inlineMarkup, EditMessageID: msg.MessageID})
	}

	handler.Bot.answerCallbackQuery(cb.ID)
}

func (handler *Handler) handleSeasonCallback(cb *tgbotapi.CallbackQuery, param string) {
	season, err := strconv.Atoi(param)
	if err != nil {
		log.Printf("handleSeasonCallback: invalid season: %s", param)
		return
	}

	userID := cb.From.ID
	msg := cb.Message
	chatID := msg.Chat.ID

	userCtx := handler.Bot.getUserContext(userID)
	if userCtx == nil {
		handler.Bot.reply(chatID, "Session expired. Please start over with /add.")
		handler.Bot.clearState(userID)
		return
	}

	handler.Bot.withUserContext(userID, func(ctx *UserContext) {
		ctx.SelectedSeason = season
	})

	episodeKeyboard, err := handler.makeEpisodeKeyboard(strconv.Itoa(userCtx.SelectedProviderID), season)
	if err != nil {
		handler.Bot.reply(chatID, "Error fetching episodes")
		return
	}

	text := fmt.Sprintf("Which episode of season %d are you on?", season)
	handler.Bot.reply(chatID, text, ReplyOptions{ReplyMarkup: episodeKeyboard, EditMessageID: msg.MessageID})

	handler.Bot.answerCallbackQuery(cb.ID)
}

func (handler *Handler) makeEpisodeKeyboard(providerShowID string, season int) (*tgbotapi.InlineKeyboardMarkup, error) {
	episodes, err := getEpisodesBySeason(handler.DB, providerShowID, season)
	if err != nil {
		return nil, err
	}
	var rows [][][]string
	for _, episode := range episodes {
		label := fmt.Sprintf("%d. %s", episode.Number, episode.Title)
		cbData := fmt.Sprintf("selectEpisode:%d", episode.Number)

		rows = append(rows, [][]string{{label, cbData}})
	}
	rows = append(rows, [][]string{{"❌ Cancel", "cancel"}})
	inlineMarkup := makeKeyboardMarkup(rows)
	return inlineMarkup, nil
}

func (handler *Handler) handleEpisodeCallback(cb *tgbotapi.CallbackQuery, param string) {
	episodeNumber, err := strconv.Atoi(param)
	if err != nil {
		log.Printf("handleEpisodeCallback: invalid episode number: %s", param)
		return
	}

	userID := cb.From.ID
	msg := cb.Message
	chatID := msg.Chat.ID

	userCtx := handler.Bot.getUserContext(userID)
	if userCtx == nil {
		handler.Bot.reply(chatID, "Session expired. Please start over with /add.")
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
		handler.Bot.reply(chatID, "I can't find the episode you specified")
		handler.Bot.clearState(userID)
		return
	}

	// Find the next episode
	nextEpisode, _ := findEpisodeByNumber(
		handler.DB, strconv.Itoa(userCtx.SelectedProviderID), season, episodeNumber+1,
	)

	err = updateLastWatchedEpisode(handler.DB, userCtx.SelectedInternalID, currentEpisode.ID)
	if err != nil {
		resultText = "Failed to update progress"
	} else {
		showName, err := getShowNameByID(handler.DB, userCtx.SelectedInternalID)
		if err != nil {
			resultText = "Failed to get show name"
		} else {
			if nextEpisode == nil {
				resultText = fmt.Sprintf("Marked \"%s\" as watched up to S%02dE%02d.", showName, season, episodeNumber)
			} else {
				if !nextEpisode.AiredAtUTC.IsZero() && nextEpisode.AiredAtUTC.After(time.Now()) {
					err = createReminder(
						handler.DB, userID, int(userCtx.SelectedInternalID), nextEpisode.ID,
						nextEpisode.AiredAtUTC, msg.Chat.ID,
					)
					if err != nil {
						resultText = "Failed to create reminder"
					} else {
						nextEpisodeAiredAtStr := nextEpisode.AiredAtUTC.Format("Mon Jan 2, 15:04")
						resultText = fmt.Sprintf(
							"Marked \"%s\" as watched up to S%02dE%02d. "+
								"Next episode \"%s\" is expected to air on %s. I'll notify you when it airs.",
							showName, season, episodeNumber, nextEpisode.Title, nextEpisodeAiredAtStr,
						)
					}
				} else {
					resultText = fmt.Sprintf(
						"Marked \"%s\" as watched up to S%02dE%02d. Next episode \"%s\" is already available.",
						showName, season, episodeNumber, nextEpisode.Title,
					)
				}
			}
		}
	}

	handler.Bot.reply(chatID, resultText, ReplyOptions{EditMessageID: msg.MessageID})
	handler.Bot.clearState(userID)
	handler.Bot.answerCallbackQuery(cb.ID)
}

// SHOWS command flow

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
	inlineMarkup := handler.makeShowsKeyboard(shows)
	handler.Bot.reply(chatID, "Your shows:", ReplyOptions{ReplyMarkup: inlineMarkup})
}

func (handler *Handler) makeShowsKeyboard(shows []ShowProgress) *tgbotapi.InlineKeyboardMarkup {
	var rows [][][]string
	for i, show := range shows {
		line := show.Name
		if show.NotificationsEnabled && show.NextAirDate.Valid && show.NextAirDate.Time.After(time.Now()) {
			line = "🔔 " + line
		}
		if show.Season.Valid && show.Episode.Valid {
			line += fmt.Sprintf(" (S%02dE%02d)", show.Season.Int32, show.Episode.Int32)
		}
		if show.NextEpisodeSeason.Valid && show.NextEpisodeNumber.Valid {
			if show.NextAirDate.Valid && show.NextAirDate.Time.After(time.Now()) {
				line += fmt.Sprintf(" - Next Ep %s", show.NextAirDate.Time.Format("Jan 2 (Mon)"))
			} else {
				line += " - Next Ep Out ✅"
			}
		}
		cbData := fmt.Sprintf("selectShow:%d", i)
		rows = append(rows, [][]string{{line, cbData}})
	}

	return makeKeyboardMarkup(rows)
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

	var rows [][][]string
	toggleText := "Disable Notifications"
	if !show.NotificationsEnabled {
		toggleText = "Enable Notifications"
	}
	rows = append(rows, [][]string{{toggleText, fmt.Sprintf("toggleNotifications:%d", idx)}})
	rows = append(rows, [][]string{{"Mark next as watched", fmt.Sprintf("markNextWatched:%d", idx)}})
	rows = append(rows, [][]string{{"<< Back to shows list", "backToShows"}})
	keyboard := makeKeyboardMarkup(rows)

	handler.Bot.reply(
		msg.Chat.ID, infoText, ReplyOptions{ReplyMarkup: keyboard, ParseMode: "HTML", EditMessageID: msg.MessageID})

	handler.Bot.answerCallbackQuery(cb.ID)
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
	showID, _, err := getShowByUserAndName(handler.DB, userID, show.Name)
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
	showID, providerShowID, err := getShowByUserAndName(handler.DB, userID, show.Name)
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
	inlineMarkup := handler.makeShowsKeyboard(shows)

	handler.Bot.reply(msg.Chat.ID, "Your shows:", ReplyOptions{ReplyMarkup: inlineMarkup, EditMessageID: msg.MessageID})

	handler.Bot.answerCallbackQuery(cb.ID)
}

// CANCEL command flow

func (handler *Handler) handleCancelCallback(cb *tgbotapi.CallbackQuery) {
	userID := cb.From.ID
	msg := cb.Message

	handler.Bot.clearState(userID)
	handler.Bot.reply(msg.Chat.ID, "Operation cancelled.", ReplyOptions{EditMessageID: msg.MessageID})

	cb_response := tgbotapi.NewCallback(cb.ID, "")
	handler.Bot.BotApi.Request(cb_response)
}
