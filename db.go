package main

import (
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

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
		  last_notified_episode_id TEXT,
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
			remind_at DATETIME NOT NULL,
			FOREIGN KEY (show_id) REFERENCES shows(id)
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

func addShow(db *sql.DB, userID int64, name, provider string, showID int) error {
	_, err := db.Exec(`
		INSERT INTO shows (user_id, name, provider, provider_show_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT DO NOTHING
	`, userID, name, provider, showID)
	return err
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

func findEpisodeByNumber(db *sql.DB, providerShowId string, season, number int) (*Episode, error) {
	rows, err := db.Query(`
		SELECT
			provider_episode_id, season, number, title, airdate
		FROM episodes_cache
		WHERE provider_show_id = ? and season = ? and number = ?
	`, providerShowId, season, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var episode Episode
	if rows.Next() {
		if err := rows.Scan(&episode.ID, &episode.Season, &episode.Number,
			&episode.Name, &episode.Airdate); err != nil {
			return nil, err
		}
		return &episode, err
	}

	return nil, errors.New("episode not found")
}

func createReminder(db *sql.DB, userID int64, showID int, remindAt time.Time) error {
	_, err := db.Exec(`
		INSERT INTO reminders (user_id, show_id, remind_at)
		VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING
	`, userID, showID, remindAt)

	return err
}
