package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
	// A credential-bearing URL as the message: ReplaceAttr reaches msg on
	// the sink side, so the ring must apply the same redaction itself.
	logger.Info("https://indexer.example.org/api?apikey=abc123")

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
	if len(recs) != 2 {
		t.Fatalf("ring holds %d records, want 2", len(recs))
	}
	if strings.Contains(recs[0].Msg, "abc123") || !strings.Contains(recs[0].Msg, "apikey="+Placeholder) {
		t.Errorf("ring message kept its credential: %q", recs[0].Msg)
	}
	if got := recs[1].Attrs["url"]; got != "https://indexer.example.org/api?apikey="+Placeholder {
		t.Errorf("ring url attr: got %v", got)
	}
	if got := recs[1].Attrs["passkey"]; got != Placeholder {
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

// A map or slice attr holds the same never-logged classes under the same
// key names, so redaction walks containers on every surface.
func TestRedactNestedContainer(t *testing.T) {
	var stdout bytes.Buffer
	recorder := NewRecorder(slog.NewJSONHandler(&stdout, &slog.HandlerOptions{ReplaceAttr: RedactAttr}), 10)
	sent := map[string]any{
		"Authorization": "Bearer live-credential",
		"Referer":       "https://indexer.example.org/api?token=abc123",
	}
	handle(t, recorder, slog.LevelInfo, "request",
		slog.Any("headers", sent),
		slog.Any("header_pairs", map[string][]string{"X-Api-Key": {"k3y"}, "Accept": {"*/*"}}),
		slog.Any("chain", []any{"https://h/t?passkey=zzz", secure.Secret("shh")}),
	)

	out := stdout.String()
	for _, secret := range []string{"live-credential", "abc123", "k3y", "zzz", "shh"} {
		if strings.Contains(out, secret) {
			t.Fatalf("stdout carries unredacted nested value %q: %s", secret, out)
		}
	}

	recs, _ := recorder.Since(slog.LevelDebug, time.Time{}, time.Time{}, 10)
	if len(recs) != 1 {
		t.Fatalf("ring holds %d records, want 1", len(recs))
	}
	headers, ok := recs[0].Attrs["headers"].(map[string]any)
	if !ok {
		t.Fatalf("headers attr stored as %T", recs[0].Attrs["headers"])
	}
	if headers["Authorization"] != Placeholder {
		t.Errorf("nested Authorization: got %v, want %q", headers["Authorization"], Placeholder)
	}
	if referer, _ := headers["Referer"].(string); strings.Contains(referer, "abc123") {
		t.Errorf("nested Referer kept its token: %q", referer)
	}
	pairs, ok := recs[0].Attrs["header_pairs"].(map[string][]string)
	if !ok {
		t.Fatalf("header_pairs attr stored as %T", recs[0].Attrs["header_pairs"])
	}
	if pairs["X-Api-Key"][0] != Placeholder {
		t.Errorf("nested X-Api-Key: got %v", pairs["X-Api-Key"])
	}
	chain, ok := recs[0].Attrs["chain"].([]any)
	if !ok {
		t.Fatalf("chain attr stored as %T", recs[0].Attrs["chain"])
	}
	if chain[1] != Placeholder {
		t.Errorf("nested secret: got %v, want %q", chain[1], Placeholder)
	}
	if s, _ := chain[0].(string); strings.Contains(s, "zzz") {
		t.Errorf("slice string kept its passkey: %q", s)
	}
	// The caller's map must be untouched — Referer is the entry whose
	// redacted form differs from its input, so an in-place mutation shows.
	if sent["Referer"] != "https://indexer.example.org/api?token=abc123" ||
		sent["Authorization"] != "Bearer live-credential" {
		t.Errorf("redaction mutated the caller-owned map: %v", sent)
	}
}

// A url.URL value satisfies neither error nor fmt.Stringer (String is a
// pointer method) and a typed-nil error or Stringer must not panic inside
// ReplaceAttr.
func TestRedactURLValueAndTypedNil(t *testing.T) {
	u, err := url.Parse("https://indexer.example.org/api?token=abc123")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	got := redactedFrom(slog.Any("u", *u))
	if s, _ := got.Any().(string); strings.Contains(s, "abc123") || !strings.Contains(s, "token="+Placeholder) {
		t.Errorf("url.URL value not redacted: %v", got.Any())
	}

	for name, value := range map[string]any{"url": (*url.URL)(nil), "err": (*url.Error)(nil)} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: redaction panicked on a typed nil: %v", name, r)
				}
			}()
			if got := redactedFrom(slog.Any(name, value)).Any(); got != nil {
				t.Errorf("%s: typed nil rendered as %v, want nil", name, got)
			}
		}()
	}
}

// handleAt feeds one record with a fixed instant — identical timestamps
// are how a page boundary is exercised.
func handleAt(t *testing.T, r *Recorder, at time.Time, msg string) {
	t.Helper()
	rec := slog.NewRecord(at, slog.LevelInfo, msg, 0)
	if err := r.Handle(context.Background(), rec); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

// TestSincePaginatesWithCursor pages a five-record ring two at a time —
// the multi-page path — including records sharing an instant, which a
// timestamp-only cursor must neither skip nor repeat.
func TestSincePaginatesWithCursor(t *testing.T) {
	recorder := NewRecorder(slog.NewJSONHandler(io.Discard, nil), 10)
	base := time.Now()
	handleAt(t, recorder, base, "m0")
	handleAt(t, recorder, base, "m1") // same tick as m0, the page-2 boundary
	handleAt(t, recorder, base.Add(time.Second), "m2")
	handleAt(t, recorder, base.Add(2*time.Second), "m3")
	handleAt(t, recorder, base.Add(2*time.Second), "m4") // same tick as m3, inside page 1

	var got []string
	var cursor time.Time
	// Five records at limit 2 need three pages; a cursor that fails to
	// advance must fall out of the loop into a readable diff, not hang.
	for page := 0; page < 8; page++ {
		recs, next := recorder.Since(slog.LevelDebug, time.Time{}, cursor, 2)
		got = append(got, messages(recs)...)
		if next.IsZero() {
			break
		}
		cursor = next
	}

	want := []string{"m4", "m3", "m2", "m1", "m0"}
	if !slices.Equal(got, want) {
		t.Fatalf("paged through %v, want %v", got, want)
	}
	if total := recorder.Count(slog.LevelDebug, time.Time{}); total != len(want) {
		t.Errorf("Count %d disagrees with the paged union %d", total, len(want))
	}
}

// The file is dl-tool.jsonl — one JSON object per line even when the
// console renders text.
func TestLogFileStaysJSONInTextFormat(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DLTOOL_CONFIG_DIR", dir)

	var stdout bytes.Buffer
	logger := NewLogger(&stdout, "info", "text")
	logger.Info("text format record")

	fileBytes, err := os.ReadFile(filepath.Join(dir, "logs", "dl-tool.jsonl"))
	if err != nil {
		t.Fatalf("log file not written: %v", err)
	}
	for i, line := range strings.Split(strings.TrimSpace(string(fileBytes)), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("file line %d is not JSON: %q: %v", i, line, err)
		}
	}
	if !strings.Contains(stdout.String(), "text format record") {
		t.Errorf("stdout lacks the record: %s", stdout.String())
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

// A record larger than the whole cap is dropped rather than parked over
// max until the next write.
func TestLogWriterDropsOversizedRecord(t *testing.T) {
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

	if _, err := writer.Write([]byte(strings.Repeat("x", 128) + "\n")); err != nil {
		t.Fatalf("oversized write: %v", err)
	}

	stat, err := os.Stat(filepath.Join(dir, "logs", "dl-tool.jsonl"))
	if err != nil {
		t.Fatalf("stat log file: %v", err)
	}
	if stat.Size() != 0 {
		t.Errorf("log file is %d bytes; an oversized record must be dropped whole, not partially written", stat.Size())
	}
}
