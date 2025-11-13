package main

import (
	"strings"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func makeKeyboardMarkup(rows [][][]string) *tgbotapi.InlineKeyboardMarkup {
	var inlineRows [][]tgbotapi.InlineKeyboardButton
	for _, row := range rows {
		var inlineRow []tgbotapi.InlineKeyboardButton
		for _, button := range row {
			inlineRow = append(inlineRow, tgbotapi.NewInlineKeyboardButtonData(button[0], button[1]))
		}
		inlineRows = append(inlineRows, inlineRow)
	}
	inlineMarkup := tgbotapi.NewInlineKeyboardMarkup(inlineRows...)
	return &inlineMarkup
}

func safeString(s *string) string {
	if s == nil {
		return "N/A"
	}
	return *s
}

func trimString(s string, maxLen int) string {
	runeCount := utf8.RuneCountInString(s)
	if runeCount <= maxLen {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxLen-3]) + "..."
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
