package main

import (
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

// Database models - separate from API models

type DBShow struct {
	ID                   int64
	UserID               int64
	Name                 string
	Provider             string
	ProviderShowID       string
	Timezone             string
	LastWatchedEpisodeID *string
	CreatedAt            time.Time
}

type DBEpisode struct {
	ID                int64
	Provider          string
	ProviderShowID    string
	ProviderEpisodeID string
	Season            int
	Number            int
	Title             string
	Airdate           string
	Airtime           string
	AiredAtUTC        time.Time
	FetchedAt         time.Time
}

type DBReminder struct {
	ID            int64
	UserID        int64
	ShowID        int64
	EpisodeID     int64
	RemindAt      time.Time
	ChatID        int64
	ShowName      string
	EpisodeTitle  string
	EpisodeNumber int
	EpisodeSeason int
}

func openDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite", "tvreminder.db")
	if err != nil {
		return nil, err
	}

	_, err = db.Exec(`
		BEGIN;

		CREATE TABLE IF NOT EXISTS shows (
		  id INTEGER PRIMARY KEY AUTOINCREMENT,
		  user_id INTEGER NOT NULL,
		  name TEXT NOT NULL,
		  provider TEXT NOT NULL DEFAULT 'local',
		  provider_show_id TEXT,
		  timezone TEXT DEFAULT 'UTC',
		  last_watched_episode_id TEXT,
		  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		  UNIQUE(user_id, provider, provider_show_id)
		);

		CREATE TABLE IF NOT EXISTS episodes_cache (
		  id INTEGER PRIMARY KEY AUTOINCREMENT,
		  provider TEXT NOT NULL,
		  provider_show_id TEXT NOT NULL,
		  provider_episode_id TEXT NOT NULL,
		  season INTEGER,
		  number INTEGER,
		  title TEXT,
		  airdate DATE,       -- yyyy-mm-dd
		  airtime TEXT,       -- hh:mm (provider may supply)
		  aired_at_utc DATETIME,  -- normalized UTC timestamp if available
		  fetched_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		  UNIQUE(provider, provider_episode_id)
		);

		CREATE TABLE IF NOT EXISTS reminders (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			show_id INTEGER NOT NULL,
			episode_id INTEGER,
			remind_at DATETIME NOT NULL,
			chat_id INTEGER NOT NULL,
			FOREIGN KEY (show_id) REFERENCES shows(id),
			FOREIGN KEY (episode_id) REFERENCES episodes_cache(id),
			UNIQUE(user_id, show_id)
		);

		CREATE INDEX IF NOT EXISTS idx_shows_user ON shows(user_id);
		CREATE INDEX IF NOT EXISTS idx_episodes_show
			ON episodes_cache(provider, provider_show_id);

		COMMIT;
	`)

	if err != nil {
		return nil, err
	}

	return db, nil
}

func addShow(db *sql.DB, userID int64, name, provider string, showID int) (int64, error) {
	result, err := db.Exec(`
		INSERT INTO shows (user_id, name, provider, provider_show_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT DO NOTHING
	`, userID, name, provider, showID)
	if err != nil {
		return 0, err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}

	if rowsAffected > 0 {
		return result.LastInsertId()
	}

	var internalID int64
	err = db.QueryRow(`
		SELECT id FROM shows 
		WHERE user_id = ? AND provider = ? AND provider_show_id = ?
	`, userID, provider, showID).Scan(&internalID)
	if err != nil {
		return 0, err
	}

	return internalID, nil
}

func listShows(db *sql.DB, userID int64) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM shows WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var shows []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		shows = append(shows, name)
	}
	return shows, nil
}

func upsertEpisode(
	db *sql.DB,
	provider, showID, episodeID, title string,
	season, number int,
	airdate, airtime string,
	airedAtUTC time.Time,
) error {
	_, err := db.Exec(`
        INSERT INTO episodes_cache
        (provider, provider_show_id, provider_episode_id, season, number, title, airdate,
		airtime, aired_at_utc, fetched_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
        ON CONFLICT(provider, provider_episode_id) DO UPDATE SET
            title=excluded.title,
            season=excluded.season,
            number=excluded.number,
            airdate=excluded.airdate,
            airtime=excluded.airtime,
            aired_at_utc=excluded.aired_at_utc,
            fetched_at=CURRENT_TIMESTAMP
	`, provider, showID, episodeID, season, number, title, airdate, airtime,
		airedAtUTC.UTC().Format(time.RFC3339))
	return err
}

func findEpisodeByNumber(db *sql.DB, providerShowId string, season, number int) (*DBEpisode, error) {
	var episode DBEpisode
	var airedAtStr string
	var fetchedAtStr string

	err := db.QueryRow(`
		SELECT
			id, provider, provider_show_id, provider_episode_id, season, number, 
			title, airdate, airtime, aired_at_utc, fetched_at
		FROM episodes_cache
		WHERE provider_show_id = ? and season = ? and number = ?
	`, providerShowId, season, number).Scan(
		&episode.ID, &episode.Provider, &episode.ProviderShowID, &episode.ProviderEpisodeID,
		&episode.Season, &episode.Number, &episode.Title, &episode.Airdate, &episode.Airtime,
		&airedAtStr, &fetchedAtStr,
	)

	if err == sql.ErrNoRows {
		return nil, errors.New("episode not found")
	}
	if err != nil {
		return nil, err
	}

	if airedAtStr != "" {
		episode.AiredAtUTC, _ = time.Parse(time.RFC3339, airedAtStr)
	}
	if fetchedAtStr != "" {
		episode.FetchedAt, _ = time.Parse(time.RFC3339, fetchedAtStr)
	}

	return &episode, nil
}

func createReminder(db *sql.DB, userID int64, showID int, episodeID int64, remindAt time.Time, chatID int64) error {
	_, err := db.Exec(`
		INSERT INTO reminders (user_id, show_id, episode_id, remind_at, chat_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING
	`, userID, showID, episodeID, remindAt, chatID)

	return err
}

func getDueReminders(db *sql.DB) ([]DBReminder, error) {
	rows, err := db.Query(`
		SELECT
			r.id, r.user_id, r.show_id, r.episode_id, r.remind_at, r.chat_id,
			s.name, e.title, e.number, e.season
		FROM reminders r
		LEFT JOIN shows s ON s.id = r.show_id
		LEFT JOIN episodes_cache e ON e.id = r.episode_id
		WHERE r.remind_at <= DATETIME('now', '+5 minutes')
		`)
	if err != nil {
		return nil, err
	}
	var reminders []DBReminder
	for rows.Next() {
		var reminder DBReminder
		if err := rows.Scan(
			&reminder.ID, &reminder.UserID, &reminder.ShowID, &reminder.EpisodeID,
			&reminder.RemindAt, &reminder.ChatID, &reminder.ShowName,
			&reminder.EpisodeTitle, &reminder.EpisodeNumber, &reminder.EpisodeSeason,
		); err != nil {
			return nil, err
		}
		reminders = append(reminders, reminder)
	}

	return reminders, nil
}

func updateLastWatchedEpisode(db *sql.DB, showID int64, episodeID int64) error {
	_, err := db.Exec(`
		UPDATE shows
		SET last_watched_episode_id = ?
		WHERE id = ?
	`, episodeID, showID)
	return err
}

func markReminderSent(db *sql.DB, reminder DBReminder) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Delete the current reminder
	_, err = tx.Exec(`DELETE FROM reminders WHERE id = ?`, reminder.ID)
	if err != nil {
		return err
	}

	// Update the show's last_watched_episode_id
	_, err = tx.Exec(`
		UPDATE shows
		SET last_watched_episode_id = ?
		WHERE id = ?
	`, reminder.EpisodeID, reminder.ShowID)
	if err != nil {
		return err
	}

	// Get current episode details to find the next one
	var currentSeason, currentNumber int
	err = tx.QueryRow(`
		SELECT season, number FROM episodes_cache WHERE id = ?
	`, reminder.EpisodeID).Scan(&currentSeason, &currentNumber)
	if err != nil {
		return err
	}

	// Find the next episode
	var nextEpisode DBEpisode
	var airedAtStr string
	var fetchedAtStr string

	err = tx.QueryRow(`
		SELECT
			id, provider, provider_show_id, provider_episode_id, season, number,
			title, airdate, airtime, aired_at_utc, fetched_at
		FROM episodes_cache
		WHERE provider_show_id = (
			SELECT provider_show_id FROM shows WHERE id = ?
		)
		AND (
			(season = ? AND number > ?) OR
			(season > ?)
		)
		ORDER BY season, number
		LIMIT 1
	`, reminder.ShowID, currentSeason, currentNumber, currentSeason).Scan(
		&nextEpisode.ID, &nextEpisode.Provider, &nextEpisode.ProviderShowID, &nextEpisode.ProviderEpisodeID,
		&nextEpisode.Season, &nextEpisode.Number, &nextEpisode.Title, &nextEpisode.Airdate, &nextEpisode.Airtime,
		&airedAtStr, &fetchedAtStr,
	)

	if err == sql.ErrNoRows {
		// No next episode found, just commit the delete and update
		return tx.Commit()
	}
	if err != nil {
		return err
	}

	// Parse timestamps
	if airedAtStr != "" {
		nextEpisode.AiredAtUTC, _ = time.Parse(time.RFC3339, airedAtStr)
	}
	if fetchedAtStr != "" {
		nextEpisode.FetchedAt, _ = time.Parse(time.RFC3339, fetchedAtStr)
	}

	// Create reminder for next episode if it has an air date
	if !nextEpisode.AiredAtUTC.IsZero() {
		_, err = tx.Exec(`
			INSERT INTO reminders (user_id, show_id, episode_id, remind_at, chat_id)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT DO NOTHING
		`, reminder.UserID, reminder.ShowID, nextEpisode.ID, nextEpisode.AiredAtUTC, reminder.ChatID)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}
