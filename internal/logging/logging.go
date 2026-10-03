// Package logging provides the adapter's structured operational log. Callers
// log only local conditions and error chains; credentials, tokens, selectors,
// subscription URLs, and requested destinations are never passed to it.
package logging

import (
	"io"
	"log/slog"
	"strings"
	"unicode/utf8"
)

const maxErrorBytes = 512

// New returns a text logger writing to w.
func New(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// Discard returns a logger that drops every record.
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// Error renders err as one bounded line suitable for a log attribute.
func Error(err error) string {
	if err == nil {
		return ""
	}
	text := strings.Join(strings.Fields(err.Error()), " ")
	if len(text) <= maxErrorBytes {
		return text
	}
	cut := maxErrorBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}
