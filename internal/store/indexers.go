package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/hkdf"

	"github.com/L-K-M/dl-tool/internal/secure"
)

// indexerKeyInfo is the HKDF info string that separates the indexer API-key
// seal from every other use of the session key (docs/12 §: at-rest sealing).
const indexerKeyInfo = "dl-tool/indexer-api-key/v1"

// indexerKeySize is the AES-256 key length derived for the seal.
const indexerKeySize = 32

// DefaultIndexerPriority is the documented create-time default. The API
// layer applies it explicitly so an intentional 0 still round-trips verbatim.
const DefaultIndexerPriority = 50

// Indexer mirrors the indexers DDL of docs/04-data-model.md section 3.4.
// APIKeyEnc never leaves this package in clear; readers call OpenAPIKey.
// The allow_private_network column deliberately has no field: the flag
// lives in the settings_json document (task T055).
type Indexer struct {
	ID               string  `db:"id"                json:"id"`
	Name             string  `db:"name"              json:"name"`
	Kind             string  `db:"kind"              json:"kind"` // torznab | newznab | dlsearch
	Enabled          bool    `db:"enabled"           json:"enabled"`
	URL              *string `db:"url"               json:"url"`
	APIKeyEnc        *string `db:"api_key_enc"       json:"-"`
	DefinitionID     *string `db:"definition_id"     json:"definition_id"`
	DefinitionSource *string `db:"definition_source" json:"definition_source"` // bundled | user | imported
	Provenance       *string `db:"provenance"        json:"provenance"`
	LegalTier        string  `db:"legal_tier"        json:"legal_tier"`
	Priority         int     `db:"priority"          json:"priority"`
	SeedersUnknown   bool    `db:"seeders_unknown"   json:"seeders_unknown"`
	SettingsJSON     *string `db:"settings_json"     json:"-"`
	CategoriesJSON   *string `db:"categories_json"   json:"-"`
	LastTestAt       *int64  `db:"last_test_at"      json:"-"`
	LastError        *string `db:"last_error"        json:"last_error"`
	CreatedAt        int64   `db:"created_at"        json:"-"`
	UpdatedAt        int64   `db:"updated_at"        json:"-"`
}

// IndexerPatch carries only the fields docs/05-api-contract.md section 9.1
// accepts; a nil field is unchanged. APIKey set to a pointer to "" clears the
// stored key.
type IndexerPatch struct {
	Name, URL, DefinitionID *string
	Kind, Provenance        *string
	Enabled                 *bool
	Priority                *int
	APIKey                  *secure.Secret
	SettingsJSON            *string
}

// IndexerStore seals api_key_enc with AES-256-GCM under a 32-byte key derived
// by HKDF-SHA256 from cfg.SecretKey with the info string
// "dl-tool/indexer-api-key/v1". No new environment variable and no new secret
// carrier is introduced.
type IndexerStore struct {
	db   *sqlx.DB
	aead cipher.AEAD
}

// NewIndexerStore derives the sealing key from sessionKey (the at-rest
// secret key of docs/11-config-reference.md) and returns the store. An empty
// key is an error: sealing under an empty secret would silently reduce to a
// fixed key.
func NewIndexerStore(db *sqlx.DB, sessionKey secure.Secret) (*IndexerStore, error) {
	if sessionKey.Reveal() == "" {
		return nil, errors.New("store: indexer store requires a non-empty secret key")
	}

	key := make([]byte, indexerKeySize)
	if _, err := io.ReadFull(
		hkdf.New(sha256.New, []byte(sessionKey.Reveal()), nil, []byte(indexerKeyInfo)),
		key,
	); err != nil {
		return nil, fmt.Errorf("store: derive indexer key: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("store: build indexer cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("store: build indexer aead: %w", err)
	}

	return &IndexerStore{db: db, aead: aead}, nil
}

// indexerColumns is the explicit column list every indexers SELECT shares,
// so a later migration cannot silently widen a StructScan target
// (docs/14-conventions.md section 2.4).
const indexerColumns = `id, name, kind, enabled, url, api_key_enc, definition_id,
definition_source, provenance, legal_tier, priority, seeders_unknown,
settings_json, categories_json, last_test_at, last_error, created_at, updated_at`

const queryCreateIndexer = `INSERT INTO indexers
(id, name, kind, enabled, url, api_key_enc, definition_id, definition_source,
 provenance, legal_tier, priority, seeders_unknown, settings_json,
 categories_json, last_test_at, last_error, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// Create inserts one row with a fresh idx_ id and the sealed api_key; the
// stored row is returned. A duplicate definition_id is ErrConflict — the API
// maps it to 409 /problems/conflict.
func (s *IndexerStore) Create(ctx context.Context, in Indexer, apiKey secure.Secret) (Indexer, error) {
	enc, err := s.seal(apiKey)
	if err != nil {
		return Indexer{}, err
	}

	in.ID = NewID(PrefixIndexer)
	in.APIKeyEnc = enc
	if in.LegalTier == "" {
		// The column CHECK admits only legitimate|user-supplied; the API
		// cannot create a legitimate row, so an unset tier means the DDL
		// default made explicit.
		in.LegalTier = "user-supplied"
	}
	// Priority is stored verbatim: the API layer applies the documented
	// default of 50 at create time, so an explicit 0 round-trips the same
	// way PATCH stores it.
	now := time.Now().UnixMilli()
	in.CreatedAt, in.UpdatedAt = now, now

	if _, err := s.db.ExecContext(
		ctx, queryCreateIndexer,
		in.ID, in.Name, in.Kind, in.Enabled, in.URL, in.APIKeyEnc,
		in.DefinitionID, in.DefinitionSource, in.Provenance, in.LegalTier,
		in.Priority, in.SeedersUnknown, in.SettingsJSON, in.CategoriesJSON,
		in.LastTestAt, in.LastError, in.CreatedAt, in.UpdatedAt,
	); err != nil {
		if isUniqueViolation(err) {
			return Indexer{}, fmt.Errorf("store: create indexer %s: %w", in.Name, ErrConflict)
		}

		return Indexer{}, fmt.Errorf("store: create indexer %s: %w", in.Name, err)
	}

	return in, nil
}

const queryIndexerByID = `SELECT ` + indexerColumns + `
FROM indexers WHERE id = ?`

// Get resolves one row by id. ErrNotFound means the id addresses no row.
func (s *IndexerStore) Get(ctx context.Context, id string) (Indexer, error) {
	var row Indexer
	err := s.db.GetContext(ctx, &row, queryIndexerByID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Indexer{}, fmt.Errorf("store: indexer %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Indexer{}, fmt.Errorf("store: indexer %s: %w", id, err)
	}

	return row, nil
}

// List returns every row ordered by priority then name; enabledOnly narrows
// the set to enabled = 1.
func (s *IndexerStore) List(ctx context.Context, enabledOnly bool) ([]Indexer, error) {
	query := `SELECT ` + indexerColumns + ` FROM indexers`
	if enabledOnly {
		query += ` WHERE enabled = 1`
	}
	query += ` ORDER BY priority, name`

	var rows []Indexer
	if err := s.db.SelectContext(ctx, &rows, query); err != nil {
		return nil, fmt.Errorf("store: list indexers: %w", err)
	}

	return rows, nil
}

// Update writes the non-nil patch fields onto the addressed row and always
// stamps updated_at, then reads the row back. A nil patch still moves
// updated_at, matching the patch contract. ErrNotFound means id addresses no
// row; ErrConflict means definition_id belongs to another row.
func (s *IndexerStore) Update(ctx context.Context, id string, patch IndexerPatch) (Indexer, error) {
	sets := []string{}
	args := []any{}
	add := func(col string, val any) {
		sets = append(sets, col+" = ?")
		args = append(args, val)
	}
	if patch.Name != nil {
		add("name", *patch.Name)
	}
	if patch.URL != nil {
		add("url", *patch.URL)
	}
	if patch.DefinitionID != nil {
		add("definition_id", *patch.DefinitionID)
	}
	if patch.Kind != nil {
		add("kind", *patch.Kind)
	}
	if patch.Provenance != nil {
		add("provenance", *patch.Provenance)
	}
	if patch.Enabled != nil {
		add("enabled", *patch.Enabled)
	}
	if patch.Priority != nil {
		add("priority", *patch.Priority)
	}
	if patch.SettingsJSON != nil {
		add("settings_json", *patch.SettingsJSON)
	}
	if patch.APIKey != nil {
		if patch.APIKey.Reveal() == "" {
			// A pointer to "" clears the stored key (interface contract).
			sets = append(sets, "api_key_enc = NULL")
		} else {
			enc, err := s.seal(*patch.APIKey)
			if err != nil {
				return Indexer{}, err
			}
			add("api_key_enc", *enc)
		}
	}
	add("updated_at", time.Now().UnixMilli())
	args = append(args, id)

	result, err := s.db.ExecContext(
		ctx, `UPDATE indexers SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return Indexer{}, fmt.Errorf("store: update indexer %s: %w", id, ErrConflict)
		}

		return Indexer{}, fmt.Errorf("store: update indexer %s: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Indexer{}, fmt.Errorf("store: update indexer %s: read rows affected: %w", id, err)
	}
	if rows == 0 {
		return Indexer{}, fmt.Errorf("store: update indexer %s: %w", id, ErrNotFound)
	}

	return s.Get(ctx, id)
}

// Delete removes the row. ErrNotFound means id addresses no row.
func (s *IndexerStore) Delete(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM indexers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete indexer %s: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete indexer %s: read rows affected: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete indexer %s: %w", id, ErrNotFound)
	}

	return nil
}

// SetCaps stores the flattened categories document of a fetched caps and the
// indexer's seeders_unknown flag. ErrNotFound means id addresses no row.
func (s *IndexerStore) SetCaps(ctx context.Context, id string, categoriesJSON string, seedersUnknown bool) error {
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE indexers SET categories_json = ?, seeders_unknown = ?, updated_at = ? WHERE id = ?`,
		categoriesJSON, seedersUnknown, time.Now().UnixMilli(), id,
	)
	if err != nil {
		return fmt.Errorf("store: set indexer caps %s: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set indexer caps %s: read rows affected: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: set indexer caps %s: %w", id, ErrNotFound)
	}

	return nil
}

// RecordTest stamps the outcome of one probe: last_test_at always moves,
// last_error carries the failure or is cleared by a nil lastErr.
// ErrNotFound means id addresses no row.
func (s *IndexerStore) RecordTest(ctx context.Context, id string, at int64, lastErr *string) error {
	result, err := s.db.ExecContext(
		ctx,
		`UPDATE indexers SET last_test_at = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		at, lastErr, time.Now().UnixMilli(), id,
	)
	if err != nil {
		return fmt.Errorf("store: record indexer test %s: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: record indexer test %s: read rows affected: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: record indexer test %s: %w", id, ErrNotFound)
	}

	return nil
}

// OpenAPIKey unseals the row's stored key. A row with no key returns the
// empty Secret. A ciphertext that does not open is corrupt or sealed under a
// different key; either way it is an error, never a silently wrong key.
func (s *IndexerStore) OpenAPIKey(row Indexer) (secure.Secret, error) {
	if row.APIKeyEnc == nil || *row.APIKeyEnc == "" {
		return "", nil
	}

	raw, err := base64.StdEncoding.DecodeString(*row.APIKeyEnc)
	if err != nil {
		return "", fmt.Errorf("store: decode indexer key %s: %w", row.ID, err)
	}
	if len(raw) < s.aead.NonceSize() {
		return "", fmt.Errorf("store: indexer key %s: ciphertext shorter than nonce", row.ID)
	}
	plain, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("store: open indexer key %s: %w", row.ID, err)
	}

	return secure.Secret(plain), nil
}

// seal encrypts one API key: base64(nonce || ciphertext). An empty key seals
// to nil — the column stays NULL so api_key_set reports false.
func (s *IndexerStore) seal(key secure.Secret) (*string, error) {
	if key.Reveal() == "" {
		return nil, nil
	}

	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("store: seal indexer key: nonce: %w", err)
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(key.Reveal()), nil)
	enc := base64.StdEncoding.EncodeToString(sealed)

	return &enc, nil
}
