package store

import (
	"encoding/base32"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"
)

// The two identities of one hybrid torrent: a v1 SHA-1 and a v2 SHA-256,
// as 40 and 64 lowercase hex characters.
const (
	fixtureV1 = "8f9c3a2b1d4e5f60718293a4b5c6d7e8f9a0b1c2"
	fixtureV2 = "5a7f9c3b1d4e5f60718293a4b5c6d7e8f9a0b1c2d3e4f5061728394a5b6c7d8e"
)

// fixtureV1Base32 is fixtureV1 in the 32-character base32 form BEP 9 also
// permits, derived from the hex so the two spellings cannot drift apart.
func fixtureV1Base32() string {
	raw, err := hex.DecodeString(fixtureV1)
	if err != nil {
		panic(err)
	}

	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
}

// seedTorrentTask inserts one live qbittorrent task with the given hashes
// and returns its id. CreateLogged mirrors the create path: a task row
// never exists without its task.created event (FR-150).
func seedTorrentTask(t *testing.T, s *TaskStore, v1, v2, engineRef string) string {
	t.Helper()

	task, err := s.CreateLogged(t.Context(), Task{
		Engine:      "qbittorrent",
		EngineRef:   ptrOrNil(engineRef),
		SourceKind:  "magnet",
		Name:        "fixture",
		InfohashV1:  ptrOrNil(v1),
		InfohashV2:  ptrOrNil(v2),
		State:       "downloading",
		Destination: "/data",
	})
	require.NoError(t, err)

	return task.ID
}

// ptrOrNil maps "" to nil, the storage form of an absent hash.
func ptrOrNil(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

// storedInfohashes reads both columns of one row as plain strings, "" for
// NULL.
func storedInfohashes(t *testing.T, s *TaskStore, id string) (string, string) {
	t.Helper()

	var row struct {
		InfohashV1 *string `db:"infohash_v1"`
		InfohashV2 *string `db:"infohash_v2"`
	}
	require.NoError(t, s.db.GetContext(t.Context(), &row, queryTaskInfohashes, id))

	return textOrEmpty(row.InfohashV1), textOrEmpty(row.InfohashV2)
}

// TestNormaliseInfohash pins the four accepted spellings of FR-022, the
// lowercase rule and the rejections: each accepted form normalises to
// lowercase hex of exactly its column's width.
func TestNormaliseInfohash(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  string
		wants string // the error text's head, non-empty for a rejection
	}{
		{name: "empty", in: "", want: ""},
		{name: "v1 hex", in: fixtureV1, want: fixtureV1},
		{name: "v1 hex upper", in: strings.ToUpper(fixtureV1), want: fixtureV1},
		{name: "v1 base32", in: fixtureV1Base32(), want: fixtureV1},
		{name: "v1 base32 lower", in: strings.ToLower(fixtureV1Base32()), want: fixtureV1},
		{name: "v2 hex", in: fixtureV2, want: fixtureV2},
		{name: "v2 hex upper", in: strings.ToUpper(fixtureV2), want: fixtureV2},
		{name: "v2 multihash", in: btmhSHA256Tag + fixtureV2, want: fixtureV2},
		{name: "v2 multihash upper", in: "1220" + strings.ToUpper(fixtureV2), want: fixtureV2},
		{name: "v1 hex too short", in: fixtureV1[:39], wants: "store: infohash has 39 characters"},
		{name: "v1 not hex", in: strings.ReplaceAll(fixtureV1, "a", "z"), wants: "store: infohash"},
		{name: "v1 base32 not base32", in: "11111111111111111111111111111119", wants: "store: infohash"},
		{name: "v2 not hex", in: strings.Repeat("g", 64), wants: "store: infohash"},
		{name: "v2 multihash wrong tag", in: "1210" + fixtureV2, wants: "store: infohash"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormaliseInfohash(tc.in)
			if tc.wants != "" {
				require.ErrorContains(t, err, tc.wants)
				require.Empty(t, got)

				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestBase32AndHexAreOneTask pins FR-022's first rule: a magnet submitted
// with its v1 hash in base32 and the same magnet in 40-hex are one torrent,
// so the base32 form resolves to the row the hex form created — and an
// uppercase submission is stored lowercase.
func TestBase32AndHexAreOneTask(t *testing.T) {
	s := NewTaskStore(mustOpenTestStore(t))

	id := seedTorrentTask(t, s, "", "", "ref-a")

	// The base32 spelling resolves through NormaliseInfohash to the hex
	// one, and the write stores it lowercase.
	require.NoError(t, s.SetInfohashes(t.Context(), id, fixtureV1Base32(), ""))

	v1, v2 := storedInfohashes(t, s, id)
	require.Equal(t, fixtureV1, v1)
	require.Empty(t, v2)

	// The hex spelling finds the same row the base32 one wrote.
	byHex, err := s.FindByInfohash(t.Context(), fixtureV1, "")
	require.NoError(t, err)
	require.Equal(t, id, byHex.ID)

	byBase32, err := s.FindByInfohash(t.Context(), fixtureV1Base32(), "")
	require.NoError(t, err)
	require.Equal(t, id, byBase32.ID, "the base32 and hex spellings must be one task")

	// Both columns are exactly their documented widths.
	require.Len(t, v1, 40)
}

// TestHybridDedupBothDirections pins FR-023 on a hybrid torrent: added by
// its v1 magnet, resolved to carry both hashes, it is found again by the
// v2 magnet alone, and a second task cannot take either hash — while the
// lookup never touches engine_ref.
func TestHybridDedupBothDirections(t *testing.T) {
	s := NewTaskStore(mustOpenTestStore(t))

	first := seedTorrentTask(t, s, fixtureV1, "", "ref-first")

	// Metadata resolves: the same torrent proves hybrid by growing its v2.
	require.NoError(t, s.SetInfohashes(t.Context(), first, "", fixtureV2))
	v1, v2 := storedInfohashes(t, s, first)
	require.Equal(t, fixtureV1, v1)
	require.Equal(t, fixtureV2, v2)

	// Both directions of the lookup land on the one row, by hash alone —
	// the second task's engine_ref differs, so a handle-based match could
	// not produce this.
	byV1, err := s.FindByInfohash(t.Context(), fixtureV1, "")
	require.NoError(t, err)
	require.Equal(t, first, byV1.ID)

	byV2, err := s.FindByInfohash(t.Context(), "", fixtureV2)
	require.NoError(t, err)
	require.Equal(t, first, byV2.ID)

	// Both hashes set at once — a hybrid magnet's two xt params — land on
	// the same row too.
	byBoth, err := s.FindByInfohash(t.Context(), fixtureV1, fixtureV2)
	require.NoError(t, err)
	require.Equal(t, first, byBoth.ID)

	// A second task adopting either hash is a duplicate.
	second := seedTorrentTask(t, s, "", "", "ref-second")
	err = s.SetInfohashes(t.Context(), second, fixtureV1, "")
	require.ErrorIs(t, err, ErrDuplicateInfohash)

	err = s.SetInfohashes(t.Context(), second, "", fixtureV2)
	require.ErrorIs(t, err, ErrDuplicateInfohash)

	// The refused writes left the second row hashless.
	v1, v2 = storedInfohashes(t, s, second)
	require.Empty(t, v1)
	require.Empty(t, v2)

	// The duplicate does not depend on the first task's liveness only by
	// accident of state: a tombstoned first task frees the identity.
	require.NoError(t, s.MarkRemoved(t.Context(), first, nil))
	_, err = s.FindByInfohash(t.Context(), "", fixtureV2)
	require.ErrorIs(t, err, ErrNotFound)

	// And the identity is truly freed: the second task can now claim it.
	require.NoError(t, s.SetInfohashes(t.Context(), second, "", fixtureV2))
	v1, v2 = storedInfohashes(t, s, second)
	require.Empty(t, v1)
	require.Equal(t, fixtureV2, v2)
}

// TestCrossFormCollision pins the re-check's exclusion of the row being
// written: a row that already holds v1 and is resolving the same torrent's
// v2 — held by another live task — must read the OTHER row's v2 as the
// collision, not its own reflection through the shared v1. Before the
// exclusion this surfaced as a raw unique-index refusal at the UPDATE
// instead of ErrDuplicateInfohash, and the late-collision pause never ran.
func TestCrossFormCollision(t *testing.T) {
	s := NewTaskStore(mustOpenTestStore(t))

	byV1 := seedTorrentTask(t, s, fixtureV1, "", "ref-by-v1")
	byV2 := seedTorrentTask(t, s, "", fixtureV2, "ref-by-v2")

	err := s.SetInfohashes(t.Context(), byV1, "", fixtureV2)
	require.ErrorIs(t, err, ErrDuplicateInfohash)
	require.Contains(t, err.Error(), byV2, "the collision must name the other task")

	// Through the write-back entry point the same collision pauses.
	duplicate, err := s.ResolveInfohashes(t.Context(), "qbittorrent", "ref-by-v1", "", fixtureV2)
	require.NoError(t, err)
	require.True(t, duplicate)
	paused, err := s.Get(t.Context(), byV1)
	require.NoError(t, err)
	require.Equal(t, "paused", paused.State)
	require.NotNil(t, paused.ErrorCode)
	require.Equal(t, errorCodeTorrentDuplicate, *paused.ErrorCode)
}

// TestV2OnlyLeavesV1Null pins the v2-only storage rule: a torrent with no
// v1 identity stores infohash_v1 as NULL — never ” — while the v2 column
// holds the 64 hex digits, accepted both bare and behind the multihash
// tag.
func TestV2OnlyLeavesV1Null(t *testing.T) {
	s := NewTaskStore(mustOpenTestStore(t))

	id := seedTorrentTask(t, s, "", "", "ref-v2")

	require.NoError(t, s.SetInfohashes(t.Context(), id, "", btmhSHA256Tag+strings.ToUpper(fixtureV2)))

	var row struct {
		InfohashV1 *string `db:"infohash_v1"`
		InfohashV2 *string `db:"infohash_v2"`
	}
	require.NoError(t, s.db.GetContext(t.Context(), &row, queryTaskInfohashes, id))
	require.Nil(t, row.InfohashV1, "a v2-only torrent must store infohash_v1 as NULL, not ''")
	require.NotNil(t, row.InfohashV2)
	require.Equal(t, fixtureV2, *row.InfohashV2)
}

// TestNullHashesCoexist pins the partial unique indices: two tasks with no
// infohash at all coexist, because the indices cover the non-null rows
// only — a magnet awaiting metadata and an http download are never each
// other's duplicate.
func TestNullHashesCoexist(t *testing.T) {
	s := NewTaskStore(mustOpenTestStore(t))

	first := seedTorrentTask(t, s, "", "", "ref-null-a")
	second := seedTorrentTask(t, s, "", "", "ref-null-b")
	require.NotEqual(t, first, second)

	_, err := s.FindByInfohash(t.Context(), "", "")
	require.ErrorIs(t, err, ErrNotFound, "empty arguments must never match")

	// The empty lookup cannot have been vacuous: the same rows are found
	// once a hash arrives.
	require.NoError(t, s.SetInfohashes(t.Context(), second, fixtureV1, ""))
	found, err := s.FindByInfohash(t.Context(), fixtureV1, "")
	require.NoError(t, err)
	require.Equal(t, second, found.ID)
}

// TestSetInfohashesIdempotentAndRefusing pins the write contract: a repeat
// of the stored pair writes nothing (not even updated_at), and a different
// value for a column that already holds one is refused.
func TestSetInfohashesIdempotentAndRefusing(t *testing.T) {
	s := NewTaskStore(mustOpenTestStore(t))

	id := seedTorrentTask(t, s, fixtureV1, "", "ref-idem")
	require.NoError(t, s.SetInfohashes(t.Context(), id, fixtureV1, fixtureV2))

	var updatedBefore int64
	require.NoError(t, s.db.GetContext(t.Context(), &updatedBefore, `SELECT updated_at FROM tasks WHERE id = ?`, id))

	// Let the clock move so an unlawful write would change updated_at.
	// tasks.updated_at is stored in Unix milliseconds (docs/04-data-model.md
	// section 3.3), so two milliseconds span two distinct stamps.
	time.Sleep(2 * time.Millisecond)
	require.NoError(t, s.SetInfohashes(t.Context(), id, fixtureV1, fixtureV2))
	require.NoError(t, s.SetInfohashes(t.Context(), id, fixtureV1, ""))

	// An empty v2 means "leave the column alone", never "clear it".
	v1, v2 := storedInfohashes(t, s, id)
	require.Equal(t, fixtureV1, v1)
	require.Equal(t, fixtureV2, v2)

	var updatedAfter int64
	require.NoError(t, s.db.GetContext(t.Context(), &updatedAfter, `SELECT updated_at FROM tasks WHERE id = ?`, id))
	require.Equal(t, updatedBefore, updatedAfter, "an idempotent re-set must not bump updated_at")

	// A different v1 for a row that already holds one is refused.
	err := s.SetInfohashes(t.Context(), id, strings.Repeat("c", 40), "")
	require.ErrorContains(t, err, "refusing")

	// A missing id is ErrNotFound, not a silent no-op.
	err = s.SetInfohashes(t.Context(), "tsk_missing", fixtureV1, "")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestResolveInfohashesLandsAndPauses pins the write-back the delta path
// drives: a handle's resolved hashes land on its own task, a foreign
// handle is a no-op, and a late collision pauses the task that resolved
// second — error_code torrent_duplicate, one task_events row, the row and
// its counters untouched.
func TestResolveInfohashesLandsAndPauses(t *testing.T) {
	s := NewTaskStore(mustOpenTestStore(t))

	// The first task already holds the identity the second will resolve to.
	first := seedTorrentTask(t, s, fixtureV1, "", "ref-owned")
	require.NoError(t, s.Transition(t.Context(), first, "paused", CodeTaskPaused, "operator park"))

	// The second task is mid-transfer when its metadata arrives.
	second := seedTorrentTask(t, s, "", "", "ref-late")
	require.NoError(t, s.UpdateProgress(t.Context(), second, Progress{CompletedBytes: 4096}))

	// A handle no live task carries is a no-op, not an error.
	_, err := s.ResolveInfohashes(t.Context(), "qbittorrent", "ref-nobody", fixtureV1, "")
	require.NoError(t, err)

	// The late collision: the delta resolves the second task onto the
	// first task's identity.
	lateDuplicate, err := s.ResolveInfohashes(t.Context(), "qbittorrent", "ref-late", fixtureV1, "")
	require.NoError(t, err)
	require.True(t, lateDuplicate)

	row, err := s.Get(t.Context(), second)
	require.NoError(t, err, "the duplicate task must still exist")
	require.Equal(t, "paused", row.State)
	require.NotNil(t, row.ErrorCode)
	require.Equal(t, errorCodeTorrentDuplicate, *row.ErrorCode)
	require.Equal(t, int64(4096), row.CompletedBytes, "the pause must not disturb the counters")

	// Exactly one event row landed with the pause.
	var events []TaskEvent
	require.NoError(t, s.db.SelectContext(t.Context(), &events,
		`SELECT id, task_id, at, level, code, message, detail_json, created_at, updated_at
		 FROM task_events WHERE task_id = ? ORDER BY at, id`, second))
	require.Len(t, events, 2, "task.created plus the duplicate pause")
	require.Equal(t, CodeTaskDuplicatePaused, events[1].Code)

	// The landing is idempotent: a caller retrying after its engine-side
	// stop failed neither errors nor writes a second event.
	again, err := s.ResolveInfohashes(t.Context(), "qbittorrent", "ref-late", fixtureV1, "")
	require.NoError(t, err)
	require.True(t, again)
	require.NoError(t, s.db.SelectContext(t.Context(), &events,
		`SELECT id, task_id, at, level, code, message, detail_json, created_at, updated_at
		 FROM task_events WHERE task_id = ? ORDER BY at, id`, second))
	require.Len(t, events, 2, "the retry wrote no second event")

	// The winner is untouched.
	winner, err := s.Get(t.Context(), first)
	require.NoError(t, err)
	require.Equal(t, "paused", winner.State)
	require.Nil(t, winner.ErrorCode)
}

// TestFindByInfohashRejectsMalformed pins the validation entry: a malformed
// hash is an error before any query runs, so a caller cannot smuggle a
// junk value into the duplicate check.
func TestFindByInfohashRejectsMalformed(t *testing.T) {
	s := NewTaskStore(mustOpenTestStore(t))

	_, err := s.FindByInfohash(t.Context(), "not-a-hash", "")
	require.ErrorContains(t, err, "store: infohash")

	err = s.SetInfohashes(t.Context(), "tsk_any", "short", "")
	require.ErrorContains(t, err, "store: infohash")

	_, err = s.ResolveInfohashes(t.Context(), "qbittorrent", "ref-any", "", "")
	require.NoError(t, err, "an empty resolution is a no-op")
}

// mustOpenTestStore opens the migrated test store and returns its handle.
func mustOpenTestStore(t *testing.T) *sqlx.DB {
	t.Helper()

	db, _, _ := openTestStore(t)

	return db
}
