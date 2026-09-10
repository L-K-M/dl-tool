// The file listing and selection operations of docs/05-api-contract.md
// section 5.8: GET /tasks/{id}/files lists a task's files with their
// selection and priority, and PATCH /tasks/{id}/files changes them on the
// running engine and in task_files. The wire vocabulary is skip, normal,
// high, maximum; the stored integers are 0, 1, 6, 7 (docs/04-data-model.md
// section 4.3), and the mapping between them lives here and nowhere else.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	operationListTaskFiles  = "list-task-files"
	operationPatchTaskFiles = "patch-task-files"

	filesDetailUnknownTask        = "the addressed task does not exist"
	filesDetailEngineFailed       = "the engine did not accept the file change"
	filesDetailNoPrioritySupport  = "the task's engine does not support per-file priority"
	filesDetailNotAdmitted        = "the task has not been handed to its engine yet; select files at creation time instead"
	filesDetailEntryIncomplete    = "each entry needs at least one of selected and priority"
	filesDetailInvalid            = "the file selection holds entries that failed validation"
	filesDetailUnknownIndexFormat = "file index %d is not part of the task"
	filesDetailUnknownPriority    = "priority is not one of skip, normal, high, maximum"
	filesDetailDisagree           = "selected and priority disagree; they are one concept, so send one value"
	filesDetailListingMoved       = "the task's file listing changed underneath the request; get the listing and retry"
)

// The stored priority integers of docs/04-data-model.md section 4.3, the
// qBittorrent WebAPI vocabulary. There is no 4: it is libtorrent's
// internal scale, rejected before any engine call.
const (
	prioritySkip    = 0
	priorityNormal  = 1
	priorityHigh    = 6
	priorityMaximum = 7
)

// priorityNames maps the stored integers of 04 section 4.3 onto the wire
// vocabulary of 05 section 5.8. There is no "low": Download Station's
// four-level scale collapses low onto normal.
var priorityNames = map[int]string{prioritySkip: "skip", priorityNormal: "normal", priorityHigh: "high", priorityMaximum: "maximum"}

// priorityValues is priorityNames inverted, the PATCH body's decoder.
var priorityValues = map[string]int{"skip": prioritySkip, "normal": priorityNormal, "high": priorityHigh, "maximum": priorityMaximum}

// TaskFileDTO is one file of the listing. path is relative to
// tasks.destination; priority is null for an engine that does not declare
// per_file_priority, which drives selected alone (aria2's shape).
type TaskFileDTO struct {
	Index          int     `json:"index"`
	Path           string  `json:"path" doc:"Relative to the task's destination"`
	SizeBytes      int64   `json:"size_bytes"`
	CompletedBytes int64   `json:"completed_bytes"`
	Progress       float64 `json:"progress"`
	Selected       bool    `json:"selected"`
	Priority       *string `json:"priority" enum:"skip,normal,high,maximum" doc:"Null when the engine has no per-file priority"`
}

// ListTaskFilesInput addresses one task by id.
type ListTaskFilesInput struct {
	ID string `path:"id" doc:"The tsk_ id of the task"`
}

// ListTaskFilesOutput is the full file list both verbs return.
type ListTaskFilesOutput struct {
	Body struct {
		Files []TaskFileDTO `json:"files"`
	}
}

// FileSelection is one entry of the PATCH body. Selected and Priority are
// each optional, but at least one must be present. selected:false and
// priority:"skip" are one concept: setting either sets both.
type FileSelection struct {
	Index    int     `json:"index"    minimum:"0" doc:"0-based file index of the listing"`
	Selected *bool   `json:"selected,omitempty" doc:"Deselect is priority skip; select without a priority is normal"`
	Priority *string `json:"priority,omitempty" enum:"skip,normal,high,maximum"`
}

// PatchTaskFilesInput carries the selection entries.
type PatchTaskFilesInput struct {
	ID   string `path:"id" doc:"The tsk_ id of the task"`
	Body struct {
		Files []FileSelection `json:"files" minItems:"1" doc:"Unlisted indices are untouched"`
	}
}

// ListTaskFiles serves GET /tasks/{id}/files (doc 05 section 5.8): the
// engine's listing lands in task_files and the answer comes from the
// store, so the response shape is identical to PATCH's and survives a
// briefly down engine on the rows the last listing left behind.
func (h *TaskHandlers) ListTaskFiles(ctx context.Context, in *ListTaskFilesInput) (*ListTaskFilesOutput, error) {
	task, e, err := h.taskWithEngine(ctx, in.ID)
	if err != nil {
		return nil, err
	}

	if err := h.refreshTaskFiles(ctx, task, e); err != nil {
		return nil, err
	}

	files, err := h.taskFileDTOs(ctx, task, e)
	if err != nil {
		return nil, err
	}

	output := &ListTaskFilesOutput{}
	output.Body.Files = files

	return output, nil
}

// PatchTaskFiles serves PATCH /tasks/{id}/files (doc 05 section 5.8).
// Every entry is validated and resolved first — selected:false and
// priority:"skip" set each other — then one Engine.SetFiles call applies
// the change at the engine, UpdateFileSelection mirrors it in task_files,
// and the answer is the full list from the store, exactly the body GET
// answers with.
func (h *TaskHandlers) PatchTaskFiles(ctx context.Context, in *PatchTaskFilesInput) (*ListTaskFilesOutput, error) {
	task, e, err := h.taskWithEngine(ctx, in.ID)
	if err != nil {
		return nil, err
	}

	if !hasCapability(e, engine.CapPerFilePriority) {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, filesDetailNoPrioritySupport)
	}
	if task.EngineRef == nil {
		// No engine holds the transfer, so no engine can take a selection;
		// the create-time selection of T033 is the moment for it.
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, filesDetailNotAdmitted)
	}

	rows, err := h.tasks.ListFiles(ctx, in.ID)
	if err != nil {
		return nil, internalFailure(ctx, "list task files", err)
	}
	if len(rows) == 0 {
		// The task was never listed: pull the engine listing so index
		// validation judges the real file set instead of an empty one.
		if err := h.refreshTaskFiles(ctx, task, e); err != nil {
			return nil, err
		}
		if rows, err = h.tasks.ListFiles(ctx, in.ID); err != nil {
			return nil, internalFailure(ctx, "list task files", err)
		}
	}

	priorities, selection, fieldErrs := resolveFileSelection(rows, in.Body.Files)
	if len(fieldErrs) > 0 {
		return nil, fileSelectionProblem(fieldErrs)
	}

	// Engine first: a daemon that cannot take the change leaves task_files
	// untouched. selected stays nil — every entry carries a resolved
	// priority, and a nil selection leaves the unlisted indices alone.
	engineID := engineTaskID(task.Engine, task.EngineRef)
	if err := e.SetFiles(ctx, engineID, nil, priorities); err != nil {
		if errors.Is(err, engine.ErrNotSupported) {
			return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, filesDetailNoPrioritySupport)
		}

		return nil, Problem(SlugEngineUnavailable, http.StatusServiceUnavailable, filesDetailEngineFailed)
	}

	if err := h.tasks.UpdateFileSelection(ctx, in.ID, selection); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The row set moved under the validated listing; the honest
			// answer is the same validation failure shape, naming the retry.
			return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, filesDetailListingMoved)
		}

		return nil, internalFailure(ctx, "update file selection", err)
	}

	files, err := h.taskFileDTOs(ctx, task, e)
	if err != nil {
		return nil, err
	}

	output := &ListTaskFilesOutput{}
	output.Body.Files = files

	return output, nil
}

// taskWithEngine loads the task and resolves the engine that holds it:
// 404 for an unknown task, 503 when the task's engine is not registered
// at all — the same availability answer the mutation paths give.
func (h *TaskHandlers) taskWithEngine(ctx context.Context, id string) (store.Task, engine.Engine, error) {
	task, err := h.tasks.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Task{}, nil, Problem(SlugNotFound, http.StatusNotFound, filesDetailUnknownTask)
	}
	if err != nil {
		return store.Task{}, nil, internalFailure(ctx, "get task", err)
	}

	e, ok := h.engines.Get(task.Engine)
	if !ok {
		return store.Task{}, nil, engineUnavailable(task.Engine)
	}

	return task, e, nil
}

// refreshTaskFiles pulls the engine listing into task_files. A task no
// engine holds yet has nothing to list and keeps whatever rows it has. A
// failed engine call is a warning while the store still holds a listing —
// the stable answer of doc 05 section 5.8 — and the 503 only when there
// is nothing stored to serve.
func (h *TaskHandlers) refreshTaskFiles(ctx context.Context, task store.Task, e engine.Engine) error {
	if task.EngineRef == nil {
		return nil
	}

	entries, err := e.Files(ctx, engineTaskID(task.Engine, task.EngineRef))
	if err != nil {
		stored, listErr := h.tasks.ListFiles(ctx, task.ID)
		if listErr != nil {
			return internalFailure(ctx, "list task files", listErr)
		}
		if len(stored) > 0 {
			logFromContext(ctx).Warn("task file listing served from the store; the engine call failed",
				slog.String("task_id", task.ID), slog.Any("err", err))

			return nil
		}

		return Problem(SlugEngineUnavailable, http.StatusServiceUnavailable, filesDetailEngineFailed)
	}

	files := make([]store.TaskFile, 0, len(entries))
	for _, entry := range entries {
		files = append(files, store.TaskFile{
			TaskID:         task.ID,
			FileIndex:      entry.Index,
			Path:           entry.Path,
			SizeBytes:      entry.Size,
			CompletedBytes: entry.Completed,
			Selected:       boolToInt(entry.Selected),
			Priority:       entry.Priority,
		})
	}
	if err := h.tasks.UpsertFiles(ctx, task.ID, files); err != nil {
		return internalFailure(ctx, "upsert task files", err)
	}

	return nil
}

// taskFileDTOs renders the store's rows as the wire listing. Priority is
// named only for an engine that declares per_file_priority; every file of
// one that does not reports null and drives selected alone (06 section
// 1.1, the aria2 shape).
func (h *TaskHandlers) taskFileDTOs(ctx context.Context, task store.Task, e engine.Engine) ([]TaskFileDTO, error) {
	rows, err := h.tasks.ListFiles(ctx, task.ID)
	if err != nil {
		return nil, internalFailure(ctx, "list task files", err)
	}

	prioritised := hasCapability(e, engine.CapPerFilePriority)
	files := make([]TaskFileDTO, 0, len(rows))
	for _, row := range rows {
		var priority *string
		if prioritised && row.Priority != nil {
			// A stored value outside the vocabulary cannot exist (the DDL
			// CHECK pins 0, 1, 6, 7); leaving priority null for a nameless
			// value keeps the renderer total regardless.
			if name, ok := priorityNames[*row.Priority]; ok {
				priority = &name
			}
		}
		files = append(files, TaskFileDTO{
			Index:          row.FileIndex,
			Path:           row.Path,
			SizeBytes:      row.SizeBytes,
			CompletedBytes: row.CompletedBytes,
			Progress:       fileProgress(row.SizeBytes, row.CompletedBytes),
			Selected:       row.Selected == 1,
			Priority:       priority,
		})
	}

	return files, nil
}

// resolveFileSelection validates every PATCH entry against the known
// listing and resolves it onto one (selected, priority) pair: a priority
// implies its selection, a selection without a priority is normal when
// true and skip when false, and an entry carrying both must agree. The
// returned maps share the entry's index key: priorities feeds the engine
// call, selection the store write.
func resolveFileSelection(rows []store.TaskFile, entries []FileSelection) (
	priorities map[int]int,
	selection map[int]store.TaskFileSelection,
	fieldErrs []*huma.ErrorDetail,
) {
	known := make(map[int]struct{}, len(rows))
	for _, row := range rows {
		known[row.FileIndex] = struct{}{}
	}

	priorities = make(map[int]int, len(entries))
	selection = make(map[int]store.TaskFileSelection, len(entries))
	for i, entry := range entries {
		location := fmt.Sprintf("body.files[%d]", i)

		if entry.Selected == nil && entry.Priority == nil {
			fieldErrs = append(fieldErrs, &huma.ErrorDetail{
				Message: filesDetailEntryIncomplete, Location: location,
			})

			continue
		}
		if _, ok := known[entry.Index]; !ok {
			fieldErrs = append(fieldErrs, &huma.ErrorDetail{
				Message: fmt.Sprintf(filesDetailUnknownIndexFormat, entry.Index), Location: location + ".index",
			})

			continue
		}

		priority := priorityNormal
		selected := true
		if entry.Priority != nil {
			value, ok := priorityValues[*entry.Priority]
			if !ok {
				fieldErrs = append(fieldErrs, &huma.ErrorDetail{
					Message: filesDetailUnknownPriority, Location: location + ".priority",
				})

				continue
			}
			priority = value
			selected = value != prioritySkip
		}
		if entry.Selected != nil {
			if entry.Priority != nil && *entry.Selected != selected {
				fieldErrs = append(fieldErrs, &huma.ErrorDetail{
					Message: filesDetailDisagree, Location: location,
				})

				continue
			}
			selected = *entry.Selected
			if entry.Priority == nil {
				priority = priorityNormal
				if !selected {
					priority = prioritySkip
				}
			}
		}

		priorities[entry.Index] = priority
		selection[entry.Index] = store.TaskFileSelection{Selected: boolToInt(selected), Priority: &priority}
	}

	return priorities, selection, fieldErrs
}

// fileSelectionProblem renders the collected field errors as one 422;
// the per-entry reason lives in errors[].
func fileSelectionProblem(fieldErrs []*huma.ErrorDetail) error {
	problem := Problem(SlugValidationFailed, http.StatusUnprocessableEntity, filesDetailInvalid)
	var model *huma.ErrorModel
	errors.As(problem, &model)
	model.Errors = fieldErrs

	return problem
}

// fileProgress derives progress as completed / size, 0.0 while the size is
// unknown or zero (the same derivation the Task object carries).
func fileProgress(size, completed int64) float64 {
	if size == 0 {
		return 0
	}

	return min(float64(completed)/float64(size), 1.0)
}

// hasCapability reports whether an engine declares a capability.
func hasCapability(e engine.Engine, want engine.Capability) bool {
	return slices.Contains(e.Capabilities(), want)
}
