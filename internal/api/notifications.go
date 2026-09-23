package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/jobs"
	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	operationListChannels  = "list-notification-channels"
	operationCreateChannel = "create-notification-channel"
	operationPatchChannel  = "patch-notification-channel"
	operationDeleteChannel = "delete-notification-channel"
	operationTestChannel   = "test-notification-channel"

	channelKindDetail          = "kind must be one of webhook, ntfy, gotify, apprise"
	channelNameDetail          = "name must be non-empty"
	channelNameTakenDetail     = "a notification channel with that name already exists"
	channelKindImmutableDetail = "kind is immutable after creation"
	channelSecretDetail        = "secret must be a JSON string or null"
	channelBlockedDetail       = "a config URL resolved to a blocked address"

	// channelTestName and channelTestMessage are the synthetic event's
	// payload of doc 05 section 14.1: unmistakably a test, rendered through
	// the same templates a real task event takes.
	channelTestName    = "dl-tool channel test"
	channelTestMessage = "this is a test notification"
)

// ChannelView is the only shape returned for an existing channel. There is
// no secret member: secret_set reports whether one is stored
// (docs/05-api-contract.md section 14).
type ChannelView struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind" enum:"webhook,ntfy,gotify,apprise"`
	Name       string         `json:"name"`
	Enabled    bool           `json:"enabled"`
	Config     map[string]any `json:"config"`
	SecretSet  bool           `json:"secret_set"`
	EventMask  []string       `json:"event_mask"`
	LastSendAt *time.Time     `json:"last_send_at" format:"date-time"`
	LastError  *string        `json:"last_error"`
	CreatedAt  time.Time      `json:"created_at"   format:"date-time"`
	UpdatedAt  time.Time      `json:"updated_at"   format:"date-time"`
}

// ChannelWriteBody is the create and patch body. Secret is write-only.
// It is json.RawMessage rather than *string because the member has three
// wire states — absent leaves the stored secret, null clears it and a
// string replaces it — and *string collapses null and absent into the
// same nil. "__redacted__" is a rendered form and leaves the secret too.
type ChannelWriteBody struct {
	Kind      string          `json:"kind,omitempty"       enum:"webhook,ntfy,gotify,apprise" doc:"Immutable after creation"`
	Name      *string         `json:"name,omitempty"`
	Enabled   *bool           `json:"enabled,omitempty"`
	Config    map[string]any  `json:"config,omitempty"     doc:"Non-secret configuration; the key set is fixed per kind"`
	Secret    json.RawMessage `json:"secret,omitempty"     doc:"Write-only; __redacted__ leaves the stored secret, null clears it"`
	EventMask []string        `json:"event_mask,omitempty" doc:"task_events.code values, or [\"*\"] for every code; default [\"*\"]"`
}

// CreateChannelInput is the JSON body of POST /notifications.
type CreateChannelInput struct {
	Body ChannelWriteBody
}

// PatchChannelInput addresses one channel; every body field is optional
// and an omitted field stays untouched.
type PatchChannelInput struct {
	ID   string `path:"id" doc:"The ntf_… channel id"`
	Body ChannelWriteBody
}

// DeleteChannelInput addresses one channel for DELETE.
type DeleteChannelInput struct {
	ID string `path:"id" doc:"The ntf_… channel id"`
}

// TestInput selects which event template is rendered; the default code is
// task.completed.
type TestInput struct {
	ID   string `path:"id" doc:"The ntf_… channel id"`
	Body struct {
		Code string `json:"code,omitempty" doc:"task_events.code value of the synthetic event; default task.completed"`
	}
}

// ListChannelsOutput is the GET /notifications body.
type ListChannelsOutput struct {
	Body struct {
		Channels []ChannelView `json:"channels"`
	}
}

// ChannelOutput carries 201 from Create and 200 from Patch.
type ChannelOutput struct {
	Status int `json:"-"`
	Body   ChannelView
}

// TestOutput is jobs.RawReply verbatim: status_line, status, headers and
// the first 8 KiB of body, neither parsed nor reformatted. A channel that
// answered is always 200 with ok reflecting the upstream status; a
// channel that could not be reached is 200 with response null and the
// transport failure in error.
type TestOutput struct{ Body jobs.RawReply }

// channelConfigKeys is the fixed config key set of docs/04-data-model.md
// section 4.8 per kind; an unknown key is a 422, never a silently ignored
// field. The delivery path enforces the same table inside internal/jobs.
var channelConfigKeys = map[string][]string{
	"webhook": {"url", "method", "headers", "body_template"},
	"ntfy":    {"server_url", "topic", "priority", "tags", "click_url"},
	"gotify":  {"server_url", "priority"},
	"apprise": {"base_url", "config_key", "urls", "tag", "type", "format"},
}

// channelURLKeys is the URL-carrying member of each kind's config. Doc 05
// section 14 subjects every URL in config to the SSRF policy at save time
// — click_url and urls included, even though dl-tool forwards them to the
// channel service rather than fetching them itself. The send-time
// re-check the same section names is the guarded client's dialer.
var channelURLKeys = map[string][]string{
	"webhook": {"url"},
	"ntfy":    {"server_url", "click_url"},
	"gotify":  {"server_url"},
	"apprise": {"base_url", "urls"},
}

// NotificationHandlers owns the /notifications operations of doc 05
// section 14. notifier is the T077 delivery engine the test operation
// sends through; guard and resolver are the save-time SSRF preflight
// pair, the same process-wide policy the task-submission check uses.
type NotificationHandlers struct {
	settings *store.SettingsStore
	key      secure.Secret
	notifier *jobs.Notifier
	guard    *secure.Guard
	resolver secure.Resolver
}

// NewNotificationHandlers builds the notification handlers over db. key
// is cfg.SecretKey, the at-rest key channel secrets seal under; client is
// the shared SSRF-guarded outbound client the notifier sends through.
func NewNotificationHandlers(
	db *sqlx.DB,
	key secure.Secret,
	client *http.Client,
	guard *secure.Guard,
	resolver secure.Resolver,
) *NotificationHandlers {
	return &NotificationHandlers{
		settings: store.NewSettingsStore(db),
		key:      key,
		notifier: jobs.NewNotifier(db, key, client),
		guard:    guard,
		resolver: resolver,
	}
}

// Register mounts the five operations on the Huma API;
// Server.registerOperations is the call site.
func (h *NotificationHandlers) Register(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID: operationListChannels,
		Method:      http.MethodGet,
		Path:        "/notifications",
		Summary:     "List the notification channels",
		Description: "Every channel sorted by name. No response member carries the stored secret: secret_set reports whether one is stored.",
		Tags:        []string{"notifications"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.List)

	huma.Register(hapi, huma.Operation{
		OperationID:   operationCreateChannel,
		Method:        http.MethodPost,
		Path:          "/notifications",
		DefaultStatus: http.StatusCreated,
		Summary:       "Create a notification channel",
		Description:   "Creates one channel of kind webhook, ntfy, gotify or apprise. kind and name are required; enabled defaults true, event_mask defaults [\"*\"]. The config key set is fixed per kind and an unknown key is 422; every URL in config is checked against the SSRF guard and a blocked target is 403 /problems/ssrf-blocked. A duplicate name is 409 /problems/conflict.",
		Tags:          []string{"notifications"},
		Security:      credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Create)

	huma.Register(hapi, huma.Operation{
		OperationID: operationPatchChannel,
		Method:      http.MethodPatch,
		Path:        "/notifications/{id}",
		Summary:     "Update a notification channel",
		Description: "Partial update of name, enabled, config, secret and event_mask; omitted fields are untouched. kind is immutable: a patch changing it is 422. secret is write-only — a string replaces, \"__redacted__\" leaves it unchanged and null clears it. Renaming onto an existing name is 409 /problems/conflict.",
		Tags:        []string{"notifications"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Patch)

	huma.Register(hapi, huma.Operation{
		OperationID:   operationDeleteChannel,
		Method:        http.MethodDelete,
		Path:          "/notifications/{id}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Delete a notification channel",
		Description:   "Removes the channel row; pending deliveries to it die with it. No task and no file is touched.",
		Tags:          []string{"notifications"},
		Security:      credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Delete)

	huma.Register(hapi, huma.Operation{
		OperationID: operationTestChannel,
		Method:      http.MethodPost,
		Path:        "/notifications/{id}/test",
		Summary:     "Deliver a test event to a channel",
		Description: "Sends one synthetic event — code selects the template, default task.completed — to exactly this channel and returns the raw upstream reply: the status line and the first 8 KiB of body verbatim, so a failing channel is diagnosable without reading the server log. A channel that answered is always 200 with ok reflecting the upstream status; an unreachable channel is 200 with response null and the transport failure in error. A disabled channel is still testable. The send is not recorded against event_mask and writes no task_events row. 403 /problems/ssrf-blocked when the target resolves to a blocked address.",
		Tags:        []string{"notifications"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Test)
}

// List serves GET /notifications: every channel, sorted by name.
func (h *NotificationHandlers) List(ctx context.Context, _ *struct{}) (*ListChannelsOutput, error) {
	rows, err := h.settings.ListNotificationChannels(ctx)
	if err != nil {
		return nil, internalFailure(ctx, "list notification channels", err)
	}

	output := &ListChannelsOutput{}
	output.Body.Channels = make([]ChannelView, 0, len(rows))
	for _, row := range rows {
		view, err := channelView(row)
		if err != nil {
			return nil, internalFailure(ctx, "render notification channel "+row.ID, err)
		}
		output.Body.Channels = append(output.Body.Channels, view)
	}

	return output, nil
}

// Create serves POST /notifications. kind and name are required here
// rather than by the schema: the body type is shared with PATCH, where
// both are optional.
func (h *NotificationHandlers) Create(ctx context.Context, in *CreateChannelInput) (*ChannelOutput, error) {
	if in.Body.Kind == "" {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, channelKindDetail)
	}
	if in.Body.Name == nil || *in.Body.Name == "" {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, channelNameDetail)
	}
	if err := h.validateConfig(ctx, in.Body.Kind, in.Body.Config); err != nil {
		return nil, err
	}

	config := in.Body.Config
	if config == nil {
		config = map[string]any{}
	}
	configJSON, err := encodeChannelJSON(config)
	if err != nil {
		return nil, internalFailure(ctx, "encode notification channel config", err)
	}
	_, enc, err := h.secretPatch(in.Body.Secret)
	if err != nil {
		return nil, h.secretError(ctx, err)
	}
	mask := in.Body.EventMask
	if mask == nil {
		mask = []string{"*"}
	}
	maskJSON, err := encodeChannelJSON(mask)
	if err != nil {
		return nil, internalFailure(ctx, "encode notification channel event_mask", err)
	}

	enabled := 1
	if in.Body.Enabled != nil && !*in.Body.Enabled {
		enabled = 0
	}
	channel := store.NotificationChannel{
		ID:         store.NewID(store.PrefixNotificationChannel),
		Kind:       in.Body.Kind,
		Name:       *in.Body.Name,
		Enabled:    enabled,
		ConfigJSON: configJSON,
		SecretEnc:  enc,
		EventMask:  maskJSON,
	}
	if err := h.settings.CreateNotificationChannel(ctx, channel); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, Problem(SlugConflict, http.StatusConflict, channelNameTakenDetail)
		}

		return nil, internalFailure(ctx, "create notification channel", err)
	}

	created, err := h.settings.GetNotificationChannel(ctx, channel.ID)
	if err != nil {
		return nil, internalFailure(ctx, "read back notification channel", err)
	}
	view, err := channelView(created)
	if err != nil {
		return nil, internalFailure(ctx, "render notification channel "+created.ID, err)
	}

	return &ChannelOutput{Status: http.StatusCreated, Body: view}, nil
}

// Patch serves PATCH /notifications/{id}. kind is immutable: a patch
// carrying a different one is 422 before the store is called. The
// response is the row read back; a read-back ErrNotFound is the row
// vanishing after a committed update, an internal failure rather than a
// second 404.
func (h *NotificationHandlers) Patch(ctx context.Context, in *PatchChannelInput) (*ChannelOutput, error) {
	channel, err := h.settings.GetNotificationChannel(ctx, in.ID)
	if err != nil {
		return nil, FromStore(err)
	}
	if in.Body.Kind != "" && in.Body.Kind != channel.Kind {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, channelKindImmutableDetail)
	}

	patch := store.NotificationChannelPatch{}
	if in.Body.Name != nil {
		if *in.Body.Name == "" {
			return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, channelNameDetail)
		}
		patch.Name = in.Body.Name
	}
	if in.Body.Enabled != nil {
		flag := 0
		if *in.Body.Enabled {
			flag = 1
		}
		patch.Enabled = &flag
	}
	if in.Body.Config != nil {
		if err := h.validateConfig(ctx, channel.Kind, in.Body.Config); err != nil {
			return nil, err
		}
		encoded, err := encodeChannelJSON(in.Body.Config)
		if err != nil {
			return nil, internalFailure(ctx, "encode notification channel config", err)
		}
		patch.ConfigJSON = &encoded
	}
	set, enc, err := h.secretPatch(in.Body.Secret)
	if err != nil {
		return nil, h.secretError(ctx, err)
	}
	patch.SecretSet = set
	patch.SecretEnc = enc
	if in.Body.EventMask != nil {
		encoded, err := encodeChannelJSON(in.Body.EventMask)
		if err != nil {
			return nil, internalFailure(ctx, "encode notification channel event_mask", err)
		}
		patch.EventMask = &encoded
	}

	if err := h.settings.UpdateNotificationChannel(ctx, in.ID, patch); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, Problem(SlugConflict, http.StatusConflict, channelNameTakenDetail)
		}

		return nil, FromStore(err)
	}

	updated, err := h.settings.GetNotificationChannel(ctx, in.ID)
	if err != nil {
		return nil, internalFailure(ctx, "read back notification channel", err)
	}
	view, err := channelView(updated)
	if err != nil {
		return nil, internalFailure(ctx, "render notification channel "+updated.ID, err)
	}

	return &ChannelOutput{Status: http.StatusOK, Body: view}, nil
}

// Delete serves DELETE /notifications/{id}: the row goes and pending
// deliveries to it die with it.
func (h *NotificationHandlers) Delete(ctx context.Context, in *DeleteChannelInput) (*struct{}, error) {
	if err := h.settings.DeleteNotificationChannel(ctx, in.ID); err != nil {
		return nil, FromStore(err)
	}

	return nil, nil
}

// Test serves POST /notifications/{id}/test: one synthetic event to
// exactly this channel through the delivery path, the raw upstream reply
// back verbatim — never re-rendered, re-wrapped or summarised. A disabled
// channel is still testable; the send touches no event_mask bookkeeping
// and writes no task_events row.
func (h *NotificationHandlers) Test(ctx context.Context, in *TestInput) (*TestOutput, error) {
	channel, err := h.settings.GetNotificationChannel(ctx, in.ID)
	if err != nil {
		return nil, FromStore(err)
	}

	code := in.Body.Code
	if code == "" {
		code = store.CodeTaskCompleted
	}
	event := jobs.Event{
		Code:    code,
		Name:    channelTestName,
		Message: channelTestMessage,
		At:      time.Now(),
	}

	reply, err := h.notifier.Send(ctx, channel, event)
	if err != nil {
		if errors.Is(err, secure.ErrSSRFBlocked) {
			return nil, Problem(SlugSSRFBlocked, http.StatusForbidden, channelBlockedDetail)
		}

		return nil, internalFailure(ctx, "test notification channel "+channel.ID, err)
	}

	return &TestOutput{Body: reply}, nil
}

// validateConfig enforces the per-kind config key set of doc 04 section
// 4.8 and the save-time SSRF check of doc 05 section 14: an unknown key
// is a 422 whose errors[].location names it, and a URL resolving to a
// blocked address is a 403.
func (h *NotificationHandlers) validateConfig(ctx context.Context, kind string, config map[string]any) error {
	allowed, known := channelConfigKeys[kind]
	if !known {
		return Problem(SlugValidationFailed, http.StatusUnprocessableEntity, channelKindDetail)
	}

	var details []*huma.ErrorDetail
	for key := range config {
		if !slices.Contains(allowed, key) {
			details = append(details, &huma.ErrorDetail{
				Message:  "unknown " + kind + " config key",
				Location: "body.config." + key,
			})
		}
	}
	if len(details) > 0 {
		slices.SortFunc(details, func(a, b *huma.ErrorDetail) int {
			return cmp.Compare(a.Location, b.Location)
		})

		return &huma.ErrorModel{
			Type:   SlugValidationFailed,
			Title:  http.StatusText(http.StatusUnprocessableEntity),
			Status: http.StatusUnprocessableEntity,
			Detail: "config carries keys outside the " + kind + " set",
			Errors: details,
		}
	}

	for _, key := range channelURLKeys[kind] {
		raw, present := config[key]
		if !present {
			continue
		}
		urls, ok := channelURLValues(raw)
		if !ok {
			return &huma.ErrorModel{
				Type:   SlugValidationFailed,
				Title:  http.StatusText(http.StatusUnprocessableEntity),
				Status: http.StatusUnprocessableEntity,
				Detail: "config." + key + " must be a string, a string array or an object of strings",
				Errors: []*huma.ErrorDetail{{
					Message:  "unsupported shape for a URL config key",
					Location: "body.config." + key,
				}},
			}
		}
		for _, target := range urls {
			if err := secure.PreflightURI(ctx, h.guard, h.resolver, target); err != nil {
				return &huma.ErrorModel{
					Type:   SlugSSRFBlocked,
					Title:  http.StatusText(http.StatusForbidden),
					Status: http.StatusForbidden,
					Detail: channelBlockedDetail,
					Errors: []*huma.ErrorDetail{{
						Message:  "the URL resolved to a blocked address",
						Location: "body.config." + key,
					}},
				}
			}
		}
	}

	return nil
}

// channelURLValues flattens a URL-carrying config member to the strings
// the preflight checks: a bare string, a string array or — the dict form
// apprise's urls member also takes — an object whose every value is a
// string. The bool reports whether the shape was one of those three;
// anything else is a 422 so no URL slips past the check.
func channelURLValues(value any) ([]string, bool) {
	switch typed := value.(type) {
	case nil:
		return nil, true
	case string:
		if typed == "" {
			return nil, true
		}

		return []string{typed}, true
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			if s != "" {
				out = append(out, s)
			}
		}

		return out, true
	case map[string]any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			if s != "" {
				out = append(out, s)
			}
		}

		return out, true
	default:
		return nil, false
	}
}

// secretPatch interprets the write-only member's three wire states. The
// bool reports whether secret_enc is written at all: absent and
// "__redacted__" leave it, null clears it (set with a nil *string), any
// other string seals and replaces it.
func (h *NotificationHandlers) secretPatch(raw json.RawMessage) (bool, *string, error) {
	switch {
	case len(raw) == 0:
		return false, nil, nil
	case string(raw) == "null":
		return true, nil, nil
	}

	var secret string
	if err := json.Unmarshal(raw, &secret); err != nil {
		return false, nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, channelSecretDetail)
	}
	if secret == redactedValue {
		return false, nil, nil
	}

	sealed, err := store.SealNotificationSecret(h.key, secret)
	if err != nil {
		return false, nil, err
	}

	return true, sealed, nil
}

// secretError maps a secretPatch failure: a problem it built is returned
// as-is, a sealing failure is internal.
func (h *NotificationHandlers) secretError(ctx context.Context, err error) error {
	var statusErr huma.StatusError
	if errors.As(err, &statusErr) {
		return err
	}

	return internalFailure(ctx, "seal notification secret", err)
}

// encodeChannelJSON renders one column value.
func encodeChannelJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode channel column: %w", err)
	}

	return string(encoded), nil
}

// channelView renders one store row through the wire shape: no secret
// member exists, secret_set reports whether secret_enc is stored, and the
// unix-millisecond columns become times. A malformed stored column is a
// decode error, never a silently empty member.
func channelView(ch store.NotificationChannel) (ChannelView, error) {
	var config map[string]any
	if err := json.Unmarshal([]byte(ch.ConfigJSON), &config); err != nil {
		return ChannelView{}, fmt.Errorf("decode config_json of %s: %w", ch.ID, err)
	}
	var mask []string
	if err := json.Unmarshal([]byte(ch.EventMask), &mask); err != nil {
		return ChannelView{}, fmt.Errorf("decode event_mask of %s: %w", ch.ID, err)
	}

	view := ChannelView{
		ID:        ch.ID,
		Kind:      ch.Kind,
		Name:      ch.Name,
		Enabled:   ch.Enabled != 0,
		Config:    config,
		SecretSet: ch.SecretEnc != nil && *ch.SecretEnc != "",
		EventMask: mask,
		LastError: ch.LastError,
		CreatedAt: time.UnixMilli(ch.CreatedAt).UTC(),
		UpdatedAt: time.UnixMilli(ch.UpdatedAt).UTC(),
	}
	if ch.LastSendAt != nil {
		at := time.UnixMilli(*ch.LastSendAt).UTC()
		view.LastSendAt = &at
	}
	if view.Config == nil {
		view.Config = map[string]any{}
	}
	if view.EventMask == nil {
		view.EventMask = []string{}
	}

	return view, nil
}
