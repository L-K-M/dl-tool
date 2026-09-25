// Package obs provides process observability.
package obs

import (
	"io"
	"log/slog"
	"os"

	"github.com/lmittmann/tint"
)

const (
	textFormat = "text"

	// recorderCapacity is the ring depth GET /system/logs pages over —
	// the 5 000 newest records a diagnostics bundle collects
	// (docs/17-operations-and-runbook.md section 7).
	recorderCapacity = 5000

	// logFileMaxBytes caps <CONFIG_DIR>/logs/dl-tool.jsonl: roomy enough
	// for the full recorder ring at typical line sizes, small enough that
	// an operator's /config cannot fill up.
	logFileMaxBytes = 10 << 20
)

// NewLogger builds the process logger. Unknown formats and levels use JSON and info.
// Every record passes through RedactAttr before it is stored, then lands in
// the Recorder ring and on the teed sink, so stdout, the log file and
// GET /system/logs carry the same redacted text.
func NewLogger(w io.Writer, level, format string) *slog.Logger {
	options := &slog.HandlerOptions{Level: parseLevel(level), ReplaceAttr: RedactAttr}

	// A missing or unwritable config directory leaves the file sink off:
	// logger construction cannot fail — the signature is fixed by the
	// cmd/dl-tool call sites — and neither the openapi subcommand's
	// document nor a unit test may gain a stray warning line on stdout.
	sink := w
	if tee, _, err := NewLogWriter(w, logConfigDir(), logFileMaxBytes); err == nil {
		sink = tee
	}

	var h slog.Handler = slog.NewJSONHandler(sink, options)
	if format == textFormat {
		h = tint.NewTextHandler(sink, &tint.Options{Level: options.Level, ReplaceAttr: RedactAttr})
	}

	return slog.New(NewRecorder(h, recorderCapacity))
}

// logConfigDir resolves DLTOOL_CONFIG_DIR exactly as config.Load does
// (docs/11-config-reference.md section 2). The read is duplicated here —
// internal/config does not export it — because NewLogger's signature
// carries no configDir and its cmd/dl-tool call sites stand outside this
// task's Files table.
func logConfigDir() string {
	if dir := os.Getenv("DLTOOL_CONFIG_DIR"); dir != "" {
		return dir
	}

	return "/config"
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
