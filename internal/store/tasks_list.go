package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jmoiron/sqlx"
)

// ErrStaleCursor is returned when a cursor does not belong to the supplied
// filter and sort, or cannot be decoded at all. The API answers both with
// 422 /problems/validation-failed rather than a wrong page.
var ErrStaleCursor = errors.New("store: cursor does not match this filter")

// ErrInvalidSort is returned when the sort key is outside the allowlist
// below. The column names never reach SQL unvalidated: the allowlist is the
// only way a sort expression is chosen.
var ErrInvalidSort = errors.New("store: unknown sort column")

// ErrUnknownFilterState is returned when State names neither a canonical
// state nor a sidebar filter.
var ErrUnknownFilterState = errors.New("store: unknown state or sidebar filter")

const (
	// taskListDefaultLimit and taskListMaxLimit are the range of
	// docs/05-api-contract.md section 1.4: limit is optional, default 100,
	// range 1..500. Huma enforces it for the endpoint; ListTasks re-checks
	// it for every other caller.
	taskListDefaultLimit = 100
	taskListMaxLimit     = 500

	// defaultTaskSort is the documented default of GET /tasks: newest
	// first.
	defaultTaskSort = "-added_at"
)

// TaskFilter is the parsed query of GET /tasks. An empty field means "no
// constraint"; Category and Tag set to the empty string with the matching
// Has flag select uncategorised and untagged tasks.
type TaskFilter struct {
	State       string // a canonical state, or a sidebar filter name
	Category    string
	HasCategory bool
	Tag         string
	HasTag      bool
	Query       string // case-insensitive substring of name
	Sort        string // a column from the allowlist, optionally prefixed with '-'
	Limit       int    // 1..500, default 100
	Cursor      string // opaque; bound to the filter and sort that produced it
}

// sidebarFilterSets resolves the sidebar filters of docs/04-data-model.md
// section 4.1 in SQL and nowhere else. `all` is derived from the state
// vocabulary rather than spelled out, so it stays "every state except
// removed" by construction.
var sidebarFilterSets = map[string][]string{
	"all":         slices.DeleteFunc(slices.Clone(taskStates), func(s string) bool { return s == "removed" }),
	"downloading": {"downloading"},
	"completed":   {"completed", "seeding"},
	"active":      {"downloading", "seeding"},
	"inactive":    {"error", "queued", "paused"},
	"stopped":     {"paused"},
	"error":       {"error"},
}

// taskSortColumns maps every documented sort key to its SQL expression.
// Only values from this map are ever interpolated into ORDER BY or a cursor
// predicate; a user-supplied key is looked up, never concatenated.
// progress sorts on the derived expression pinned by docs/05-api-contract.md
// section 3, which is NULL while total_bytes is unknown or zero.
var taskSortColumns = map[string]string{
	"added_at":       "added_at",
	"completed_at":   "completed_at",
	"name":           "name",
	"total_bytes":    "total_bytes",
	"progress":       "CAST(completed_bytes AS REAL) / NULLIF(total_bytes, 0)",
	"state":          "state",
	"download_rate":  "download_rate",
	"upload_rate":    "upload_rate",
	"eta_seconds":    "eta_seconds",
	"ratio":          "ratio",
	"queue_position": "queue_position",
}

// taskSortKeys is the allowlist in documented order, for legible errors.
var taskSortKeys = []string{
	"added_at", "completed_at", "name", "total_bytes", "progress", "state",
	"download_rate", "upload_rate", "eta_seconds", "ratio", "queue_position",
}

// taskPageCursor is the decoded page token: the sort value and id of the
// last row of the page that issued it, plus a hash binding it to the filter
// and sort that produced it (docs/05-api-contract.md section 1.4).
type taskPageCursor struct {
	Hash   string `json:"h"`
	LastID string `json:"i"`
	Value  any    `json:"v"` // int64, float64, string or nil, by sort column
}

// taskListRow scans one page row. It extends Task with ratio, which the
// canonical Task object derives from the row but store.Task does not carry
// yet; the DTO reads stay with the tasks that write those columns, so ratio
// is used for the cursor alone.
type taskListRow struct {
	Task
	Ratio float64 `db:"ratio"`
}

const queryListTasksPage = `SELECT id, engine, engine_ref, source_kind, source_uri, name, infohash_v1, infohash_v2,
 state, error_code, error_message, destination, content_path, category_id, total_bytes, completed_bytes,
 uploaded_bytes, download_rate, upload_rate, eta_seconds, sequential, queue_position,
 added_at, started_at, completed_at, created_at, updated_at, ratio
FROM tasks
WHERE `

// ListTasks returns one page plus the next cursor and the total matching
// the filter, ignoring the cursor. ErrStaleCursor is returned when the
// cursor was issued for a different filter or sort. The page is ordered by
// the sort column with the id as tiebreak, so the ordering is total and the
// cursor walk misses nothing.
func (s *TaskStore) ListTasks(ctx context.Context, f TaskFilter) ([]Task, string, int, error) {
	limit := f.Limit
	if limit == 0 {
		limit = taskListDefaultLimit
	}
	if limit < 1 || limit > taskListMaxLimit {
		return nil, "", 0, fmt.Errorf("store: list tasks: limit %d outside 1..%d", limit, taskListMaxLimit)
	}

	sortKey := f.Sort
	if sortKey == "" {
		sortKey = defaultTaskSort
	}
	sortColumn, descending, err := parseTaskSort(sortKey)
	if err != nil {
		return nil, "", 0, err
	}

	states, err := taskFilterStates(f.State)
	if err != nil {
		return nil, "", 0, err
	}

	where, args, err := taskFilterWhere(f)
	if err != nil {
		return nil, "", 0, err
	}

	pageWhere := strings.Join(where, " AND ")
	pageArgs := args
	if f.Cursor != "" {
		cursor, err := decodeTaskCursor(f.Cursor)
		if err != nil {
			return nil, "", 0, err
		}
		if cursor.Hash != taskFilterHash(states, sortColumn, descending, f) {
			return nil, "", 0, fmt.Errorf("%w: issued for a different filter or sort", ErrStaleCursor)
		}

		predicate, cursorArgs := taskCursorPredicate(sortColumn, descending, cursor.Value, cursor.LastID)
		pageWhere += " AND " + predicate
		pageArgs = append(pageArgs, cursorArgs...)
	}

	// total counts the filter, ignoring the cursor (docs/05-api-contract.md
	// section 1.4); it runs after the cursor check so an invalid token
	// fails fast instead of paying for the count scan.
	var total int
	countQuery, countArgs, err := sqlx.In("SELECT COUNT(*) FROM tasks WHERE "+strings.Join(where, " AND "), args...)
	if err != nil {
		return nil, "", 0, fmt.Errorf("store: list tasks: build count: %w", err)
	}
	if err := s.db.GetContext(ctx, &total, countQuery, countArgs...); err != nil {
		return nil, "", 0, fmt.Errorf("store: list tasks: count: %w", err)
	}

	direction := "DESC"
	if !descending {
		direction = "ASC"
	}
	// One row past the limit decides whether another page exists, so
	// next_cursor is null exactly on the last page.
	query := queryListTasksPage + pageWhere +
		" ORDER BY " + sortColumn + " " + direction + ", id " + direction +
		fmt.Sprintf(" LIMIT %d", limit+1)
	query, queryArgs, err := sqlx.In(query, pageArgs...)
	if err != nil {
		return nil, "", 0, fmt.Errorf("store: list tasks: build page query: %w", err)
	}

	var rows []taskListRow
	if err := s.db.SelectContext(ctx, &rows, query, queryArgs...); err != nil {
		return nil, "", 0, fmt.Errorf("store: list tasks: read page: %w", err)
	}
	if len(rows) <= limit {
		return taskListRowsToTasks(rows), "", total, nil
	}
	rows = rows[:limit]

	last := rows[len(rows)-1]
	nextCursor, err := encodeTaskCursor(taskPageCursor{
		Hash:   taskFilterHash(states, sortColumn, descending, f),
		LastID: last.ID,
		Value:  taskSortValue(last, strings.TrimPrefix(sortKey, "-")),
	})
	if err != nil {
		return nil, "", 0, fmt.Errorf("store: list tasks: encode cursor: %w", err)
	}

	return taskListRowsToTasks(rows), nextCursor, total, nil
}

// taskListRowsToTasks drops the list-only columns the Task value type does
// not carry.
func taskListRowsToTasks(rows []taskListRow) []Task {
	tasks := make([]Task, len(rows))
	for i, row := range rows {
		tasks[i] = row.Task
	}

	return tasks
}

// taskFilterWhere builds the filter predicates with bound placeholders
// only. The first predicate is always the state set, expanded into an
// IN (?, ?, …) clause by sqlx.In at query time.
func taskFilterWhere(f TaskFilter) ([]string, []any, error) {
	states, err := taskFilterStates(f.State)
	if err != nil {
		return nil, nil, err
	}

	where := []string{"state IN (?)"}
	args := []any{states}
	if f.HasCategory {
		if f.Category == "" {
			where = append(where, "category_id IS NULL")
		} else {
			where = append(where, "category_id = (SELECT id FROM categories WHERE name = ?)")
			args = append(args, f.Category)
		}
	}
	if f.HasTag {
		if f.Tag == "" {
			where = append(where, "id NOT IN (SELECT task_id FROM task_tags)")
		} else {
			where = append(where, `id IN (SELECT task_id FROM task_tags
WHERE tag_id = (SELECT id FROM tags WHERE name = ?))`)
			args = append(args, f.Tag)
		}
	}
	if f.Query != "" {
		// SQLite's LIKE is ASCII-case-insensitive by default, which is the
		// "case-insensitive substring" of docs/05-api-contract.md 5.1; the
		// percent wildcards make it a substring match.
		where = append(where, `name LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLikePattern(f.Query)+"%")
	}

	return where, args, nil
}

// taskFilterStates resolves the State field: a canonical state names
// itself, a sidebar filter names its set (docs/04-data-model.md 4.1), and
// the empty value behaves like `all` because a normal task list omits
// removed tombstones — only an explicit state=removed lists them
// (docs/05-api-contract.md 5.1).
func taskFilterStates(state string) ([]string, error) {
	if state == "" || state == "all" {
		return sidebarFilterSets["all"], nil
	}
	if set, ok := sidebarFilterSets[state]; ok {
		return set, nil
	}
	if slices.Contains(taskStates, state) {
		return []string{state}, nil
	}

	return nil, fmt.Errorf("%w: %q", ErrUnknownFilterState, state)
}

// parseTaskSort splits a leading '-' off the sort key and maps the rest to
// its SQL expression. Anything outside the allowlist is ErrInvalidSort
// before it can reach a query.
func parseTaskSort(sort string) (column string, descending bool, err error) {
	key, descending := strings.CutPrefix(sort, "-")

	column, ok := taskSortColumns[key]
	if !ok {
		return "", false, fmt.Errorf("%w: %q, want one of %q", ErrInvalidSort, sort, taskSortKeys)
	}

	return column, descending, nil
}

// taskCursorPredicate is the keyset pagination predicate for one sort
// column and direction: every row strictly after the cursor's (value, id)
// tuple in the order the page is read in. SQLite sorts NULLs first
// ascending and last descending, so the NULL cluster has a predicate of
// its own depending on where the cursor sits — inside the cluster
// (value nil) or past it. column comes from taskSortColumns only.
func taskCursorPredicate(column string, descending bool, value any, lastID string) (string, []any) {
	if value == nil {
		if descending {
			// NULLs close a descending result: only the rest of the cluster
			// can follow a NULL cursor.
			return fmt.Sprintf("(%s IS NULL AND id < ?)", column), []any{lastID}
		}

		// NULLs lead an ascending result: everything non-NULL follows,
		// plus the cluster rows not emitted yet.
		return fmt.Sprintf("(%s IS NOT NULL OR (%s IS NULL AND id > ?))", column, column), []any{lastID}
	}

	if descending {
		// Every NULL still sorts after a non-NULL cursor value, so the
		// whole cluster is ahead of it.
		return fmt.Sprintf(
			"(%s IS NULL OR %s < ? OR (%s = ? AND id < ?))",
			column, column, column,
		), []any{value, value, lastID}
	}

	return fmt.Sprintf(
		"(%s > ? OR (%s = ? AND id > ?))",
		column, column,
	), []any{value, value, lastID}
}

// taskFilterHash binds a cursor to the filter and sort that produced it:
// the resolved state set (so equivalent spellings like "" and "all" stay
// interchangeable), the category, tag and name constraints, and the parsed
// sort.
func taskFilterHash(states []string, column string, descending bool, f TaskFilter) string {
	canonical := strings.Join([]string{
		"states:" + strings.Join(states, ","),
		fmt.Sprintf("category:%t:%s", f.HasCategory, f.Category),
		fmt.Sprintf("tag:%t:%s", f.HasTag, f.Tag),
		"query:" + f.Query,
		fmt.Sprintf("sort:%s:%t", column, descending),
	}, "\x00")
	sum := sha256.Sum256([]byte(canonical))

	return base64.RawStdEncoding.EncodeToString(sum[:])
}

// encodeTaskCursor renders a page token as base64 JSON.
func encodeTaskCursor(c taskPageCursor) (string, error) {
	encoded, err := json.Marshal(c)
	if err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

// decodeTaskCursor parses a page token. A token that is not base64 JSON of
// the cursor shape is reported as ErrStaleCursor: it does not belong to
// this (or any) filter, and the wire outcome is the same 422.
func decodeTaskCursor(token string) (taskPageCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return taskPageCursor{}, fmt.Errorf("%w: token is not valid base64", ErrStaleCursor)
	}

	var cursor taskPageCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		return taskPageCursor{}, fmt.Errorf("%w: token is not a page cursor", ErrStaleCursor)
	}
	if cursor.LastID == "" {
		return taskPageCursor{}, fmt.Errorf("%w: token carries no row", ErrStaleCursor)
	}
	// Only the shapes a sort value can take bind to SQL; a crafted token
	// holding anything else answers 422 instead of a driver error.
	switch cursor.Value.(type) {
	case nil, string, float64:
	default:
		return taskPageCursor{}, fmt.Errorf("%w: token carries an unusable sort value", ErrStaleCursor)
	}

	return cursor, nil
}

// taskSortValue extracts the cursor's sort value from the last row of a
// page. NULL columns decode as nil, which the cursor predicate's IS NULL
// branch answers.
func taskSortValue(row taskListRow, key string) any {
	switch key {
	case "added_at":
		return row.AddedAt
	case "completed_at":
		return nilOrValue(row.CompletedAt)
	case "name":
		return row.Name
	case "total_bytes":
		return nilOrValue(row.TotalBytes)
	case "progress":
		if row.TotalBytes == nil || *row.TotalBytes == 0 {
			return nil
		}

		return float64(row.CompletedBytes) / float64(*row.TotalBytes)
	case "state":
		return row.State
	case "download_rate":
		return row.DownloadRate
	case "upload_rate":
		return row.UploadRate
	case "eta_seconds":
		return nilOrValue(row.ETASeconds)
	case "ratio":
		return row.Ratio
	case "queue_position":
		return nilOrValue(row.QueuePosition)
	default:
		// Unreachable: parseTaskSort already rejected every other key.
		return nil
	}
}

// nilOrValue flattens a nullable column for the cursor payload.
func nilOrValue[T any](v *T) any {
	if v == nil {
		return nil
	}

	return *v
}

// escapeLikePattern makes %, _ and the escape character literal inside a
// LIKE pattern, so a name query that happens to contain them still matches
// as a substring.
func escapeLikePattern(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

	return replacer.Replace(s)
}
