// The DELETE /tasks/{id} operation of docs/05-api-contract.md section 5.6:
// the only irreversible operation in the product. The six steps run in the
// contract's exact order and are never approximated — every target is
// validated before any side effect, the engine handle is gone before the
// first unlink, and the events and the tombstone land in one transaction.
package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/danielgtaylor/huma/v2"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/fsx"
	"github.com/L-K-M/dl-tool/internal/store"
)

const operationDeleteTask = "delete-task"

const (
	bothDeleteFlagsDetail = "delete_data and force_complete are mutually exclusive"

	pathRejectedDetail = "%d recorded path(s) resolve outside every configured data root"
	outsideRootMessage = "resolves outside every configured data root"
)

// DeleteTaskInput carries the two mutually exclusive query flags of
// docs/05-api-contract.md section 5.6.
type DeleteTaskInput struct {
	ID            string `path:"id"            doc:"The tsk_ id of the task"`
	DeleteData    bool   `query:"delete_data"    doc:"Also unlink the downloaded data; the only irreversible operation in the product"`
	ForceComplete bool   `query:"force_complete" doc:"Move the incomplete data to the destination and mark the task completed instead of removing it"`
}

// DeleteTaskOutput reports the outcome. Missing counts files recorded in
// task_files that were already gone; that is not an error.
type DeleteTaskOutput struct {
	Body struct {
		Removed       bool  `json:"removed"        doc:"Whether the task entered the removed state"`
		DeleteData    bool  `json:"delete_data"    doc:"Whether the recorded files were unlinked"`
		FilesUnlinked int   `json:"files_unlinked" doc:"Files actually unlinked"`
		BytesUnlinked int64 `json:"bytes_unlinked" doc:"Recorded byte total of the unlinked files"`
		Missing       int   `json:"missing"        doc:"Recorded files that were already gone"`
	}
}

// deleteTarget is one recorded file resolved against tasks.destination,
// with the recorded size the byte total is summed from (step 1).
type deleteTarget struct {
	path  string
	bytes int64
}

// DeleteTask serves DELETE /tasks/{id}. delete_data=false (the default)
// skips steps 1, 2 and 4: the retained task enters removed and every byte
// stays. delete_data=true unlinks exactly the recorded files after
// re-checking every resolved path. force_complete=true completes the task
// instead of removing it, with no unlink at all.
func (h *TaskHandlers) DeleteTask(ctx context.Context, in *DeleteTaskInput) (*DeleteTaskOutput, error) {
	// The flags are mutually exclusive; rejecting the pair first means a
	// contradictory request touches no row, no engine and no file.
	if in.DeleteData && in.ForceComplete {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, bothDeleteFlagsDetail)
	}

	task, err := h.tasks.Get(ctx, in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, Problem(SlugNotFound, http.StatusNotFound, detailTaskNotFound)
	}
	if err != nil {
		return nil, internalFailure(ctx, "get task for delete", err)
	}

	if in.ForceComplete {
		return h.forceCompleteTask(ctx, task)
	}
	// Step 1: enumerate the targets — only task_files rows resolved against
	// tasks.destination, never a glob, never a directory walk and never
	// content_path alone.
	files, err := h.tasks.ListFiles(ctx, task.ID)
	if err != nil {
		return nil, internalFailure(ctx, "list task files", err)
	}
	targets := recordedTargets(task.Destination, files)

	var output DeleteTaskOutput

	if in.DeleteData {
		// Step 2: validate every resolved path, symlinks included, before
		// any side effect. One escaping path aborts the whole request with
		// the engine and the filesystem untouched.
		if problem := validateTargets(h.roots, targets); problem != nil {
			return nil, problem
		}

		// Step 3: remove the engine handle — Pause then Remove, which
		// always retains payload data — so no file is still open by the
		// engine when the unlinks start.
		if err := h.removeEngineHandle(ctx, task, detachAfterPause); err != nil {
			return nil, err
		}

		// Step 4: one unlink per recorded file, then the task's own
		// directory only while it is empty.
		files, bytes, missing := unlinkTargets(ctx, targets)
		removeOwnDir(ctx, h.roots, task.Destination, targets)

		output.Body.DeleteData = true
		output.Body.FilesUnlinked = files
		output.Body.BytesUnlinked = bytes
		output.Body.Missing = missing
	} else if err := h.removeEngineHandle(ctx, task, detachAfterPause); err != nil {
		return nil, err
	}

	// Steps 5 and 6: the task.data_deleted event (delete_data only), the
	// task.removed event and the tombstone columns, in one transaction.
	var data *store.DeletedData
	if in.DeleteData {
		data = &store.DeletedData{Files: output.Body.FilesUnlinked, Bytes: output.Body.BytesUnlinked}
	}
	if err := h.tasks.MarkRemoved(ctx, task.ID, data); err != nil {
		return nil, removalProblem(ctx, "remove task", err)
	}

	output.Body.Removed = true

	return &output, nil
}

// forceCompleteTask is the force_complete flag (doc 05 section 5.6): the
// engine handle is dropped with the data retained and the task moves to
// completed instead of removed. Nothing is unlinked at all, so the response
// reports no removal and no deletion.
func (h *TaskHandlers) forceCompleteTask(ctx context.Context, task store.Task) (*DeleteTaskOutput, error) {
	if err := h.removeEngineHandle(ctx, task, detachDirectly); err != nil {
		return nil, err
	}

	if task.State != string(engine.StateCompleted) {
		err := h.tasks.Transition(ctx, task.ID, string(engine.StateCompleted), eventTaskForceCompleted, "completed by user request")
		if err != nil {
			return nil, removalProblem(ctx, "force-complete task", err)
		}
	}

	return &DeleteTaskOutput{}, nil
}

// recordedTargets resolves every recorded file against the task's
// destination (doc 05 section 5.6 step 1). filepath.Join cleans the result,
// so a recorded "../.." is folded to where it really points before the
// validation of step 2 judges it.
func recordedTargets(destination string, files []store.TaskFile) []deleteTarget {
	targets := make([]deleteTarget, 0, len(files))
	for _, file := range files {
		targets = append(targets, deleteTarget{
			path:  filepath.Join(destination, file.Path),
			bytes: file.SizeBytes,
		})
	}

	return targets
}

// validateTargets re-checks every resolved target against the configured
// roots (step 2), collecting every failure first: one escaping path aborts
// the whole request before the engine or the filesystem is touched.
func validateTargets(roots []string, targets []deleteTarget) error {
	var failures []string
	for _, target := range targets {
		if _, err := fsx.ResolveDestination(roots, target.path); err != nil {
			failures = append(failures, target.path)
		}
	}
	if len(failures) == 0 {
		return nil
	}

	details := make([]*huma.ErrorDetail, 0, len(failures))
	for _, path := range failures {
		details = append(details, &huma.ErrorDetail{Message: outsideRootMessage, Value: path})
	}

	return &huma.ErrorModel{
		Type:   SlugPathRejected,
		Title:  http.StatusText(http.StatusForbidden),
		Status: http.StatusForbidden,
		Detail: fmt.Sprintf(pathRejectedDetail, len(failures)),
		Errors: details,
	}
}

// engineDetachMode names the two step-3 shapes: the unlinking delete
// path pauses first so the engine closes its files before the first
// unlink, while force_complete drops the handle without pausing, exactly
// like the bulk action of the same name (doc 05 section 5.7).
type engineDetachMode int

const (
	detachAfterPause engineDetachMode = iota
	detachDirectly
)

// removeEngineHandle removes the engine handle (doc 05 section 5.6
// step 3). Remove always instructs the engine to retain payload data and
// must finish before the first unlink. A task the admission pass has not
// handed to an engine yet carries no engine_ref and needs no round-trip.
// An unavailable engine is 503 with the row and every byte left in place.
func (h *TaskHandlers) removeEngineHandle(ctx context.Context, task store.Task, mode engineDetachMode) error {
	if task.EngineRef == nil {
		return nil
	}

	e, ok := h.engines.Get(task.Engine)
	if !ok {
		return Problem(SlugEngineUnavailable, http.StatusServiceUnavailable, detailEngineFailed)
	}

	id := engineTaskID(task.Engine, task.EngineRef)
	if mode == detachAfterPause {
		if err := e.Pause(ctx, id); err != nil {
			return engineFailureProblem(ctx, "pause task for removal", task.ID, err)
		}
	}
	if err := e.Remove(ctx, id); err != nil {
		return engineFailureProblem(ctx, "remove task from engine", task.ID, err)
	}

	return nil
}

// engineFailureProblem maps a step-3 engine error: ErrUnavailable is the
// 503 of doc 05 section 5.6, anything else an internal failure — either way
// nothing has been unlinked yet.
func engineFailureProblem(ctx context.Context, operation, taskID string, err error) error {
	if errors.Is(err, engine.ErrUnavailable) {
		return Problem(SlugEngineUnavailable, http.StatusServiceUnavailable, detailEngineFailed)
	}

	return internalFailure(ctx, operation, fmt.Errorf("task %s: %w", taskID, err))
}

// unlinkTargets performs step 4: one unlink per recorded file, counting
// what happened. A recorded file that no longer exists is missing, not an
// error. A hardlinked file's removal is expected and safe: the library
// copy survives on its own name (ADR-0012), which is why nothing here
// detects or warns about hardlinks. An unlink failing for any other reason
// is logged and left in place — the operation is irreversible, so it
// continues and the counts report what was actually unlinked.
func unlinkTargets(ctx context.Context, targets []deleteTarget) (files int, bytes int64, missing int) {
	for _, target := range targets {
		switch err := os.Remove(target.path); {
		case err == nil:
			files++
			bytes += target.bytes
		case errors.Is(err, fs.ErrNotExist):
			missing++
		default:
			logFromContext(ctx).Warn("unlink of recorded task file failed",
				slog.String("path", target.path), slog.Any("err", err))
		}
	}

	return files, bytes, missing
}

// removeOwnDir takes the task's own directory with it, only while it is
// empty: os.Remove of a directory fails while anything lives inside it,
// which is exactly that rule, so a non-empty directory is simply left in
// place. The destination itself is never the task's own, and the directory
// must still resolve inside the roots like every other target.
func removeOwnDir(ctx context.Context, roots []string, destination string, targets []deleteTarget) {
	dir := ownDir(destination, targets)
	if dir == "" {
		return
	}
	if _, err := fsx.ResolveDestination(roots, dir); err != nil {
		logFromContext(ctx).Warn("task's own directory does not resolve inside the data roots; leaving it in place",
			slog.String("dir", dir), slog.Any("err", err))

		return
	}

	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logFromContext(ctx).Warn("removal of the task's own directory failed",
			slog.String("dir", dir), slog.Any("err", err))
	}
}

// ownDir is the one directory every recorded target shares when it is not
// the destination itself — the only directory that belongs to the task
// alone. It is "" when the targets span several directories or sit
// directly in the shared destination.
func ownDir(destination string, targets []deleteTarget) string {
	if len(targets) == 0 {
		return ""
	}

	dir := filepath.Dir(targets[0].path)
	for _, target := range targets[1:] {
		if filepath.Dir(target.path) != dir {
			return ""
		}
	}
	if dir == filepath.Clean(destination) {
		return ""
	}

	return dir
}

// removalProblem maps the MarkRemoved and force-complete transition errors:
// an unknown id is 404 and an illegal move 422; everything else is internal.
func removalProblem(ctx context.Context, operation string, err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return Problem(SlugNotFound, http.StatusNotFound, detailTaskNotFound)
	case errors.Is(err, store.ErrIllegalTransition), errors.Is(err, store.ErrTransitionConflict):
		return Problem(SlugValidationFailed, http.StatusUnprocessableEntity, detailIllegalState)
	default:
		return internalFailure(ctx, operation, err)
	}
}
