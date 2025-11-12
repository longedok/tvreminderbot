package main

import (
	"context"
	"log"
	"time"
	"database/sql"
	"fmt"
)

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