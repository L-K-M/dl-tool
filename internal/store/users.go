package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// User is the single operator account (docs/04-data-model.md section 3.1).
type User struct {
	ID           string `db:"id" json:"id"`
	Username     string `db:"username" json:"username"`
	PasswordHash string `db:"password_hash" json:"-"`
	Enabled      bool   `db:"enabled" json:"enabled"`
	Locale       string `db:"locale" json:"locale"`
	LastLoginAt  *int64 `db:"last_login_at" json:"-"`
	CreatedAt    int64  `db:"created_at" json:"-"`
	UpdatedAt    int64  `db:"updated_at" json:"-"`
}

// APIToken is one row of the api_tokens table (docs/04-data-model.md
// section 3.1). The bearer value itself is never stored, only its SHA-256
// hex in TokenHash; TokenHash and RevokedAt never serialize, so the struct
// can never leak them into a response.
type APIToken struct {
	ID         string `db:"id" json:"id"`
	UserID     string `db:"user_id" json:"-"`
	Name       string `db:"name" json:"name"`
	TokenHash  string `db:"token_hash" json:"-"`
	Prefix     string `db:"prefix" json:"prefix"`
	LastUsedAt *int64 `db:"last_used_at" json:"-"`
	ExpiresAt  *int64 `db:"expires_at" json:"expires_at"`
	RevokedAt  *int64 `db:"revoked_at" json:"-"`
	CreatedAt  int64  `db:"created_at" json:"created_at"`
	UpdatedAt  int64  `db:"updated_at" json:"-"`
}

// Session is a cookie-authenticated login. The cookie value itself is never
// stored, only its SHA-256 hex in TokenHash. last_seen_at is throttled
// bookkeeping, not an idle timeout; only expires_at bounds a session.
type Session struct {
	ID         string `db:"id" json:"id"`
	UserID     string `db:"user_id" json:"user_id"`
	TokenHash  string `db:"token_hash" json:"-"`
	CSRFToken  string `db:"csrf_token" json:"-"`
	ExpiresAt  int64  `db:"expires_at" json:"expires_at"`
	LastSeenAt int64  `db:"last_seen_at" json:"last_seen_at"`
}

const (
	queryCountUsers = `SELECT COUNT(*) FROM users`

	queryUserByID = `SELECT id, username, password_hash, enabled, locale, last_login_at, created_at, updated_at
FROM users
WHERE id = ?`

	queryCreateUser = `INSERT INTO users
(id, username, password_hash, enabled, locale, last_login_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

	queryUserByUsername = `SELECT id, username, password_hash, enabled, locale, last_login_at, created_at, updated_at
FROM users
WHERE username = ?`

	queryTouchLastLogin = `UPDATE users
SET last_login_at = ?, updated_at = ?
WHERE id = ?`

	queryCreateSession = `INSERT INTO sessions
(id, user_id, token_hash, csrf_token, expires_at, last_seen_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

	// The join filters the expired session and the disabled account in SQL, so
	// a rejected lookup is indistinguishable from an unknown token.
	querySessionByTokenHash = `SELECT
s.id, s.user_id, s.token_hash, s.csrf_token, s.expires_at, s.last_seen_at,
u.username, u.password_hash, u.enabled, u.locale, u.last_login_at, u.created_at, u.updated_at
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = ? AND s.expires_at > ? AND u.enabled = 1`

	queryTouchSession = `UPDATE sessions
SET last_seen_at = ?, updated_at = ?
WHERE id = ?`

	queryDeleteSession = `DELETE FROM sessions WHERE id = ?`

	queryDeleteExpiredSessions = `DELETE FROM sessions WHERE expires_at <= ?`

	queryUpdateUserProfile = `UPDATE users
SET username = ?, locale = ?, updated_at = ?
WHERE id = ?`

	queryUpdatePasswordHash = `UPDATE users
SET password_hash = ?, updated_at = ?
WHERE id = ?`

	// The empty exceptSessionID a token-authenticated caller passes matches
	// no session id, so the predicate revokes every session of the account.
	queryDeleteOtherSessions = `DELETE FROM sessions WHERE user_id = ? AND id <> ?`

	// A revoked or expired token — or a disabled account — is
	// indistinguishable from an unknown token.
	queryUserByAPITokenHash = `SELECT
t.id AS token_id,
u.id, u.username, u.password_hash, u.enabled, u.locale, u.last_login_at, u.created_at, u.updated_at
FROM api_tokens t
JOIN users u ON u.id = t.user_id
WHERE t.token_hash = ?
AND t.revoked_at IS NULL
AND (t.expires_at IS NULL OR t.expires_at > ?)
AND u.enabled = 1`

	// The stamp is rate-bounded and rechecks revoked_at, so a token revoked
	// after the lookup above is still never stamped.
	queryTouchAPIToken = `UPDATE api_tokens
SET last_used_at = ?, updated_at = ?
WHERE id = ?
AND revoked_at IS NULL
AND (last_used_at IS NULL OR last_used_at < ?)`

	// The caller owns every field of the row except updated_at, which
	// mirrors created_at on insert, so the creation response can render
	// exactly what was stored.
	queryCreateAPIToken = `INSERT INTO api_tokens
(id, user_id, name, token_hash, prefix, last_used_at, expires_at, revoked_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	// Revoked tokens stay in the table for the audit trail but are neither
	// listed nor re-revocable, so both queries filter them out.
	queryCountAPITokens = `SELECT COUNT(*) FROM api_tokens
WHERE user_id = ? AND revoked_at IS NULL`

	queryListAPITokens = `SELECT id, user_id, name, prefix, last_used_at, expires_at, created_at, updated_at
FROM api_tokens
WHERE user_id = ? AND revoked_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT ?`

	// The cursor predicate is the keyset form of the page's ORDER BY — the
	// same (created_at, id) tuple task_events uses.
	queryListAPITokensBefore = `SELECT id, user_id, name, prefix, last_used_at, expires_at, created_at, updated_at
FROM api_tokens
WHERE user_id = ? AND revoked_at IS NULL
AND (created_at < ? OR (created_at = ? AND id < ?))
ORDER BY created_at DESC, id DESC
LIMIT ?`

	// A second revoke finds no row, so a repeated DELETE answers 404 like
	// an unknown id.
	queryRevokeAPIToken = `UPDATE api_tokens
SET revoked_at = ?, updated_at = ?
WHERE id = ? AND user_id = ? AND revoked_at IS NULL`
)

// CountUsers returns the number of rows in users — 0 before first-run setup, 1 after.
func CountUsers(ctx context.Context, db *sqlx.DB) (int64, error) {
	var count int64
	if err := db.GetContext(ctx, &count, queryCountUsers); err != nil {
		return 0, fmt.Errorf("store: count users: %w", err)
	}

	return count, nil
}

// UserByID returns the user with the given id, or ErrNotFound.
func UserByID(ctx context.Context, db *sqlx.DB, id string) (User, error) {
	var user User
	err := db.GetContext(ctx, &user, queryUserByID, id)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("store: user %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("store: user %q: %w", id, err)
	}

	return user, nil
}

// CreateUser inserts the operator account; created_at and updated_at are
// set to now. last_login_at comes from the struct, NULL for first-run setup.
func CreateUser(ctx context.Context, db *sqlx.DB, u User) error {
	now := time.Now().UnixMilli()
	_, err := db.ExecContext(
		ctx,
		queryCreateUser,
		u.ID, u.Username, u.PasswordHash, u.Enabled, u.Locale, u.LastLoginAt, now, now,
	)
	if err != nil {
		return fmt.Errorf("store: create user %q: %w", u.Username, err)
	}

	return nil
}

// UserByUsername returns the user with the given username, or ErrNotFound.
// A failed lookup must be answered like a wrong password by the caller.
func UserByUsername(ctx context.Context, db *sqlx.DB, username string) (User, error) {
	var user User
	err := db.GetContext(ctx, &user, queryUserByUsername, username)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("store: user %q: %w", username, ErrNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("store: user %q: %w", username, err)
	}

	return user, nil
}

// TouchLastLogin stamps the user's last successful login.
func TouchLastLogin(ctx context.Context, db *sqlx.DB, id string, now int64) error {
	if _, err := db.ExecContext(ctx, queryTouchLastLogin, now, now, id); err != nil {
		return fmt.Errorf("store: touch last login %q: %w", id, err)
	}

	return nil
}

// CreateSession inserts a session; created_at and updated_at are set to now.
func CreateSession(ctx context.Context, db *sqlx.DB, s Session) error {
	now := time.Now().UnixMilli()
	_, err := db.ExecContext(
		ctx,
		queryCreateSession,
		s.ID, s.UserID, s.TokenHash, s.CSRFToken, s.ExpiresAt, s.LastSeenAt, now, now,
	)
	if err != nil {
		return fmt.Errorf("store: create session %q: %w", s.ID, err)
	}

	return nil
}

// SessionByTokenHash joins sessions to users and returns both. An expired
// session or a disabled user is ErrNotFound, like an unknown hash.
func SessionByTokenHash(ctx context.Context, db *sqlx.DB, hash string) (Session, User, error) {
	var row sessionUserRow
	err := db.GetContext(ctx, &row, querySessionByTokenHash, hash, time.Now().UnixMilli())
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, User{}, fmt.Errorf("store: session by token hash: %w", ErrNotFound)
	}
	if err != nil {
		return Session{}, User{}, fmt.Errorf("store: session by token hash: %w", err)
	}

	session, user := row.split()

	return session, user, nil
}

// TouchSession bumps last_seen_at; the caller throttles the call rate.
func TouchSession(ctx context.Context, db *sqlx.DB, id string, now int64) error {
	if _, err := db.ExecContext(ctx, queryTouchSession, now, now, id); err != nil {
		return fmt.Errorf("store: touch session %q: %w", id, err)
	}

	return nil
}

// DeleteSession removes a session; deleting an unknown id is not an error.
func DeleteSession(ctx context.Context, db *sqlx.DB, id string) error {
	if _, err := db.ExecContext(ctx, queryDeleteSession, id); err != nil {
		return fmt.Errorf("store: delete session %q: %w", id, err)
	}

	return nil
}

// DeleteExpiredSessions removes every session whose expiry has passed and
// returns how many.
func DeleteExpiredSessions(ctx context.Context, db *sqlx.DB, now int64) (int64, error) {
	result, err := db.ExecContext(ctx, queryDeleteExpiredSessions, now)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired sessions: %w", err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete expired sessions: %w", err)
	}

	return deleted, nil
}

// UpdateUserProfile applies the PATCH /account profile fields (doc 05
// section 12): the caller passes the resolved username and locale — the
// stored values for fields the request omitted. An unknown id is
// ErrNotFound.
func UpdateUserProfile(ctx context.Context, db *sqlx.DB, id, username, locale string) error {
	now := time.Now().UnixMilli()
	result, err := db.ExecContext(ctx, queryUpdateUserProfile, username, locale, now, id)
	if err != nil {
		return fmt.Errorf("store: update user profile %q: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update user profile %q: read rows affected: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("store: update user profile %q: %w", id, ErrNotFound)
	}

	return nil
}

// UpdatePasswordHash replaces the account's password_hash with an
// already-hashed value — the store never sees the clear text. An unknown
// id is ErrNotFound.
func UpdatePasswordHash(ctx context.Context, db *sqlx.DB, id, passwordHash string) error {
	now := time.Now().UnixMilli()
	result, err := db.ExecContext(ctx, queryUpdatePasswordHash, passwordHash, now, id)
	if err != nil {
		return fmt.Errorf("store: update password hash %q: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update password hash %q: read rows affected: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("store: update password hash %q: %w", id, ErrNotFound)
	}

	return nil
}

// DeleteOtherSessions revokes every session the user holds except
// exceptSessionID; the password-change rule of doc 05 section 12 revokes
// the attacker's stolen sessions while keeping the caller logged in.
// PATCH /account accepts token auth, where the caller holds no session:
// pass exceptSessionID == "" and every session is revoked. API tokens are
// unaffected — they are revoked individually through the token endpoints.
func DeleteOtherSessions(ctx context.Context, db *sqlx.DB, userID, exceptSessionID string) (int64, error) {
	result, err := db.ExecContext(ctx, queryDeleteOtherSessions, userID, exceptSessionID)
	if err != nil {
		return 0, fmt.Errorf("store: delete other sessions %q: %w", userID, err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete other sessions %q: read rows affected: %w", userID, err)
	}

	return deleted, nil
}

// UserByAPITokenHash resolves a bearer token hash to its user and stamps
// last_used_at, at most once per apiTokenTouchIntervalMS. A revoked or
// expired token is ErrNotFound, like an unknown hash.
func UserByAPITokenHash(ctx context.Context, db *sqlx.DB, hash string) (User, error) {
	now := time.Now().UnixMilli()

	var row tokenUserRow
	err := db.GetContext(ctx, &row, queryUserByAPITokenHash, hash, now)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("store: user by api token hash: %w", ErrNotFound)
	}
	if err != nil {
		return User{}, fmt.Errorf("store: user by api token hash: %w", err)
	}

	if err := NewUserStore(db).TouchAPIToken(ctx, row.TokenID, now); err != nil {
		return User{}, fmt.Errorf("store: stamp api token use: %w", err)
	}

	return row.user(), nil
}

// apiTokenTouchIntervalMS bounds the last_used_at write rate, mirroring the
// session touch in internal/api.
const apiTokenTouchIntervalMS = int64(time.Minute / time.Millisecond)

// sessionUserRow is the flat scan target of the sessions⋈users join.
type sessionUserRow struct {
	ID           string `db:"id"`
	UserID       string `db:"user_id"`
	TokenHash    string `db:"token_hash"`
	CSRFToken    string `db:"csrf_token"`
	ExpiresAt    int64  `db:"expires_at"`
	LastSeenAt   int64  `db:"last_seen_at"`
	Username     string `db:"username"`
	PasswordHash string `db:"password_hash"`
	Enabled      bool   `db:"enabled"`
	Locale       string `db:"locale"`
	LastLoginAt  *int64 `db:"last_login_at"`
	CreatedAt    int64  `db:"created_at"`
	UpdatedAt    int64  `db:"updated_at"`
}

func (row sessionUserRow) split() (Session, User) {
	session := Session{
		ID:         row.ID,
		UserID:     row.UserID,
		TokenHash:  row.TokenHash,
		CSRFToken:  row.CSRFToken,
		ExpiresAt:  row.ExpiresAt,
		LastSeenAt: row.LastSeenAt,
	}
	user := User{
		ID:           row.UserID,
		Username:     row.Username,
		PasswordHash: row.PasswordHash,
		Enabled:      row.Enabled,
		Locale:       row.Locale,
		LastLoginAt:  row.LastLoginAt,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}

	return session, user
}

// tokenUserRow is the flat scan target of the api_tokens⋈users join; TokenID
// is needed to stamp last_used_at on the token row.
type tokenUserRow struct {
	TokenID      string `db:"token_id"`
	ID           string `db:"id"`
	Username     string `db:"username"`
	PasswordHash string `db:"password_hash"`
	Enabled      bool   `db:"enabled"`
	Locale       string `db:"locale"`
	LastLoginAt  *int64 `db:"last_login_at"`
	CreatedAt    int64  `db:"created_at"`
	UpdatedAt    int64  `db:"updated_at"`
}

func (row tokenUserRow) user() User {
	return User{
		ID:           row.ID,
		Username:     row.Username,
		PasswordHash: row.PasswordHash,
		Enabled:      row.Enabled,
		Locale:       row.Locale,
		LastLoginAt:  row.LastLoginAt,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}
}

// UserStore owns the api_tokens table: issue, list and revoke of
// docs/05-api-contract.md section 12. The bearer resolution above stays a
// package function for its existing caller; everything else goes through
// this wrapper, like SettingsStore and TaskStore wrap theirs.
type UserStore struct{ db *sqlx.DB }

// NewUserStore wraps db. A nil db — the openapi subcommand — builds a store
// whose calls fail on use, which serving prevents.
func NewUserStore(db *sqlx.DB) *UserStore {
	return &UserStore{db: db}
}

// CreateAPIToken inserts one api_tokens row. The caller owns every field:
// t.ID (a tok_ ULID), t.TokenHash (the SHA-256 hex of the bearer value) and
// t.Prefix (its first 8 characters) included, so the clear-text value never
// reaches the store — only its hash does.
func (s *UserStore) CreateAPIToken(ctx context.Context, t APIToken) error {
	_, err := s.db.ExecContext(
		ctx,
		queryCreateAPIToken,
		t.ID, t.UserID, t.Name, t.TokenHash, t.Prefix, t.LastUsedAt, t.ExpiresAt, t.RevokedAt,
		t.CreatedAt, t.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: create api token %q: %w", t.ID, err)
	}

	return nil
}

// ListAPITokens returns one page of the account's live tokens, newest first,
// with the same cursor envelope as every other list endpoint
// (docs/05-api-contract.md section 1.4): limit defaults to 100 and is
// re-checked against the 1..500 range, total counts every live token ignoring
// the cursor, and nextCursor is empty exactly on the last page. A cursor that
// is not a page token is ErrStaleCursor; a well-formed one simply continues
// the walk — there is one account, so there is no filter to bind it to.
// Revoked rows are the audit trail only and never list.
func (s *UserStore) ListAPITokens(
	ctx context.Context,
	userID string,
	limit int,
	cursor string,
) ([]APIToken, string, int, error) {
	if limit == 0 {
		limit = taskListDefaultLimit
	}
	if limit < 1 || limit > taskListMaxLimit {
		return nil, "", 0, fmt.Errorf("store: list api tokens: limit %d outside 1..%d", limit, taskListMaxLimit)
	}

	var page apiTokenPageCursor
	if cursor != "" {
		decoded, err := decodeAPITokenCursor(cursor)
		if err != nil {
			return nil, "", 0, fmt.Errorf("store: list api tokens: %w", err)
		}
		page = decoded
	}

	var total int
	if err := s.db.GetContext(ctx, &total, queryCountAPITokens, userID); err != nil {
		return nil, "", 0, fmt.Errorf("store: list api tokens: count: %w", err)
	}

	// One row past the limit decides whether another page exists, so
	// nextCursor is empty exactly on the last page.
	var tokens []APIToken
	var err error
	if cursor == "" {
		err = s.db.SelectContext(ctx, &tokens, queryListAPITokens, userID, limit+1)
	} else {
		err = s.db.SelectContext(
			ctx,
			&tokens,
			queryListAPITokensBefore,
			userID, page.At, page.At, page.ID, limit+1,
		)
	}
	if err != nil {
		return nil, "", 0, fmt.Errorf("store: list api tokens: read page: %w", err)
	}

	if len(tokens) <= limit {
		return tokens, "", total, nil
	}
	tokens = tokens[:limit]

	last := tokens[len(tokens)-1]
	nextCursor, err := encodeAPITokenCursor(apiTokenPageCursor{At: last.CreatedAt, ID: last.ID})
	if err != nil {
		return nil, "", 0, fmt.Errorf("store: list api tokens: encode cursor: %w", err)
	}

	return tokens, nextCursor, total, nil
}

// RevokeAPIToken sets revoked_at on the token owned by userID. The row is
// kept — it is the audit trail — but it no longer lists or authenticates.
// An unknown, foreign or already-revoked id is ErrNotFound.
func (s *UserStore) RevokeAPIToken(ctx context.Context, userID, id string) error {
	now := time.Now().UnixMilli()
	result, err := s.db.ExecContext(ctx, queryRevokeAPIToken, now, now, id, userID)
	if err != nil {
		return fmt.Errorf("store: revoke api token %q: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: revoke api token %q: read rows affected: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("store: revoke api token %q: %w", id, ErrNotFound)
	}

	return nil
}

// TouchAPIToken stamps last_used_at, at most once per
// apiTokenTouchIntervalMS: the update is a no-op while the stored stamp is
// fresher, so a chatty bearer client never turns into a write per request.
// The revoked_at recheck keeps a token revoked after its lookup unstamped.
func (s *UserStore) TouchAPIToken(ctx context.Context, id string, at int64) error {
	_, err := s.db.ExecContext(ctx, queryTouchAPIToken, at, at, id, at-apiTokenTouchIntervalMS)
	if err != nil {
		return fmt.Errorf("store: touch api token %q: %w", id, err)
	}

	return nil
}

// apiTokenPageCursor is the decoded token page token: the (created_at, id) of
// the last row of the page that issued it — the same keyset codec
// task_events uses.
type apiTokenPageCursor struct {
	At int64  `json:"a"`
	ID string `json:"i"`
}

// encodeAPITokenCursor renders a page token as base64 JSON — the same codec
// every other list endpoint uses (docs/05-api-contract.md section 1.4).
func encodeAPITokenCursor(c apiTokenPageCursor) (string, error) {
	encoded, err := json.Marshal(c)
	if err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

// decodeAPITokenCursor parses a page token. A token that is not base64 JSON
// of the cursor shape is ErrStaleCursor: it belongs to no page, and the wire
// outcome is the same 422 as any other stale cursor.
func decodeAPITokenCursor(token string) (apiTokenPageCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return apiTokenPageCursor{}, fmt.Errorf("%w: token is not valid base64", ErrStaleCursor)
	}

	var cursor apiTokenPageCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		return apiTokenPageCursor{}, fmt.Errorf("%w: token is not a page cursor", ErrStaleCursor)
	}
	if cursor.ID == "" || cursor.At <= 0 {
		return apiTokenPageCursor{}, fmt.Errorf("%w: token carries no row", ErrStaleCursor)
	}

	return cursor, nil
}
