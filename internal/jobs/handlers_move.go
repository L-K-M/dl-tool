package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/fsx"
	"github.com/L-K-M/dl-tool/internal/store"
)

// The postprocess.move.* task_events codes of docs/14-conventions.md
// section 4, emitted through Transition like the extract handler's.
const (
	eventMoveStarted   = "postprocess.move.started"
	eventMoveCompleted = "postprocess.move.completed"
	eventMoveFailed    = "postprocess.move.failed"
)

// The disk_full pair the pause lands. codeMoveDiskFull is
// engine.ErrorCodeDiskFull's storage form — the admission pass selects
// parked tasks by the literal, and jobs must not import internal/engine for
// the constant, the same sharing the store's guarded queries already pin.
// The two sentences are the ones PauseDiskFull writes, so a move pause is
// indistinguishable from a transfer pause to the resume selection and to
// the operator.
const (
	codeMoveDiskFull         = "disk_full"
	moveDiskFullMessage      = "no space left on device; the task resumes once space returns"
	moveDiskFullEventMessage = "paused by the disk-space guard: no space left on device"
)

// settingMinFreeSpace is the settings row the destination pre-check folds
// into the reservation's floor — the same key the admission policy loader
// reads (docs/11-config-reference.md section 5).
const settingMinFreeSpace = "min_free_space"

// querySetMoveContentPath writes the payload's new home. No TaskStore
// method covers content_path — this handler is its only writer after task
// creation.
const querySetMoveContentPath = `UPDATE tasks SET content_path = ?, updated_at = ? WHERE id = ?`

// movePayload is the durable job row's payload: the path to relocate and
// the path it must land at. The pair is recorded at enqueue so a retried
// job — after a crash or a parked disk-full wait — moves the same bytes to
// the same destination the chain decided on, not whatever the row happens
// to hold then.
type movePayload struct {
	TaskID string `json:"task_id"`
	Src    string `json:"src"`
	Dst    string `json:"dst"`
}

// MoveHandler runs job kind "move". Registered with the T012 worker pool as
// worker.Register(JobKindMove, h.Handle).
type MoveHandler struct {
	db    *sqlx.DB
	tasks *store.TaskStore
	roots []string
}

// NewMoveHandler returns the move handler. db carries the content_path
// write and the min_free_space read — the task's Files table keeps
// TaskStore unchanged; roots are the configured data roots
// (DLTOOL_DATA_ROOTS) the destination's min-free floor resolves against.
func NewMoveHandler(db *sqlx.DB, tasks *store.TaskStore, roots []string) *MoveHandler {
	return &MoveHandler{db: db, tasks: tasks, roots: roots}
}

// Handle runs one move job end to end: claim the row into moving, pre-check
// the destination's free space when the move will copy, relocate the
// payload through fsx.Move, then land content_path and the return to
// completed — which re-enters the postprocess chain for the steps after
// move. A verdict the task row already recorded — or a job whose work is
// already done — returns nil so the worker marks the job done; only an
// infrastructure failure that could not be recorded is returned for retry.
func (h *MoveHandler) Handle(ctx context.Context, job store.Job) error {
	var payload movePayload
	if err := json.Unmarshal([]byte(job.PayloadJSON), &payload); err != nil {
		return fmt.Errorf("jobs: decode %q payload: %w", JobKindMove, err)
	}
	taskID := payload.TaskID
	if taskID == "" && job.TaskID != nil {
		taskID = *job.TaskID
	}
	if taskID == "" || payload.Src == "" || payload.Dst == "" {
		return fmt.Errorf("jobs: %s job %q carries an incomplete payload", JobKindMove, job.ID)
	}
	src, dst := payload.Src, payload.Dst

	task, err := h.tasks.Get(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil // the row is gone; nothing left to move
	}
	if err != nil {
		return fmt.Errorf("jobs: move task %q: %w", taskID, err)
	}
	switch task.State {
	case "completed", "moving", "seeding":
		// completed is the fresh claim; moving is a re-entry — a crash or a
		// duplicate claim mid-run, or the PATCH destination path's own
		// moving — and seeding only reaches the already-landed recovery
		// below: the engine owns a seeding payload's live data.
	default:
		return nil // paused, error, removed or mid-download — not ours to move
	}
	if src == dst {
		return nil // the payload is already where the job would put it
	}

	// The idempotent re-delivery the contract names: a job whose src is
	// already gone and whose dst already exists settles the bookkeeping a
	// crash between the staging rename and the transition skipped.
	srcInfo, srcErr := os.Stat(src)
	dstInfo, dstErr := os.Stat(dst)
	switch {
	case errors.Is(srcErr, fs.ErrNotExist) && dstErr == nil:
		return h.settle(ctx, task, dst)
	case srcErr != nil && !errors.Is(srcErr, fs.ErrNotExist):
		return fmt.Errorf("jobs: move task %q: stat payload: %w", taskID, srcErr)
	case dstErr != nil && !errors.Is(dstErr, fs.ErrNotExist):
		return fmt.Errorf("jobs: move task %q: stat destination: %w", taskID, dstErr)
	}
	// A missing source reaches fsx.Move and comes back through the failure
	// path; a live seeding payload is the engine's, never this job's.
	if task.State == "seeding" {
		return nil
	}
	if task.State != "moving" {
		err := h.tasks.Transition(ctx, taskID, "moving", eventMoveStarted, "moving payload to destination")
		switch {
		case err == nil:
		case errors.Is(err, store.ErrNotFound),
			errors.Is(err, store.ErrIllegalTransition),
			errors.Is(err, store.ErrTransitionConflict):
			// The row moved on between the read and the claim — an
			// operator action or the reconciler owns it now.
			return nil
		default:
			return fmt.Errorf("jobs: move task %q: %w", taskID, err)
		}
		task.State = "moving"
	}

	// Both sides existing is the crash window between the staging rename
	// and the source removal: a destination already holding the same tree
	// is only owed the removal. A different tree is a real collision, and
	// the move must fail loudly — a file-to-file rename(2) would silently
	// replace the foreign payload, the outcome the extract handler's
	// loud-collision rule already refuses.
	if srcErr == nil && dstErr == nil {
		same, err := samePayload(srcInfo, dstInfo, src, dst)
		if err != nil {
			return fmt.Errorf("jobs: move task %q: compare existing destination: %w", taskID, err)
		}
		if same {
			if err := os.RemoveAll(src); err != nil {
				return fmt.Errorf("jobs: move task %q: remove duplicate source: %w", taskID, err)
			}
			return h.settle(ctx, task, dst)
		}
		return h.fail(ctx, taskID, fmt.Errorf("destination %q already exists with different content", dst))
	}

	// A same-filesystem move is one rename and needs no headroom; the
	// reservation pre-check of FR-045 gates only the copy fallback. An
	// unidentified filesystem fails the check open — the copy's own
	// ENOSPC surfaces through the same disk-full pause a refused check
	// produces, one hop later.
	if same, err := fsx.SameFilesystem(src, dst); err == nil && !same {
		total, err := dirSizeBytes(src)
		if err != nil {
			return h.fail(ctx, taskID, fmt.Errorf("measure payload: %w", err))
		}
		admitted, err := h.destinationAdmits(ctx, dst, total)
		if err != nil {
			return fmt.Errorf("jobs: move task %q: %w", taskID, err)
		}
		if !admitted {
			return h.pauseDiskFull(ctx, taskID)
		}
	}

	var lastTotal int64
	moveErr := fsx.Move(ctx, src, dst, func(copied, total int64) {
		lastTotal = total
		h.forwardProgress(ctx, taskID, copied, total, task.UploadedBytes)
	})
	switch {
	case moveErr == nil:
		// The throttle's trailing ticks never landed, so land the full
		// count now: a completed row reads its own gauge, and the
		// reservation pool counts total-minus-completed for as long as the
		// row sits in moving.
		if lastTotal > 0 {
			h.forwardProgress(ctx, taskID, lastTotal, lastTotal, task.UploadedBytes)
		}
		return h.settle(ctx, task, dst)
	case ctx.Err() != nil:
		// The pool is shutting down, not a move verdict: leave the row in
		// moving and let the rescheduled job retry.
		return fmt.Errorf("jobs: move task %q: %w", taskID, ctx.Err())
	case fsx.IsENOSPC(moveErr):
		// The copy ran out of room mid-flight: pause with disk_full like
		// the pre-check refusal; fsx already removed the staging tree and
		// left the source untouched.
		return h.pauseDiskFull(ctx, taskID)
	default:
		return h.fail(ctx, taskID, moveErr)
	}
}

// settle lands the post-move bookkeeping. content_path follows the payload
// wherever the row stands — the bytes verifiably sit at dst, so a row that
// left moving mid-run (a reconciler adoption, an operator action) still
// needs the pointer corrected — and only a row still in moving is walked
// back to completed, which re-enters the postprocess chain for the steps
// after move.
func (h *MoveHandler) settle(ctx context.Context, task store.Task, dst string) error {
	if task.ContentPath == nil || *task.ContentPath != dst {
		if err := h.setContentPath(ctx, task.ID, dst); err != nil {
			return fmt.Errorf("jobs: move task %q: %w", task.ID, err)
		}
	}
	if task.State != "moving" {
		return nil
	}
	err := h.tasks.Transition(ctx, task.ID, "completed", eventMoveCompleted, "payload moved to destination")
	switch {
	case err == nil,
		errors.Is(err, store.ErrNotFound),
		errors.Is(err, store.ErrIllegalTransition),
		errors.Is(err, store.ErrTransitionConflict):
		return nil
	default:
		return fmt.Errorf("jobs: move task %q: %w", task.ID, err)
	}
}

// setContentPath writes the payload's new location; the move handler is the
// column's only writer after task creation.
func (h *MoveHandler) setContentPath(ctx context.Context, taskID, path string) error {
	if _, err := h.db.ExecContext(ctx, querySetMoveContentPath, path, time.Now().UnixMilli(), taskID); err != nil {
		return fmt.Errorf("store: set content path of task %q: %w", taskID, err)
	}

	return nil
}

// forwardProgress adapts the fsx copy callback onto the task row: the
// move's copied bytes ride completed_bytes against the payload total so the
// UI's progress channel keeps reporting while the task sits in moving
// (docs/10-deployment-and-compose.md section 3.5). The uploaded count and
// total pass through untouched — a move rewrites neither — and a lost
// write is a stalled gauge, not a failed move: the copy's own error path
// carries the verdict.
func (h *MoveHandler) forwardProgress(ctx context.Context, taskID string, copied, total, uploaded int64) {
	err := h.tasks.UpdateProgress(ctx, taskID, store.Progress{
		TotalBytes:     &total,
		CompletedBytes: copied,
		UploadedBytes:  uploaded,
	})
	if err != nil {
		slog.WarnContext(ctx, "jobs: move progress write failed", "task_id", taskID, "error", err)
	}
}

// destinationAdmits is the FR-045 pre-check for the copy fallback: the
// destination filesystem's reservation — live free space, the committed
// bytes of every counted task on the same filesystem, and the owning root's
// min_free_space floor — must admit the payload's full byte count.
func (h *MoveHandler) destinationAdmits(ctx context.Context, dst string, total int64) (bool, error) {
	fsID, err := fsx.FilesystemID(dst)
	if err != nil {
		return true, nil
	}
	space, err := fsx.FreeSpace(dst)
	if err != nil {
		return true, nil
	}

	perDestination, err := h.tasks.SumRemainingByDestination(ctx)
	if err != nil {
		return false, fmt.Errorf("read committed bytes: %w", err)
	}
	var committed int64
	for destination, remaining := range perDestination {
		id, err := fsx.FilesystemID(destination)
		if err == nil && id == fsID {
			committed += remaining
		}
	}

	minFree, err := h.minFreeSpace(ctx)
	if err != nil {
		return false, err
	}

	return fsx.Reservation{
		FilesystemID:   fsID,
		FreeBytes:      space.FreeBytes,
		CommittedBytes: committed,
		MinFreeBytes:   fsx.Floor(minFree, owningRoot(h.roots, dst)),
	}.Admits(total), nil
}

// minFreeSpace reads the min_free_space settings row into the map fsx.Floor
// looks up; a missing or malformed row yields an empty map — every root's
// documented default floor then applies.
func (h *MoveHandler) minFreeSpace(ctx context.Context) (map[string]int64, error) {
	var raw string
	err := h.db.GetContext(ctx, &raw, `SELECT value_json FROM settings WHERE key = ?`, settingMinFreeSpace)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: read settings key %s: %w", settingMinFreeSpace, err)
	}

	floors := map[string]int64{}
	if err := json.Unmarshal([]byte(raw), &floors); err != nil {
		// A malformed row falls back to the documented default rather than
		// refusing every move — the same tolerance boolSetting keeps.
		slog.WarnContext(ctx, "jobs: settings key holds malformed JSON, using default", "key", settingMinFreeSpace, "err", err)

		return map[string]int64{}, nil
	}

	return floors, nil
}

// owningRoot is the longest configured data root owning path — the floor's
// lookup key. The rule is the admission pass's rootOf restated for the move
// destination (jobs cannot import internal/engine): separator-bounded, "/"
// owns everything, no match yields the default floor.
func owningRoot(roots []string, path string) string {
	best := ""
	for _, root := range roots {
		trimmed := strings.TrimRight(root, "/")
		if trimmed == "" {
			trimmed = "/"
		}
		if len(trimmed) > len(best) && rootOwns(path, trimmed) {
			best = trimmed
		}
	}

	return best
}

// rootOwns reports whether path is root itself or a path under it.
func rootOwns(path, root string) bool {
	if root == "/" {
		return strings.HasPrefix(path, "/")
	}

	return path == root || strings.HasPrefix(path, root+"/")
}

// pauseDiskFull lands the FR-045 park: paused with disk_full, one event,
// and nothing unlinked. The job itself is done — the verdict is durable,
// and the admission pass re-releases the task once space returns; the
// chain's move step re-arms the job on the next completed transition. A row
// that left moving between the read and the landing is not dragged back.
func (h *MoveHandler) pauseDiskFull(ctx context.Context, taskID string) error {
	err := h.tasks.PauseWithCode(ctx, taskID, store.CodedPause{
		EventCode:    store.CodeTaskPaused,
		EventMessage: moveDiskFullEventMessage,
		ErrorCode:    codeMoveDiskFull,
		ErrorMessage: moveDiskFullMessage,
		FromStates:   []string{"moving"},
	})
	if err != nil &&
		!errors.Is(err, store.ErrNotFound) &&
		!errors.Is(err, store.ErrIllegalTransition) &&
		!errors.Is(err, store.ErrTransitionConflict) {
		return fmt.Errorf("jobs: move task %q: %w", taskID, err)
	}

	return nil
}

// fail records the failure on the task — state error, the mapped
// error_code, the postprocess.move.failed event inside the transition —
// then reports the job done: the verdict is durable, so the worker must not
// retry it. A task the operator moved out of moving mid-run has already
// been answered elsewhere — there is nothing left to record.
func (h *MoveHandler) fail(ctx context.Context, taskID string, runErr error) error {
	task, err := h.tasks.Get(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("jobs: move task %q: %w", taskID, err)
	}
	if task.State != "moving" {
		return nil
	}

	// SetErrorCode is an idempotent overwrite, so a retry after a partial
	// first attempt converges; Transition is the commit point — if it
	// fails, the state is unchanged and the retry starts clean.
	codeErr := h.tasks.SetErrorCode(ctx, taskID, moveErrorCode(runErr), runErr.Error())
	transitionErr := h.tasks.Transition(ctx, taskID, "error", eventMoveFailed, runErr.Error())
	if errors.Is(transitionErr, store.ErrIllegalTransition) {
		transitionErr = nil
	}
	if err := errors.Join(transitionErr, codeErr); err != nil {
		return fmt.Errorf("jobs: record move failure of task %q: %w", taskID, err)
	}

	return nil
}

// moveErrorCode maps a move failure onto the doc 04 section 4.2 code the
// task row records; the vocabulary has no move-specific code, so the mapped
// causes keep their names and everything else lands on unknown.
func moveErrorCode(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "file_not_exist"
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return "destination_denied"
	default:
		return "unknown"
	}
}

// samePayload reports whether dst already holds the move's content — a file
// of the same size or the same tree entry for entry — the re-run check for
// the crash window where the destination landed but the source removal did
// not. The extract handler's sameExtractedTree does the directory half.
func samePayload(srcInfo, dstInfo fs.FileInfo, src, dst string) (bool, error) {
	if srcInfo.IsDir() {
		if !dstInfo.IsDir() {
			return false, nil
		}
		return sameExtractedTree(src, dst)
	}

	return dstInfo.Mode().IsRegular() && dstInfo.Size() == srcInfo.Size(), nil
}
