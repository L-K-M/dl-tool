package jobs

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

// notifyTestKey stands in for cfg.SecretKey: the at-rest key channel
// secrets seal under.
const notifyTestKey = secure.Secret("notify-test-key-0001")

// notifyClient builds the same SSRF-guarded client the notifier is handed
// in production, lifted for the loopback stub the way the RSS tests do it
// (allow-private plus ForOrigin so the stub's port is permitted).
func notifyClient(t *testing.T, rawURL string) *http.Client {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	return secure.NewClient(secure.NewGuard(log, true).ForOrigin(u))
}

// insertChannel stores one notification_channels row with the secret
// sealed exactly as a T106 write will, and returns its id.
func insertChannel(
	t *testing.T,
	db *sqlx.DB,
	kind, name string,
	enabled bool,
	config map[string]any,
	mask []string,
	secret string,
) string {
	t.Helper()

	configJSON, err := json.Marshal(config)
	require.NoError(t, err)
	maskJSON, err := json.Marshal(mask)
	require.NoError(t, err)
	enc, err := store.SealNotificationSecret(notifyTestKey, secret)
	require.NoError(t, err)

	id := store.NewID(store.PrefixNotificationChannel)
	flag := 0
	if enabled {
		flag = 1
	}
	now := time.Now().UnixMilli()
	_, err = db.ExecContext(
		t.Context(),
		`INSERT INTO notification_channels
(id, kind, name, enabled, config_json, secret_enc, event_mask, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, kind, name, flag, string(configJSON), enc, string(maskJSON), now, now,
	)
	require.NoError(t, err)

	return id
}

func getChannel(t *testing.T, db *sqlx.DB, id string) store.NotificationChannel {
	t.Helper()
	ch, err := store.NewSettingsStore(db).GetNotificationChannel(t.Context(), id)
	require.NoError(t, err)

	return ch
}

// recordedRequest is one delivery a stub server saw.
type recordedRequest struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
}

// recordingStub answers every request with status and respBody and keeps
// what it received. The handler runs on the server's goroutine while the
// test reads — a worker-driven delivery races a waitFor poll — so every
// access goes through the mutex.
type recordingStub struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
}

func newRecordingStub(t *testing.T, status int, respBody string) *recordingStub {
	t.Helper()
	stub := &recordingStub{}
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		stub.mu.Lock()
		stub.requests = append(stub.requests, recordedRequest{
			Method: r.Method,
			URL:    r.URL.String(),
			Header: r.Header.Clone(),
			Body:   body,
		})
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := w.Write([]byte(respBody)); err != nil {
			t.Errorf("stub write: %v", err)
		}
	}))
	t.Cleanup(stub.srv.Close)

	return stub
}

// all returns a copy of the recorded requests, safe to read while the
// server may still be appending.
func (s *recordingStub) all() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]recordedRequest(nil), s.requests...)
}

// count reports how many requests arrived so far.
func (s *recordingStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.requests)
}

func testEvent() Event {
	return Event{
		Code:    store.CodeTaskCompleted,
		TaskID:  "tsk_test",
		Name:    "release.iso",
		State:   "completed",
		Message: "download finished",
		At:      time.UnixMilli(1_700_000_000_000),
	}
}

func TestMaskSelectsOnlyListedCodes(t *testing.T) {
	require.True(t, Matches([]string{"task.completed"}, "task.completed"))
	require.False(t, Matches([]string{"task.completed"}, "task.error"))
	require.True(t, Matches([]string{"*"}, "task.completed"))
	require.True(t, Matches([]string{"*"}, "task.error"))
	require.False(t, Matches(nil, "task.completed"))
	// The mask is a list, not a glob: "task.*" is not a wildcard.
	require.False(t, Matches([]string{"task.*"}, "task.completed"))

	db := newTestDB(t)
	notifier := NewNotifier(db, notifyTestKey, nil)

	matched := insertChannel(t, db, "webhook", "matched", true,
		map[string]any{"url": "http://example.invalid/"}, []string{"task.completed"}, "")
	star := insertChannel(t, db, "webhook", "star", true,
		map[string]any{"url": "http://example.invalid/"}, []string{"*"}, "")
	insertChannel(t, db, "webhook", "disabled", false,
		map[string]any{"url": "http://example.invalid/"}, []string{"task.completed"}, "")

	require.NoError(t, notifier.Fanout(t.Context(), Event{Code: "task.completed"}))
	require.Equal(t, []string{matched, star}, fanoutChannels(t, db))

	// A code the mask does not list enqueues for ["*"] only; the disabled
	// channel receives nothing.
	require.NoError(t, notifier.Fanout(t.Context(), Event{Code: "task.error"}))
	require.Equal(t, []string{matched, star, star}, fanoutChannels(t, db))
}

// fanoutChannels lists the channel_id of every webhook job row, in
// insertion order — rowid, because two inserts can share a millisecond
// and the ULID's random tail does not preserve call order.
func fanoutChannels(t *testing.T, db *sqlx.DB) []string {
	t.Helper()
	var payloads []string
	require.NoError(t, db.SelectContext(
		t.Context(), &payloads,
		`SELECT payload_json FROM jobs WHERE kind = ? ORDER BY rowid`, JobKindWebhook,
	))
	ids := make([]string, 0, len(payloads))
	for _, raw := range payloads {
		var payload webhookPayload
		require.NoError(t, json.Unmarshal([]byte(raw), &payload))
		ids = append(ids, payload.ChannelID)
	}

	return ids
}

func TestWebhookShape(t *testing.T) {
	db := newTestDB(t)
	stub := newRecordingStub(t, http.StatusNoContent, "")

	id := insertChannel(t, db, "webhook", "hook", true, map[string]any{
		"url":           stub.srv.URL + "/hook?apikey=qsekrit",
		"method":        "PUT",
		"headers":       map[string]string{"X-Custom": "yes"},
		"body_template": `{"code":"{{.Code}}","task":"{{.TaskID}}"}`,
	}, []string{"task.completed"}, "wh-secret")

	notifier := NewNotifier(db, notifyTestKey, notifyClient(t, stub.srv.URL))
	reply, err := notifier.Send(t.Context(), getChannel(t, db, id), testEvent())
	require.NoError(t, err)
	require.True(t, reply.OK)

	require.Len(t, stub.all(), 1)
	got := stub.all()[0]
	require.Equal(t, http.MethodPut, got.Method)
	require.Equal(t, "/hook?apikey=qsekrit", got.URL)
	require.Equal(t, "yes", got.Header.Get("X-Custom"))
	require.Equal(t, "Bearer wh-secret", got.Header.Get("Authorization"))
	require.JSONEq(t, `{"code":"task.completed","task":"tsk_test"}`, string(got.Body))

	// The reply's request member redacts the query secret.
	require.Equal(t, stub.srv.URL+"/hook", reply.Request.URL)
}

// TestWebhookDefaultBody covers the empty body_template case: the Event
// JSON is the body.
func TestWebhookDefaultBody(t *testing.T) {
	db := newTestDB(t)
	stub := newRecordingStub(t, http.StatusOK, "{}")

	id := insertChannel(t, db, "webhook", "hook", true, map[string]any{
		"url": stub.srv.URL,
	}, []string{"*"}, "")

	notifier := NewNotifier(db, notifyTestKey, notifyClient(t, stub.srv.URL))
	reply, err := notifier.Send(t.Context(), getChannel(t, db, id), testEvent())
	require.NoError(t, err)
	require.True(t, reply.OK)

	require.Len(t, stub.all(), 1)
	got := stub.all()[0]
	require.Equal(t, http.MethodPost, got.Method)
	require.Equal(t, "application/json", got.Header.Get("Content-Type"))
	require.Empty(t, got.Header.Get("Authorization"))
	var event Event
	require.NoError(t, json.Unmarshal(got.Body, &event))
	require.Equal(t, testEvent().Code, event.Code)
	require.Equal(t, testEvent().TaskID, event.TaskID)
}

func TestNtfyShape(t *testing.T) {
	db := newTestDB(t)
	stub := newRecordingStub(t, http.StatusOK, "{}")

	id := insertChannel(t, db, "ntfy", "phone", true, map[string]any{
		"server_url": stub.srv.URL,
		"topic":      "dl-tool-alice",
		"priority":   4,
		"tags":       []string{"inbox_tray", "dl"},
		"click_url":  "https://example.com/task",
	}, []string{"*"}, "ntfy-token")

	notifier := NewNotifier(db, notifyTestKey, notifyClient(t, stub.srv.URL))
	reply, err := notifier.Send(t.Context(), getChannel(t, db, id), testEvent())
	require.NoError(t, err)
	require.True(t, reply.OK)

	require.Len(t, stub.all(), 1)
	got := stub.all()[0]
	require.Equal(t, http.MethodPost, got.Method)
	require.Equal(t, "/dl-tool-alice", got.URL)
	require.Equal(t, "4", got.Header.Get("Priority"))
	require.Equal(t, "inbox_tray,dl", got.Header.Get("Tags"))
	require.Equal(t, "https://example.com/task", got.Header.Get("Click"))
	require.Equal(t, "Bearer ntfy-token", got.Header.Get("Authorization"))
	require.Contains(t, got.Header.Get("Content-Type"), "text/plain")
	require.Equal(t, "task.completed: release.iso: download finished", string(got.Body))
}

func TestGotifyShape(t *testing.T) {
	db := newTestDB(t)
	stub := newRecordingStub(t, http.StatusOK, "{}")

	id := insertChannel(t, db, "gotify", "server", true, map[string]any{
		"server_url": stub.srv.URL,
		"priority":   5,
	}, []string{"*"}, "gotify-app-token")

	notifier := NewNotifier(db, notifyTestKey, notifyClient(t, stub.srv.URL))
	reply, err := notifier.Send(t.Context(), getChannel(t, db, id), testEvent())
	require.NoError(t, err)
	require.True(t, reply.OK)

	require.Len(t, stub.all(), 1)
	got := stub.all()[0]
	require.Equal(t, http.MethodPost, got.Method)
	require.Equal(t, "/message", got.URL)
	require.Equal(t, "gotify-app-token", got.Header.Get("X-Gotify-Key"))
	require.Equal(t, "application/x-www-form-urlencoded", got.Header.Get("Content-Type"))
	form, err := url.ParseQuery(string(got.Body))
	require.NoError(t, err)
	require.Equal(t, "release.iso", form.Get("title"))
	require.Equal(t, "download finished", form.Get("message"))
	require.Equal(t, "5", form.Get("priority"))
}

func TestAppriseShape(t *testing.T) {
	db := newTestDB(t)
	stub := newRecordingStub(t, http.StatusOK, "{}")

	config := map[string]any{
		"base_url":   stub.srv.URL + "/api",
		"config_key": "mykey",
		"urls":       []string{"json://localhost/hook"},
		"tag":        "all",
		"type":       "success",
		"format":     "text",
	}
	id := insertChannel(t, db, "apprise", "bus", true, config, []string{"*"}, "")

	notifier := NewNotifier(db, notifyTestKey, notifyClient(t, stub.srv.URL))
	reply, err := notifier.Send(t.Context(), getChannel(t, db, id), testEvent())
	require.NoError(t, err)
	require.True(t, reply.OK)

	require.Len(t, stub.all(), 1)
	got := stub.all()[0]
	require.Equal(t, http.MethodPost, got.Method)
	require.Equal(t, "/api/notify/mykey", got.URL)
	require.Equal(t, "application/json", got.Header.Get("Content-Type"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(got.Body, &body))
	require.Equal(t, "release.iso", body["title"])
	require.Equal(t, "task.completed: release.iso: download finished", body["body"])
	require.Equal(t, "success", body["type"])
	require.Equal(t, "text", body["format"])
	require.Equal(t, "all", body["tag"])
	require.Equal(t, []any{"json://localhost/hook"}, body["urls"])

	// With a stored secret the urls member is the secret itself: apprise
	// URLs embed their credentials, so the sensitive list lives in
	// secret_enc, not in plaintext config_json.
	secretID := insertChannel(t, db, "apprise", "bus-secret", true, config, []string{"*"}, "discord://id/token")
	_, err = notifier.Send(t.Context(), getChannel(t, db, secretID), testEvent())
	require.NoError(t, err)
	require.Len(t, stub.all(), 2)
	require.NoError(t, json.Unmarshal(stub.all()[1].Body, &body))
	require.Equal(t, "discord://id/token", body["urls"])
}

func TestRawReplyCarriesStatusLine(t *testing.T) {
	db := newTestDB(t)
	upstream := `{"code":40301,"http":403,"error":"forbidden"}`
	stub := newRecordingStub(t, http.StatusForbidden, upstream)

	id := insertChannel(t, db, "webhook", "hook", true,
		map[string]any{"url": stub.srv.URL}, []string{"*"}, "")

	notifier := NewNotifier(db, notifyTestKey, notifyClient(t, stub.srv.URL))
	reply, err := notifier.Send(t.Context(), getChannel(t, db, id), testEvent())
	require.NoError(t, err)
	require.False(t, reply.OK)
	require.Nil(t, reply.Error)
	require.NotNil(t, reply.Response)
	require.Equal(t, "HTTP/1.1 403 Forbidden", reply.Response.StatusLine)
	require.Equal(t, http.StatusForbidden, reply.Response.Status)
	require.Equal(t, upstream, reply.Response.Body)

	// The channel row records the attempt: last_send_at stamped,
	// last_error carrying the upstream's own status line.
	ch := getChannel(t, db, id)
	require.NotNil(t, ch.LastSendAt)
	require.NotNil(t, ch.LastError)
	require.Equal(t, "HTTP/1.1 403 Forbidden", *ch.LastError)
}

func TestBodyTruncatedAt8KiB(t *testing.T) {
	db := newTestDB(t)
	payload := strings.Repeat("0123456789abcdef", BodyCap/16+64)
	stub := newRecordingStub(t, http.StatusOK, payload)

	id := insertChannel(t, db, "webhook", "hook", true,
		map[string]any{"url": stub.srv.URL}, []string{"*"}, "")

	notifier := NewNotifier(db, notifyTestKey, notifyClient(t, stub.srv.URL))
	reply, err := notifier.Send(t.Context(), getChannel(t, db, id), testEvent())
	require.NoError(t, err)
	require.True(t, reply.OK)
	require.NotNil(t, reply.Response)
	require.Len(t, reply.Response.Body, BodyCap)
	require.Equal(t, payload[:BodyCap], reply.Response.Body)
}

func TestSecretNeverEchoed(t *testing.T) {
	db := newTestDB(t)
	const secret = "ntfy-super-secret-value"

	// The secret rides inside the request URL's query here, so any echo —
	// request member, transport error or channel last_error — that keeps
	// the raw URL leaks it.
	stub := newRecordingStub(t, http.StatusOK, "{}")
	id := insertChannel(t, db, "webhook", "hook", true, map[string]any{
		"url": stub.srv.URL + "/hook?apikey=" + secret,
	}, []string{"*"}, "unrelated-token")

	notifier := NewNotifier(db, notifyTestKey, notifyClient(t, stub.srv.URL))
	reply, err := notifier.Send(t.Context(), getChannel(t, db, id), testEvent())
	require.NoError(t, err)
	encoded, err := json.Marshal(reply)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), secret)
	require.NotContains(t, string(encoded), "unrelated-token")

	// The transport-failure path redacts the URL inside *url.Error too:
	// a refused connection carries the same query string and must not
	// leak it into reply.Error or last_error.
	dead := newRecordingStub(t, http.StatusOK, "")
	deadURL := dead.srv.URL
	dead.srv.Close()
	deadID := insertChannel(t, db, "webhook", "dead", true, map[string]any{
		"url": deadURL + "/hook?apikey=" + secret,
	}, []string{"*"}, "")
	deadNotifier := NewNotifier(db, notifyTestKey, notifyClient(t, deadURL))
	reply, err = deadNotifier.Send(t.Context(), getChannel(t, db, deadID), testEvent())
	require.NoError(t, err)
	require.NotNil(t, reply.Error)
	require.NotContains(t, *reply.Error, secret)
	ch := getChannel(t, db, deadID)
	require.NotNil(t, ch.LastError)
	require.NotContains(t, *ch.LastError, secret)
}

func TestUnreachableUpstream(t *testing.T) {
	db := newTestDB(t)
	stub := newRecordingStub(t, http.StatusOK, "")
	deadURL := stub.srv.URL
	stub.srv.Close()

	id := insertChannel(t, db, "webhook", "hook", true,
		map[string]any{"url": deadURL}, []string{"*"}, "")
	notifier := NewNotifier(db, notifyTestKey, notifyClient(t, deadURL))

	reply, err := notifier.Send(t.Context(), getChannel(t, db, id), testEvent())
	require.NoError(t, err)
	require.False(t, reply.OK)
	require.Nil(t, reply.Response)
	require.NotNil(t, reply.Error)
	require.Contains(t, *reply.Error, "refused")
}

// TestChainFanoutDeliversEvent runs the whole path the composition root
// wires: OnCompleted's tail fans the terminal event out, the worker pool
// claims the webhook job, and the stub records the Event JSON.
func TestChainFanoutDeliversEvent(t *testing.T) {
	db := newTestDB(t)
	stub := newRecordingStub(t, http.StatusOK, "{}")

	id := insertChannel(t, db, "webhook", "hook", true,
		map[string]any{"url": stub.srv.URL}, []string{"task.completed"}, "")
	notifier := NewNotifier(db, notifyTestKey, notifyClient(t, stub.srv.URL))

	// A payload inside its own destination owes neither extract nor move,
	// so the chain reaches the tail on the first pass.
	dest := t.TempDir()
	taskID := newCompletedTask(t, db, dest, filepath.Join(dest, "payload.bin"))

	chain := NewChain(db, store.NewTaskStore(db))
	chain.SetNotifier(notifier)
	require.NoError(t, chain.OnCompleted(t.Context(), taskID))

	worker := newTestWorker(db)
	worker.Register(JobKindWebhook, notifier.Handle)
	startWorker(t, worker)

	waitFor(t, "webhook delivery", func() bool {
		return stub.count() == 1
	})
	got := stub.all()[0]
	require.Equal(t, http.MethodPost, got.Method)
	var event Event
	require.NoError(t, json.Unmarshal(got.Body, &event))
	require.Equal(t, store.CodeTaskCompleted, event.Code)
	require.Equal(t, taskID, event.TaskID)
	require.Equal(t, "payload.bin", event.Name)

	// The channel row records the successful delivery.
	ch := getChannel(t, db, id)
	require.NotNil(t, ch.LastSendAt)
	require.Nil(t, ch.LastError)
}

// TestFanoutDeduplicatesIdenticalEvent covers the enqueue-level
// idempotence: a re-entered chain pass rebuilds the identical payload, so
// the second Fanout adds no row.
func TestFanoutDeduplicatesIdenticalEvent(t *testing.T) {
	db := newTestDB(t)
	insertChannel(t, db, "webhook", "hook", true,
		map[string]any{"url": "http://example.invalid/"}, []string{"*"}, "")
	notifier := NewNotifier(db, notifyTestKey, nil)

	ev := testEvent()
	require.NoError(t, notifier.Fanout(t.Context(), ev))
	require.NoError(t, notifier.Fanout(t.Context(), ev))
	require.Len(t, fanoutChannels(t, db), 1)
}

// TestRedeliveredJobResends pins the at-least-once contract: a replayed
// job — the crash window between Send's success and the worker's done
// write — delivers again rather than guessing per-event dedupe from the
// channel's last_send_at, which would silently drop any queued event
// older than the newest send.
func TestRedeliveredJobResends(t *testing.T) {
	db := newTestDB(t)
	stub := newRecordingStub(t, http.StatusOK, "{}")

	insertChannel(t, db, "webhook", "hook", true,
		map[string]any{"url": stub.srv.URL}, []string{"*"}, "")
	notifier := NewNotifier(db, notifyTestKey, notifyClient(t, stub.srv.URL))

	ev := testEvent()
	require.NoError(t, notifier.Fanout(t.Context(), ev))

	job, err := store.ClaimJob(t.Context(), db, time.Now().UnixMilli())
	require.NoError(t, err)
	require.Equal(t, JobKindWebhook, job.Kind)
	// The row keeps task_id NULL so the auto-remove tail cannot
	// cascade-delete a pending delivery.
	require.Nil(t, job.TaskID)

	require.NoError(t, notifier.Handle(t.Context(), job))
	require.Len(t, stub.all(), 1)

	// Replaying the same job — as a re-claimed crash window would —
	// delivers a second time. Two events queued on one channel must both
	// arrive, so no send-level suppression exists.
	require.NoError(t, notifier.Handle(t.Context(), job))
	require.Len(t, stub.all(), 2)
}

// TestMalformedMaskSkipsOnlyThatChannel covers the isolation rule: one
// channel's broken event_mask never suppresses the other channels'
// deliveries.
func TestMalformedMaskSkipsOnlyThatChannel(t *testing.T) {
	db := newTestDB(t)

	good := insertChannel(t, db, "webhook", "good", true,
		map[string]any{"url": "http://example.invalid/"}, []string{"*"}, "")
	// A row T106 would never write — event_mask is not JSON at all.
	badID := store.NewID(store.PrefixNotificationChannel)
	now := time.Now().UnixMilli()
	_, err := db.ExecContext(
		t.Context(),
		`INSERT INTO notification_channels
(id, kind, name, enabled, config_json, event_mask, created_at, updated_at)
VALUES (?, 'webhook', 'broken', 1, '{"url":"http://example.invalid/"}', 'not-json', ?, ?)`,
		badID, now, now,
	)
	require.NoError(t, err)

	notifier := NewNotifier(db, notifyTestKey, nil)
	require.NoError(t, notifier.Fanout(t.Context(), testEvent()))
	require.Equal(t, []string{good}, fanoutChannels(t, db))
}

// TestFailedDeliveryRetries covers the backoff contract of doc 04
// section 4.8: a non-2xx upstream makes the handler return an error so
// the worker reschedules, and last_error carries the status line.
func TestFailedDeliveryRetries(t *testing.T) {
	db := newTestDB(t)
	stub := newRecordingStub(t, http.StatusInternalServerError, "boom")

	id := insertChannel(t, db, "webhook", "hook", true,
		map[string]any{"url": stub.srv.URL}, []string{"*"}, "")
	notifier := NewNotifier(db, notifyTestKey, notifyClient(t, stub.srv.URL))

	require.NoError(t, notifier.Fanout(t.Context(), testEvent()))
	job, err := store.ClaimJob(t.Context(), db, time.Now().UnixMilli())
	require.NoError(t, err)

	err = notifier.Handle(t.Context(), job)
	require.Error(t, err)
	require.Contains(t, err.Error(), "500 Internal Server Error")
	ch := getChannel(t, db, id)
	require.NotNil(t, ch.LastError)
	require.Equal(t, "HTTP/1.1 500 Internal Server Error", *ch.LastError)
}

// TestUnknownConfigKeyRejected covers the validation rule of the per-kind
// key sets: an unknown key is an error, never a silently ignored field —
// and the failed attempt is still recorded on the channel row so an
// operator sees why nothing went out.
func TestUnknownConfigKeyRejected(t *testing.T) {
	db := newTestDB(t)
	stub := newRecordingStub(t, http.StatusOK, "{}")

	id := insertChannel(t, db, "gotify", "server", true, map[string]any{
		"server_url": stub.srv.URL,
		"bogus":      "not a gotify key",
	}, []string{"*"}, "")
	notifier := NewNotifier(db, notifyTestKey, notifyClient(t, stub.srv.URL))

	_, err := notifier.Send(t.Context(), getChannel(t, db, id), testEvent())
	require.Error(t, err)
	require.Contains(t, err.Error(), "bogus")
	require.Empty(t, stub.all())

	ch := getChannel(t, db, id)
	require.NotNil(t, ch.LastSendAt)
	require.NotNil(t, ch.LastError)
	require.Contains(t, *ch.LastError, "bogus")
}

// TestNtfyTopicValidated covers the path-injection guard: a topic
// carrying a path or query delimiter is a config error, not a request.
func TestNtfyTopicValidated(t *testing.T) {
	db := newTestDB(t)
	stub := newRecordingStub(t, http.StatusOK, "{}")

	id := insertChannel(t, db, "ntfy", "phone", true, map[string]any{
		"server_url": stub.srv.URL,
		"topic":      "a/b?admin=1",
	}, []string{"*"}, "")
	notifier := NewNotifier(db, notifyTestKey, notifyClient(t, stub.srv.URL))

	_, err := notifier.Send(t.Context(), getChannel(t, db, id), testEvent())
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid ntfy topic")
	require.Empty(t, stub.all())
}

// TestDisabledChannelIsSkipped covers a channel disabled between enqueue
// and claim: the handler declines it without a request.
func TestDisabledChannelIsSkipped(t *testing.T) {
	db := newTestDB(t)
	stub := newRecordingStub(t, http.StatusOK, "{}")

	id := insertChannel(t, db, "webhook", "hook", false,
		map[string]any{"url": stub.srv.URL}, []string{"*"}, "")
	notifier := NewNotifier(db, notifyTestKey, notifyClient(t, stub.srv.URL))

	require.NoError(t, notifier.Fanout(t.Context(), testEvent()))
	require.Empty(t, fanoutChannels(t, db))

	// A row that predates the disable is still declined by the handler.
	payload, err := json.Marshal(webhookPayload{ChannelID: id, Event: testEvent()})
	require.NoError(t, err)
	job := store.Job{ID: "job_test", Kind: JobKindWebhook, PayloadJSON: string(payload)}
	require.NoError(t, notifier.Handle(t.Context(), job))
	require.Empty(t, stub.all())
}

// TestSendToBlockedTargetIsInBand covers the send-time SSRF re-check: a
// blocked target surfaces as reply.Error and last_error, and Send also
// returns the wrapped ErrSSRFBlocked for the API's 403 mapping.
func TestSendToBlockedTargetIsInBand(t *testing.T) {
	db := newTestDB(t)

	// 169.254.169.254 stays denied under every allow-private switch.
	id := insertChannel(t, db, "webhook", "hook", true, map[string]any{
		"url": "http://169.254.169.254/latest/meta-data",
	}, []string{"*"}, "")
	notifier := NewNotifier(
		db, notifyTestKey,
		secure.NewClient(secure.NewGuard(slog.New(slog.NewTextHandler(io.Discard, nil)), true)),
	)

	reply, err := notifier.Send(t.Context(), getChannel(t, db, id), testEvent())
	require.Error(t, err)
	require.ErrorIs(t, err, secure.ErrSSRFBlocked)
	require.False(t, reply.OK)
	require.Nil(t, reply.Response)
	require.NotNil(t, reply.Error)
	ch := getChannel(t, db, id)
	require.NotNil(t, ch.LastSendAt)
	require.NotNil(t, ch.LastError)
}
