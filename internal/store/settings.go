package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"

	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/hkdf"

	"github.com/L-K-M/dl-tool/internal/secure"
)

// EngineIDPrefix pairs with the engine kinds to form the stable row ids of
// the engines table: eng_aria2, eng_qbittorrent, eng_ytdlp
// (docs/05-api-contract.md section 11.3).
const EngineIDPrefix = PrefixEngine

// EngineIDs are the fixed row ids the API addresses engines by. The
// migration seeds no engines rows; the composition root creates them from
// the environment when an engine is configured.
const (
	EngineIDAria2       = EngineIDPrefix + "aria2"
	EngineIDQBittorrent = EngineIDPrefix + "qbittorrent"
	EngineIDYTDLP       = EngineIDPrefix + "ytdlp"
)

// ErrConflict is the sentinel a duplicate-name write returns; the API
// maps it to 409 /problems/conflict. isUniqueViolation already detects
// the driver's SQLITE_CONSTRAINT_UNIQUE for it.
var ErrConflict = errors.New("store: conflict")

// Category is one row of the categories table (docs/04-data-model.md
// section 3.2) with the count of its non-removed tasks attached. The id
// stays off the wire: the API addresses a category by its unique name.
type Category struct {
	ID        string `db:"id"         json:"-"`
	Name      string `db:"name"       json:"name"`
	SavePath  string `db:"save_path"  json:"save_path"`
	TaskCount int    `db:"task_count" json:"task_count"`
}

// Tag is one row of the tags table with the count of its non-removed
// tasks attached; the id never leaves the store because the API addresses
// a tag by its unique name.
type Tag struct {
	Name      string `db:"name"       json:"name"`
	TaskCount int    `db:"task_count" json:"task_count"`
}

// Engine is one row of the engines table (docs/04-data-model.md section
// 3.2). secret_enc and username are deliberately absent: this model feeds
// GET /engines, and an engine secret is never returned by any API, so the
// column is not even selected.
type Engine struct {
	ID         string  `db:"id"`
	Kind       string  `db:"kind"`
	Name       string  `db:"name"`
	Enabled    int     `db:"enabled"`
	URL        *string `db:"url"`
	Version    *string `db:"version"`
	LastSeenAt *int64  `db:"last_seen_at"`
	LastError  *string `db:"last_error"`
}

// SettingsStore reaches the configuration tables of docs/04-data-model.md
// section 3.2: the engines table here, and the settings rows with the
// tasks that own them (T092 and later extend this file).
type SettingsStore struct{ db *sqlx.DB }

// NewSettingsStore builds the configuration-table store over db.
func NewSettingsStore(db *sqlx.DB) *SettingsStore {
	return &SettingsStore{db: db}
}

// engineColumns is the explicit column list every engines SELECT shares,
// so secret_enc and username can never ride along on a widening of one
// query and the list and detail reads cannot drift apart.
const engineColumns = `id, kind, name, enabled, url, version, last_seen_at, last_error`

// queryListEngines: kind order is aria2, qbittorrent, ytdlp — the stable
// order the API example uses.
const queryListEngines = `SELECT ` + engineColumns + `
FROM engines ORDER BY kind`

// ListEngines returns every engines row, ordered by kind.
func (s *SettingsStore) ListEngines(ctx context.Context) ([]Engine, error) {
	var engines []Engine
	if err := s.db.SelectContext(ctx, &engines, queryListEngines); err != nil {
		return nil, fmt.Errorf("store: list engines: %w", err)
	}

	return engines, nil
}

// queryEnsureEngine carries the identity columns the environment owns.
// enabled is not in the UPDATE set: a row disabled by hand keeps its flag
// across restarts, and the DDL default of 1 covers the first insert.
const queryEnsureEngine = `INSERT INTO engines (id, kind, name, enabled, url, created_at, updated_at)
VALUES (?, ?, ?, 1, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  name = excluded.name,
  url = excluded.url,
  updated_at = excluded.updated_at`

// EnsureEngine creates or refreshes the identity row of a configured
// engine. The environment is the source of truth for name and url, so the
// row cannot drift when an operator moves an engine; the probe history
// columns (version, last_seen_at, last_error) stay owned by TouchEngine.
func (s *SettingsStore) EnsureEngine(ctx context.Context, id, kind, name, url string, at int64) error {
	if _, err := s.db.ExecContext(ctx, queryEnsureEngine, id, kind, name, url, at, at); err != nil {
		return fmt.Errorf("store: ensure engine %s: %w", id, err)
	}

	return nil
}

// TouchEngine records the outcome of a probe. A success (lastErr nil)
// stamps last_seen_at, clears last_error and stores version whenever the
// probe resolved one; a failure records last_error alone, so the last
// successful contact survives an outage.
func (s *SettingsStore) TouchEngine(ctx context.Context, id string, version, lastErr *string, at int64) error {
	var result sql.Result
	var err error

	if lastErr != nil {
		result, err = s.db.ExecContext(
			ctx,
			`UPDATE engines SET last_error = ?, updated_at = ? WHERE id = ?`,
			*lastErr, at, id,
		)
	} else {
		result, err = s.db.ExecContext(
			ctx,
			`UPDATE engines
SET last_seen_at = ?, last_error = NULL, version = COALESCE(?, version), updated_at = ?
WHERE id = ?`,
			at, version, at, id,
		)
	}
	if err != nil {
		return fmt.Errorf("store: touch engine %s: %w", id, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: touch engine %s: read rows affected: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: touch engine %s: %w", id, ErrNotFound)
	}

	return nil
}

// queryEngineByID reads the same column list as ListEngines over the
// primary key.
const queryEngineByID = `SELECT ` + engineColumns + `
FROM engines WHERE id = ?`

// EngineByID resolves one row by id. ErrNotFound means the id addresses
// no known engine.
func (s *SettingsStore) EngineByID(ctx context.Context, id string) (Engine, error) {
	var e Engine
	err := s.db.GetContext(ctx, &e, queryEngineByID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Engine{}, fmt.Errorf("store: engine %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Engine{}, fmt.Errorf("store: engine %s: %w", id, err)
	}

	return e, nil
}

// queryListCategories carries the task_count of every category in one
// statement: the LEFT JOIN counts only the tasks whose state is not
// 'removed' (removal is a tombstone, not a delete — docs/04-data-model.md
// section 3.3), so a category whose tasks were all removed reports 0.
const queryListCategories = `SELECT c.id, c.name, c.save_path, COUNT(t.id) AS task_count
FROM categories c
LEFT JOIN tasks t ON t.category_id = c.id AND t.state <> 'removed'
GROUP BY c.id
ORDER BY c.name`

// ListCategories returns every categories row, ordered by name, each with
// the count of its non-removed tasks.
func (s *SettingsStore) ListCategories(ctx context.Context) ([]Category, error) {
	var categories []Category
	if err := s.db.SelectContext(ctx, &categories, queryListCategories); err != nil {
		return nil, fmt.Errorf("store: list categories: %w", err)
	}

	return categories, nil
}

// queryCategoryByName is queryListCategories over one unique name.
const queryCategoryByName = `SELECT c.id, c.name, c.save_path, COUNT(t.id) AS task_count
FROM categories c
LEFT JOIN tasks t ON t.category_id = c.id AND t.state <> 'removed'
WHERE c.name = ?
GROUP BY c.id`

// CategoryByName resolves one row by its unique name, carrying the same
// task_count the list does. ErrNotFound means no category carries it.
func (s *SettingsStore) CategoryByName(ctx context.Context, name string) (Category, error) {
	var c Category
	err := s.db.GetContext(ctx, &c, queryCategoryByName, name)
	if errors.Is(err, sql.ErrNoRows) {
		return Category{}, fmt.Errorf("store: category %s: %w", name, ErrNotFound)
	}
	if err != nil {
		return Category{}, fmt.Errorf("store: category %s: %w", name, err)
	}

	return c, nil
}

const queryCreateCategory = `INSERT INTO categories (id, name, save_path, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)`

// CreateCategory inserts one row; a name already taken is ErrConflict.
// The caller owns c.ID (a cat_ ULID) and the already-resolved SavePath.
func (s *SettingsStore) CreateCategory(ctx context.Context, c Category) error {
	now := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(ctx, queryCreateCategory, c.ID, c.Name, c.SavePath, now, now); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: create category %s: %w", c.Name, ErrConflict)
		}

		return fmt.Errorf("store: create category %s: %w", c.Name, err)
	}

	return nil
}

// queryUpdateCategory merges the patch inside the UPDATE itself: a NULL
// argument leaves its column untouched, so two concurrent PATCHes cannot
// lose each other's field. updated_at still moves on every call.
const queryUpdateCategory = `UPDATE categories
SET name = COALESCE(?, name), save_path = COALESCE(?, save_path), updated_at = ?
WHERE name = ?`

// UpdateCategory writes the addressed row's name and save_path; a nil
// argument leaves that column untouched. ErrNotFound means name addresses
// no row; ErrConflict means newName belongs to another row.
func (s *SettingsStore) UpdateCategory(ctx context.Context, name string, newName, savePath *string) error {
	result, err := s.db.ExecContext(
		ctx, queryUpdateCategory, newName, savePath, time.Now().UnixMilli(), name,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: update category %s: %w", name, ErrConflict)
		}

		return fmt.Errorf("store: update category %s: %w", name, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update category %s: read rows affected: %w", name, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: update category %s: %w", name, ErrNotFound)
	}

	return nil
}

const queryDeleteCategory = `DELETE FROM categories WHERE name = ?`

// DeleteCategory removes the row. The tasks.category_id and
// watch_folders.category_id references carry ON DELETE SET NULL
// (docs/04-data-model.md section 3.3), so its tasks and watch folders
// become uncategorised and no task row and no file is touched.
// ErrNotFound means name addresses no row.
func (s *SettingsStore) DeleteCategory(ctx context.Context, name string) error {
	result, err := s.db.ExecContext(ctx, queryDeleteCategory, name)
	if err != nil {
		return fmt.Errorf("store: delete category %s: %w", name, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete category %s: read rows affected: %w", name, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete category %s: %w", name, ErrNotFound)
	}

	return nil
}

// queryListTags counts every non-removed task carrying each tag in one
// statement: the task_tags rows of a removed task survive its tombstone,
// so the join to tasks — not the link alone — decides the count, and a
// tag with no tasks at all still lists with 0.
const queryListTags = `SELECT t.name, COUNT(k.id) AS task_count
FROM tags t
LEFT JOIN task_tags tt ON tt.tag_id = t.id
LEFT JOIN tasks k ON k.id = tt.task_id AND k.state <> 'removed'
GROUP BY t.id
ORDER BY t.name`

// ListTags returns every row of tags sorted by name, including tags with
// no tasks; task_count counts every non-removed task carrying the tag.
func (s *SettingsStore) ListTags(ctx context.Context) ([]Tag, error) {
	var tags []Tag
	if err := s.db.SelectContext(ctx, &tags, queryListTags); err != nil {
		return nil, fmt.Errorf("store: list tags: %w", err)
	}

	return tags, nil
}

// settingDefaultDestination is the settings key of
// docs/11-config-reference.md section 5 the create path falls back to
// when a submission carries neither a destination nor a category.
const settingDefaultDestination = "default_destination"

const queryDefaultDestination = `SELECT value_json FROM settings WHERE key = ?`

// DefaultDestination returns the default_destination settings row's
// value (docs/11-config-reference.md section 5). The migration seeds no
// row: an absent row or an empty value returns "" with a nil error, and
// the caller's first-root fallback applies.
func (s *SettingsStore) DefaultDestination(ctx context.Context) (string, error) {
	var valueJSON string
	err := s.db.GetContext(ctx, &valueJSON, queryDefaultDestination, settingDefaultDestination)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read settings key %s: %w", settingDefaultDestination, err)
	}

	var value string
	if valueJSON == "" {
		return "", nil
	}
	if err := json.Unmarshal([]byte(valueJSON), &value); err != nil {
		return "", fmt.Errorf("store: decode settings key %s: want a JSON string: %w", settingDefaultDestination, err)
	}

	return value, nil
}

// Settings returns the sibling store over the same database, for a
// collaborator that spans both table families — the extract handler's
// password source reads tasks.extract_password and the extract_passwords
// settings row.
func (s *TaskStore) Settings() *SettingsStore { return NewSettingsStore(s.db) }

// queryTaskExtractPassword reads the per-task candidate of the extract
// handler (docs/12-security-and-threat-model.md section 4.2).
const queryTaskExtractPassword = `SELECT extract_password FROM tasks WHERE id = ?`

// ExtractPassword returns the task's stored extraction password, the empty
// Secret when the column is NULL. ErrNotFound means the id addresses no
// task. The value is a secret: it never enters a log line, an error string
// or an API payload.
func (s *TaskStore) ExtractPassword(ctx context.Context, id string) (secure.Secret, error) {
	var password sql.NullString
	err := s.db.GetContext(ctx, &password, queryTaskExtractPassword, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("store: task %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("store: read extract password of task %s: %w", id, err)
	}

	return secure.Secret(password.String), nil
}

// settingExtractPasswords is the settings key of the shared extraction
// password list (docs/11-config-reference.md section 5). The migration
// seeds no row: absent means the empty list.
const settingExtractPasswords = "extract_passwords"

// MaxExtractPasswords is the doc 12 section 4.2 cap on the shared list.
// jobs.MaxCandidates aliases it so the write-side trim and the read-side
// candidate cap cannot drift apart.
const MaxExtractPasswords = 16

const queryExtractPasswords = `SELECT value_json FROM settings WHERE key = ?`

// ExtractPasswords reads the extract_passwords settings key. It returns an
// empty slice when the key is absent. The value is a secret: it is never
// logged and GET /settings renders it "__redacted__". One caveat is already
// documented in doc 12 section 4.2: the extract handler passes each
// candidate to 7zz as -p<password>, visible in /proc/<pid>/cmdline for the
// child's lifetime — acceptable while every container process runs as the
// same unprivileged user.
func (s *SettingsStore) ExtractPasswords(ctx context.Context) ([]string, error) {
	var valueJSON string
	err := s.db.GetContext(ctx, &valueJSON, queryExtractPasswords, settingExtractPasswords)
	if errors.Is(err, sql.ErrNoRows) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read settings key %s: %w", settingExtractPasswords, err)
	}

	var list []string
	if err := json.Unmarshal([]byte(valueJSON), &list); err != nil {
		return nil, fmt.Errorf("store: decode settings key %s: want a JSON array: %w", settingExtractPasswords, err)
	}

	return list, nil
}

const queryUpsertExtractPasswords = `INSERT INTO settings (id, key, value_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(key) DO UPDATE SET
  value_json = excluded.value_json,
  updated_at = excluded.updated_at`

// AppendExtractPassword appends pw to the extract_passwords array in one
// transaction when it is absent, keeping at most maxExtractPasswords
// entries, oldest dropped first. The password is a secret: it is bound as
// a parameter, never interpolated into a query, log line or error.
func (s *SettingsStore) AppendExtractPassword(ctx context.Context, pw string) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: append settings key %s: %w", settingExtractPasswords, err)
	}
	// Rolls back on any early return; after Commit this is sql.ErrTxDone,
	// which is the expected outcome and not worth a warning.
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "store: rollback of extract_passwords append failed", "error", err)
		}
	}()

	var valueJSON string
	err = tx.GetContext(ctx, &valueJSON, queryExtractPasswords, settingExtractPasswords)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: append settings key %s: %w", settingExtractPasswords, err)
	}

	var list []string
	if err == nil {
		if err := json.Unmarshal([]byte(valueJSON), &list); err != nil {
			return fmt.Errorf("store: decode settings key %s: want a JSON array: %w", settingExtractPasswords, err)
		}
	}
	if slices.Contains(list, pw) {
		return nil
	}

	list = append(list, pw)
	if len(list) > MaxExtractPasswords {
		list = list[len(list)-MaxExtractPasswords:]
	}

	encoded, err := json.Marshal(list)
	if err != nil {
		return fmt.Errorf("store: encode settings key %s: %w", settingExtractPasswords, err)
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(
		ctx, queryUpsertExtractPasswords,
		NewID(PrefixSetting), settingExtractPasswords, string(encoded), now, now,
	); err != nil {
		return fmt.Errorf("store: append settings key %s: %w", settingExtractPasswords, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: append settings key %s: commit: %w", settingExtractPasswords, err)
	}

	return nil
}

// queryPrefs reads every ui_prefs row of one account: one row per top-level
// member of the preference document (docs/04-data-model.md section 3.6).
const queryPrefs = `SELECT key, value_json FROM ui_prefs WHERE user_id = ?`

// Prefs returns the account's ui_prefs rows assembled into one document, or
// an empty document when the account has never stored one — the SPA owns the
// defaults (docs/09-web-ui-spec.md section 3.3). Members the server does not
// model come back verbatim, which is what lets the SPA add a preference
// without a server change.
func (s *SettingsStore) Prefs(ctx context.Context, userID string) (map[string]any, error) {
	var rows []struct {
		Key       string `db:"key"`
		ValueJSON string `db:"value_json"`
	}
	if err := s.db.SelectContext(ctx, &rows, queryPrefs, userID); err != nil {
		return nil, fmt.Errorf("store: list ui_prefs for user %s: %w", userID, err)
	}

	doc := make(map[string]any, len(rows))
	for _, row := range rows {
		var value any
		if err := json.Unmarshal([]byte(row.ValueJSON), &value); err != nil {
			return nil, fmt.Errorf("store: decode ui_prefs key %s: %w", row.Key, err)
		}
		doc[row.Key] = value
	}

	return doc, nil
}

const queryDeletePrefs = `DELETE FROM ui_prefs WHERE user_id = ?`

const queryInsertPref = `INSERT INTO ui_prefs (id, user_id, key, value_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)`

// PutPrefs replaces the document wholesale in one transaction: the account's
// existing rows are deleted and one row per top-level member is inserted,
// each value_json holding the member's JSON. Members the server does not
// model are stored verbatim (docs/05-api-contract.md section 11.4).
func (s *SettingsStore) PutPrefs(ctx context.Context, userID string, doc map[string]any) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: replace ui_prefs for user %s: %w", userID, err)
	}
	// Rolls back on any early return; after Commit this is sql.ErrTxDone,
	// which is the expected outcome and not worth a warning.
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "store: rollback of ui_prefs replace failed", "user_id", userID, "error", err)
		}
	}()

	if _, err := tx.ExecContext(ctx, queryDeletePrefs, userID); err != nil {
		return fmt.Errorf("store: replace ui_prefs for user %s: %w", userID, err)
	}

	now := time.Now().UnixMilli()
	for key, value := range doc {
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("store: encode ui_prefs key %s: %w", key, err)
		}
		if _, err := tx.ExecContext(ctx, queryInsertPref, NewID(PrefixUIPref), userID, key, string(encoded), now, now); err != nil {
			return fmt.Errorf("store: replace ui_prefs for user %s: %w", userID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: replace ui_prefs for user %s: commit: %w", userID, err)
	}

	return nil
}

// NotificationChannel is one row of the notification_channels table
// (docs/04-data-model.md section 4.8). It is a store-internal shape: every
// field carries json:"-" so the row can never be serialised to an API
// response — the API renders channels through its own view — and
// SecretEnc only ever leaves the row as ciphertext. The notifier
// (internal/jobs) is the sole caller that opens it.
type NotificationChannel struct {
	ID         string  `db:"id"           json:"-"`
	Kind       string  `db:"kind"         json:"-"` // webhook | ntfy | gotify | apprise
	Name       string  `db:"name"         json:"-"`
	Enabled    int     `db:"enabled"      json:"-"`
	ConfigJSON string  `db:"config_json"  json:"-"`
	SecretEnc  *string `db:"secret_enc"   json:"-"`
	EventMask  string  `db:"event_mask"   json:"-"`
	LastSendAt *int64  `db:"last_send_at" json:"-"`
	LastError  *string `db:"last_error"   json:"-"`
	CreatedAt  int64   `db:"created_at"   json:"-"`
	UpdatedAt  int64   `db:"updated_at"   json:"-"`
}

// notificationChannelColumns is the explicit column list both channel
// reads share, so the list and detail reads cannot drift apart and a
// widening SELECT cannot change what either returns. secret_enc is
// included deliberately: the notifier decrypts it inside Send, and the
// struct is unserialisable so the ciphertext can reach no response.
const notificationChannelColumns = `id, kind, name, enabled, config_json, secret_enc,
event_mask, last_send_at, last_error, created_at, updated_at`

const queryListNotificationChannels = `SELECT ` + notificationChannelColumns + `
FROM notification_channels ORDER BY name`

// ListNotificationChannels returns every notification_channels row,
// ordered by name. The fan-out filters on enabled and event_mask itself —
// a disabled channel is still listed so the test send can reach it.
func (s *SettingsStore) ListNotificationChannels(ctx context.Context) ([]NotificationChannel, error) {
	var channels []NotificationChannel
	if err := s.db.SelectContext(ctx, &channels, queryListNotificationChannels); err != nil {
		return nil, fmt.Errorf("store: list notification channels: %w", err)
	}

	return channels, nil
}

const queryNotificationChannelByID = `SELECT ` + notificationChannelColumns + `
FROM notification_channels WHERE id = ?`

// GetNotificationChannel resolves one row by id. ErrNotFound means the id
// addresses no known channel.
func (s *SettingsStore) GetNotificationChannel(ctx context.Context, id string) (NotificationChannel, error) {
	var ch NotificationChannel
	err := s.db.GetContext(ctx, &ch, queryNotificationChannelByID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return NotificationChannel{}, fmt.Errorf("store: notification channel %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return NotificationChannel{}, fmt.Errorf("store: notification channel %s: %w", id, err)
	}

	return ch, nil
}

const queryTouchNotificationChannel = `UPDATE notification_channels
SET last_send_at = ?, last_error = ?, updated_at = ?
WHERE id = ?`

// TouchNotificationChannel records the outcome of one delivery attempt:
// last_send_at is the attempt time on success and failure alike, and
// last_error carries the failure text or NULL after a success (the
// semantics of docs/04-data-model.md section 3.2). ErrNotFound means the
// channel was deleted between enqueue and delivery.
func (s *SettingsStore) TouchNotificationChannel(ctx context.Context, id string, lastErr *string, at int64) error {
	result, err := s.db.ExecContext(ctx, queryTouchNotificationChannel, at, lastErr, at, id)
	if err != nil {
		return fmt.Errorf("store: touch notification channel %s: %w", id, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: touch notification channel %s: read rows affected: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: touch notification channel %s: %w", id, ErrNotFound)
	}

	return nil
}

const (
	// notificationSecretInfo is the HKDF info string separating the
	// notification-channel seal from every other use of the at-rest key
	// (docs/11-config-reference.md section 6); notificationKeySize is the
	// AES-256 key length derived from it — the same construction
	// NewIndexerStore applies to api_key_enc.
	notificationSecretInfo = "dl-tool/notification-secret/v1"
	notificationKeySize    = 32
)

// notificationAEAD derives the channel-secret cipher from the at-rest key.
// An empty key is an error: sealing under an empty secret would silently
// reduce to a fixed key.
func notificationAEAD(key secure.Secret) (cipher.AEAD, error) {
	if key.Reveal() == "" {
		return nil, errors.New("store: notification secret sealing requires a non-empty secret key")
	}

	raw := make([]byte, notificationKeySize)
	if _, err := io.ReadFull(
		hkdf.New(sha256.New, []byte(key.Reveal()), nil, []byte(notificationSecretInfo)),
		raw,
	); err != nil {
		return nil, fmt.Errorf("store: derive notification key: %w", err)
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("store: build notification cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("store: build notification aead: %w", err)
	}

	return aead, nil
}

// SealNotificationSecret encrypts one channel secret for secret_enc:
// base64(nonce || ciphertext). An empty plaintext seals to nil — the
// column stays NULL so secret_set reports false.
func SealNotificationSecret(key secure.Secret, plain string) (*string, error) {
	if plain == "" {
		return nil, nil
	}

	aead, err := notificationAEAD(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("store: seal notification secret: nonce: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, []byte(plain), nil)
	enc := base64.StdEncoding.EncodeToString(sealed)

	return &enc, nil
}

// OpenNotificationSecret decrypts one secret_enc value back to the stored
// secret; a NULL column returns the empty Secret. A ciphertext that does
// not open is corrupt or sealed under a different key — either way an
// error, never a silently wrong value.
func OpenNotificationSecret(key secure.Secret, enc *string) (secure.Secret, error) {
	if enc == nil || *enc == "" {
		return "", nil
	}

	aead, err := notificationAEAD(key)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(*enc)
	if err != nil {
		return "", fmt.Errorf("store: decode notification secret: %w", err)
	}
	if len(raw) < aead.NonceSize() {
		return "", errors.New("store: notification secret ciphertext shorter than nonce")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("store: open notification secret: %w", err)
	}

	return secure.Secret(plain), nil
}
