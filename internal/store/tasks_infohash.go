// BitTorrent identity: the two infohash columns of the tasks table and the
// duplicate rule FR-023 keys on. A torrent's identity is the pair
// (infohash_v1, infohash_v2) — a hybrid torrent carries both, and the same
// torrent submitted once by its v1 magnet and once by its v2 magnet must
// resolve to one row — so every lookup here queries the two columns
// together and never one at a time, and never engine_ref: the engine
// handle is the daemon's own id, not the torrent's identity
// (docs/06-download-engines.md section 3.5).

package store

import (
	"context"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"modernc.org/sqlite"
)

// ErrDuplicateInfohash is returned when either hash already belongs to a
// task whose state is not 'removed'. The API maps it to a rejected[] entry
// of type /problems/conflict (the slug registry of doc 05 section 1.3 is
// closed) and the metadata write-back maps it to the torrent_duplicate
// pause of a task that already exists.
var ErrDuplicateInfohash = errors.New("store: torrent already present")

// The infohash shapes of BEP 9 and BEP 52, in characters. The store cannot
// reach internal/uri's spellings of the same spec constants — it is a leaf
// that imports nothing internal — so the lengths are pinned here beside
// their only consumers.
const (
	infohashV1HexChars    = 40 // hex of the 20-byte SHA-1 of a v1 info dict
	infohashV1Base32Chars = 32 // base32 of the same 20 bytes
	infohashV2HexChars    = 64 // hex of the 32-byte SHA-256 of a v2 info dict
	// btmhSHA256Tag is the multihash tag of an urn:btmh exact topic:
	// 0x12 = sha2-256, 0x20 = 32-byte length, followed by 64 hex digits.
	btmhSHA256Tag = "1220"
	// infohashV1Bytes is the byte length the base32 form must decode to.
	infohashV1Bytes = 20
)

// errorCodeTorrentDuplicate is the tasks.error_code value of a task whose
// resolved identity collided with another live task — the storage literal
// of the vocabulary row in docs/04-data-model.md section 4.2.
const errorCodeTorrentDuplicate = "torrent_duplicate"

// CodeTaskDuplicatePaused is the task_events code of the pause that lands
// when a duplicate is discovered after metadata resolved: the transfer is
// paused in place and nothing is deleted (FR-023).
const CodeTaskDuplicatePaused = "task.duplicate_paused"

// messageTaskDuplicatePaused is the event message that pairs with
// CodeTaskDuplicatePaused.
const messageTaskDuplicatePaused = "metadata resolved to a torrent another task already holds; paused without deleting anything"

// duplicatePauseFromStates is the set a torrent_duplicate pause may land
// from: every non-terminal state except paused itself — a paused row cannot
// move to paused (the state machine forbids self-loops), and a magnet whose
// task is paused is not fetching metadata either.
var duplicatePauseFromStates = []string{
	"queued", "downloading", "checking", "seeding", "extracting", "moving", "error",
}

const (
	// The duplicate lookup of FR-023: one query over both columns, an empty
	// argument guarded to never match. The guard is the point — a v1-only
	// caller passes v2 = "" and that must not join against the v2 column —
	// so the pair is bound twice, once for the comparison and once for its
	// own non-emptiness. state <> 'removed' keeps a tombstoned task from
	// holding its hashes hostage: its row stays for history, its identity
	// is free again. engine_ref is deliberately absent: identity is the
	// infohash pair, never the engine handle.
	queryFindTaskByInfohash = `SELECT id, engine, engine_ref, source_kind, source_uri, name, infohash_v1, infohash_v2,
 state, error_code, error_message, destination, content_path, category_id, total_bytes, completed_bytes,
 uploaded_bytes, download_rate, upload_rate, eta_seconds, sequential, queue_position,
 added_at, started_at, completed_at, created_at, updated_at
FROM tasks
WHERE ((infohash_v1 = ? AND ? <> '') OR (infohash_v2 = ? AND ? <> '')) AND state <> 'removed'
LIMIT 1`

	// The write-back target of ResolveInfohashes: the daemon's own hash is
	// the value tasks.engine_ref stores verbatim, so the lookup keys on
	// exactly that pair.
	queryTaskIDByEngineRef = `SELECT id FROM tasks
WHERE engine = ? AND engine_ref = ? AND state <> 'removed'
LIMIT 1`

	queryTaskInfohashes = `SELECT infohash_v1, infohash_v2 FROM tasks WHERE id = ?`

	// An empty argument means "unknown, keep the stored value" — never a
	// wipe — so the write is two whole columns every time.
	querySetTaskInfohashes = `UPDATE tasks
SET infohash_v1 = ?, infohash_v2 = ?, updated_at = ?
WHERE id = ?`
)

// NormaliseInfohash lowercases and validates one hash. It accepts 40 hex
// (v1), 32 base32 characters (v1, decoded to 40 hex), 64 hex (v2) and the
// 68-character multihash form 1220<64 hex> (v2, stripped to its 64 hex
// digits). It returns "" and no error for an empty input, so a caller
// normalising an absent v2 of a hybrid torrent needs no branch of its own.
func NormaliseInfohash(s string) (string, error) {
	s = strings.TrimSpace(s)

	switch len(s) {
	case 0:
		return "", nil
	case infohashV1HexChars, infohashV2HexChars:
		if !isHex(s) {
			return "", fmt.Errorf("store: infohash %q is not hex", s)
		}

		return strings.ToLower(s), nil
	case infohashV1Base32Chars:
		raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(s))
		if err != nil {
			return "", fmt.Errorf("store: infohash %q is not base32: %w", s, err)
		}
		if len(raw) != infohashV1Bytes {
			return "", fmt.Errorf("store: infohash %q decodes to %d bytes, want %d", s, len(raw), infohashV1Bytes)
		}

		return hex.EncodeToString(raw), nil
	case len(btmhSHA256Tag) + infohashV2HexChars:
		if !strings.HasPrefix(strings.ToLower(s), btmhSHA256Tag) {
			return "", fmt.Errorf("store: infohash %q is not a sha2-256 multihash", s)
		}
		digest := s[len(btmhSHA256Tag):]
		if !isHex(digest) {
			return "", fmt.Errorf("store: infohash %q carries no hex digest", s)
		}

		return strings.ToLower(digest), nil
	default:
		return "", fmt.Errorf(
			"store: infohash has %d characters, want %d hex or %d base32 (v1), %d hex or %d multihash (v2)",
			len(s), infohashV1HexChars, infohashV1Base32Chars, infohashV2HexChars, len(btmhSHA256Tag)+infohashV2HexChars,
		)
	}
}

// isHex reports whether every character of s is a hex digit, either case.
func isHex(s string) bool {
	for _, c := range s {
		isDigit := '0' <= c && c <= '9'
		isLowerHex := 'a' <= c && c <= 'f'
		isUpperHex := 'A' <= c && c <= 'F'
		if !isDigit && !isLowerHex && !isUpperHex {
			return false
		}
	}

	return true
}

// FindByInfohash returns the live task matching either hash. Empty
// arguments never match. A hybrid torrent submitted by its v1 magnet and
// later by its v2 magnet resolves to the same row, which is why the two
// columns are always queried together and never one at a time. It returns
// ErrNotFound when no live task carries either hash.
func (s *TaskStore) FindByInfohash(ctx context.Context, v1, v2 string) (Task, error) {
	v1n, err := NormaliseInfohash(v1)
	if err != nil {
		return Task{}, fmt.Errorf("store: find by infohash: %w", err)
	}
	v2n, err := NormaliseInfohash(v2)
	if err != nil {
		return Task{}, fmt.Errorf("store: find by infohash: %w", err)
	}

	var task Task
	err = s.db.GetContext(ctx, &task, queryFindTaskByInfohash, v1n, v1n, v2n, v2n)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("store: find by infohash %q/%q: %w", v1n, v2n, ErrNotFound)
	}
	if err != nil {
		return Task{}, fmt.Errorf("store: find by infohash %q/%q: %w", v1n, v2n, err)
	}

	return task, nil
}

// SetInfohashes fills both columns once metadata resolves. It is idempotent
// — a repeated call with the values the row already stores writes nothing,
// not even updated_at — refuses to overwrite a non-empty column with a
// different value, and returns ErrDuplicateInfohash when the resolved hash
// collides with another live task. An empty argument means "unknown", never
// "clear": the stored column survives it, and an empty string is stored as
// NULL because the partial unique indices cover the non-null rows only.
func (s *TaskStore) SetInfohashes(ctx context.Context, taskID, v1, v2 string) error {
	v1n, err := NormaliseInfohash(v1)
	if err != nil {
		return fmt.Errorf("store: set infohashes of task %q: %w", taskID, err)
	}
	v2n, err := NormaliseInfohash(v2)
	if err != nil {
		return fmt.Errorf("store: set infohashes of task %q: %w", taskID, err)
	}

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: set infohashes of task %q: %w", taskID, err)
	}
	// Rolls back on any early return; after Commit this is sql.ErrTxDone,
	// which is the expected outcome and not worth a warning.
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			slog.WarnContext(ctx, "store: rollback of infohash write failed", "task_id", taskID, "error", err)
		}
	}()

	// The current pair read inside the transaction decides the idempotent
	// no-op and the refused overwrite, and separates a missing id from a
	// declined write.
	var current struct {
		InfohashV1 *string `db:"infohash_v1"`
		InfohashV2 *string `db:"infohash_v2"`
	}
	err = tx.GetContext(ctx, &current, queryTaskInfohashes, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: set infohashes of task %q: %w", taskID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("store: set infohashes of task %q: read pair: %w", taskID, err)
	}

	// An empty argument keeps the stored value; only a non-empty argument
	// writes its column, and a different value for a column that already
	// holds one is refused — a torrent's identity does not change.
	nextV1, nextV2 := textOrEmpty(current.InfohashV1), textOrEmpty(current.InfohashV2)
	if v1n != "" {
		if nextV1 != "" && nextV1 != v1n {
			return fmt.Errorf("store: set infohashes of task %q: infohash_v1 already holds %q, refusing %q", taskID, nextV1, v1n)
		}
		nextV1 = v1n
	}
	if v2n != "" {
		if nextV2 != "" && nextV2 != v2n {
			return fmt.Errorf("store: set infohashes of task %q: infohash_v2 already holds %q, refusing %q", taskID, nextV2, v2n)
		}
		nextV2 = v2n
	}

	if nextV1 == textOrEmpty(current.InfohashV1) && nextV2 == textOrEmpty(current.InfohashV2) {
		// The row already stores exactly this pair: a reconciliation loop
		// re-learning the same resolution writes nothing.
		return nil
	}

	// The collision re-check inside the same transaction, excluding the
	// row being written: that row matches the pair itself whenever one of
	// its columns is unchanged, and a LIMIT 1 scan may return it in place
	// of the real holder — a hybrid resolving its v2 onto another task's
	// v2 must not read as its own reflection. The partial unique indices
	// would still catch the duplicate at the UPDATE, but the caller is
	// owed ErrDuplicateInfohash — and the pair, not the row.
	duplicateID, collides, err := findOtherTaskByInfohash(ctx, tx, taskID, nextV1, nextV2)
	if err != nil {
		return fmt.Errorf("store: set infohashes of task %q: %w", taskID, err)
	}
	if collides {
		return fmt.Errorf("store: set infohashes of task %q: task %q already holds %q/%q: %w",
			taskID, duplicateID, nextV1, nextV2, ErrDuplicateInfohash)
	}

	if _, err := tx.ExecContext(
		ctx, querySetTaskInfohashes,
		nullableText(nextV1), nullableText(nextV2), time.Now().UnixMilli(), taskID,
	); err != nil {
		// A concurrent resolution can commit the same hash between the
		// re-check above and this UPDATE; the partial unique index then
		// refuses the write, and the refusal is the same collision the
		// re-check reports — the caller is owed the sentinel, not a driver
		// dump.
		if isUniqueViolation(err) {
			return fmt.Errorf("store: set infohashes of task %q: another live task already holds %q/%q: %w",
				taskID, nextV1, nextV2, ErrDuplicateInfohash)
		}

		return fmt.Errorf("store: set infohashes of task %q: %w", taskID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: set infohashes of task %q: commit: %w", taskID, err)
	}

	return nil
}

// queryOtherTaskByInfohash is the SetInfohashes re-check: the duplicate
// lookup of queryFindTaskByInfohash with the row being written excluded.
const queryOtherTaskByInfohash = `SELECT id FROM tasks
WHERE ((infohash_v1 = ? AND ? <> '') OR (infohash_v2 = ? AND ? <> '')) AND state <> 'removed' AND id <> ?
LIMIT 1`

// findOtherTaskByInfohash is queryOtherTaskByInfohash over the open
// transaction the SetInfohashes re-check runs inside, excluding the task
// being written: an unchanged hash makes that row match the pair too, and
// LIMIT 1 could return it in place of the real holder. found is false
// when no other live task carries the pair, which is exactly what the
// caller tests.
func findOtherTaskByInfohash(ctx context.Context, tx *sqlx.Tx, excludeID, v1, v2 string) (string, bool, error) {
	var id string
	err := tx.GetContext(ctx, &id, queryOtherTaskByInfohash, v1, v1, v2, v2, excludeID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}

	return id, true, nil
}

// sqliteConstraintUnique is modernc's SQLITE_CONSTRAINT_UNIQUE — the
// result code the partial unique indices on the infohash columns raise.
const sqliteConstraintUnique = 2067

// isUniqueViolation reports whether err is the driver's refusal of a
// unique index, whatever wrapper carried it up.
func isUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error

	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqliteConstraintUnique
}

// ResolveInfohashes lands one engine-reported metadata resolution: it maps
// the engine handle to the live task it belongs to, writes both hashes
// through SetInfohashes, and pauses the task when the resolved identity
// collides with another live task — error_code torrent_duplicate, one
// task_events row, nothing deleted (FR-023's late collision). duplicate
// reports that this landing paused a task, so the caller stops the
// engine-side transfer too. A handle no live task carries is a no-op,
// not an error: the delta path reports every owned torrent, and ownership
// lapses for exactly the rows that moved on. It is the single method the
// qBittorrent delta path needs, so the adapter stays free of any store
// type (the layering of docs/03-architecture.md section 5.2 keeps
// adapters off the store).
func (s *TaskStore) ResolveInfohashes(ctx context.Context, engineName, ref, v1, v2 string) (bool, error) {
	v1n, err := NormaliseInfohash(v1)
	if err != nil {
		return false, fmt.Errorf("store: resolve infohashes of %s:%s: %w", engineName, ref, err)
	}
	v2n, err := NormaliseInfohash(v2)
	if err != nil {
		return false, fmt.Errorf("store: resolve infohashes of %s:%s: %w", engineName, ref, err)
	}
	if v1n == "" && v2n == "" {
		// Nothing resolved yet — the daemon keys exist but carry no value.
		return false, nil
	}

	var id string
	err = s.db.GetContext(ctx, &id, queryTaskIDByEngineRef, engineName, ref)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: resolve infohashes of %s:%s: %w", engineName, ref, err)
	}

	err = s.SetInfohashes(ctx, id, v1n, v2n)
	if errors.Is(err, ErrNotFound) {
		// The task was removed between the handle lookup and this write;
		// its ownership lapsed, so the landing is a no-op like any other.
		return false, nil
	}
	if errors.Is(err, ErrDuplicateInfohash) {
		if err := s.PauseDuplicate(ctx, id); err != nil {
			return false, err
		}

		return true, nil
	}
	if err != nil {
		return false, err
	}

	return false, nil
}

// PauseDuplicate lands the late collision of FR-023: the task whose
// metadata resolved onto another live task's identity is paused in place
// with error_code torrent_duplicate and one task_events row, and neither
// the row nor its data is deleted — the transfer stops, the history
// stays, and the operator decides. It is idempotent: a row already paused
// under this code reports success without writing, so a caller retrying
// after its engine-side stop failed neither errors nor double-logs.
func (s *TaskStore) PauseDuplicate(ctx context.Context, taskID string) error {
	var current struct {
		State     string  `db:"state"`
		ErrorCode *string `db:"error_code"`
		// error_message rides queryTaskErrorCode's column list; its value is
		// not read here, only the code identifies the landing.
		ErrorMessage *string `db:"error_message"`
	}
	err := s.db.GetContext(ctx, &current, queryTaskErrorCode, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: pause duplicate task %q: %w", taskID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("store: pause duplicate task %q: read row: %w", taskID, err)
	}
	if current.State == "paused" && current.ErrorCode != nil && *current.ErrorCode == errorCodeTorrentDuplicate {
		return nil
	}

	err = s.PauseWithCode(ctx, taskID, CodedPause{
		EventCode:    CodeTaskDuplicatePaused,
		EventMessage: messageTaskDuplicatePaused,
		ErrorCode:    errorCodeTorrentDuplicate,
		ErrorMessage: "another task already holds this torrent's identity",
		FromStates:   duplicatePauseFromStates,
	})
	if err != nil {
		return fmt.Errorf("store: pause duplicate task %q: %w", taskID, err)
	}

	return nil
}
