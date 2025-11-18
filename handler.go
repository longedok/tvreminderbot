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
			if err := handler.acceptShowName(msg); err != nil {
				handler.Bot.reply(msg.Chat.ID, getUserMessage(err))
			}
		default:
			handler.Bot.reply(msg.Chat.ID, "Unexpected message received, see /help for available commands.")
		}
	}
}

func (handler *Handler) handleCommand(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	command := msg.Command()

	var err error
	switch command {
	case "start":
		err = handler.handleStartCommand(msg)
	case "help":
		err = handler.handleHelpCommand(msg)
	case "add":
		err = handler.handleAddCommand(msg)
	case "shows":
		err = handler.handleShowsCommand(msg)
	case "history":
		err = handler.handleHistoryCommand(msg)
	default:
		err = NewUserError(
			fmt.Errorf("unknown command: %s", command),
			fmt.Sprintf("Unknown command: /%s. See /help for available commands.", command),
		)
	}

	if err != nil {
		handler.Bot.reply(chatID, getUserMessage(err))
	}
}

func (handler *Handler) handleCallback(cb *tgbotapi.CallbackQuery) {
	action, callbackParam, found := strings.Cut(cb.Data, ":")
	if !found {
		log.Printf("handleCallback: invalid callback data: %s", cb.Data)
		return
	}

	var err error
	switch action {
	case "acceptShowName":
		err = handler.handleShowNameCallback(cb, callbackParam)
	case "selectSeason":
		err = handler.handleSeasonCallback(cb, callbackParam)
	case "selectEpisode":
		err = handler.handleEpisodeCallback(cb, callbackParam)
	case "selectShow":
		err = handler.handleSelectShowCallback(cb, callbackParam)
	case "backToShows":
		err = handler.handleBackToShowsCallback(cb, callbackParam)
	case "toggleNotifications":
		err = handler.handleToggleNotificationsCallback(cb, callbackParam)
	case "markNextWatched":
		err = handler.handleMarkNextWatchedCallback(cb, callbackParam)
	case "cancel":
		err = handler.handleCancelCallback(cb)
	}

	if err != nil {
		handler.Bot.reply(cb.Message.Chat.ID, getUserMessage(err))
		handler.Bot.answerCallbackQuery(cb.ID)
	}
}

// ADD command flow

func (handler *Handler) handleAddCommand(msg *tgbotapi.Message) error {
	chatID := msg.Chat.ID
	args := strings.TrimSpace(msg.CommandArguments())
	if args == "" {
		handler.Bot.reply(chatID, "Enter show name:")
		handler.Bot.setState(msg.From.ID, StateAwaitingShowName)
		return nil
	}
	return handler.searchAndSelectShow(args, msg.From.ID, chatID)
}

func (handler *Handler) acceptShowName(msg *tgbotapi.Message) error {
	query := msg.Text
	chatID := msg.Chat.ID
	userID := msg.From.ID
	return handler.searchAndSelectShow(query, userID, chatID)
}

func (handler *Handler) searchAndSelectShow(query string, userID int64, chatID int64) error {
	if query == "" {
		handler.Bot.reply(chatID, "Enter show name")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	results, err := SearchShow(ctx, query)
	if err != nil {
		return NewUserError(
			fmt.Errorf("searching show %q: %w", query, err),
			fmt.Sprintf("Error searching show %s", query),
		)
	}

	if len(results) == 0 {
		handler.Bot.reply(chatID, "No shows found for: "+query)
		return nil
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
	return nil
}

func (handler *Handler) handleShowNameCallback(cb *tgbotapi.CallbackQuery, callbackParam string) error {
	searchResultIdx, err := strconv.Atoi(callbackParam)
	if err != nil {
		log.Printf("handleShowNameCallback: invalid callback parameter: %s", callbackParam)
		return nil
	}

	userID := cb.From.ID
	msg := cb.Message
	chatID := msg.Chat.ID

	userCtx := handler.Bot.getUserContext(userID)
	if userCtx == nil || len(userCtx.SearchResults) == 0 {
		handler.Bot.clearState(userID)
		return NewUserError(
			fmt.Errorf("no search results for user %d", userID),
			"No search results found. Please start over with /add.",
		)
	}

	showSearchResult := userCtx.SearchResults[searchResultIdx-1]

	internalID, err := addShow(
		handler.DB, userID, showSearchResult.Name, "tvmaze", showSearchResult.ID,
	)
	if err != nil {
		log.Printf("Error adding show: %s\n", err)
		return NewUserError(
			fmt.Errorf("adding show for user %d provider tvmaze id %d: %w", userID, showSearchResult.ID, err),
			"Error adding show, please try again later.",
		)
	}

	handler.Bot.withUserContext(userID, func(ctx *UserContext) {
		ctx.SelectedInternalID = internalID
		ctx.SelectedProviderID = showSearchResult.ID
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	episodes, err := FetchEpisodes(ctx, showSearchResult.ID)
	if err != nil {
		return NewUserError(
			fmt.Errorf("fetching episodes for show %d: %w", showSearchResult.ID, err),
			fmt.Sprintf("Episode fetching failed: %s", err),
		)
	}

	for _, episode := range episodes {
		showIdStr := strconv.Itoa(showSearchResult.ID)
		episodeIdStr := strconv.Itoa(episode.ID)
		airstampTime, err := time.Parse(time.RFC3339, episode.Airstamp)
		if err != nil {
			return nil
		}
		err = upsertEpisode(
			handler.DB, "tvmaze", showIdStr, episodeIdStr, episode.Name, episode.Season,
			episode.Number, episode.Airdate, episode.Airtime, airstampTime)
		if err != nil {
			return nil
		}
	}

	seasons, err := getSeasons(handler.DB, strconv.Itoa(showSearchResult.ID))
	if err != nil {
		return NewUserError(
			fmt.Errorf("getting seasons for show %d: %w", showSearchResult.ID, err),
			"Error fetching seasons",
		)
	}

	if len(seasons) == 1 {
		// Skip season selection, go directly to episode selection
		handler.Bot.withUserContext(userID, func(ctx *UserContext) {
			ctx.SelectedSeason = seasons[0]
			ctx.State = StateAwaitingSeasonEpisode
		})
		episodeKeyboard, err := handler.makeEpisodeKeyboard(strconv.Itoa(showSearchResult.ID), seasons[0])
		if err != nil {
			return NewUserError(
				fmt.Errorf("making episode keyboard for show %d season %d: %w", showSearchResult.ID, seasons[0], err),
				"Error fetching episodes",
			)
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
	return nil
}

func (handler *Handler) handleSeasonCallback(cb *tgbotapi.CallbackQuery, callbackParam string) error {
	season, err := strconv.Atoi(callbackParam)
	if err != nil {
		log.Printf("handleSeasonCallback: invalid season: %s", callbackParam)
		return nil
	}

	userID := cb.From.ID
	msg := cb.Message
	chatID := msg.Chat.ID

	userCtx := handler.Bot.getUserContext(userID)
	if userCtx == nil {
		handler.Bot.clearState(userID)
		return NewUserError(
			fmt.Errorf("session expired for user %d", userID),
			"Session expired. Please start over with /add.",
		)
	}

	handler.Bot.withUserContext(userID, func(ctx *UserContext) {
		ctx.SelectedSeason = season
	})

	episodeKeyboard, err := handler.makeEpisodeKeyboard(strconv.Itoa(userCtx.SelectedProviderID), season)
	if err != nil {
		return NewUserError(
			fmt.Errorf("making episode keyboard for show %d season %d: %w", userCtx.SelectedProviderID, season, err),
			"Error fetching episodes",
		)
	}

	text := fmt.Sprintf("Which episode of season %d are you on?", season)
	handler.Bot.reply(chatID, text, ReplyOptions{ReplyMarkup: episodeKeyboard, EditMessageID: msg.MessageID})

	handler.Bot.answerCallbackQuery(cb.ID)
	return nil
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

func (handler *Handler) handleEpisodeCallback(cb *tgbotapi.CallbackQuery, callbackParam string) error {
	episodeNumber, err := strconv.Atoi(callbackParam)
	if err != nil {
		log.Printf("handleEpisodeCallback: invalid episode number: %s", callbackParam)
		return nil
	}

	userID := cb.From.ID
	msg := cb.Message
	chatID := msg.Chat.ID

	userCtx := handler.Bot.getUserContext(userID)
	if userCtx == nil {
		handler.Bot.clearState(userID)
		return NewUserError(
			fmt.Errorf("session expired for user %d", userID),
			"Session expired. Please start over with /add.",
		)
	}

	season := userCtx.SelectedSeason

	var resultText string

	// Find the current episode
	currentEpisode, err := findEpisodeByNumber(
		handler.DB, strconv.Itoa(userCtx.SelectedProviderID), season, episodeNumber,
	)
	if err != nil {
		handler.Bot.clearState(userID)
		return NewUserError(
			fmt.Errorf("finding episode for show %d season %d episode %d: %w", userCtx.SelectedProviderID, season, episodeNumber, err),
			"I can't find the episode you specified",
		)
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
	return nil
}

// SHOWS/HISTORY command flow

func (handler *Handler) handleShowsCommand(msg *tgbotapi.Message) error {
	chatID := msg.Chat.ID
	shows, err := listCurrentShowsWithProgress(handler.DB, msg.From.ID)
	if err != nil {
		return NewUserError(
			fmt.Errorf("listing current shows for user %d: %w", msg.From.ID, err),
			"Error: can't list shows at this time",
		)
	}
	if len(shows) == 0 {
		handler.Bot.reply(chatID, "You have no current shows. Use /add <show> to add one, or /history to see all shows.")
		return nil
	}
	handler.Bot.withUserContext(msg.From.ID, func(ctx *UserContext) {
		ctx.ShowsList = shows
	})
	inlineMarkup := handler.makeShowsKeyboard(shows, "current")
	handler.Bot.reply(chatID, "Your current shows:", ReplyOptions{ReplyMarkup: inlineMarkup})
	return nil
}

func (handler *Handler) handleHistoryCommand(msg *tgbotapi.Message) error {
	chatID := msg.Chat.ID
	shows, err := listShowsWithProgress(handler.DB, msg.From.ID)
	if err != nil {
		return NewUserError(
			fmt.Errorf("listing shows for user %d: %w", msg.From.ID, err),
			"Error: can't list shows at this time",
		)
	}
	if len(shows) == 0 {
		handler.Bot.reply(chatID, "You have no shows yet. Use /add <show> to add one.")
		return nil
	}
	handler.Bot.withUserContext(msg.From.ID, func(ctx *UserContext) {
		ctx.ShowsList = shows
	})
	inlineMarkup := handler.makeShowsKeyboard(shows, "history")
	handler.Bot.reply(chatID, "Your show history:", ReplyOptions{ReplyMarkup: inlineMarkup})
	return nil
}

func (handler *Handler) makeShowsKeyboard(shows []ShowProgress, listType string) *tgbotapi.InlineKeyboardMarkup {
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
		cbData := fmt.Sprintf("selectShow:%d:%s", i, listType)
		rows = append(rows, [][]string{{line, cbData}})
	}

	return makeKeyboardMarkup(rows)
}

func (handler *Handler) handleSelectShowCallback(cb *tgbotapi.CallbackQuery, callbackParam string) error {
	showIdxStr, listType, found := strings.Cut(callbackParam, ":")
	if !found {
		log.Printf("handleSelectShowCallback: invalid callback parameter: %s", callbackParam)
		return nil
	}
	showIdx, err := strconv.Atoi(showIdxStr)
	if err != nil {
		log.Printf("handleSelectShowCallback: invalid show index: %s", showIdxStr)
		return nil
	}

	userID := cb.From.ID
	msg := cb.Message
	chatID := msg.Chat.ID

	show, err := handler.validateAndGetShow(userID, chatID, showIdx, listType)
	if err != nil {
		return err
	}

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
	rows = append(rows, [][]string{{toggleText, fmt.Sprintf("toggleNotifications:%d:%s", showIdx, listType)}})
	rows = append(rows, [][]string{{"Mark next as watched", fmt.Sprintf("markNextWatched:%d:%s", showIdx, listType)}})
	rows = append(rows, [][]string{{"<< Back to shows list", fmt.Sprintf("backToShows:%s", listType)}})
	keyboard := makeKeyboardMarkup(rows)

	handler.Bot.reply(
		msg.Chat.ID, infoText, ReplyOptions{ReplyMarkup: keyboard, ParseMode: "HTML", EditMessageID: msg.MessageID})

	handler.Bot.answerCallbackQuery(cb.ID)
	return nil
}

func (handler *Handler) validateAndGetShow(userID int64, chatID int64, showIdx int, listType string) (*ShowProgress, error) {
	userCtx := handler.Bot.getUserContext(userID)
	if userCtx == nil || len(userCtx.ShowsList) == 0 {
		handler.Bot.clearState(userID)
		if listType == "current" {
			return nil, NewUserError(
				fmt.Errorf("no shows in context for user %d", userID),
				"No shows found. Please start over with /shows",
			)
		}
		return nil, NewUserError(
			fmt.Errorf("no shows in context for user %d", userID),
			"No shows found. Please start over with /history",
		)
	}
	if showIdx < 0 || showIdx >= len(userCtx.ShowsList) {
		return nil, NewUserError(
			fmt.Errorf("invalid show index %d for user %d", showIdx, userID),
			"Invalid show selection.",
		)
	}
	return &userCtx.ShowsList[showIdx], nil
}

func findShowIndex(shows []ShowProgress, show ShowProgress) int {
	for i, s := range shows {
		if s.InternalID == show.InternalID {
			return i
		}
	}
	return -1
}

func (handler *Handler) handleToggleNotificationsCallback(cb *tgbotapi.CallbackQuery, callbackParam string) error {
	showIdxStr, listType, found := strings.Cut(callbackParam, ":")
	if !found {
		log.Printf("handleToggleNotificationsCallback: invalid callback parameter: %s", callbackParam)
		return nil
	}
	showIdx, err := strconv.Atoi(showIdxStr)
	if err != nil {
		log.Printf("handleToggleNotificationsCallback: invalid show index: %s", showIdxStr)
		return nil
	}

	userID := cb.From.ID
	msg := cb.Message

	show, err := handler.validateAndGetShow(userID, msg.Chat.ID, showIdx, listType)
	if err != nil {
		return err
	}

	showID, _, err := getShowByUserAndName(handler.DB, userID, show.Name)
	if err != nil {
		return NewUserError(
			fmt.Errorf("getting show %q for user %d: %w", show.Name, userID, err),
			"Error toggling notifications",
		)
	}

	err = toggleShowNotifications(handler.DB, showID)
	if err != nil {
		return NewUserError(
			fmt.Errorf("toggling notifications for show %d: %w", showID, err),
			"Error toggling notifications",
		)
	}

	var shows []ShowProgress
	if listType == "current" {
		shows, err = listCurrentShowsWithProgress(handler.DB, userID)
	} else {
		shows, err = listShowsWithProgress(handler.DB, userID)
	}
	if err != nil {
		return NewUserError(
			fmt.Errorf("refreshing shows list for user %d: %w", userID, err),
			"Error refreshing shows list",
		)
	}
	handler.Bot.withUserContext(userID, func(ctx *UserContext) {
		ctx.ShowsList = shows
	})

	newIdx := findShowIndex(shows, *show)
	if newIdx == -1 {
		return NewUserError(
			fmt.Errorf("show %d not found in refreshed list for user %d", show.InternalID, userID),
			"Error refreshing shows list",
		)
	}

	return handler.handleSelectShowCallback(cb, fmt.Sprintf("%d:%s", newIdx, listType))
}

func (handler *Handler) handleMarkNextWatchedCallback(cb *tgbotapi.CallbackQuery, callbackParam string) error {
	showIdxStr, listType, found := strings.Cut(callbackParam, ":")
	if !found {
		log.Printf("handleMarkNextWatchedCallback: invalid callback parameter: %s", callbackParam)
		return nil
	}
	showIdx, err := strconv.Atoi(showIdxStr)
	if err != nil {
		log.Printf("handleMarkNextWatchedCallback: invalid show index: %s", showIdxStr)
		return nil
	}

	userID := cb.From.ID
	msg := cb.Message

	show, err := handler.validateAndGetShow(userID, msg.Chat.ID, showIdx, listType)
	if err != nil {
		return err
	}

	showID, providerShowID, err := getShowByUserAndName(handler.DB, userID, show.Name)
	if err != nil {
		return NewUserError(
			fmt.Errorf("getting show %q for user %d: %w", show.Name, userID, err),
			"Error finding show",
		)
	}

	nextEpisode, err := findNextEpisode(handler.DB, providerShowID, show.Season, show.Episode)
	if err != nil {
		return NewUserError(
			fmt.Errorf("finding next episode for show %s: %w", providerShowID, err),
			"No next episode found.",
		)
	}

	err = updateLastWatchedEpisode(handler.DB, showID, nextEpisode.ID)
	if err != nil {
		return NewUserError(
			fmt.Errorf("updating last watched episode for show %d: %w", showID, err),
			"Error updating progress",
		)
	}

	var shows []ShowProgress
	if listType == "current" {
		shows, err = listCurrentShowsWithProgress(handler.DB, userID)
	} else {
		shows, err = listShowsWithProgress(handler.DB, userID)
	}
	if err != nil {
		return NewUserError(
			fmt.Errorf("refreshing shows list for user %d: %w", userID, err),
			"Error refreshing shows list",
		)
	}
	handler.Bot.withUserContext(userID, func(ctx *UserContext) {
		ctx.ShowsList = shows
	})

	newIdx := findShowIndex(shows, *show)
	if newIdx == -1 {
		return NewUserError(
			fmt.Errorf("show %d not found in refreshed list for user %d", show.InternalID, userID),
			"Error refreshing shows list",
		)
	}

	return handler.handleSelectShowCallback(cb, fmt.Sprintf("%d:%s", newIdx, listType))
}

func (handler *Handler) handleBackToShowsCallback(cb *tgbotapi.CallbackQuery, callbackParam string) error {
	listType := callbackParam

	userID := cb.From.ID
	msg := cb.Message

	userCtx := handler.Bot.getUserContext(userID)
	if userCtx == nil || len(userCtx.ShowsList) == 0 {
		handler.Bot.clearState(userID)
		return NewUserError(
			fmt.Errorf("no shows in context for user %d", userID),
			"No shows found. Please start over with /shows",
		)
	}

	shows := userCtx.ShowsList
	inlineMarkup := handler.makeShowsKeyboard(shows, listType)

	text := "Your shows:"
	if listType == "current" {
		text = "Your current shows:"
	} else {
		text = "Your show history:"
	}

	handler.Bot.reply(msg.Chat.ID, text, ReplyOptions{ReplyMarkup: inlineMarkup, EditMessageID: msg.MessageID})
	handler.Bot.answerCallbackQuery(cb.ID)
	return nil
}

// CANCEL callback

func (handler *Handler) handleCancelCallback(cb *tgbotapi.CallbackQuery) error {
	userID := cb.From.ID
	msg := cb.Message

	handler.Bot.clearState(userID)
	handler.Bot.reply(msg.Chat.ID, "Operation cancelled.", ReplyOptions{EditMessageID: msg.MessageID})

	cb_response := tgbotapi.NewCallback(cb.ID, "")
	handler.Bot.BotApi.Request(cb_response)
	return nil
}

// START/HELP commands

func (handler *Handler) handleStartCommand(msg *tgbotapi.Message) error {
	chatID := msg.Chat.ID
	startText := dedent(`
	Hello! I'm a bot that helps you track your TV shows and notify you when new episodes air.

	/add - Add a TV show to track
	/shows - List your current shows
	/history - List all your shows
	`)
	handler.Bot.reply(chatID, startText)
	return nil
}

func (handler *Handler) handleHelpCommand(msg *tgbotapi.Message) error {
	chatID := msg.Chat.ID
	helpText := dedent(`
	Commands:

	/add <show>
	/shows - list your current shows
	/history - list all your shows
	/help - show this help
	`)
	handler.Bot.reply(chatID, helpText)
	return nil
}
