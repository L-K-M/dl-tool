// Log redaction and the stored-record sink of
// docs/17-operations-and-runbook.md section 6: every record is redacted
// before it is stored, so stdout, <CONFIG_DIR>/logs/dl-tool.jsonl and
// GET /system/logs all show the same redacted text.
package obs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/L-K-M/dl-tool/internal/secure"
)

// Placeholder is the literal that replaces every redacted value, in logs and in API responses.
const Placeholder = "__redacted__"

// RedactedAttrKeys are attribute keys whose value is always replaced, whatever its type.
var RedactedAttrKeys = []string{
	"authorization", "cookie", "x-api-key", "api_key", "apikey", "token", "passkey",
	"password", "secret", "session_key", "csrf_key",
}

// redactedURLParameters are the query parameters of an indexer or tracker
// URL whose values never reach a stored record (doc 17 section 6).
var redactedURLParameters = []string{"apikey", "api_key", "token", "passkey"}

// RedactAttr is the slog.HandlerOptions.ReplaceAttr function. It replaces any attribute whose key
// is in RedactedAttrKeys, any value of type secure.Secret, and rewrites any string value that
// parses as a URL through RedactURL. Maps and slices are walked to their leaves — a headers map or
// a settings dump holds the same never-logged classes under the same key names.
func RedactAttr(_ []string, a slog.Attr) slog.Attr {
	if redactedKey(a.Key) {
		return slog.String(a.Key, Placeholder)
	}

	// Resolve so a LogValuer is judged by the value it actually carries;
	// the handlers resolve it the same way at render time.
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, RedactURL(v.String()))
	case slog.KindAny:
		switch v.Any().(type) {
		case secure.Secret, *secure.Secret:
			// A secret is caught by the type alone — under any key, as a
			// value or a pointer, nil included, which is never dereferenced.
			return slog.String(a.Key, Placeholder)
		}

		return slog.Any(a.Key, redactAny(v.Any()))
	default:
		return a
	}
}

// redactedKey reports whether key names a never-logged class
// (docs/17-operations-and-runbook.md section 6), case-insensitively.
func redactedKey(key string) bool {
	return slices.ContainsFunc(RedactedAttrKeys, func(k string) bool {
		return strings.EqualFold(key, k)
	})
}

// redactAny deep-copies a container, applying the redaction rules at every
// leaf: a map key on RedactedAttrKeys, a secure.Secret of either type, and
// a string that parses as a URL — including what an error or a Stringer
// renders, so url.URL and *url.Error values do not leak credentials. The
// copy keeps the caller's value immutable; a non-container is returned
// unchanged.
func redactAny(value any) any {
	switch v := value.(type) {
	case secure.Secret, *secure.Secret:
		return Placeholder
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if redactedKey(key) {
				out[key] = Placeholder
				continue
			}
			out[key] = redactAny(item)
		}

		return out
	case map[string]string:
		out := make(map[string]string, len(v))
		for key, item := range v {
			if redactedKey(key) {
				out[key] = Placeholder
				continue
			}
			out[key] = RedactURL(item)
		}

		return out
	case map[string][]string:
		// http.Header's shape — the never-logged headers' canonical carrier.
		out := make(map[string][]string, len(v))
		for key, items := range v {
			if redactedKey(key) {
				out[key] = []string{Placeholder}
				continue
			}
			redacted := make([]string, len(items))
			for i, item := range items {
				redacted[i] = RedactURL(item)
			}
			out[key] = redacted
		}

		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactAny(item)
		}

		return out
	case []string:
		out := make([]string, len(v))
		for i, item := range v {
			out[i] = RedactURL(item)
		}

		return out
	case error:
		return RedactURL(v.Error())
	case fmt.Stringer:
		return RedactURL(v.String())
	case string:
		return RedactURL(v)
	default:
		return value
	}
}

// RedactURL strips userinfo and rewrites the query parameters apikey, api_key, token and passkey
// to Placeholder. A value that does not parse as a URL is returned unchanged.
//
//	RedactURL("https://indexer.example.org/api?t=search&apikey=abc123")
//	  == "https://indexer.example.org/api?t=search&apikey=__redacted__"
//	RedactURL("ftp://user:pw@host/f.iso") == "ftp://__redacted__@host/f.iso"
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	changed := false
	if u.User != nil {
		// The placeholder takes userinfo's place so a reader sees a
		// credential stood there; url.User renders it bare, without the
		// colon a password would add.
		u.User = url.User(Placeholder)
		changed = true
	}
	if u.RawQuery != "" {
		// Rewrite in place rather than through url.Values, whose Encode
		// sorts keys and would churn every logged URL.
		parts := strings.Split(u.RawQuery, "&")
		for i, part := range parts {
			key, _, _ := strings.Cut(part, "=")
			decoded, unescapeErr := url.QueryUnescape(key)
			if unescapeErr != nil {
				decoded = key
			}
			for _, name := range redactedURLParameters {
				if strings.EqualFold(decoded, name) {
					parts[i] = key + "=" + Placeholder
					changed = true

					break
				}
			}
		}
		if changed {
			u.RawQuery = strings.Join(parts, "&")
		}
	}
	if !changed {
		return raw
	}

	return u.String()
}

// Record is one stored log line, already redacted.
type Record struct {
	At    time.Time      `json:"at"    format:"date-time"`
	Level string         `json:"level" enum:"debug,info,warn,error"`
	Msg   string         `json:"msg"`
	Attrs map[string]any `json:"attrs"`

	// lvl keeps the numeric level alongside its wire tag so Since filters
	// without parsing the string back.
	lvl slog.Level
}

// boundAttr is a WithAttrs attribute together with the group path it was
// captured under: a later WithGroup must not pull an earlier WithAttrs
// attribute into its group, per the slog contract.
type boundAttr struct {
	groups []string
	attr   slog.Attr
}

// recordRing is the fixed-capacity store every Recorder clone shares.
type recordRing struct {
	mu     sync.RWMutex
	buf    []Record
	head   int // index of the oldest live record
	n      int
	lastAt time.Time // At of the newest record; stored instants strictly increase
}

func (r *recordRing) push(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// The page cursor is the previous page's last At, so two records
	// sharing an instant are indistinguishable at a boundary — the older
	// one would be skipped forever. Keep the stored instant strictly
	// increasing in arrival order; the nudge is a nanosecond and only
	// fires on a tie or a backward clock.
	if !rec.At.After(r.lastAt) {
		rec.At = r.lastAt.Add(time.Nanosecond)
	}
	r.lastAt = rec.At

	if r.n < len(r.buf) {
		r.buf[(r.head+r.n)%len(r.buf)] = rec
		r.n++

		return
	}
	// Full: overwrite the oldest and advance head past it.
	r.buf[r.head] = rec
	r.head = (r.head + 1) % len(r.buf)
}

// at returns the i-th record counting from the newest (i=0).
func (r *recordRing) at(i int) Record {
	return r.buf[(r.head+r.n-1-i+len(r.buf))%len(r.buf)]
}

// Recorder is a slog.Handler that forwards to next and keeps the newest capacity records in a ring.
// The ring is shared by every WithAttrs/WithGroup clone, so a request-scoped logger's records reach
// the same store the process logger's do.
type Recorder struct {
	next   slog.Handler
	ring   *recordRing
	bound  []boundAttr
	groups []string
}

var _ slog.Handler = (*Recorder)(nil)

func NewRecorder(next slog.Handler, capacity int) *Recorder {
	if capacity < 1 {
		capacity = 1
	}

	return &Recorder{next: next, ring: &recordRing{buf: make([]Record, capacity)}}
}

func (r *Recorder) Enabled(ctx context.Context, level slog.Level) bool {
	return r.next.Enabled(ctx, level)
}

// Handle materialises the already-redacted record into the ring and then forwards to next.
func (r *Recorder) Handle(ctx context.Context, rec slog.Record) error {
	stored := Record{
		At:    rec.Time.UTC(),
		Level: levelTag(rec.Level),
		lvl:   rec.Level,
		// ReplaceAttr reaches the message on the sink side — the JSON
		// handler runs it on the msg attribute — so the ring applies the
		// same redaction or the API would serve what stdout never shows.
		Msg:   RedactURL(rec.Message),
		Attrs: make(map[string]any, rec.NumAttrs()+len(r.bound)),
	}
	for _, b := range r.bound {
		putAttr(stored.Attrs, b.groups, b.attr)
	}
	rec.Attrs(func(a slog.Attr) bool {
		putAttr(stored.Attrs, r.groups, a)

		return true
	})
	r.ring.push(stored)

	return r.next.Handle(ctx, rec)
}

func (r *Recorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	bound := make([]boundAttr, 0, len(r.bound)+len(attrs))
	bound = append(bound, r.bound...)
	for _, a := range attrs {
		bound = append(bound, boundAttr{groups: slices.Clone(r.groups), attr: a})
	}

	return &Recorder{next: r.next.WithAttrs(attrs), ring: r.ring, bound: bound, groups: r.groups}
}

func (r *Recorder) WithGroup(name string) slog.Handler {
	if name == "" {
		return r
	}

	return &Recorder{
		next:   r.next.WithGroup(name),
		ring:   r.ring,
		bound:  r.bound,
		groups: append(slices.Clone(r.groups), name),
	}
}

// Since returns up to limit records at or above minLevel, newest first. after bounds the page to
// records at or after that instant; cursor is the At of the last record of the previous page, so the
// page resumes strictly older than it — the ring is newest-first. A zero cursor starts at the newest.
// next is the cursor to pass for the following page, or the zero time when the page is the last.
func (r *Recorder) Since(minLevel slog.Level, after time.Time, cursor time.Time, limit int) (recs []Record, next time.Time) {
	recs, next, _ = r.Page(minLevel, after, cursor, limit)

	return recs, next
}

// Page is the one read GET /system/logs needs: a single locked pass that
// collects the newest-first page and counts every record matching the
// level and since filters — the envelope's total — so items and total
// never come from different snapshots of a ring the process logger keeps
// writing. A limit below 1 collects nothing but still counts.
func (r *Recorder) Page(minLevel slog.Level, after time.Time, cursor time.Time, limit int) (recs []Record, next time.Time, total int) {
	r.ring.mu.RLock()
	defer r.ring.mu.RUnlock()

	for i := 0; i < r.ring.n; i++ {
		rec := r.ring.at(i)
		if !rec.matches(minLevel, after) {
			continue
		}
		total++
		if !rec.olderThan(cursor) {
			continue
		}
		if len(recs) >= limit {
			// A further eligible record exists: the page continues at the
			// At of this page's last record.
			if next.IsZero() && len(recs) > 0 {
				next = recs[len(recs)-1].At
			}

			continue
		}
		recs = append(recs, rec)
	}

	return recs, next, total
}

// Count reports how many stored records satisfy the level and since
// filters while ignoring the cursor — the pagination envelope's total
// (docs/05-api-contract.md section 1.4).
func (r *Recorder) Count(minLevel slog.Level, after time.Time) int {
	_, _, total := r.Page(minLevel, after, time.Time{}, 0)

	return total
}

// matches applies the level floor and the since bound — the filters the
// envelope's total shares with the page itself.
func (r Record) matches(minLevel slog.Level, after time.Time) bool {
	return r.lvl >= minLevel && (after.IsZero() || !r.At.Before(after))
}

// olderThan applies the page cursor: the page is newest-first, so a
// record shows only when its At is older than the previous page's last
// record. Stored instants strictly increase with arrival — see
// recordRing.push — so the comparison never drops a record that shares
// the boundary's clock tick.
func (r Record) olderThan(cursor time.Time) bool {
	return cursor.IsZero() || r.At.Before(cursor)
}

// putAttr stores one already-redacted attribute under its group path in dst.
func putAttr(dst map[string]any, groups []string, a slog.Attr) {
	node := dst
	for _, g := range groups {
		child, ok := node[g].(map[string]any)
		if !ok {
			child = make(map[string]any)
			node[g] = child
		}
		node = child
	}

	a = RedactAttr(groups, a)
	if a.Value.Kind() == slog.KindGroup {
		sub := make(map[string]any, len(a.Value.Group()))
		for _, member := range a.Value.Group() {
			putAttr(sub, nil, member)
		}
		node[a.Key] = sub

		return
	}
	node[a.Key] = attrValue(a.Value)
}

// attrValue renders a slog.Value as the JSON-marshalable value the API
// returns; strings and secrets arrive already redacted by RedactAttr.
func attrValue(v slog.Value) any {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindBool:
		return v.Bool()
	case slog.KindTime:
		return v.Time().UTC()
	case slog.KindDuration:
		// Nanoseconds, matching the JSON handler's rendering.
		return v.Duration().Nanoseconds()
	case slog.KindLogValuer:
		return attrValue(v.Resolve())
	case slog.KindGroup:
		sub := make(map[string]any, len(v.Group()))
		for _, member := range v.Group() {
			putAttr(sub, nil, member)
		}

		return sub
	default:
		value := v.Any()
		// A LogValuer resolved above can still surface a Secret under a
		// key RedactAttr does not know — catch it by type again.
		if _, isSecret := value.(secure.Secret); isSecret {
			return Placeholder
		}
		if _, isSecret := value.(*secure.Secret); isSecret {
			return Placeholder
		}
		if err, ok := value.(error); ok {
			return err.Error()
		}

		return value
	}
}

// levelTag renders the four stored levels of the Record contract.
func levelTag(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}

// NewLogWriter returns a writer that tees to stdout and to <configDir>/logs/dl-tool.jsonl,
// truncating the file when it passes maxBytes so an operator's /config cannot fill up.
// The close function releases the file; it exists for callers that own a
// shorter-lived sink — the process logger keeps it open for the process
// lifetime, as it does stdout.
func NewLogWriter(stdout io.Writer, configDir string, maxBytes int64) (io.Writer, func() error, error) {
	file, closeFile, err := logFileWriter(configDir, maxBytes)
	if err != nil {
		return nil, nil, err
	}

	return io.MultiWriter(stdout, file), closeFile, nil
}

// logFileWriter opens <configDir>/logs/dl-tool.jsonl under the single size
// cap — the file side of the tee NewLogWriter builds, and the always-JSON
// sink NewLogger pairs with the console handler. The directory gets 0755:
// world-writable would let another local user pre-place a symlink at the
// file's path. The file keeps 0666, the mode the task contract prescribes
// so the container umask decides the result.
func logFileWriter(configDir string, maxBytes int64) (io.Writer, func() error, error) {
	dir := filepath.Join(configDir, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create log directory: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, "dl-tool.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o666)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file: %w", err)
	}

	capped := &cappedWriter{file: file, max: maxBytes}
	if stat, err := file.Stat(); err == nil {
		capped.size = stat.Size()
	}

	return capped, file.Close, nil
}

// fanoutHandler mirrors every record to two handlers — the console in the
// operator's chosen format and the log file, which is always JSON because
// the .jsonl name promises one object per line whatever stdout shows.
type fanoutHandler struct {
	console slog.Handler
	file    slog.Handler
}

var _ slog.Handler = fanoutHandler{}

func (f fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return f.console.Enabled(ctx, level) || f.file.Enabled(ctx, level)
}

func (f fanoutHandler) Handle(ctx context.Context, rec slog.Record) error {
	if err := f.console.Handle(ctx, rec); err != nil {
		return err
	}

	return f.file.Handle(ctx, rec)
}

func (f fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return fanoutHandler{console: f.console.WithAttrs(attrs), file: f.file.WithAttrs(attrs)}
}

func (f fanoutHandler) WithGroup(name string) slog.Handler {
	return fanoutHandler{console: f.console.WithGroup(name), file: f.file.WithGroup(name)}
}

// cappedWriter truncates the file back to empty once a write would carry it
// past max — the single size cap, no rotation, per the task's scope rule.
type cappedWriter struct {
	file *os.File
	max  int64
	size int64
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	// A single record larger than the cap would leave the file over max
	// even right after truncation; dropping it reports success without
	// writing — the io.Writer deviation is deliberate for a capped sink.
	if w.max > 0 && int64(len(p)) > w.max {
		return len(p), nil
	}
	if w.max > 0 && w.size+int64(len(p)) > w.max {
		if err := w.file.Truncate(0); err != nil {
			return 0, fmt.Errorf("truncate log file: %w", err)
		}
		w.size = 0
	}
	n, err := w.file.Write(p)
	w.size += int64(n)

	return n, err
}
