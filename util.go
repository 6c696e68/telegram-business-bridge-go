package main

import (
	"io"
	"log"
	"os"
	"strings"
	"unicode/utf8"
)

var logger = log.New(os.Stdout, "", log.LstdFlags)

// logf never receives message text, tokens or keys.
func logf(level, format string, a ...any) {
	logger.Printf(level+" "+format, a...)
}

// truncRunes keeps the first n code points, like Python's s[:n].
// It never splits a multi-byte character.
func truncRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// truncUTF16 keeps at most limit UTF-16 code units (Telegram's message limit
// is counted this way, so emoji count as 2).
func truncUTF16(s string, limit int) string {
	units := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if units+w > limit {
			return s[:i]
		}
		units += w
	}
	return s
}

const telegramUnitLimit = 4000 // Telegram hard limit is 4096; keep headroom

func truncReply(s string, maxChars int) string {
	return truncUTF16(truncRunes(s, maxChars), telegramUnitLimit)
}

// normCmd turns "/Status@my_bot extra" into "/status".
func normCmd(text string) string {
	f := strings.Fields(text)
	if len(f) == 0 {
		return ""
	}
	c := f[0]
	if i := strings.IndexByte(c, '@'); i >= 0 {
		c = c[:i]
	}
	return strings.ToLower(c)
}

func tail6(s string) string {
	if len(s) > 6 {
		return s[len(s)-6:]
	}
	return s
}

// silence is used by tests to keep output clean.
var silence = io.Discard

func runeCount(s string) int { return utf8.RuneCountInString(s) }
