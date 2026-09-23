package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/jobs"
	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

// notifyAPITestKey stands in for cfg.SecretKey: the at-rest key channel
// secrets seal under.
const notifyAPITestKey = secure.Secret("notify-api-test-key-0001")

// notifyTestEnv is one humatest server against a real migrated store with
// a seeded bearer token, the channel-secret key configured and the
// resolver permissive — the fixture hostnames (hook.example.com and
// friends) resolve nowhere, so every host answers one public address,
// while literal-IP configs still face the guard's tables.
type notifyTestEnv struct {
	api    humatest.TestAPI
	db     *sqlx.DB
	server *Server
	bearer string
}

// newNotifyTestEnv builds the env; client is the SSRF-guarded outbound
// client the test send uses — nil leaves the server-built default.
func newNotifyTestEnv(t *testing.T, client *http.Client) *notifyTestEnv {
	t.Helper()

	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	if err := os.Mkdir(dataRoot, 0o755); err != nil {
		t.Fatalf("make data root: %v", err)
	}
	configDir := filepath.Join(root, "config")
	db, err := store.Open(
		t.Context(),
		filepath.Join(configDir, "dl-tool.db"),
		filepath.Join(root, "backups"),
	)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	server, err := NewServer(
		&config.Config{
			ConfigDir:  configDir,
			SessionTTL: time.Hour,
			DataRoots:  []string{dataRoot},
			SecretKey:  notifyAPITestKey,
		},
		db,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Deps{HTTP: client},
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// Registered after the store's cleanup, so it runs first: the
	// background loops stop before the database they poll closes.
	t.Cleanup(server.Shutdown)
	server.notifications.resolver = permissiveResolver{}

	user := seedUser(t, db)

	return &notifyTestEnv{
		api:    humatest.Wrap(t, server.API),
		db:     db,
		server: server,
		bearer: seedLiveAPIToken(t, db, user.ID),
	}
}

func (e *notifyTestEnv) getChannels(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Get("/notifications", "Authorization: Bearer "+e.bearer)
}

func (e *notifyTestEnv) createChannel(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Post("/notifications", body, "Authorization: Bearer "+e.bearer)
}

func (e *notifyTestEnv) patchChannel(t *testing.T, id string, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Patch("/notifications/"+url.PathEscape(id), body, "Authorization: Bearer "+e.bearer)
}

func (e *notifyTestEnv) deleteChannel(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Delete("/notifications/"+url.PathEscape(id), "Authorization: Bearer "+e.bearer)
}

func (e *notifyTestEnv) testChannel(t *testing.T, id string, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Post("/notifications/"+url.PathEscape(id)+"/test", body, "Authorization: Bearer "+e.bearer)
}

// decodeChannelBody decodes the flat channel object POST and PATCH
// return.
func decodeChannelBody(t *testing.T, recorder *httptest.ResponseRecorder) ChannelView {
	t.Helper()

	var body ChannelView
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode channel body %q: %v", recorder.Body.String(), err)
	}

	return body
}

// decodeChannelList decodes the GET /notifications envelope.
func decodeChannelList(t *testing.T, recorder *httptest.ResponseRecorder) []ChannelView {
	t.Helper()

	var body struct {
		Channels []ChannelView `json:"channels"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode channel list %q: %v", recorder.Body.String(), err)
	}

	return body.Channels
}

// seedChannel stores one notification_channels row through the store
// write path, sealing the secret exactly as an API create does, so a
// config the API's own validation could never accept — the loopback stub
// address — still reaches the delivery side. It returns the ntf_ id.
func (e *notifyTestEnv) seedChannel(
	t *testing.T,
	kind, name string,
	enabled int,
	config map[string]any,
	mask []string,
	secret string,
) string {
	t.Helper()

	enc, err := store.SealNotificationSecret(notifyAPITestKey, secret)
	if err != nil {
		t.Fatalf("seal channel secret: %v", err)
	}
	id := store.NewID(store.PrefixNotificationChannel)
	err = store.NewSettingsStore(e.db).CreateNotificationChannel(t.Context(), store.NotificationChannel{
		ID:         id,
		Kind:       kind,
		Name:       name,
		Enabled:    enabled,
		ConfigJSON: mustJSON(t, config),
		SecretEnc:  enc,
		EventMask:  mustJSON(t, mask),
	})
	if err != nil {
		t.Fatalf("seed channel %q: %v", name, err)
	}

	return id
}

// channelSecret opens the stored ciphertext of one channel row.
func (e *notifyTestEnv) channelSecret(t *testing.T, id string) secure.Secret {
	t.Helper()

	channel, err := store.NewSettingsStore(e.db).GetNotificationChannel(t.Context(), id)
	if err != nil {
		t.Fatalf("read channel %s: %v", id, err)
	}
	secret, err := store.OpenNotificationSecret(notifyAPITestKey, channel.SecretEnc)
	if err != nil {
		t.Fatalf("open channel %s secret: %v", id, err)
	}

	return secret
}

// webhookJobPayload decodes one enqueued webhook job's payload_json.
type webhookJobPayload struct {
	ChannelID string `json:"channel_id"`
	Event     struct {
		Code string `json:"code"`
	} `json:"event"`
}

// webhookJobs returns every queued webhook job's payload, oldest first.
func (e *notifyTestEnv) webhookJobs(t *testing.T) []webhookJobPayload {
	t.Helper()

	var rows []string
	if err := e.db.SelectContext(
		t.Context(), &rows,
		`SELECT payload_json FROM jobs WHERE kind = ? ORDER BY created_at, id`, jobs.JobKindWebhook,
	); err != nil {
		t.Fatalf("list webhook jobs: %v", err)
	}

	payloads := make([]webhookJobPayload, 0, len(rows))
	for _, row := range rows {
		var payload webhookJobPayload
		if err := json.Unmarshal([]byte(row), &payload); err != nil {
			t.Fatalf("decode webhook payload %q: %v", row, err)
		}
		payloads = append(payloads, payload)
	}

	return payloads
}

// replyStub answers every request with one fixed status and body and
// keeps what it received; the handler runs on the server's goroutine, so
// access goes through the mutex.
type replyStub struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []recordedCall
}

type recordedCall struct {
	Method string
	Header http.Header
	Body   []byte
}

func newReplyStub(t *testing.T, status int, respBody string) *replyStub {
	t.Helper()
	stub := &replyStub{}
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		stub.mu.Lock()
		stub.requests = append(stub.requests, recordedCall{
			Method: r.Method,
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

func (s *replyStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.requests)
}

// stubbedClient builds the SSRF-guarded client the notifier gets in
// production, lifted for the loopback stub the way the jobs tests do it:
// allow-private plus ForOrigin so the stub's own port is permitted.
func stubbedClient(t *testing.T, rawURL string) *http.Client {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse stub url %q: %v", rawURL, err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	return secure.NewClient(secure.NewGuard(log, true).ForOrigin(u))
}

// TestChannelCrudAllKinds pins doc 05 section 14: one channel of each of
// the four kinds is created, listed, patched and deleted; event_mask
// defaults to ["*"] and the empty list encodes [], never null.
func TestChannelCrudAllKinds(t *testing.T) {
	env := newNotifyTestEnv(t, nil)

	response := env.getChannels(t)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"channels":[]`) {
		t.Errorf("empty channel list = %s, want \"channels\":[]", response.Body.String())
	}

	kinds := []struct {
		kind   string
		config map[string]any
	}{
		{"webhook", map[string]any{"url": "https://hook.example.com/in", "method": "POST"}},
		{"ntfy", map[string]any{"server_url": "https://ntfy.example.com", "topic": "dl-tool"}},
		{"gotify", map[string]any{"server_url": "https://gotify.example.com", "priority": 5}},
		{"apprise", map[string]any{"base_url": "https://apprise.example.com", "config_key": "dl"}},
	}
	ids := map[string]string{}
	for _, tc := range kinds {
		response = env.createChannel(t, map[string]any{
			"kind":   tc.kind,
			"name":   tc.kind + "-chan",
			"config": tc.config,
		})
		if response.Code != http.StatusCreated {
			t.Fatalf("%s create status = %d, want %d; body %s", tc.kind, response.Code, http.StatusCreated, response.Body.String())
		}
		created := decodeChannelBody(t, response)
		if !strings.HasPrefix(created.ID, "ntf_") {
			t.Errorf("%s id = %q, want the ntf_ prefix", tc.kind, created.ID)
		}
		if created.Kind != tc.kind || created.Name != tc.kind+"-chan" || !created.Enabled {
			t.Errorf("%s created = %+v, want kind %q name %q enabled", tc.kind, created, tc.kind, tc.kind+"-chan")
		}
		if created.SecretSet {
			t.Errorf("%s secret_set = true, want false — no secret was sent", tc.kind)
		}
		if len(created.EventMask) != 1 || created.EventMask[0] != "*" {
			t.Errorf("%s event_mask = %v, want the [\"*\"] default", tc.kind, created.EventMask)
		}
		if len(created.Config) != len(tc.config) {
			t.Errorf("%s config = %v, want %v echoed", tc.kind, created.Config, tc.config)
		}
		if since := time.Since(created.CreatedAt); since < 0 || since > time.Minute {
			t.Errorf("%s created_at = %v, want the stored timestamp — never epoch 0", tc.kind, created.CreatedAt)
		}
		ids[tc.kind] = created.ID
	}

	channels := decodeChannelList(t, env.getChannels(t))
	if len(channels) != 4 {
		t.Fatalf("channels = %+v, want all four", channels)
	}
	for i, want := range []string{"apprise-chan", "gotify-chan", "ntfy-chan", "webhook-chan"} {
		if channels[i].Name != want {
			t.Fatalf("channels[%d].Name = %q, want %q — the list is name-sorted", i, channels[i].Name, want)
		}
	}

	// An explicit empty name is 422, not a silent no-op.
	response = env.patchChannel(t, ids["ntfy"], map[string]any{"name": ""})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	// The patch merges: a rename, a disable and a mask change land while
	// kind and config survive untouched.
	response = env.patchChannel(t, ids["ntfy"], map[string]any{
		"name":       "phone",
		"enabled":    false,
		"event_mask": []string{"task.error"},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("patch status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	patched := decodeChannelBody(t, response)
	if patched.Name != "phone" || patched.Enabled || patched.Kind != "ntfy" {
		t.Errorf("patched = %+v, want {phone ntfy disabled}", patched)
	}
	if len(patched.EventMask) != 1 || patched.EventMask[0] != "task.error" {
		t.Errorf("patched event_mask = %v, want [task.error]", patched.EventMask)
	}
	if patched.Config["server_url"] != "https://ntfy.example.com" {
		t.Errorf("patched config = %v, want the stored config untouched", patched.Config)
	}

	for kind, id := range ids {
		if response = env.deleteChannel(t, id); response.Code != http.StatusNoContent {
			t.Fatalf("%s delete status = %d, want %d; body %s", kind, response.Code, http.StatusNoContent, response.Body.String())
		}
	}

	response = env.getChannels(t)
	if !strings.Contains(response.Body.String(), `"channels":[]`) {
		t.Errorf("emptied channel list = %s, want \"channels\":[]", response.Body.String())
	}
}

// TestSecretNeverReturned pins the write-only rule of doc 05 section 14:
// the stored secret appears in no response body — create, list, patch —
// while secret_set reports its presence.
func TestSecretNeverReturned(t *testing.T) {
	env := newNotifyTestEnv(t, nil)

	const secret = "tok-abc123-live-secret"
	response := env.createChannel(t, map[string]any{
		"kind":   "ntfy",
		"name":   "phone",
		"config": map[string]any{"server_url": "https://ntfy.example.com", "topic": "dl-tool"},
		"secret": secret,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) {
		t.Fatalf("create response leaks the secret: %s", response.Body.String())
	}
	created := decodeChannelBody(t, response)
	if !created.SecretSet {
		t.Errorf("secret_set = false, want true after a write")
	}
	var raw map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw body: %v", err)
	}
	if _, present := raw["secret"]; present {
		t.Errorf("response carries a secret member: %s", response.Body.String())
	}

	if got := env.channelSecret(t, created.ID); got.Reveal() != secret {
		t.Fatalf("stored secret = %q, want %q — the write must land", got.Reveal(), secret)
	}

	for _, response := range []*httptest.ResponseRecorder{
		env.getChannels(t),
		env.patchChannel(t, created.ID, map[string]any{"name": "phone-2"}),
	} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("response leaks the secret: %s", response.Body.String())
		}
	}
}

// TestRedactedLeavesSecret pins the "__redacted__" write-back rule: a
// patch echoing the rendered form is a no-op on the stored secret.
func TestRedactedLeavesSecret(t *testing.T) {
	env := newNotifyTestEnv(t, nil)

	response := env.createChannel(t, map[string]any{
		"kind":   "gotify",
		"name":   "ops",
		"config": map[string]any{"server_url": "https://gotify.example.com"},
		"secret": "live-token-1",
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	id := decodeChannelBody(t, response).ID

	response = env.patchChannel(t, id, map[string]any{"secret": redactedValue})
	if response.Code != http.StatusOK {
		t.Fatalf("patch status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if !decodeChannelBody(t, response).SecretSet {
		t.Errorf("secret_set = false after a redacted patch, want true")
	}
	if got := env.channelSecret(t, id); got.Reveal() != "live-token-1" {
		t.Errorf("stored secret = %q, want %q — a redacted echo must not overwrite it", got.Reveal(), "live-token-1")
	}
}

// TestNullClearsSecret pins the third secret state: null clears the
// stored secret while an omitted member leaves it.
func TestNullClearsSecret(t *testing.T) {
	env := newNotifyTestEnv(t, nil)

	response := env.createChannel(t, map[string]any{
		"kind":   "webhook",
		"name":   "hooks",
		"config": map[string]any{"url": "https://hook.example.com/in"},
		"secret": "bearer-token-2",
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	id := decodeChannelBody(t, response).ID

	// An omitted member leaves the secret: only the name changes.
	response = env.patchChannel(t, id, map[string]any{"name": "hooks-2"})
	if response.Code != http.StatusOK || !decodeChannelBody(t, response).SecretSet {
		t.Fatalf("patch without secret = %d secret_set %v, want 200 true", response.Code, decodeChannelBody(t, response).SecretSet)
	}
	if got := env.channelSecret(t, id); got.Reveal() != "bearer-token-2" {
		t.Fatalf("stored secret = %q, want it untouched", got.Reveal())
	}

	response = env.patchChannel(t, id, map[string]any{"secret": nil})
	if response.Code != http.StatusOK {
		t.Fatalf("null patch status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if decodeChannelBody(t, response).SecretSet {
		t.Errorf("secret_set = true after a null patch, want false")
	}
	if got := env.channelSecret(t, id); got.Reveal() != "" {
		t.Errorf("stored secret = %q, want cleared", got.Reveal())
	}
}

// TestEventMaskFilters pins doc 04 section 4.8: a mask holding only
// task.completed receives a completed event and not an error one — the
// API-stored mask drives the delivery fan-out itself.
func TestEventMaskFilters(t *testing.T) {
	env := newNotifyTestEnv(t, nil)

	response := env.createChannel(t, map[string]any{
		"kind":       "webhook",
		"name":       "hooks",
		"config":     map[string]any{"url": "https://hook.example.com/in"},
		"event_mask": []string{"task.completed"},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	id := decodeChannelBody(t, response).ID

	notifier := env.server.notifications.notifier
	if err := notifier.Fanout(t.Context(), jobs.Event{Code: store.CodeTaskCompleted}); err != nil {
		t.Fatalf("fanout task.completed: %v", err)
	}
	queued := env.webhookJobs(t)
	if len(queued) != 1 || queued[0].ChannelID != id || queued[0].Event.Code != store.CodeTaskCompleted {
		t.Fatalf("webhook jobs = %+v, want exactly the channel's completed delivery", queued)
	}

	if err := notifier.Fanout(t.Context(), jobs.Event{Code: "task.error"}); err != nil {
		t.Fatalf("fanout task.error: %v", err)
	}
	if queued = env.webhookJobs(t); len(queued) != 1 {
		t.Fatalf("webhook jobs = %+v, want the completed delivery alone — task.error is masked out", queued)
	}
}

// TestKindImmutable pins doc 05 section 14: a PATCH changing kind is 422;
// a PATCH repeating the stored kind is a no-op, not a rejection.
func TestKindImmutable(t *testing.T) {
	env := newNotifyTestEnv(t, nil)

	response := env.createChannel(t, map[string]any{
		"kind":   "webhook",
		"name":   "hooks",
		"config": map[string]any{"url": "https://hook.example.com/in"},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	id := decodeChannelBody(t, response).ID

	response = env.patchChannel(t, id, map[string]any{"kind": "ntfy"})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	response = env.patchChannel(t, id, map[string]any{"kind": "webhook"})
	if response.Code != http.StatusOK {
		t.Fatalf("same-kind patch status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if got := decodeChannelBody(t, response); got.Kind != "webhook" {
		t.Errorf("kind = %q, want webhook", got.Kind)
	}
}

// TestDuplicateNameConflict pins doc 05 section 14: a name already taken
// conflicts on create and on a rename onto it — never a silent merge.
func TestDuplicateNameConflict(t *testing.T) {
	env := newNotifyTestEnv(t, nil)

	create := map[string]any{"kind": "gotify", "name": "ops", "config": map[string]any{"server_url": "https://gotify.example.com"}}
	if response := env.createChannel(t, create); response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	response := env.createChannel(t, map[string]any{
		"kind": "gotify", "name": "ops-2", "config": map[string]any{"server_url": "https://gotify.example.com"},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("second create status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	second := decodeChannelBody(t, response).ID

	response = env.createChannel(t, create)
	assertProblem(t, response, http.StatusConflict, SlugConflict)

	response = env.patchChannel(t, second, map[string]any{"name": "ops"})
	assertProblem(t, response, http.StatusConflict, SlugConflict)

	if channels := decodeChannelList(t, env.getChannels(t)); len(channels) != 2 {
		t.Errorf("channels = %+v, want both rows intact", channels)
	}
}

// TestTestReturnsRawUpstreamReply pins doc 05 section 14.1: the test send
// answers 200 with the upstream's status line and body verbatim, ok
// reflecting the upstream status and error null — a failing channel is
// diagnosable without reading the server log.
func TestTestReturnsRawUpstreamReply(t *testing.T) {
	const upstreamBody = `{"code":40301,"http":403,"error":"forbidden"}`
	stub := newReplyStub(t, http.StatusForbidden, upstreamBody)
	env := newNotifyTestEnv(t, stubbedClient(t, stub.srv.URL))

	id := env.seedChannel(
		t, "webhook", "hook", 1,
		map[string]any{"url": stub.srv.URL}, []string{"*"}, "stub-secret-9",
	)

	response := env.testChannel(t, id, map[string]any{})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "stub-secret-9") {
		t.Fatalf("test reply leaks the channel secret: %s", response.Body.String())
	}

	var reply jobs.RawReply
	if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil {
		t.Fatalf("decode reply %q: %v", response.Body.String(), err)
	}
	if reply.OK {
		t.Errorf("ok = true, want false for a 403 upstream")
	}
	if reply.Error != nil {
		t.Errorf("error = %q, want null — the channel answered", *reply.Error)
	}
	if reply.Response == nil {
		t.Fatalf("response = nil, want the upstream reply verbatim")
	}
	if reply.Response.StatusLine != "HTTP/1.1 403 Forbidden" {
		t.Errorf("status_line = %q, want the upstream's verbatim", reply.Response.StatusLine)
	}
	if reply.Response.Status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", reply.Response.Status)
	}
	if reply.Response.Body != upstreamBody {
		t.Errorf("body = %q, want the upstream body verbatim %q", reply.Response.Body, upstreamBody)
	}
	if reply.Request.Method != http.MethodPost || reply.Request.URL != stub.srv.URL {
		t.Errorf("request = %+v, want POST %s", reply.Request, stub.srv.URL)
	}
	if stub.count() != 1 {
		t.Errorf("stub saw %d requests, want the single test send", stub.count())
	}

	// A disabled channel is still testable (doc 05 §14.1): the test send
	// is a diagnostic, not a fan-out.
	disabled := env.seedChannel(
		t, "webhook", "off", 0,
		map[string]any{"url": stub.srv.URL}, []string{"*"}, "",
	)
	response = env.testChannel(t, disabled, map[string]any{})
	if response.Code != http.StatusOK {
		t.Fatalf("disabled channel test status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	// No test send writes a task_events row (doc 05 §14.1).
	var events int
	if err := env.db.GetContext(
		t.Context(), &events, `SELECT COUNT(*) FROM task_events`,
	); err != nil {
		t.Fatalf("count task_events: %v", err)
	}
	if events != 0 {
		t.Errorf("task_events rows = %d, want 0 — a test send records no event", events)
	}
}

// TestUnknownConfigKeyRejected pins doc 05 section 14: a config key
// outside the kind's fixed set is 422 and errors[].location names it.
func TestUnknownConfigKeyRejected(t *testing.T) {
	env := newNotifyTestEnv(t, nil)

	response := env.createChannel(t, map[string]any{
		"kind":   "ntfy",
		"name":   "phone",
		"config": map[string]any{"server_url": "https://ntfy.example.com", "topic": "dl-tool", "bogus": 1},
	})
	problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	if len(problem.Errors) != 1 || problem.Errors[0].Location != "body.config.bogus" {
		t.Errorf("errors = %+v, want one entry locating body.config.bogus", problem.Errors)
	}

	if channels := decodeChannelList(t, env.getChannels(t)); len(channels) != 0 {
		t.Errorf("channels = %+v, want the rejected write to store nothing", channels)
	}
}

// TestConfigURLBlocked pins the save-time half of doc 05 section 14's
// SSRF rule: a config URL resolving to a blocked address is 403
// /problems/ssrf-blocked — link-local stays denied under every
// allow-private switch, and RFC 1918 under the default policy.
func TestConfigURLBlocked(t *testing.T) {
	env := newNotifyTestEnv(t, nil)

	for _, target := range []string{"http://169.254.169.254/latest", "http://10.0.0.4/hook", "http://example.com:8080/hook"} {
		response := env.createChannel(t, map[string]any{
			"kind":   "webhook",
			"name":   "blocked-" + target,
			"config": map[string]any{"url": target},
		})
		assertProblem(t, response, http.StatusForbidden, SlugSSRFBlocked)
	}

	// Every URL reaches the preflight whatever the member's shape: the
	// apprise urls dict and array forms are checked member by member.
	response := env.createChannel(t, map[string]any{
		"kind":   "apprise",
		"name":   "dict-blocked",
		"config": map[string]any{"base_url": "https://apprise.example.com", "config_key": "dl", "urls": map[string]any{"alerts": "http://169.254.169.254/x"}},
	})
	assertProblem(t, response, http.StatusForbidden, SlugSSRFBlocked)

	response = env.createChannel(t, map[string]any{
		"kind":   "apprise",
		"name":   "array-blocked",
		"config": map[string]any{"base_url": "https://apprise.example.com", "config_key": "dl", "urls": []any{"https://apprise.example.com/ok", "http://10.0.0.4/x"}},
	})
	assertProblem(t, response, http.StatusForbidden, SlugSSRFBlocked)

	// A URL member in a shape none of the three forms take is 422, never
	// a silent pass around the guard.
	for _, urls := range []any{42, map[string]any{"k": 42}} {
		response = env.createChannel(t, map[string]any{
			"kind":   "apprise",
			"name":   fmt.Sprintf("bad-shape-%v", urls),
			"config": map[string]any{"base_url": "https://apprise.example.com", "config_key": "dl", "urls": urls},
		})
		assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	}

	if channels := decodeChannelList(t, env.getChannels(t)); len(channels) != 0 {
		t.Errorf("channels = %+v, want every blocked write refused", channels)
	}
}

// TestChannelNotFound pins the 404s of doc 05 section 14: patch, delete
// and test of an id no row carries.
func TestChannelNotFound(t *testing.T) {
	env := newNotifyTestEnv(t, nil)

	response := env.patchChannel(t, "ntf_ghost", map[string]any{"name": "spectre"})
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)

	response = env.deleteChannel(t, "ntf_ghost")
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)

	response = env.testChannel(t, "ntf_ghost", map[string]any{})
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
}
