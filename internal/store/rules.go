package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// Rule is one row of rules (docs/04-data-model.md section 3.5):
// DefinitionJSON is the validated rule document of docs/08-rss-automation.md
// section 4 rendered compact, and name, enabled and priority are its
// mirrored columns — the evaluation order reads them without parsing the
// document.
type Rule struct {
	ID             string `db:"id"              json:"id"`
	Name           string `db:"name"            json:"name"`
	Enabled        bool   `db:"enabled"         json:"enabled"`
	Priority       int    `db:"priority"        json:"priority"`
	DefinitionJSON string `db:"definition_json" json:"-"`
	LastMatchAt    *int64 `db:"last_match_at"   json:"-"`
	CreatedAt      int64  `db:"created_at"      json:"-"`
	UpdatedAt      int64  `db:"updated_at"      json:"-"`
}

// ruleColumns is the explicit column list every rules SELECT shares, so a
// later migration cannot silently widen a StructScan target
// (docs/14-conventions.md section 2.4).
const ruleColumns = `id, name, enabled, priority, definition_json,
last_match_at, created_at, updated_at`

// ListRules returns every rule ordered by (priority ASC, name ASC) — the
// evaluation order of docs/08-rss-automation.md section 5. enabledOnly
// selects the enabled rows, the poller's working set.
func ListRules(ctx context.Context, db *sqlx.DB, enabledOnly bool) ([]Rule, error) {
	query := `SELECT ` + ruleColumns + ` FROM rules`
	if enabledOnly {
		query += ` WHERE enabled = 1`
	}
	query += ` ORDER BY priority, name`

	var rules []Rule
	if err := db.SelectContext(ctx, &rules, query); err != nil {
		return nil, fmt.Errorf("store: list rules: %w", err)
	}

	return rules, nil
}

// RuleByID resolves one rule by id. ErrNotFound means the id addresses no
// row.
func RuleByID(ctx context.Context, db *sqlx.DB, id string) (Rule, error) {
	var rule Rule
	err := db.GetContext(ctx, &rule, `SELECT `+ruleColumns+` FROM rules WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Rule{}, fmt.Errorf("store: rule %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Rule{}, fmt.Errorf("store: rule %s: %w", id, err)
	}

	return rule, nil
}

// RuleByName resolves one rule by its unique name, ErrNotFound when none
// carries it. The `auto:<feed_id>` lifecycle resolves by name, never by a
// stored back-reference (docs/05-api-contract.md section 10.1).
func RuleByName(ctx context.Context, db *sqlx.DB, name string) (Rule, error) {
	var rule Rule
	err := db.GetContext(ctx, &rule, `SELECT `+ruleColumns+` FROM rules WHERE name = ?`, name)
	if errors.Is(err, sql.ErrNoRows) {
		return Rule{}, fmt.Errorf("store: rule %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return Rule{}, fmt.Errorf("store: rule %q: %w", name, err)
	}

	return rule, nil
}

// queryCreateRule writes every column but last_match_at, which starts NULL
// and only SetRuleLastMatchAt advances.
const queryCreateRule = `INSERT INTO rules (
id, name, enabled, priority, definition_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`

// CreateRule inserts one row. The caller owns r.ID (a rul_ ULID) and the
// mirrored columns name, enabled, priority and DefinitionJSON; created_at
// and updated_at stamp here. A duplicate name is ErrConflict — the API maps
// it to 409 /problems/conflict.
func CreateRule(ctx context.Context, db *sqlx.DB, r Rule) error {
	now := time.Now().UnixMilli()
	if _, err := db.ExecContext(ctx, queryCreateRule,
		r.ID, r.Name, r.Enabled, r.Priority, r.DefinitionJSON, now, now,
	); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: create rule %s: %w", r.ID, ErrConflict)
		}

		return fmt.Errorf("store: create rule %s: %w", r.ID, err)
	}

	return nil
}

// queryUpdateRule mirrors queryCreateRule: the mirrored columns and the
// document move, last_match_at is SetRuleLastMatchAt's alone.
const queryUpdateRule = `UPDATE rules
SET name = ?, enabled = ?, priority = ?, definition_json = ?, updated_at = ?
WHERE id = ?`

// UpdateRule writes the mirrored columns and definition_json of an existing
// row and leaves last_match_at untouched. ErrNotFound means id addresses no
// row; ErrConflict means name belongs to another row.
func UpdateRule(ctx context.Context, db *sqlx.DB, r Rule) error {
	result, err := db.ExecContext(ctx, queryUpdateRule,
		r.Name, r.Enabled, r.Priority, r.DefinitionJSON,
		time.Now().UnixMilli(), r.ID,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("store: update rule %s: %w", r.ID, ErrConflict)
		}

		return fmt.Errorf("store: update rule %s: %w", r.ID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update rule %s: read rows affected: %w", r.ID, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: update rule %s: %w", r.ID, ErrNotFound)
	}

	return nil
}

// DeleteRule removes the row. ErrNotFound means id addresses no row.
func DeleteRule(ctx context.Context, db *sqlx.DB, id string) error {
	result, err := db.ExecContext(ctx, `DELETE FROM rules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete rule %s: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete rule %s: read rows affected: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete rule %s: %w", id, ErrNotFound)
	}

	return nil
}

// SetRuleLastMatchAt advances the dedup watermark of docs/08-rss-automation.md
// section 5 step 14: the newest published_at the rule committed a grab for.
// The write is monotonic — MAX keeps the larger of the stored value and at,
// so a late or retried call cannot re-open the consumed window and re-grab
// items the rule already committed — and updated_at moves only when the
// watermark does, so a non-advancing write leaves no phantom modification.
// ErrNotFound means id addresses no row.
func SetRuleLastMatchAt(ctx context.Context, db *sqlx.DB, id string, at int64) error {
	result, err := db.ExecContext(ctx,
		`UPDATE rules
		SET updated_at = CASE WHEN last_match_at IS NULL OR last_match_at < ? THEN ? ELSE updated_at END,
			last_match_at = MAX(IFNULL(last_match_at, ?), ?)
		WHERE id = ?`,
		at, time.Now().UnixMilli(), at, at, id,
	)
	if err != nil {
		return fmt.Errorf("store: set last_match_at of rule %s: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set last_match_at of rule %s: read rows affected: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("store: set last_match_at of rule %s: %w", id, ErrNotFound)
	}

	return nil
}
