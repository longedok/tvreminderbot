package main

import (
	"strings"
	"unicode/utf8"
)

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
