package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

// JobKindWebhook is the jobs.kind the notifier enqueues: one row per
// enabled channel whose event_mask selects the event
// (docs/04-data-model.md section 4.8).
const JobKindWebhook = "webhook"

// BodyCap is the verbatim-body cap of docs/05-api-contract.md section
// 14.1: the first 8 KiB of the upstream body, truncated, never summarised.
const BodyCap = 8 << 10

// Event is the payload rendered into a channel request. Code is a
// task_events.code value (docs/14-conventions.md section 4).
type Event struct {
	Code    string         `json:"code"`
	TaskID  string         `json:"task_id"`
	Name    string         `json:"name"`
	State   string         `json:"state"`
	Message string         `json:"message"`
	At      time.Time      `json:"at"`
	Detail  map[string]any `json:"detail,omitempty"`
}

// RawRequest is what was sent, as the reply reports it. URL has its
// credentials and query secrets redacted.
type RawRequest struct {
	Method string `json:"method"`
	URL    string `json:"url"`
}

// RawResponse is exactly what the upstream answered, unparsed and
// unreformatted.
type RawResponse struct {
	StatusLine string            `json:"status_line"`
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"` // first 8 KiB verbatim, truncated, never summarised
}

// RawReply is exactly what the upstream answered. It is the body of
// POST /notifications/{id}/test in docs/05-api-contract.md section 14.1.
type RawReply struct {
	OK        bool         `json:"ok"`
	ElapsedMS int64        `json:"elapsed_ms"`
	Request   RawRequest   `json:"request"`
	Response  *RawResponse `json:"response"`
	Error     *string      `json:"error"` // transport failure when Response is nil
}

// Notifier delivers one Event to one notification channel. The plan's
// store.Store does not exist (PLAN-REVIEW-FINDINGS F086), so the
// constructor takes the shared database handle — the T075 precedent for
// the same defect — and wraps its SettingsStore internally.
type Notifier struct {
	db       *sqlx.DB
	settings *store.SettingsStore
	key      secure.Secret
	client   *http.Client
}

// NewNotifier takes the SSRF-guarded client returned by secure.NewClient;
// it never builds its own. key is cfg.SecretKey, the at-rest key
// secret_enc is sealed under.
func NewNotifier(db *sqlx.DB, key secure.Secret, client *http.Client) *Notifier {
	return &Notifier{db: db, settings: store.NewSettingsStore(db), key: key, client: client}
}

// Matches reports whether ch.EventMask selects code. The mask ["*"]
// selects every code; every other element is an exact task_events.code
// match — the mask is a list, not a glob.
func Matches(mask []string, code string) bool {
	return slices.Contains(mask, "*") || slices.Contains(mask, code)
}

// webhookPayload is the durable content of one webhook job: the channel
// to deliver to and the event to render. The jobs row's task_id stays
// NULL deliberately — a task_id would FK-cascade the pending delivery
// away when the chain's auto-remove tail deletes a completed task row —
// so the event carries its task reference inside the payload instead.
type webhookPayload struct {
	ChannelID string `json:"channel_id"`
	Event     Event  `json:"event"`
}

// The fan-out insert is keyed on the payload itself: a re-entered chain
// pass — the extract and move legs each return to completed — cannot
// enqueue a second delivery of the same event to the same channel.
const queryEnqueueWebhook = `INSERT INTO jobs (id, kind, task_id, payload_json, run_after, created_at, updated_at)
SELECT ?, ?, NULL, ?, ?, ?, ?
WHERE NOT EXISTS (SELECT 1 FROM jobs WHERE kind = ? AND payload_json = ?)`

// Fanout enqueues one "webhook" job per enabled channel whose mask
// selects ev.Code.
func (n *Notifier) Fanout(ctx context.Context, ev Event) error {
	channels, err := n.settings.ListNotificationChannels(ctx)
	if err != nil {
		return err
	}

	now := time.Now().UnixMilli()
	for _, ch := range channels {
		if ch.Enabled == 0 {
			continue
		}
		var mask []string
		if err := json.Unmarshal([]byte(ch.EventMask), &mask); err != nil {
			// One malformed row must not suppress every other channel's
			// delivery — isolate the damage to the broken channel.
			slog.WarnContext(
				ctx, "jobs: notification channel has malformed event_mask, skipping",
				"channel_id", ch.ID, "error", err,
			)
			continue
		}
		if !Matches(mask, ev.Code) {
			continue
		}
		payload, err := json.Marshal(webhookPayload{ChannelID: ch.ID, Event: ev})
		if err != nil {
			return fmt.Errorf("jobs: encode webhook payload for channel %s: %w", ch.ID, err)
		}
		if _, err := n.db.ExecContext(
			ctx, queryEnqueueWebhook,
			store.NewID(store.PrefixJob), JobKindWebhook, string(payload), now, now, now,
			JobKindWebhook, string(payload),
		); err != nil {
			return fmt.Errorf("jobs: enqueue webhook for channel %s: %w", ch.ID, err)
		}
	}

	return nil
}

// Handle runs one webhook job end to end. Registered with the T012 worker
// pool as worker.Register(JobKindWebhook, n.Handle). Delivery is
// at-least-once: the job row itself is the (kind, task_id) idempotence
// ADR-0015 names — a done row is never re-claimed — and a replay in the
// crash window between a successful Send and the row's done write
// delivers twice. That is the standard webhook contract and strictly
// better than inferring per-event dedupe from the channel-level
// last_send_at, which would silently drop any queued event older than
// the channel's newest send.
func (n *Notifier) Handle(ctx context.Context, job store.Job) error {
	var payload webhookPayload
	if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil {
		return fmt.Errorf("jobs: decode %q payload: %w", JobKindWebhook, err)
	}
	if payload.ChannelID == "" {
		return fmt.Errorf("jobs: %s job %q carries no channel_id", JobKindWebhook, job.ID)
	}

	ch, err := n.settings.GetNotificationChannel(ctx, payload.ChannelID)
	if errors.Is(err, store.ErrNotFound) {
		return nil // the channel was deleted; the delivery dies with it
	}
	if err != nil {
		return fmt.Errorf("jobs: webhook job %q: %w", job.ID, err)
	}
	if ch.Enabled == 0 {
		return nil // disabled between enqueue and claim
	}

	reply, err := n.Send(ctx, ch, payload.Event)
	if err != nil {
		return fmt.Errorf("jobs: webhook job %q: %w", job.ID, err)
	}
	if !reply.OK {
		// A non-2xx answer or a transport failure is a delivery failure:
		// Send already recorded it on the channel row, and the returned
		// error puts the job on the standard backoff.
		switch {
		case reply.Error != nil:
			return fmt.Errorf("jobs: webhook job %q: %s", job.ID, *reply.Error)
		case reply.Response != nil:
			return fmt.Errorf("jobs: webhook job %q: upstream answered %q", job.ID, reply.Response.StatusLine)
		default:
			return fmt.Errorf("jobs: webhook job %q: delivery failed", job.ID)
		}
	}

	return nil
}

// Send renders ev for the channel's kind, performs the request through
// the SSRF-guarded client — the resolved peer is re-checked inside the
// dialer at send time — and returns the raw reply. A non-2xx upstream is
// not a Go error: it is returned with OK false and the response filled
// in. A blocked target returns the reply with Error set and also the
// wrapped secure.ErrSSRFBlocked so the API layer can map it to 403
// /problems/ssrf-blocked. Every attempt writes the channel's
// last_send_at and last_error.
func (n *Notifier) Send(ctx context.Context, ch store.NotificationChannel, ev Event) (RawReply, error) {
	secret, err := store.OpenNotificationSecret(n.key, ch.SecretEnc)
	if err != nil {
		return RawReply{}, fmt.Errorf("jobs: channel %s: %w", ch.ID, err)
	}
	req, err := renderRequest(ctx, ch, ev, secret)
	if err != nil {
		// A failed render is still a failed attempt: the channel row
		// records it so an operator sees why nothing went out, and the
		// caller receives the same redacted error — a parse error can
		// quote the raw URL, query secrets included, and the returned
		// value flows to the job row's last_error.
		redErr := secure.RedactError(err)
		msg := redErr.Error()
		n.touch(ctx, ch.ID, &msg)

		return RawReply{}, redErr
	}

	var reply RawReply
	reply.Request.Method = req.Method
	reply.Request.URL = secure.RedactURL(req.URL.String())

	start := time.Now()
	resp, err := n.client.Do(req)
	reply.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		// A transport failure — including the dial-time SSRF re-check —
		// is reported in-band: response stays nil and the redacted error
		// text lands in error.
		msg := secure.RedactError(err).Error()
		reply.Error = &msg
		n.touch(ctx, ch.ID, &msg)
		if errors.Is(err, secure.ErrSSRFBlocked) {
			return reply, err
		}

		return reply, nil
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.WarnContext(ctx, "jobs: close notification response body failed", "error", err)
		}
	}()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, BodyCap))
	reply.ElapsedMS = time.Since(start).Milliseconds()
	reply.OK = resp.StatusCode >= 200 && resp.StatusCode <= 299
	reply.Response = &RawResponse{
		StatusLine: resp.Proto + " " + resp.Status,
		Status:     resp.StatusCode,
		Headers:    responseHeaders(resp),
		Body:       string(body),
	}
	if readErr != nil {
		msg := fmt.Sprintf("read response body: %v", readErr)
		reply.Error = &msg
	}

	// last_error is the failure text of the last attempt — the upstream's
	// own status line for a non-2xx answer — and NULL after a success.
	var lastErr *string
	switch {
	case reply.Error != nil:
		lastErr = reply.Error
	case !reply.OK:
		lastErr = &reply.Response.StatusLine
	}
	n.touch(ctx, ch.ID, lastErr)

	return reply, nil
}

// touch records the attempt outcome on the channel row; a failed write is
// degraded — the delivery already happened — so it logs rather than
// failing the send.
func (n *Notifier) touch(ctx context.Context, channelID string, lastErr *string) {
	if err := n.settings.TouchNotificationChannel(ctx, channelID, lastErr, time.Now().UnixMilli()); err != nil {
		slog.WarnContext(
			ctx, "jobs: notification channel bookkeeping failed",
			"channel_id", channelID, "error", err,
		)
	}
}

// responseHeaders flattens the upstream's headers verbatim into the reply
// shape: one member per header, multi-values joined per the HTTP field
// syntax.
func responseHeaders(resp *http.Response) map[string]string {
	headers := make(map[string]string, len(resp.Header))
	for name, values := range resp.Header {
		headers[name] = strings.Join(values, ", ")
	}

	return headers
}

// channelConfigKeys is the fixed key set of docs/04-data-model.md
// section 4.8 per kind; an unknown key is a validation error, never a
// silently ignored field.
var channelConfigKeys = map[string][]string{
	"webhook": {"url", "method", "headers", "body_template"},
	"ntfy":    {"server_url", "topic", "priority", "tags", "click_url"},
	"gotify":  {"server_url", "priority"},
	"apprise": {"base_url", "config_key", "urls", "tag", "type", "format"},
}

// channelConfig decodes config_json and rejects any key outside the
// kind's key set.
func channelConfig(ch store.NotificationChannel) (map[string]json.RawMessage, error) {
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal([]byte(ch.ConfigJSON), &cfg); err != nil {
		return nil, fmt.Errorf("jobs: channel %s: decode config_json: %w", ch.ID, err)
	}
	allowed, known := channelConfigKeys[ch.Kind]
	if !known {
		return nil, fmt.Errorf("jobs: channel %s: unknown kind %q", ch.ID, ch.Kind)
	}
	for key := range cfg {
		if !slices.Contains(allowed, key) {
			return nil, fmt.Errorf("jobs: channel %s: unknown %s config key %q", ch.ID, ch.Kind, key)
		}
	}

	return cfg, nil
}

// renderRequest builds the channel's request of the task's per-kind
// table. secret is the decrypted secret_enc — it is set on the request
// and never returned in the reply.
func renderRequest(
	ctx context.Context,
	ch store.NotificationChannel,
	ev Event,
	secret secure.Secret,
) (*http.Request, error) {
	cfg, err := channelConfig(ch)
	if err != nil {
		return nil, err
	}

	switch ch.Kind {
	case "webhook":
		return renderWebhook(ctx, ch, cfg, ev, secret)
	case "ntfy":
		return renderNtfy(ctx, ch, cfg, ev, secret)
	case "gotify":
		return renderGotify(ctx, ch, cfg, ev, secret)
	case "apprise":
		return renderApprise(ctx, ch, cfg, ev, secret)
	default:
		// channelConfig already rejected the kind; unreachable.
		return nil, fmt.Errorf("jobs: channel %s: unknown kind %q", ch.ID, ch.Kind)
	}
}

// configString reads one string member; absent and null both mean unset.
func configString(cfg map[string]json.RawMessage, key string) (string, error) {
	raw, ok := cfg[key]
	if !ok || string(raw) == "null" {
		return "", nil
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("jobs: notification config key %q: want a string: %w", key, err)
	}

	return out, nil
}

// configInt reads one integer member; the bool reports presence so an
// unset priority stays the service's default instead of an explicit 0.
func configInt(cfg map[string]json.RawMessage, key string) (int, bool, error) {
	raw, ok := cfg[key]
	if !ok || string(raw) == "null" {
		return 0, false, nil
	}
	var out int
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, false, fmt.Errorf("jobs: notification config key %q: want an integer: %w", key, err)
	}

	return out, true, nil
}

// configStrings reads one string-array member.
func configStrings(cfg map[string]json.RawMessage, key string) ([]string, error) {
	raw, ok := cfg[key]
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("jobs: notification config key %q: want a string array: %w", key, err)
	}

	return out, nil
}

// configStringMap reads one object member as a string map.
func configStringMap(cfg map[string]json.RawMessage, key string) (map[string]string, error) {
	raw, ok := cfg[key]
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("jobs: notification config key %q: want a string map: %w", key, err)
	}

	return out, nil
}

// requireString reads one mandatory string member.
func requireString(cfg map[string]json.RawMessage, ch store.NotificationChannel, key string) (string, error) {
	value, err := configString(cfg, key)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("jobs: channel %s: missing %s config key %q", ch.ID, ch.Kind, key)
	}

	return value, nil
}

// eventTitle is the short headline the structured kinds carry: the task
// name when the event has one, else the code itself.
func eventTitle(ev Event) string {
	if ev.Name != "" {
		return ev.Name
	}

	return ev.Code
}

// eventLine renders the event as one human-readable line for the
// plain-text bodies: "code: name: message" with absent parts dropped.
func eventLine(ev Event) string {
	parts := []string{ev.Code}
	if ev.Name != "" {
		parts = append(parts, ev.Name)
	}
	if ev.Message != "" {
		parts = append(parts, ev.Message)
	}

	return strings.Join(parts, ": ")
}

// renderWebhook builds "<method> <url>" with the configured headers. The
// body is body_template rendered from the Event, or the Event JSON when
// the template is empty. A stored secret is the bearer token — it
// overrides a same-named plaintext header so the credential never sits in
// config_json.
func renderWebhook(
	ctx context.Context,
	ch store.NotificationChannel,
	cfg map[string]json.RawMessage,
	ev Event,
	secret secure.Secret,
) (*http.Request, error) {
	target, err := requireString(cfg, ch, "url")
	if err != nil {
		return nil, err
	}
	method, err := configString(cfg, "method")
	if err != nil {
		return nil, err
	}
	if method == "" {
		method = http.MethodPost
	}
	headers, err := configStringMap(cfg, "headers")
	if err != nil {
		return nil, err
	}
	bodyTemplate, err := configString(cfg, "body_template")
	if err != nil {
		return nil, err
	}

	var body []byte
	if bodyTemplate == "" {
		body, err = json.Marshal(ev)
		if err != nil {
			return nil, fmt.Errorf("jobs: channel %s: encode event: %w", ch.ID, err)
		}
	} else {
		tmpl, err := template.New("body_template").Option("missingkey=error").Parse(bodyTemplate)
		if err != nil {
			return nil, fmt.Errorf("jobs: channel %s: parse body_template: %w", ch.ID, err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, ev); err != nil {
			return nil, fmt.Errorf("jobs: channel %s: render body_template: %w", ch.ID, err)
		}
		body = buf.Bytes()
	}

	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("jobs: channel %s: %w", ch.ID, err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	if bodyTemplate == "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if secret.Reveal() != "" {
		req.Header.Set("Authorization", "Bearer "+secret.Reveal())
	}

	return req, nil
}

// renderNtfy builds "POST <server_url>/<topic>" with the plain-text event
// line as the body and the Priority, Tags and Click headers; the stored
// access token goes out as Authorization: Bearer.
func renderNtfy(
	ctx context.Context,
	ch store.NotificationChannel,
	cfg map[string]json.RawMessage,
	ev Event,
	secret secure.Secret,
) (*http.Request, error) {
	serverURL, err := requireString(cfg, ch, "server_url")
	if err != nil {
		return nil, err
	}
	topic, err := requireString(cfg, ch, "topic")
	if err != nil {
		return nil, err
	}
	priority, hasPriority, err := configInt(cfg, "priority")
	if err != nil {
		return nil, err
	}
	tags, err := configStrings(cfg, "tags")
	if err != nil {
		return nil, err
	}
	click, err := configString(cfg, "click_url")
	if err != nil {
		return nil, err
	}

	// The topic lands in the request path verbatim; a character that would
	// break the path or start a query is a config error, not a request.
	if strings.ContainsAny(topic, "/?#&=% ") {
		return nil, fmt.Errorf("jobs: channel %s: invalid ntfy topic %q", ch.ID, topic)
	}
	target := strings.TrimRight(serverURL, "/") + "/" + topic
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, target, strings.NewReader(eventLine(ev)),
	)
	if err != nil {
		return nil, fmt.Errorf("jobs: channel %s: %w", ch.ID, err)
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if hasPriority {
		req.Header.Set("Priority", strconv.Itoa(priority))
	}
	if len(tags) > 0 {
		req.Header.Set("Tags", strings.Join(tags, ","))
	}
	if click != "" {
		req.Header.Set("Click", click)
	}
	if secret.Reveal() != "" {
		req.Header.Set("Authorization", "Bearer "+secret.Reveal())
	}

	return req, nil
}

// renderGotify builds "POST <server_url>/message" with the title,
// message and priority form fields; the stored application token goes
// out as the X-Gotify-Key header.
func renderGotify(
	ctx context.Context,
	ch store.NotificationChannel,
	cfg map[string]json.RawMessage,
	ev Event,
	secret secure.Secret,
) (*http.Request, error) {
	serverURL, err := requireString(cfg, ch, "server_url")
	if err != nil {
		return nil, err
	}
	priority, hasPriority, err := configInt(cfg, "priority")
	if err != nil {
		return nil, err
	}

	message := ev.Message
	if message == "" {
		message = eventLine(ev)
	}
	form := url.Values{}
	form.Set("title", eventTitle(ev))
	form.Set("message", message)
	if hasPriority {
		form.Set("priority", strconv.Itoa(priority))
	}

	target := strings.TrimRight(serverURL, "/") + "/message"
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, target, strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("jobs: channel %s: %w", ch.ID, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if secret.Reveal() != "" {
		req.Header.Set("X-Gotify-Key", secret.Reveal())
	}

	return req, nil
}

// renderApprise builds "POST <base_url>/notify/<config_key>" with the
// JSON body carrying urls, tag, type, format, title and body. Apprise
// URLs embed their credentials (discord://id/token and friends), so a
// stored secret is the urls member itself — encrypted at rest because
// plaintext config_json would leak them.
func renderApprise(
	ctx context.Context,
	ch store.NotificationChannel,
	cfg map[string]json.RawMessage,
	ev Event,
	secret secure.Secret,
) (*http.Request, error) {
	baseURL, err := requireString(cfg, ch, "base_url")
	if err != nil {
		return nil, err
	}
	configKey, err := requireString(cfg, ch, "config_key")
	if err != nil {
		return nil, err
	}

	body := map[string]any{
		"title": eventTitle(ev),
		"body":  eventLine(ev),
	}
	if raw, ok := cfg["urls"]; ok && string(raw) != "null" {
		var urls any
		if err := json.Unmarshal(raw, &urls); err != nil {
			return nil, fmt.Errorf("jobs: notification config key %q: want a JSON value: %w", "urls", err)
		}
		body["urls"] = urls
	}
	for _, key := range []string{"tag", "type", "format"} {
		value, err := configString(cfg, key)
		if err != nil {
			return nil, err
		}
		if value != "" {
			body[key] = value
		}
	}
	if secret.Reveal() != "" {
		body["urls"] = secret.Reveal()
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("jobs: channel %s: encode apprise body: %w", ch.ID, err)
	}

	target := strings.TrimRight(baseURL, "/") + "/notify/" + url.PathEscape(configKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("jobs: channel %s: %w", ch.ID, err)
	}
	req.Header.Set("Content-Type", "application/json")

	return req, nil
}
