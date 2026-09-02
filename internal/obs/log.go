// Package obs provides process observability.
package obs

import (
	"io"
	"log/slog"

	"github.com/lmittmann/tint"
)

const textFormat = "text"

// NewLogger builds the process logger. Unknown formats and levels use JSON and info.
func NewLogger(w io.Writer, level, format string) *slog.Logger {
	options := &slog.HandlerOptions{Level: parseLevel(level)}

	if format == textFormat {
		return slog.New(tint.NewHandler(w, &tint.Options{Level: options.Level}))
	}

	return slog.New(slog.NewJSONHandler(w, options))
}

func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
