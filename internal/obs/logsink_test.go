package obs

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/L-K-M/dl-tool/internal/secure"
)

// redactedFrom renders one attr through the same ReplaceAttr the JSON
// handler runs and returns the value it kept.
func redactedFrom(a slog.Attr) slog.Value {
	return RedactAttr(nil, a).Value
}

func TestRedactAttrByKey(t *testing.T) {
	for _, key := range []string{"Authorization", "Cookie", "X-Api-Key", "TOKEN", "passkey"} {
		got := redactedFrom(slog.String(key, "Bearer live-credential"))
		if got.String() != Placeholder {
			t.Errorf("key %q: got %q, want %q", key, got.String(), Placeholder)
		}
	}
}

func TestRedactSecretByType(t *testing.T) {
	// The key is not on the redacted list; the type alone must catch it.
	got := redactedFrom(slog.Any("engine_password", secure.Secret("hunter2")))
	if got.String() != Placeholder {
		t.Errorf("secret by type: got %q, want %q", got.String(), Placeholder)
	}
}

func TestRedactURLPasskey(t *testing.T) {
	raw := "https://indexer.example.org/api?t=search&passkey=abc123&limit=10"
	got := RedactURL(raw)
	if strings.Contains(got, "abc123") {
		t.Fatalf("passkey survived: %q", got)
	}
	if !strings.Contains(got, "passkey="+Placeholder) || !strings.Contains(got, "t=search") {
		t.Errorf("query mangled: %q", got)
	}
}

func TestRedactURLUserinfo(t *testing.T) {
	got := RedactURL("ftp://user:pw@host/f.iso")
	if got != "ftp://"+Placeholder+"@host/f.iso" {
		t.Errorf("userinfo: got %q, want %q", got, "ftp://"+Placeholder+"@host/f.iso")
	}
}

func TestRedactLeavesPlainStringAlone(t *testing.T) {
	const plain = "engine accepted task tsk_01JK — not a URL"
	got := redactedFrom(slog.String("msg_text", plain))
	if got.String() != plain {
		t.Errorf("plain string altered: got %q", got.String())
	}
}

// handle feeds one synthetic record through the recorder.
func handle(t *testing.T, r *Recorder, level slog.Level, msg string, attrs ...slog.Attr) {
	t.Helper()
	rec := slog.NewRecord(time.Now(), level, msg, 0)
	for _, a := range attrs {
		rec.AddAttrs(a)
	}
	if err := r.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func messages(recs []Record) []string {
	out := make([]string, len(recs))
	for i, rec := range recs {
		out[i] = rec.Msg
	}

	return out
}

func TestRecorderRingWraps(t *testing.T) {
	recorder := NewRecorder(slog.NewJSONHandler(io.Discard, nil), 3)
	for _, msg := range []string{"m0", "m1", "m2", "m3", "m4"} {
		handle(t, recorder, slog.LevelInfo, msg)
	}

	recs, next := recorder.Since(slog.LevelDebug, time.Time{}, time.Time{}, 10)
	got := messages(recs)
	want := []string{"m4", "m3", "m2"}
	if len(got) != len(want) {
		t.Fatalf("ring returned %d records %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d: got %q, want %q (newest-first order broken)", i, got[i], want[i])
		}
	}
	if !next.IsZero() {
		t.Errorf("last page carries a cursor %v", next)
	}
}

func TestSystemLogsLevelFilter(t *testing.T) {
	recorder := NewRecorder(slog.NewJSONHandler(io.Discard, nil), 10)
	handle(t, recorder, slog.LevelDebug, "dbg")
	handle(t, recorder, slog.LevelInfo, "inf")
	handle(t, recorder, slog.LevelWarn, "wrn")
	handle(t, recorder, slog.LevelError, "err")

	recs, _ := recorder.Since(slog.LevelWarn, time.Time{}, time.Time{}, 10)
	got := messages(recs)
	want := []string{"err", "wrn"}
	if len(got) != len(want) {
		t.Fatalf("level=warn returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d: got %q, want %q", i, got[i], want[i])
		}
	}
	if total := recorder.Count(slog.LevelWarn, time.Time{}); total != len(want) {
		t.Errorf("Count at warn: got %d, want %d", total, len(want))
	}
}

// The acceptance rule of docs/17 section 6: redaction lands before the
// ring and before the file write, so stdout, <CONFIG_DIR>/logs/ and the
// recorder — the surface GET /system/logs reads — carry identical
// redacted text.
func TestLoggerSurfacesAgree(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DLTOOL_CONFIG_DIR", dir)

	var stdout bytes.Buffer
	logger := NewLogger(&stdout, "debug", "json")
	logger.Info("engine accepted task",
		slog.String("url", "https://indexer.example.org/api?apikey=abc123"),
		slog.String("passkey", "tracker-secret"),
	)

	stdoutText := stdout.String()
	if strings.Contains(stdoutText, "abc123") || strings.Contains(stdoutText, "tracker-secret") {
		t.Errorf("stdout carries a secret: %s", stdoutText)
	}
	if !strings.Contains(stdoutText, "apikey="+Placeholder) {
		t.Errorf("stdout lacks the placeholder: %s", stdoutText)
	}

	fileBytes, err := os.ReadFile(filepath.Join(dir, "logs", "dl-tool.jsonl"))
	if err != nil {
		t.Fatalf("log file not written: %v", err)
	}
	file := string(fileBytes)
	if strings.Contains(file, "abc123") || strings.Contains(file, "tracker-secret") {
		t.Errorf("log file carries a secret: %s", file)
	}
	if !strings.Contains(file, "apikey="+Placeholder) {
		t.Errorf("log file lacks the placeholder: %s", file)
	}

	recorder, ok := logger.Handler().(*Recorder)
	if !ok {
		t.Fatalf("logger handler is %T, not *Recorder", logger.Handler())
	}
	recs, _ := recorder.Since(slog.LevelDebug, time.Time{}, time.Time{}, 10)
	if len(recs) != 1 {
		t.Fatalf("ring holds %d records, want 1", len(recs))
	}
	if got := recs[0].Attrs["url"]; got != "https://indexer.example.org/api?apikey="+Placeholder {
		t.Errorf("ring url attr: got %v", got)
	}
	if got := recs[0].Attrs["passkey"]; got != Placeholder {
		t.Errorf("ring passkey attr: got %v, want %q", got, Placeholder)
	}
}

// A WithAttrs clone must append to the same ring, or request-scoped
// records would never reach GET /system/logs.
func TestRecorderWithAttrsSharesRing(t *testing.T) {
	recorder := NewRecorder(slog.NewJSONHandler(io.Discard, nil), 10)
	scoped, ok := recorder.WithAttrs([]slog.Attr{slog.String("request_id", "req-1")}).(*Recorder)
	if !ok {
		t.Fatalf("WithAttrs returned %T, not *Recorder", recorder.WithAttrs(nil))
	}
	handle(t, scoped, slog.LevelInfo, "scoped record")

	recs, _ := recorder.Since(slog.LevelDebug, time.Time{}, time.Time{}, 10)
	if len(recs) != 1 || recs[0].Msg != "scoped record" {
		t.Fatalf("ring missed the WithAttrs record: %v", messages(recs))
	}
	if recs[0].Attrs["request_id"] != "req-1" {
		t.Errorf("bound attr not materialised: %v", recs[0].Attrs)
	}
}

func TestLogWriterTruncatesAtCap(t *testing.T) {
	dir := t.TempDir()
	writer, closeFile, err := NewLogWriter(io.Discard, dir, 64)
	if err != nil {
		t.Fatalf("NewLogWriter: %v", err)
	}
	defer func() {
		if err := closeFile(); err != nil {
			t.Errorf("close log file: %v", err)
		}
	}()

	line := []byte(strings.Repeat("x", 32) + "\n")
	for i := 0; i < 8; i++ {
		if _, err := writer.Write(line); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	stat, err := os.Stat(filepath.Join(dir, "logs", "dl-tool.jsonl"))
	if err != nil {
		t.Fatalf("stat log file: %v", err)
	}
	if stat.Size() > 64 {
		t.Errorf("log file is %d bytes, over the 64 cap", stat.Size())
	}
}
