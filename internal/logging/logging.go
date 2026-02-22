// Package logging builds the structured logger used across gitdr. Logs are written to
// stderr so that stdout stays reserved for machine-readable command output
// (--output json). JSON is the default format.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// New returns a structured logger writing to w.
func New(level, format string, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	var h slog.Handler
	if strings.EqualFold(strings.TrimSpace(format), "text") {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}

// Default returns a logger writing to stderr with the given level/format.
func Default(level, format string) *slog.Logger { return New(level, format, os.Stderr) }

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
