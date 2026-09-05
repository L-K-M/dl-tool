package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
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
