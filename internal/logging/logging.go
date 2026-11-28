// Package logging builds the structured slog logger.
package logging

import (
	"io"
	"log/slog"
	"strings"
)

// New returns a slog logger writing to w. For the stdio transport w must be
// stderr: stdout carries the MCP JSON-RPC stream.
func New(w io.Writer, level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	if strings.EqualFold(format, "text") {
		return slog.New(slog.NewTextHandler(w, opts))
	}

	return slog.New(slog.NewJSONHandler(w, opts))
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
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
