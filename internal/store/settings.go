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
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// queryTagByName is queryListTags over one unique name.
const queryTagByName = `SELECT t.name, COUNT(k.id) AS task_count
FROM tags t
LEFT JOIN task_tags tt ON tt.tag_id = t.id
LEFT JOIN tasks k ON k.id = tt.task_id AND k.state <> 'removed'
WHERE t.name = ?
GROUP BY t.id`

// TagByName resolves one row by its unique name, carrying the same
// task_count the list does. ErrNotFound means no tag carries it.
func (s *SettingsStore) TagByName(ctx context.Context, name string) (Tag, error) {
	var tag Tag
	err := s.db.GetContext(ctx, &tag, queryTagByName, name)
	if errors.Is(err, sql.ErrNoRows) {
		return Tag{}, fmt.Errorf("store: tag %s: %w", name, ErrNotFound)
	}
	if err != nil {
		return Tag{}, fmt.Errorf("store: tag %s: %w", name, err)
	}

	return tag, nil
}

const queryRenameTag = `UPDATE tags SET name = ?, updated_at = ? WHERE name = ?`

// RenameTag renames the row in place, so every task carrying it carries
// the new name at once; the tag id is unchanged and no task row is
// touched. ErrNotFound means name addresses no row; ErrConflict means
// newName belongs to another row — a rename is a conflict, never a
// silent merge.
func (s *SettingsStore) RenameTag(ctx context.Context, name, newName string) error {
	result, err := s.db.ExecContext(ctx, queryRenameTag, newName, time.Now().UnixMilli(), name)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: rename tag %s: %w", name, ErrConflict)
		}

		return fmt.Errorf("store: rename tag %s: %w", name, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: rename tag %s: read rows affected: %w", name, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: rename tag %s: %w", name, ErrNotFound)
	}

	return nil
}

// queryDetachTag and queryDeleteTagRow run inside one transaction in
// DeleteTag: the link rows go first, then the tag row itself.
const (
	queryDetachTag    = `DELETE FROM task_tags WHERE tag_id = (SELECT id FROM tags WHERE name = ?)`
	queryDeleteTagRow = `DELETE FROM tags WHERE name = ?`
)

// DeleteTag detaches the tag from every task and deletes the row in one
// transaction, so a task observed mid-write can never hold a link to a
// tag that no longer exists. NO TASK IS EVER DELETED: only the task_tags
// links and the tag row go — the ON DELETE CASCADE on task_tags.tag_id
// is the backstop, not the mechanism. ErrNotFound means name addresses
// no row.
func (s *SettingsStore) DeleteTag(ctx context.Context, name string) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: delete tag %s: %w", name, err)
	}
	// Rolls back on any early return; after Commit this is sql.ErrTxDone,
	// which is the expected outcome and not worth a warning.
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "store: rollback of tag delete failed", "error", err)
		}
	}()

	if _, err := tx.ExecContext(ctx, queryDetachTag, name); err != nil {
		return fmt.Errorf("store: detach tag %s: %w", name, err)
	}

	result, err := tx.ExecContext(ctx, queryDeleteTagRow, name)
	if err != nil {
		return fmt.Errorf("store: delete tag %s: %w", name, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete tag %s: read rows affected: %w", name, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete tag %s: %w", name, ErrNotFound)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: delete tag %s: commit: %w", name, err)
	}

	return nil
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

// querySettingValue reads one settings row's value by key.
const querySettingValue = `SELECT value_json FROM settings WHERE key = ?`

// GetInt64 reads one settings key as an integer, returning def when the
// row is absent. The stored grammar is a bare JSON integer — `4`, never
// `"4"` or `4.0` — the same shape parseNonNegativeSettingInt enforces for
// the admission keys, so a malformed row is an error, never a guess.
func (s *SettingsStore) GetInt64(ctx context.Context, key string, def int64) (int64, error) {
	var valueJSON string
	err := s.db.GetContext(ctx, &valueJSON, querySettingValue, key)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: read settings key %s: %w", key, err)
	}

	// Decoding into *int64 rather than int64 so a stored JSON null is a
	// decode failure, not a silent 0 an operator never wrote.
	var value *int64
	if err := json.Unmarshal([]byte(valueJSON), &value); err != nil {
		return 0, fmt.Errorf("store: decode settings key %s: want an integer, got %q: %w", key, valueJSON, err)
	}
	if value == nil {
		return 0, fmt.Errorf("store: decode settings key %s: want an integer, got %q", key, valueJSON)
	}

	return *value, nil
}

// queryUpsertSetting inserts or replaces one settings row by key; the
// ON CONFLICT targets the settings.key unique index the migration creates.
const queryUpsertSetting = `INSERT INTO settings (id, key, value_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(key) DO UPDATE SET
  value_json = excluded.value_json,
  updated_at = excluded.updated_at`

// SetInt64 upserts one settings key as a bare JSON integer — the grammar
// GetInt64 accepts.
func (s *SettingsStore) SetInt64(ctx context.Context, key string, v int64) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: encode settings key %s: %w", key, err)
	}

	now := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(
		ctx, queryUpsertSetting,
		NewID(PrefixSetting), key, string(encoded), now, now,
	); err != nil {
		return fmt.Errorf("store: write settings key %s: %w", key, err)
	}

	return nil
}

// Settings is the flat, typed view of the settings table. Every field maps
// to one key in docs/11-config-reference.md section 5. ExtractPasswords is
// the only secret: it never marshals (the API emits "__redacted__"), so a
// Settings value cannot leak it into a response.
type Settings struct {
	DownloadRateLimit  int64            `json:"download_rate_limit"`
	UploadRateLimit    int64            `json:"upload_rate_limit"`
	AltDownloadRate    int64            `json:"alt_download_rate_limit"`
	AltUploadRate      int64            `json:"alt_upload_rate_limit"`
	ScheduleEnabled    bool             `json:"schedule_enabled"`
	DefaultDestination string           `json:"default_destination"`
	MinFreeSpace       map[string]int64 `json:"min_free_space"`
	MaxActiveTotal     int              `json:"max_active_total"`
	MaxActivePerEngine int              `json:"max_active_per_engine"`
	ProcessOrder       string           `json:"process_order"` // by_date_created
	RSSEnabled         bool             `json:"rss_enabled"`
	RSSIntervalS       int              `json:"rss_interval_s"`
	AutoExtract        bool             `json:"auto_extract"`
	ExtractPasswords   []secure.Secret  `json:"-"` // never marshalled; the API emits "__redacted__"
	ConfirmOnDelete    bool             `json:"confirm_on_delete"`
}

// The two write-path sentinels PATCH /settings maps to 422
// /problems/validation-failed (docs/05-api-contract.md section 11.1).
var (
	ErrUnknownSettingKey = errors.New("store: unknown setting key")
	ErrSettingOutOfRange = errors.New("store: setting value out of range")
)

// The settings keys whose constants do not already exist above
// (settingDefaultDestination, settingExtractPasswords,
// settingScheduleEnabled). The whole fifteen-key set of doc 11 section 5
// lives in settingsKeys below.
const (
	settingDownloadRateLimit  = "download_rate_limit"
	settingUploadRateLimit    = "upload_rate_limit"
	settingAltDownloadRate    = "alt_download_rate_limit"
	settingAltUploadRate      = "alt_upload_rate_limit"
	settingMinFreeSpace       = "min_free_space"
	settingMaxActiveTotal     = "max_active_total"
	settingMaxActivePerEngine = "max_active_per_engine"
	settingProcessOrder       = "process_order"
	settingRSSEnabled         = "rss_enabled"
	settingRSSIntervalS       = "rss_interval_s"
	settingAutoExtract        = "auto_extract"
	settingConfirmOnDelete    = "confirm_on_delete"
)

const (
	defaultAltDownloadRate    = 5_242_880
	defaultAltUploadRate      = 1_048_576
	defaultMaxActiveTotal     = 5
	defaultMaxActivePerEngine = 3
	defaultRSSIntervalS       = 1800
	processOrderByDateCreated = "by_date_created"

	// rssIntervalSFloor is the 5-minute minimum doc 11 section 5 puts on
	// the global RSS poll interval.
	rssIntervalSFloor = 300
)

// settingsKeys is the closed key set of doc 11 section 5 — the only keys
// GET /settings renders and PATCH /settings accepts. Internal keys such as
// watch_folder_loaded_* are outside it, so they can never be read or
// written through the settings endpoints.
var settingsKeys = []string{
	settingDownloadRateLimit, settingUploadRateLimit,
	settingAltDownloadRate, settingAltUploadRate,
	settingScheduleEnabled, settingDefaultDestination, settingMinFreeSpace,
	settingMaxActiveTotal, settingMaxActivePerEngine,
	settingProcessOrder, settingRSSEnabled, settingRSSIntervalS,
	settingAutoExtract, settingExtractPasswords, settingConfirmOnDelete,
}

// SettingsKeys returns a copy of the closed key set so other packages —
// the API's PATCH schema — share this one canonical list instead of
// duplicating it.
func SettingsKeys() []string { return slices.Clone(settingsKeys) }

// queryAllSettings reads only the documented keys; the whitelist, not a
// blacklist, so an internal key can never leak into GET /settings.
// settingsKeys must remain compile-time constants: they are interpolated
// into the SQL below, never bound as parameters.
var queryAllSettings = `SELECT key, value_json FROM settings WHERE key IN ('` +
	strings.Join(settingsKeys, `','`) + `')`

// GetSettings reads every documented settings row, applies the documented
// default for a missing key and returns the typed struct. It never returns
// a partially populated value: a stored value that does not decode into its
// key's documented type is an error, never a guess.
func (s *SettingsStore) GetSettings(ctx context.Context) (Settings, error) {
	var rows []struct {
		Key       string `db:"key"`
		ValueJSON string `db:"value_json"`
	}
	if err := s.db.SelectContext(ctx, &rows, queryAllSettings); err != nil {
		return Settings{}, fmt.Errorf("store: read settings: %w", err)
	}

	// The defaults of doc 11 section 5; default_destination's documented
	// default (first DLTOOL_DATA_ROOTS entry) is not knowable to the store,
	// so "" reports unset and the API substitutes the root.
	out := Settings{
		AltDownloadRate:    defaultAltDownloadRate,
		AltUploadRate:      defaultAltUploadRate,
		MinFreeSpace:       map[string]int64{},
		MaxActiveTotal:     defaultMaxActiveTotal,
		MaxActivePerEngine: defaultMaxActivePerEngine,
		ProcessOrder:       processOrderByDateCreated,
		RSSEnabled:         true,
		RSSIntervalS:       defaultRSSIntervalS,
		ExtractPasswords:   []secure.Secret{},
		ConfirmOnDelete:    true,
	}
	for _, row := range rows {
		var err error
		switch row.Key {
		case settingDownloadRateLimit:
			out.DownloadRateLimit, err = settingInt(row.Key, row.ValueJSON)
		case settingUploadRateLimit:
			out.UploadRateLimit, err = settingInt(row.Key, row.ValueJSON)
		case settingAltDownloadRate:
			out.AltDownloadRate, err = settingInt(row.Key, row.ValueJSON)
		case settingAltUploadRate:
			out.AltUploadRate, err = settingInt(row.Key, row.ValueJSON)
		case settingScheduleEnabled:
			out.ScheduleEnabled, err = settingBool(row.Key, row.ValueJSON)
		case settingDefaultDestination:
			out.DefaultDestination, err = settingString(row.Key, row.ValueJSON)
		case settingMinFreeSpace:
			out.MinFreeSpace, err = settingIntMap(row.Key, row.ValueJSON)
		case settingMaxActiveTotal:
			var n int64
			n, err = settingInt(row.Key, row.ValueJSON)
			out.MaxActiveTotal = int(n)
		case settingMaxActivePerEngine:
			var n int64
			n, err = settingInt(row.Key, row.ValueJSON)
			out.MaxActivePerEngine = int(n)
		case settingProcessOrder:
			out.ProcessOrder, err = settingString(row.Key, row.ValueJSON)
		case settingRSSEnabled:
			out.RSSEnabled, err = settingBool(row.Key, row.ValueJSON)
		case settingRSSIntervalS:
			var n int64
			n, err = settingInt(row.Key, row.ValueJSON)
			out.RSSIntervalS = int(n)
		case settingAutoExtract:
			out.AutoExtract, err = settingBool(row.Key, row.ValueJSON)
		case settingExtractPasswords:
			var list []string
			list, err = settingStringSlice(row.Key, row.ValueJSON)
			out.ExtractPasswords = make([]secure.Secret, len(list))
			for i, pw := range list {
				out.ExtractPasswords[i] = secure.Secret(pw)
			}
		case settingConfirmOnDelete:
			out.ConfirmOnDelete, err = settingBool(row.Key, row.ValueJSON)
		default:
			// Unreachable while settingsKeys and this switch stay in
			// lockstep; an unmapped key must error rather than silently
			// report the default for a stored override.
			return Settings{}, fmt.Errorf("store: read settings: unmapped key %q", row.Key)
		}
		if err != nil {
			return Settings{}, err
		}
	}

	return out, nil
}

// PutSettings replaces each documented key present in patch, in one
// transaction, then returns the stored settings. The extract_passwords
// key is skipped when its value is exactly "__redacted__", so a client
// that round-trips GET /settings into PATCH /settings cannot erase it;
// the placeholder is invalid for every other key. ErrUnknownSettingKey
// means the patch names a key outside doc 11 section 5;
// ErrSettingOutOfRange means a value does not decode into its key's
// documented type, is a JSON null, or is outside its documented domain.
func (s *SettingsStore) PutSettings(ctx context.Context, patch map[string]json.RawMessage) (Settings, error) {
	writes := make(map[string]string, len(patch))
	for key, raw := range patch {
		encoded, skip, err := canonicalSetting(key, raw)
		if err != nil {
			return Settings{}, err
		}
		if !skip {
			writes[key] = encoded
		}
	}

	if len(writes) > 0 {
		tx, err := s.db.BeginTxx(ctx, nil)
		if err != nil {
			return Settings{}, fmt.Errorf("store: replace settings: %w", err)
		}
		// Rolls back on any early return; after Commit this is
		// sql.ErrTxDone, which is the expected outcome and not worth a
		// warning.
		defer func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				slog.WarnContext(ctx, "store: rollback of settings replace failed", "error", err)
			}
		}()

		now := time.Now().UnixMilli()
		for key, encoded := range writes {
			if _, err := tx.ExecContext(
				ctx, queryUpsertSetting,
				NewID(PrefixSetting), key, encoded, now, now,
			); err != nil {
				return Settings{}, fmt.Errorf("store: write settings key %s: %w", key, err)
			}
		}

		if err := tx.Commit(); err != nil {
			return Settings{}, fmt.Errorf("store: replace settings: commit: %w", err)
		}
	}

	return s.GetSettings(ctx)
}

// canonicalSetting validates one PATCH member against its key's documented
// type and domain and re-encodes it as the stored JSON. skip reports the
// extract_passwords no-op: the member holding exactly "__redacted__".
func canonicalSetting(key string, raw json.RawMessage) (encoded string, skip bool, err error) {
	outOfRange := func(detail string) error {
		return fmt.Errorf("store: settings key %s: %s: %w", key, detail, ErrSettingOutOfRange)
	}

	switch key {
	case settingDownloadRateLimit, settingUploadRateLimit,
		settingAltDownloadRate, settingAltUploadRate:
		n, decErr := settingInt(key, string(raw))
		if decErr != nil || n < 0 {
			return "", false, outOfRange("want a non-negative integer")
		}
		return strconv.FormatInt(n, 10), false, nil
	case settingMaxActiveTotal, settingMaxActivePerEngine:
		n, decErr := settingInt(key, string(raw))
		// The typed Settings narrows these to int; bounding at MaxInt32
		// keeps that conversion exact on every platform.
		if decErr != nil || n < 0 || n > math.MaxInt32 {
			return "", false, outOfRange("want a non-negative integer no greater than 2147483647")
		}
		return strconv.FormatInt(n, 10), false, nil
	case settingRSSIntervalS:
		n, decErr := settingInt(key, string(raw))
		if decErr != nil || n < rssIntervalSFloor {
			return "", false, outOfRange(fmt.Sprintf("want an integer of at least %d seconds", rssIntervalSFloor))
		}
		return strconv.FormatInt(n, 10), false, nil
	case settingScheduleEnabled, settingRSSEnabled, settingAutoExtract, settingConfirmOnDelete:
		b, decErr := settingBool(key, string(raw))
		if decErr != nil {
			return "", false, outOfRange("want a boolean")
		}
		return strconv.FormatBool(b), false, nil
	case settingProcessOrder:
		v, decErr := settingString(key, string(raw))
		if decErr != nil || v != processOrderByDateCreated {
			return "", false, outOfRange(`want the enum value "by_date_created"`)
		}
		return `"` + processOrderByDateCreated + `"`, false, nil
	case settingDefaultDestination:
		v, decErr := settingString(key, string(raw))
		if decErr != nil || v == "" {
			return "", false, outOfRange("want a non-empty path string")
		}
		if v == "__redacted__" {
			return "", false, outOfRange("__redacted__ is a rendered form, not a path")
		}
		// Doc 11 section 5 types the key as an absolute path inside a
		// data root; the root-membership half lives in the API layer,
		// which knows the configured roots.
		if !filepath.IsAbs(v) || filepath.Clean(v) != v {
			return "", false, outOfRange("want an absolute canonical path")
		}
		encoded, encErr := json.Marshal(v)
		if encErr != nil {
			return "", false, fmt.Errorf("store: encode settings key %s: %w", key, encErr)
		}
		return string(encoded), false, nil
	case settingMinFreeSpace:
		m, decErr := settingIntMap(key, string(raw))
		if decErr != nil {
			return "", false, outOfRange("want an object of absolute root path to bytes")
		}
		for root, floor := range m {
			if !filepath.IsAbs(root) || filepath.Clean(root) != root {
				return "", false, outOfRange(fmt.Sprintf("key %q is not an absolute canonical path", root))
			}
			if floor < 0 {
				return "", false, outOfRange(fmt.Sprintf("value for %q is not a non-negative integer", root))
			}
		}
		encoded, encErr := json.Marshal(m)
		if encErr != nil {
			return "", false, fmt.Errorf("store: encode settings key %s: %w", key, encErr)
		}
		return string(encoded), false, nil
	case settingExtractPasswords:
		// The "__redacted__" write-back rule of doc 11 section 6: the
		// rendered form is a no-op on the stored list, so a GET/PATCH
		// round trip cannot erase it.
		if string(raw) == `"__redacted__"` {
			return "", true, nil
		}
		list, decErr := settingStringSlice(key, string(raw))
		if decErr != nil {
			return "", false, outOfRange("want an array of strings")
		}
		encoded, encErr := json.Marshal(list)
		if encErr != nil {
			return "", false, fmt.Errorf("store: encode settings key %s: %w", key, encErr)
		}
		return string(encoded), false, nil
	default:
		return "", false, fmt.Errorf("store: settings key %s: %w", key, ErrUnknownSettingKey)
	}
}

// settingInt decodes one stored settings value as a bare JSON integer —
// `4`, never `"4"`, `4.0` or `null`: a JSON null leaves the pointer nil,
// so it is an error rather than a silent zero an operator never wrote.
func settingInt(key, valueJSON string) (int64, error) {
	var value *int64
	if err := json.Unmarshal([]byte(valueJSON), &value); err != nil {
		return 0, fmt.Errorf("store: decode settings key %s: want an integer, got %q: %w", key, valueJSON, err)
	}
	if value == nil {
		return 0, fmt.Errorf("store: decode settings key %s: want an integer, got %q", key, valueJSON)
	}

	return *value, nil
}

// settingBool decodes one stored settings value as a bare JSON boolean;
// null is an error for the same reason settingInt rejects it.
func settingBool(key, valueJSON string) (bool, error) {
	var value *bool
	if err := json.Unmarshal([]byte(valueJSON), &value); err != nil {
		return false, fmt.Errorf("store: decode settings key %s: want a boolean, got %q: %w", key, valueJSON, err)
	}
	if value == nil {
		return false, fmt.Errorf("store: decode settings key %s: want a boolean, got %q", key, valueJSON)
	}

	return *value, nil
}

// settingString decodes one stored settings value as a JSON string; null
// is an error for the same reason settingInt rejects it.
func settingString(key, valueJSON string) (string, error) {
	var value *string
	if err := json.Unmarshal([]byte(valueJSON), &value); err != nil {
		return "", fmt.Errorf("store: decode settings key %s: want a string, got %q: %w", key, valueJSON, err)
	}
	if value == nil {
		return "", fmt.Errorf("store: decode settings key %s: want a string, got %q", key, valueJSON)
	}

	return *value, nil
}

// settingStringSlice decodes one stored settings value as a JSON array of
// strings; null is an error for the same reason settingInt rejects it.
func settingStringSlice(key, valueJSON string) ([]string, error) {
	var value *[]string
	if err := json.Unmarshal([]byte(valueJSON), &value); err != nil {
		return nil, fmt.Errorf("store: decode settings key %s: want an array of strings, got %q: %w", key, valueJSON, err)
	}
	if value == nil {
		return nil, fmt.Errorf("store: decode settings key %s: want an array of strings, got %q", key, valueJSON)
	}

	return *value, nil
}

// settingIntMap decodes one stored settings value as a JSON object of
// string to integer; null is an error for the same reason settingInt
// rejects it. `4.0` and `"4"` members fail the integer grammar.
func settingIntMap(key, valueJSON string) (map[string]int64, error) {
	var value *map[string]int64
	if err := json.Unmarshal([]byte(valueJSON), &value); err != nil {
		return nil, fmt.Errorf("store: decode settings key %s: want an object of root path to bytes, got %q: %w", key, valueJSON, err)
	}
	if value == nil {
		return nil, fmt.Errorf("store: decode settings key %s: want an object of root path to bytes, got %q", key, valueJSON)
	}

	return *value, nil
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

// ScheduleMode is the stored bandwidth_schedule.mode value
// (docs/04-data-model.md section 3.6). The API renders the cells as the
// integers of docs/05-api-contract.md section 11.2; the translation is
// owned there, not here.
type ScheduleMode string

const (
	ScheduleNoDownload  ScheduleMode = "no_download"
	ScheduleDefault     ScheduleMode = "default"
	ScheduleAlternative ScheduleMode = "alternative"
)

// settingScheduleEnabled is the settings key gating evaluation of the
// grid (docs/11-config-reference.md section 5). The migration seeds no
// row: absent means false, the documented default.
const settingScheduleEnabled = "schedule_enabled"

const querySchedule = `SELECT day, hour, mode FROM bandwidth_schedule ORDER BY day, hour`

// Schedule reads all 168 rows of bandwidth_schedule and returns them as
// cells indexed day*24+hour, day 0 = Monday. The table always holds
// exactly 168 rows, seeded by 00001_init.sql with 'default'; a short,
// out-of-range or unknown-mode read is an error rather than a partially
// zeroed grid the scheduler would evaluate as unintended pauses.
func (s *SettingsStore) Schedule(ctx context.Context) (cells [168]ScheduleMode, err error) {
	return scheduleCells(ctx, s.db)
}

// scheduleCells runs the Schedule read on any queryable handle — the
// store's DB for Schedule, a transaction for ScheduleSnapshot — so both
// share the one strict decode.
func scheduleCells(ctx context.Context, q sqlx.QueryerContext) (cells [168]ScheduleMode, err error) {
	var rows []struct {
		Day  int          `db:"day"`
		Hour int          `db:"hour"`
		Mode ScheduleMode `db:"mode"`
	}
	if err := sqlx.SelectContext(ctx, q, &rows, querySchedule); err != nil {
		return cells, fmt.Errorf("store: read bandwidth schedule: %w", err)
	}
	if len(rows) != len(cells) {
		return cells, fmt.Errorf("store: bandwidth schedule holds %d rows, want %d", len(rows), len(cells))
	}
	for _, row := range rows {
		if row.Day < 0 || row.Day > 6 || row.Hour < 0 || row.Hour > 23 {
			return cells, fmt.Errorf("store: bandwidth schedule row out of range: day %d, hour %d", row.Day, row.Hour)
		}
		switch row.Mode {
		case ScheduleNoDownload, ScheduleDefault, ScheduleAlternative:
		default:
			return cells, fmt.Errorf("store: bandwidth schedule cell %d holds unknown mode %q", row.Day*24+row.Hour, row.Mode)
		}
		idx := row.Day*24 + row.Hour
		if cells[idx] != "" {
			return cells, fmt.Errorf("store: bandwidth schedule has duplicate cell %d", idx)
		}
		cells[idx] = row.Mode
	}

	return cells, nil
}

// ScheduleEnabled reads the schedule_enabled settings key
// (docs/11-config-reference.md section 5): false when the row is absent,
// an error when the stored value is not a JSON boolean — the same
// grammar strictness GetInt64 applies to the integer keys.
func (s *SettingsStore) ScheduleEnabled(ctx context.Context) (bool, error) {
	return scheduleEnabled(ctx, s.db)
}

// scheduleEnabled runs the ScheduleEnabled read on any queryable handle —
// the store's DB for ScheduleEnabled, a transaction for ScheduleSnapshot.
func scheduleEnabled(ctx context.Context, q sqlx.QueryerContext) (bool, error) {
	var valueJSON string
	err := sqlx.GetContext(ctx, q, &valueJSON, querySettingValue, settingScheduleEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read settings key %s: %w", settingScheduleEnabled, err)
	}

	var value *bool
	if err := json.Unmarshal([]byte(valueJSON), &value); err != nil {
		return false, fmt.Errorf("store: decode settings key %s: want a boolean, got %q: %w", settingScheduleEnabled, valueJSON, err)
	}
	if value == nil {
		return false, fmt.Errorf("store: decode settings key %s: want a boolean, got %q", settingScheduleEnabled, valueJSON)
	}

	return *value, nil
}

// ScheduleSnapshot returns the grid and the schedule_enabled flag from
// one read transaction, so a concurrent ReplaceSchedule cannot interleave
// the pair — the response the API renders is one committed version, never
// new cells beside the old flag.
func (s *SettingsStore) ScheduleSnapshot(ctx context.Context) (cells [168]ScheduleMode, enabled bool, err error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return cells, false, fmt.Errorf("store: read bandwidth schedule snapshot: %w", err)
	}
	// Rolls back on any early return; after Commit this is sql.ErrTxDone,
	// which is the expected outcome and not worth a warning.
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "store: rollback of bandwidth schedule snapshot failed", "error", err)
		}
	}()

	if cells, err = scheduleCells(ctx, tx); err != nil {
		return cells, false, err
	}
	if enabled, err = scheduleEnabled(ctx, tx); err != nil {
		return cells, false, err
	}

	if err := tx.Commit(); err != nil {
		return cells, false, fmt.Errorf("store: read bandwidth schedule snapshot: commit: %w", err)
	}

	return cells, enabled, nil
}

const queryReplaceScheduleCell = `UPDATE bandwidth_schedule
SET mode = ?, updated_at = ?
WHERE day = ? AND hour = ?`

// ReplaceSchedule writes all 168 cells and the schedule_enabled flag in
// one sqlx.Tx — PUT /settings/schedule carries both, so a partial write
// is impossible: either every cell and the flag land or none does
// (docs/05-api-contract.md section 11.2). A mode outside the stored
// vocabulary is rejected before the transaction opens, and the flag is
// upserted as a bare JSON boolean — the grammar ScheduleEnabled accepts.
func (s *SettingsStore) ReplaceSchedule(ctx context.Context, enabled bool, cells [168]ScheduleMode) error {
	for i, mode := range cells {
		switch mode {
		case ScheduleNoDownload, ScheduleDefault, ScheduleAlternative:
		default:
			return fmt.Errorf("store: replace bandwidth schedule: cell %d holds unknown mode %q", i, mode)
		}
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: replace bandwidth schedule: %w", err)
	}
	// Rolls back on any early return; after Commit this is sql.ErrTxDone,
	// which is the expected outcome and not worth a warning.
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "store: rollback of bandwidth schedule replace failed", "error", err)
		}
	}()

	now := time.Now().UnixMilli()
	enabledJSON, err := json.Marshal(enabled)
	if err != nil {
		return fmt.Errorf("store: encode settings key %s: %w", settingScheduleEnabled, err)
	}
	if _, err := tx.ExecContext(
		ctx, queryUpsertSetting,
		NewID(PrefixSetting), settingScheduleEnabled, string(enabledJSON), now, now,
	); err != nil {
		return fmt.Errorf("store: replace bandwidth schedule: write settings key %s: %w", settingScheduleEnabled, err)
	}

	for i, mode := range cells {
		res, err := tx.ExecContext(ctx, queryReplaceScheduleCell, string(mode), now, i/24, i%24)
		if err != nil {
			return fmt.Errorf("store: replace bandwidth schedule: cell %d: %w", i, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: replace bandwidth schedule: cell %d: read rows affected: %w", i, err)
		}
		if affected != 1 {
			return fmt.Errorf("store: replace bandwidth schedule: cell %d: updated %d rows, want 1", i, affected)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: replace bandwidth schedule: commit: %w", err)
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

const queryCreateNotificationChannel = `INSERT INTO notification_channels
(id, kind, name, enabled, config_json, secret_enc, event_mask, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

// CreateNotificationChannel inserts one row; a name already taken is
// ErrConflict. The caller owns ch.ID (an ntf_ ULID) and every column —
// enabled as the stored 0/1 flag, secret_enc already sealed.
func (s *SettingsStore) CreateNotificationChannel(ctx context.Context, ch NotificationChannel) error {
	now := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(
		ctx, queryCreateNotificationChannel,
		ch.ID, ch.Kind, ch.Name, ch.Enabled, ch.ConfigJSON, ch.SecretEnc, ch.EventMask, now, now,
	); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: create notification channel %s: %w", ch.Name, ErrConflict)
		}

		return fmt.Errorf("store: create notification channel %s: %w", ch.Name, err)
	}

	return nil
}

// NotificationChannelPatch carries the fields a PATCH may write; a nil
// member leaves its column untouched. kind is absent deliberately: it is
// immutable, and the API rejects a change before the store is called.
// SecretSet distinguishes "leave secret_enc" from "write SecretEnc" —
// a nil SecretEnc with SecretSet true clears the stored secret, which
// COALESCE alone cannot express.
type NotificationChannelPatch struct {
	Name       *string
	Enabled    *int
	ConfigJSON *string
	SecretSet  bool
	SecretEnc  *string
	EventMask  *string
}

// queryUpdateNotificationChannel merges the patch inside the UPDATE: a
// NULL argument leaves its column untouched, so two concurrent PATCHes
// cannot lose each other's field. The secret member is the exception:
// SecretSet gates the write so a clear-to-NULL is expressible.
const queryUpdateNotificationChannel = `UPDATE notification_channels
SET name = COALESCE(?, name), enabled = COALESCE(?, enabled),
    config_json = COALESCE(?, config_json),
    secret_enc = CASE WHEN ? THEN ? ELSE secret_enc END,
    event_mask = COALESCE(?, event_mask), updated_at = ?
WHERE id = ?`

// UpdateNotificationChannel writes the addressed row's patch. ErrNotFound
// means id addresses no row; ErrConflict means the new name belongs to
// another row.
func (s *SettingsStore) UpdateNotificationChannel(ctx context.Context, id string, p NotificationChannelPatch) error {
	result, err := s.db.ExecContext(
		ctx, queryUpdateNotificationChannel,
		p.Name, p.Enabled, p.ConfigJSON, p.SecretSet, p.SecretEnc, p.EventMask, time.Now().UnixMilli(), id,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: update notification channel %s: %w", id, ErrConflict)
		}

		return fmt.Errorf("store: update notification channel %s: %w", id, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update notification channel %s: read rows affected: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: update notification channel %s: %w", id, ErrNotFound)
	}

	return nil
}

const queryDeleteNotificationChannel = `DELETE FROM notification_channels WHERE id = ?`

// DeleteNotificationChannel removes the row. Pending webhook jobs naming
// it fail their delivery lookup and are dropped by the handler.
// ErrNotFound means id addresses no row.
func (s *SettingsStore) DeleteNotificationChannel(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, queryDeleteNotificationChannel, id)
	if err != nil {
		return fmt.Errorf("store: delete notification channel %s: %w", id, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete notification channel %s: read rows affected: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete notification channel %s: %w", id, ErrNotFound)
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

// WatchFolder is one row of watch_folders with the category name joined in
// from categories (docs/04-data-model.md section 3.6); Category is NULL when
// the folder has none or the category row was deleted (ON DELETE SET NULL).
type WatchFolder struct {
	ID              string  `db:"id"`
	Path            string  `db:"path"`
	Enabled         int     `db:"enabled"`
	Destination     string  `db:"destination"`
	Category        *string `db:"category"`
	DeleteAfterLoad int     `db:"delete_after_load"`
	PollIntervalS   int     `db:"poll_interval_s"`
	LastScanAt      *int64  `db:"last_scan_at"`
	LastError       *string `db:"last_error"`
	CreatedAt       int64   `db:"created_at"`
	UpdatedAt       int64   `db:"updated_at"`
}

// watchFolderColumns is the explicit column list both watch-folder reads
// share — the folder's own columns plus the joined category name — so the
// list and detail reads cannot drift apart.
const watchFolderColumns = `w.id, w.path, w.enabled, w.destination, c.name AS category,
w.delete_after_load, w.poll_interval_s, w.last_scan_at, w.last_error, w.created_at, w.updated_at`

const queryListEnabledWatchFolders = `SELECT ` + watchFolderColumns + `
FROM watch_folders w LEFT JOIN categories c ON c.id = w.category_id
WHERE w.enabled = 1
ORDER BY w.created_at, w.id`

// ListEnabledWatchFolders returns every row of watch_folders with
// enabled = 1.
func (s *SettingsStore) ListEnabledWatchFolders(ctx context.Context) ([]WatchFolder, error) {
	var folders []WatchFolder
	if err := s.db.SelectContext(ctx, &folders, queryListEnabledWatchFolders); err != nil {
		return nil, fmt.Errorf("store: list enabled watch folders: %w", err)
	}

	return folders, nil
}

const queryWatchFolderByID = `SELECT ` + watchFolderColumns + `
FROM watch_folders w LEFT JOIN categories c ON c.id = w.category_id
WHERE w.id = ?`

// GetWatchFolder returns one row by id, or ErrNotFound.
func (s *SettingsStore) GetWatchFolder(ctx context.Context, id string) (WatchFolder, error) {
	var folder WatchFolder
	err := s.db.GetContext(ctx, &folder, queryWatchFolderByID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return WatchFolder{}, fmt.Errorf("store: watch folder %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return WatchFolder{}, fmt.Errorf("store: watch folder %s: %w", id, err)
	}

	return folder, nil
}

// queryTouchWatchFolder clears last_error on an empty value — NULLIF turns
// the empty string into NULL — so one statement covers both outcomes.
const queryTouchWatchFolder = `UPDATE watch_folders
SET last_scan_at = ?, last_error = NULLIF(?, ''), updated_at = ?
WHERE id = ?`

// TouchWatchFolder writes last_scan_at and last_error after a scan; an
// empty lastErr clears the column. ErrNotFound means the id addresses no
// row.
func (s *SettingsStore) TouchWatchFolder(ctx context.Context, id string, at int64, lastErr string) error {
	result, err := s.db.ExecContext(ctx, queryTouchWatchFolder, at, lastErr, at, id)
	if err != nil {
		return fmt.Errorf("store: touch watch folder %s: %w", id, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: touch watch folder %s: read rows affected: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: touch watch folder %s: %w", id, ErrNotFound)
	}

	return nil
}

// querySeedWatchFolder inserts the DLTOOL_WATCH_DIR row; the column
// defaults supply enabled, delete_after_load and poll_interval_s, and
// ON CONFLICT(path) DO NOTHING leaves an operator's row untouched across
// restarts.
const querySeedWatchFolder = `INSERT INTO watch_folders (id, path, destination, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(path) DO NOTHING`

// SeedWatchFolder inserts one enabled row for path — the DLTOOL_WATCH_DIR
// seed — and reports whether it created the row.
func (s *SettingsStore) SeedWatchFolder(ctx context.Context, path, destination string) (created bool, err error) {
	now := time.Now().UnixMilli()
	result, err := s.db.ExecContext(
		ctx, querySeedWatchFolder, NewID(PrefixWatchFolder), path, destination, now, now,
	)
	if err != nil {
		return false, fmt.Errorf("store: seed watch folder %s: %w", path, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: seed watch folder %s: read rows affected: %w", path, err)
	}

	return rows == 1, nil
}

// MaxWatchFolderLoaded bounds the per-folder loaded set; the oldest
// entries drop first, the same trim AppendExtractPassword applies.
const MaxWatchFolderLoaded = 4096

// watchFolderLoadedPrefix prefixes the per-folder settings key holding the
// persistent already_loaded record — a JSON array of infohashes, so the
// record survives a restart: the tasks row alone cannot separate
// already_loaded from torrent_duplicate.
const watchFolderLoadedPrefix = "watch_folder_loaded_"

func watchFolderLoadedKey(folderID string) string {
	return watchFolderLoadedPrefix + folderID
}

// WatchFolderLoaded reports whether the folder's loaded set already holds
// infohash. An absent settings row is the empty set.
func (s *SettingsStore) WatchFolderLoaded(ctx context.Context, folderID, infohash string) (bool, error) {
	key := watchFolderLoadedKey(folderID)

	var valueJSON string
	err := s.db.GetContext(ctx, &valueJSON, querySettingValue, key)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: read settings key %s: %w", key, err)
	}

	var list []string
	if err := json.Unmarshal([]byte(valueJSON), &list); err != nil {
		return false, fmt.Errorf("store: decode settings key %s: want a JSON array: %w", key, err)
	}

	return slices.Contains(list, infohash), nil
}

// watchFolderLoadedMu serializes loaded-set appends process-wide: the
// JSON array under one settings key is a read-modify-write, and two
// overlapping deferred transactions that both read then both write lose
// the earlier entry — or answer SQLITE_BUSY on the upgrade. Serializing
// here rather than on the instance keeps the guarantee when a caller
// holds a different SettingsStore over the same db.
var watchFolderLoadedMu sync.Mutex

// MarkWatchFolderLoaded adds infohash to the folder's loaded set in one
// transaction when it is absent, keeping at most MaxWatchFolderLoaded
// entries, oldest dropped first. Eviction is graceful but visible: a
// still-present file whose infohash was trimmed re-surfaces as
// SkipDuplicate on the next sweep — task dedup, not the loaded set, keeps
// it from loading twice.
func (s *SettingsStore) MarkWatchFolderLoaded(ctx context.Context, folderID, infohash string) error {
	watchFolderLoadedMu.Lock()
	defer watchFolderLoadedMu.Unlock()

	key := watchFolderLoadedKey(folderID)

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: append settings key %s: %w", key, err)
	}
	// Rolls back on any early return; after Commit this is sql.ErrTxDone,
	// which is the expected outcome and not worth a warning.
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "store: rollback of watch-folder loaded-set write failed", "error", err)
		}
	}()

	var valueJSON string
	err = tx.GetContext(ctx, &valueJSON, querySettingValue, key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: append settings key %s: %w", key, err)
	}

	var list []string
	if err == nil {
		if err := json.Unmarshal([]byte(valueJSON), &list); err != nil {
			return fmt.Errorf("store: decode settings key %s: want a JSON array: %w", key, err)
		}
	}
	if slices.Contains(list, infohash) {
		return nil
	}

	list = append(list, infohash)
	if len(list) > MaxWatchFolderLoaded {
		list = list[len(list)-MaxWatchFolderLoaded:]
	}

	encoded, err := json.Marshal(list)
	if err != nil {
		return fmt.Errorf("store: encode settings key %s: %w", key, err)
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(
		ctx, queryUpsertSetting,
		NewID(PrefixSetting), key, string(encoded), now, now,
	); err != nil {
		return fmt.Errorf("store: append settings key %s: %w", key, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: append settings key %s: commit: %w", key, err)
	}

	return nil
}

const queryListWatchFolders = `SELECT ` + watchFolderColumns + `
FROM watch_folders w LEFT JOIN categories c ON c.id = w.category_id
ORDER BY w.created_at, w.id`

// ListWatchFolders returns every row of watch_folders — the enabled rows
// the loader watches and the disabled rows the API still manages —
// oldest first, the same order ListEnabledWatchFolders uses.
func (s *SettingsStore) ListWatchFolders(ctx context.Context) ([]WatchFolder, error) {
	var folders []WatchFolder
	if err := s.db.SelectContext(ctx, &folders, queryListWatchFolders); err != nil {
		return nil, fmt.Errorf("store: list watch folders: %w", err)
	}

	return folders, nil
}

const queryCreateWatchFolder = `INSERT INTO watch_folders
(id, path, enabled, destination, category_id, delete_after_load, poll_interval_s, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

// CreateWatchFolder inserts one row; a path already watched is
// ErrConflict. The caller owns f.ID (a wfd_ ULID), the already-resolved
// Path and Destination, the resolved category id and the validated poll
// interval.
func (s *SettingsStore) CreateWatchFolder(ctx context.Context, f WatchFolder, categoryID *string) error {
	now := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(
		ctx, queryCreateWatchFolder,
		f.ID, f.Path, f.Enabled, f.Destination, categoryID, f.DeleteAfterLoad, f.PollIntervalS, now, now,
	); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: create watch folder %s: %w", f.Path, ErrConflict)
		}

		return fmt.Errorf("store: create watch folder %s: %w", f.Path, err)
	}

	return nil
}

// WatchFolderPatch carries the fields a PATCH may write; a nil member
// leaves its column untouched. CategorySet distinguishes "leave
// category_id" from "write CategoryID" — a nil CategoryID with
// CategorySet true clears the category, which COALESCE alone cannot
// express.
type WatchFolderPatch struct {
	Path            *string
	Enabled         *int
	Destination     *string
	CategorySet     bool
	CategoryID      *string
	DeleteAfterLoad *int
	PollIntervalS   *int
}

// queryUpdateWatchFolder merges the patch inside the UPDATE: a NULL
// argument leaves its column untouched, so two concurrent PATCHes cannot
// lose each other's field. The category member is the exception:
// CategorySet gates the write so a clear-to-NULL is expressible.
const queryUpdateWatchFolder = `UPDATE watch_folders
SET path = COALESCE(?, path), enabled = COALESCE(?, enabled),
    destination = COALESCE(?, destination),
    category_id = CASE WHEN ? THEN ? ELSE category_id END,
    delete_after_load = COALESCE(?, delete_after_load),
    poll_interval_s = COALESCE(?, poll_interval_s), updated_at = ?
WHERE id = ?`

// UpdateWatchFolder writes the addressed row's patch. ErrNotFound means
// id addresses no row; ErrConflict means the new path belongs to another
// row.
func (s *SettingsStore) UpdateWatchFolder(ctx context.Context, id string, p WatchFolderPatch) error {
	result, err := s.db.ExecContext(
		ctx, queryUpdateWatchFolder,
		p.Path, p.Enabled, p.Destination, p.CategorySet, p.CategoryID,
		p.DeleteAfterLoad, p.PollIntervalS, time.Now().UnixMilli(), id,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: update watch folder %s: %w", id, ErrConflict)
		}

		return fmt.Errorf("store: update watch folder %s: %w", id, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update watch folder %s: read rows affected: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: update watch folder %s: %w", id, ErrNotFound)
	}

	return nil
}

const queryDeleteWatchFolder = `DELETE FROM watch_folders WHERE id = ?`

// DeleteWatchFolder removes the row. The directory and its contents are
// never touched — the row is a pointer, not an owner. ErrNotFound means
// id addresses no row.
func (s *SettingsStore) DeleteWatchFolder(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, queryDeleteWatchFolder, id)
	if err != nil {
		return fmt.Errorf("store: delete watch folder %s: %w", id, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete watch folder %s: read rows affected: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete watch folder %s: %w", id, ErrNotFound)
	}

	return nil
}
