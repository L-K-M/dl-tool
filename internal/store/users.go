package store

import (
	"context"
	"database/sql"
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

// Session is a cookie-authenticated login. The cookie value itself is never
// stored, only its SHA-256 hex in TokenHash.
type Session struct {
	ID         string `db:"id"`
	UserID     string `db:"user_id"`
	TokenHash  string `db:"token_hash"`
	CSRFToken  string `db:"csrf_token"`
	ExpiresAt  int64  `db:"expires_at"`
	LastSeenAt int64  `db:"last_seen_at"`
}

const (
	queryCountUsers = `SELECT COUNT(*) FROM users`

	queryUserByID = `SELECT id, username, password_hash, enabled, locale, last_login_at, created_at, updated_at
FROM users
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

	queryTouchAPIToken = `UPDATE api_tokens
SET last_used_at = ?, updated_at = ?
WHERE id = ?`
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

// UserByAPITokenHash resolves a bearer token hash to its user and stamps
// last_used_at. A revoked or expired token is ErrNotFound, like an unknown hash.
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

	if _, err := db.ExecContext(ctx, queryTouchAPIToken, now, now, row.TokenID); err != nil {
		return User{}, fmt.Errorf("store: stamp api token use: %w", err)
	}

	return row.user(), nil
}

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
