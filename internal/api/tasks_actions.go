// The bulk-action and patch operations of docs/05-api-contract.md sections
// 5.7 and 5.5: POST /tasks/actions applies one of the nine actions to up to
// 500 ids with a per-id outcome, and PATCH /tasks/{id} updates the
// patchable columns.
package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
)

// The nine actions of docs/05-api-contract.md section 5.7. actionEnum is
// both the wire vocabulary (the schema enum tag below) and the single
// source of the action list the handler validates against.
const (
	actionPause         = "pause"
	actionResume        = "resume"
	actionRemove        = "remove"
	actionRecheck       = "recheck"
	actionForceComplete = "force_complete"
	actionQueueTop      = "queue_top"
	actionQueueUp       = "queue_up"
	actionQueueDown     = "queue_down"
	actionQueueBottom   = "queue_bottom"

	actionEnum = "pause,resume,remove,recheck,force_complete,queue_top,queue_up,queue_down,queue_bottom"

	// maxActionIDs is the batch cap of doc 05 section 5.7. The maxItems
	// schema tag enforces it for well-formed bodies; the handler enforces
	// it for the ones no schema tag can express, exactly like the create
	// endpoint's URI cap.
	maxActionIDs = 500

	// The task_events codes of the five state-changing actions.
	eventTaskPaused         = "task.paused"
	eventTaskResumed        = "task.resumed"
	eventTaskRemoved        = "task.removed"
	eventTaskRechecking     = "task.rechecking"
	eventTaskForceCompleted = "task.force_completed"

	operationTaskActions = "task-actions"
	operationPatchTask   = "patch-task"

	queryActionTasks = `SELECT id, engine, engine_ref, state FROM tasks WHERE id IN (?)`

	emptyIDsDetail      = "ids is required; send between 1 and 500 task ids"
	tooManyIDsFormat    = "ids holds %d entries; send between 1 and %d"
	unknownActionFormat = "action %q is not one of %s"

	// The per-id outcome details of doc 05 section 5.7; Type and Detail are
	// set only when Ok is false.
	detailTaskNotFound      = "the task does not exist"
	detailEngineFailed      = "the engine did not accept the action"
	detailUnsupportedAction = "the engine does not support this action"
	detailIllegalState      = "the task's state does not allow this action"
	detailNotInQueue        = "the task is not in the queue"
	detailActionFailed      = "the action could not be applied"

	emptyNameDetail     = "the display name cannot be empty"
	patchFailedDetail   = "the patch holds values that failed validation"
	negativeLimitDetail = "the limit cannot be negative; 0 means unlimited"
)

// taskActions is the action vocabulary actionEnum spells out.
var taskActions = strings.Split(actionEnum, ",")

// ActionsInput is the body of POST /tasks/actions (docs/05-api-contract.md
// section 5.7).
type ActionsInput struct {
	Body struct {
		IDs        []string `json:"ids"        minItems:"1" maxItems:"500" doc:"Task ids; one outcome per entry, in request order"`
		Action     string   `json:"action"     enum:"pause,resume,remove,recheck,force_complete,queue_top,queue_up,queue_down,queue_bottom" doc:"The action applied to every id"`
		DeleteData bool     `json:"delete_data,omitempty" doc:"Only meaningful with remove; the data unlink arrives with the delete endpoint's own task"`
	}
}

// ActionResult is one per-id outcome. Type and Detail are set only when Ok
// is false, and Type is a slug from the registry of doc 05 section 1.3.
type ActionResult struct {
	ID     string `json:"id"             doc:"The task id this outcome reports"`
	Ok     bool   `json:"ok"             doc:"Whether the action applied"`
	Type   string `json:"type,omitempty" doc:"A problem slug; set only when ok is false"`
	Detail string `json:"detail,omitempty" doc:"Why the action did not apply"`
}

// ActionsOutput is 200 whenever the batch was accepted, whatever the
// per-id outcomes.
type ActionsOutput struct {
	Body struct {
		Results []ActionResult `json:"results" doc:"One outcome per requested id, in request order"`
	}
}

// PatchTaskInput carries only the patchable fields (docs/05-api-contract.md
// section 5.5): an omitted field is untouched, and a non-nil Tags slice
// replaces the whole set — an empty array clears it. Destination is
// deliberately absent: the cross-filesystem move is owned by T076.
type PatchTaskInput struct {
	ID   string        `path:"id" doc:"The tsk_ id of the task"`
	Body PatchTaskBody `json:"-"`
}

// PatchTaskBody is the JSON body of PATCH /tasks/{id}.
type PatchTaskBody struct {
	Name             *string  `json:"name,omitempty"           doc:"Display name only; files on disk are not renamed"`
	Category         *string  `json:"category,omitempty"       doc:"Category name; must already exist"`
	Tags             []string `json:"tags,omitempty"           doc:"Replaces the whole tag set; an empty array clears it"`
	DLLimit          *int64   `json:"dl_limit,omitempty"       doc:"Bytes/second; 0 means unlimited; applied to a running task without restarting it"`
	ULLimit          *int64   `json:"ul_limit,omitempty"       doc:"As dl_limit, for the upload direction"`
	RatioLimit       *float64 `json:"ratio_limit,omitempty"    doc:"Share ratio at which seeding stops"`
	SeedingTimeLimit *int64   `json:"seeding_time_limit,omitempty" doc:"Seconds of seeding after which seeding stops"`
	Sequential       *bool    `json:"sequential,omitempty"     doc:"Download the files of a multi-file task in order"`
}

// recheckable is the optional capability an engine exposes when it can
// re-verify a task's data in place — qBittorrent's torrents/recheck, doc 06
// section 9. No M1 engine implements it (aria2 has no recheck), so against
// every engine of this milestone the action answers the per-id validation
// failure and leaves the state unchanged.
type recheckable interface {
	Recheck(ctx context.Context, id string) error
}

// actionTask is the slice of a tasks row an action needs: the engine
// routing, the engine handle and the state-machine input.
type actionTask struct {
	ID        string  `db:"id"`
	Engine    string  `db:"engine"`
	EngineRef *string `db:"engine_ref"`
	State     string  `db:"state"`
}

// Actions serves POST /tasks/actions (FR-014): one of the nine actions
// applied to up to 500 ids, with a per-id outcome so one bad id never fails
// the batch. Exactly three failures reject the whole request — an empty id
// list, more than 500 ids and an unknown action — and every other failure
// is per-id.
func (h *TaskHandlers) Actions(ctx context.Context, in *ActionsInput) (*ActionsOutput, error) {
	// Shape validation runs before any store read or engine call, so a
	// malformed batch touches nothing (doc 05 section 5.7).
	if len(in.Body.IDs) == 0 {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, emptyIDsDetail)
	}
	if len(in.Body.IDs) > maxActionIDs {
		return nil, Problem(
			SlugValidationFailed,
			http.StatusUnprocessableEntity,
			fmt.Sprintf(tooManyIDsFormat, len(in.Body.IDs), maxActionIDs),
		)
	}
	if !slices.Contains(taskActions, in.Body.Action) {
		return nil, Problem(
			SlugValidationFailed,
			http.StatusUnprocessableEntity,
			fmt.Sprintf(unknownActionFormat, in.Body.Action, actionEnum),
		)
	}

	tasks, err := h.loadActionTasks(ctx, in.Body.IDs)
	if err != nil {
		return nil, err
	}

	// The four queue actions are batch operations over the whole queue and
	// contact no engine at all; the five lifecycle actions run per-id.
	if move, ok := queueMove(in.Body.Action); ok {
		return h.queueActions(ctx, in.Body.IDs, tasks, move)
	}

	results := make([]ActionResult, 0, len(in.Body.IDs))
	for _, id := range in.Body.IDs {
		task, ok := tasks[id]
		if !ok {
			results = append(results, actionFailure(id, SlugNotFound, detailTaskNotFound))

			continue
		}
		results = append(results, h.applyAction(ctx, task, in.Body.Action))
	}

	output := &ActionsOutput{}
	output.Body.Results = results

	return output, nil
}

// loadActionTasks reads every requested id in one query (doc 05 section
// 5.7); an unknown id is reported per-id by the callers below.
func (h *TaskHandlers) loadActionTasks(ctx context.Context, ids []string) (map[string]actionTask, error) {
	query, args, err := sqlx.In(queryActionTasks, ids)
	if err != nil {
		return nil, internalFailure(ctx, "load action tasks", err)
	}

	var rows []actionTask
	if err := h.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, internalFailure(ctx, "load action tasks", err)
	}

	tasks := make(map[string]actionTask, len(rows))
	for _, row := range rows {
		tasks[row.ID] = row
	}

	return tasks, nil
}

// queueMove maps a queue action name onto its store move; ok is false for
// the five lifecycle actions.
func queueMove(action string) (store.QueueMove, bool) {
	switch action {
	case actionQueueTop:
		return store.QueueMoveTop, true
	case actionQueueUp:
		return store.QueueMoveUp, true
	case actionQueueDown:
		return store.QueueMoveDown, true
	case actionQueueBottom:
		return store.QueueMoveBottom, true
	default:
		return 0, false
	}
}

// queueActions serves the four queue actions: one store transaction
// rewrites tasks.queue_position and no engine is contacted at all, because
// dl-tool owns the queue (doc 05 section 5.7).
func (h *TaskHandlers) queueActions(
	ctx context.Context,
	ids []string,
	tasks map[string]actionTask,
	move store.QueueMove,
) (*ActionsOutput, error) {
	absent, err := h.tasks.ReorderQueue(ctx, ids, move)
	if err != nil {
		return nil, internalFailure(ctx, "reorder queue", err)
	}

	notQueued := make(map[string]bool, len(absent))
	for _, id := range absent {
		notQueued[id] = true
	}

	results := make([]ActionResult, 0, len(ids))
	for _, id := range ids {
		_, exists := tasks[id]
		switch {
		case !exists:
			results = append(results, actionFailure(id, SlugNotFound, detailTaskNotFound))
		case notQueued[id]:
			results = append(results, actionFailure(id, SlugValidationFailed, detailNotInQueue))
		default:
			results = append(results, ActionResult{ID: id, Ok: true})
		}
	}

	output := &ActionsOutput{}
	output.Body.Results = results

	return output, nil
}

// applyAction runs one lifecycle action against one task: the engine call
// first — so an engine failure leaves the state untouched — then the
// transition through the store, which writes the action's task event. A
// task already in the action's target state is an idempotent success: the
// action's "any → paused" includes the task that is paused, and a non-move
// needs neither an engine round-trip nor an event.
func (h *TaskHandlers) applyAction(ctx context.Context, task actionTask, action string) ActionResult {
	// recheck rides the optional capability interface instead of the base
	// Engine interface, and its transition happens only once an engine
	// that can re-verify accepted the request.
	if action == actionRecheck {
		return h.recheck(ctx, task)
	}

	target, code, message := actionOutcome(action)
	if task.State == target {
		return ActionResult{ID: task.ID, Ok: true}
	}

	// The engine call the action's table row names, if it names one. A task
	// the admission pass has not handed to an engine yet carries no
	// engine_ref: the action applies to dl-tool's row alone.
	if call := actionEngineCall(action); call != nil {
		e, err := h.engineFor(ctx, task)
		if err == nil && e != nil {
			err = call(ctx, e, engineTaskID(task.Engine, task.EngineRef))
		}
		if err != nil {
			return engineFailure(ctx, task.ID, err)
		}
	}

	return h.transitionAction(ctx, task, target, code, message)
}

// actionOutcome maps an action onto its target state and the task event
// the transition writes (docs/05-api-contract.md section 5.7).
func actionOutcome(action string) (target, code, message string) {
	switch action {
	case actionPause:
		return string(engine.StatePaused), eventTaskPaused, "paused by user request"
	case actionResume:
		// No engine call: the admission pass (T098) owns Engine.Resume for
		// a queued task.
		return string(engine.StateQueued), eventTaskResumed, "resumed by user request"
	case actionRemove:
		return string(engine.StateRemoved), eventTaskRemoved, "removed by user request"
	default: // actionForceComplete
		return string(engine.StateCompleted), eventTaskForceCompleted, "completed by user request"
	}
}

// actionEngineCall returns the engine method an action invokes, or nil
// for resume, which contacts no engine (doc 05 section 5.7).
func actionEngineCall(action string) func(context.Context, engine.Engine, string) error {
	switch action {
	case actionPause:
		return func(ctx context.Context, e engine.Engine, id string) error { return e.Pause(ctx, id) }
	case actionRemove, actionForceComplete:
		// Both drop the engine handle while the data is retained — exactly
		// Engine.Remove's contract.
		return func(ctx context.Context, e engine.Engine, id string) error { return e.Remove(ctx, id) }
	default: // actionResume
		return nil
	}
}

// engineFor resolves the engine a task's action must reach. It returns
// nil, nil when the admission pass has not handed the task to an engine
// yet — there is nothing to contact, the action applies to dl-tool's row
// alone — and a wrapped ErrUnavailable when the task's engine is not
// registered, so the per-id mapping reports the availability failure.
func (h *TaskHandlers) engineFor(_ context.Context, task actionTask) (engine.Engine, error) {
	if task.EngineRef == nil {
		return nil, nil
	}

	e, ok := h.engines.Get(task.Engine)
	if !ok {
		return nil, fmt.Errorf("%w: %s is not registered", engine.ErrUnavailable, task.Engine)
	}

	return e, nil
}

// engineTaskID renders the engine-namespaced task id — the TaskInfo.ID
// shape, for example "aria2:2089b05ecca3d829" — from the stored columns;
// adapters strip their own namespace again (docs/04-data-model.md 3.3).
func engineTaskID(engineName string, ref *string) string {
	if ref == nil {
		return ""
	}

	return engineName + ":" + *ref
}

// engineFailure maps an engine error onto the per-id outcome of doc 05
// section 5.7: ErrUnavailable is the engine-availability failure,
// ErrNotSupported the validation failure, anything else the internal one.
func engineFailure(ctx context.Context, id string, err error) ActionResult {
	switch {
	case errors.Is(err, engine.ErrUnavailable):
		return actionFailure(id, SlugEngineUnavailable, detailEngineFailed)
	case errors.Is(err, engine.ErrNotSupported):
		return actionFailure(id, SlugValidationFailed, detailUnsupportedAction)
	default:
		logFromContext(ctx).Error("task action engine call failed", slog.String("task_id", id), slog.Any("err", err))

		return actionFailure(id, SlugInternal, detailActionFailed)
	}
}

// transitionAction applies the action's state move through the store,
// which writes the task event in the same transaction. Arriving in the
// target state already is an idempotent success — the action table's
// "any → paused" includes the task that is paused — and no second event is
// written for a move that is not one.
func (h *TaskHandlers) transitionAction(ctx context.Context, task actionTask, target, code, message string) ActionResult {
	if task.State == target {
		return ActionResult{ID: task.ID, Ok: true}
	}

	err := h.tasks.Transition(ctx, task.ID, target, code, message)
	if err == nil {
		return ActionResult{ID: task.ID, Ok: true}
	}

	switch {
	case errors.Is(err, store.ErrNotFound):
		return actionFailure(task.ID, SlugNotFound, detailTaskNotFound)
	case errors.Is(err, store.ErrIllegalTransition), errors.Is(err, store.ErrTransitionConflict):
		return actionFailure(task.ID, SlugValidationFailed, detailIllegalState)
	default:
		logFromContext(ctx).Error("task action transition failed", slog.String("task_id", task.ID), slog.Any("err", err))

		return actionFailure(task.ID, SlugInternal, detailActionFailed)
	}
}

// recheck applies the recheck action through the optional capability
// interface; the transition to checking happens only when an engine that
// can re-verify accepted the request.
func (h *TaskHandlers) recheck(ctx context.Context, task actionTask) ActionResult {
	e, err := h.engineFor(ctx, task)
	if err != nil {
		return engineFailure(ctx, task.ID, err)
	}

	r, ok := e.(recheckable)
	if !ok {
		return actionFailure(task.ID, SlugValidationFailed, detailUnsupportedAction)
	}

	if err := r.Recheck(ctx, engineTaskID(task.Engine, task.EngineRef)); err != nil {
		return engineFailure(ctx, task.ID, err)
	}

	return h.transitionAction(ctx, task, string(engine.StateChecking), eventTaskRechecking, "recheck requested by user")
}

// actionFailure renders one failed per-id outcome.
func actionFailure(id, slug, detail string) ActionResult {
	return ActionResult{ID: id, Ok: false, Type: slug, Detail: detail}
}

// PatchTask serves PATCH /tasks/{id} (doc 05 section 5.5): the display
// name, category, tags, per-task rate limits, share limits and the
// sequential flag. Omitted fields are untouched; a non-nil tags slice
// replaces the whole set. The response is the full updated Task object.
func (h *TaskHandlers) PatchTask(ctx context.Context, in *PatchTaskInput) (*GetTaskOutput, error) {
	task, err := h.tasks.Get(ctx, in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, Problem(SlugNotFound, http.StatusNotFound, detailTaskNotFound)
	}
	if err != nil {
		return nil, internalFailure(ctx, "get task for patch", err)
	}

	patch, fieldErrs, err := h.buildTaskPatch(ctx, in.Body)
	if err != nil {
		return nil, err
	}
	if len(fieldErrs) > 0 {
		problem := Problem(SlugValidationFailed, http.StatusUnprocessableEntity, patchFailedDetail)
		var model *huma.ErrorModel
		errors.As(problem, &model)
		model.Errors = fieldErrs

		return nil, problem
	}

	// The live application runs before the store write: an engine that
	// cannot take the new limit fails the request with nothing persisted.
	if err := h.applyLiveRateLimits(ctx, task, in.Body.DLLimit, in.Body.ULLimit); err != nil {
		return nil, err
	}

	// A tags-only patch carries no column; the tag rewrite below is the
	// whole change, and Update answers an empty patch with an error.
	if !patch.Empty() {
		if err := h.tasks.Update(ctx, in.ID, patch); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, Problem(SlugNotFound, http.StatusNotFound, detailTaskNotFound)
			}

			return nil, internalFailure(ctx, "patch task", err)
		}
	}

	if in.Body.Tags != nil {
		if err := h.replaceTaskTags(ctx, in.ID, in.Body.Tags); err != nil {
			return nil, internalFailure(ctx, "replace task tags", err)
		}
	}

	updated, err := h.tasks.Get(ctx, in.ID)
	if err != nil {
		return nil, internalFailure(ctx, "reread patched task", err)
	}
	items, err := h.renderTasks(ctx, []store.Task{updated})
	if err != nil {
		return nil, err
	}

	dto := items[0]
	// store.Task does not carry the four limit columns yet — their read
	// path arrives with the tasks that own them — so the patch response
	// reads them from the row the patch produced.
	limits, err := h.taskLimits(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	dto.DLLimit = limits.DLLimit
	dto.ULLimit = limits.ULLimit
	dto.RatioLimit = limits.RatioLimit
	dto.SeedingTimeLimit = limits.SeedingTimeLimit

	return &GetTaskOutput{Body: dto}, nil
}

// patchedLimits is the four limit columns of one tasks row.
type patchedLimits struct {
	DLLimit          int64    `db:"dl_limit"`
	ULLimit          int64    `db:"ul_limit"`
	RatioLimit       *float64 `db:"ratio_limit"`
	SeedingTimeLimit *int64   `db:"seeding_time_limit"`
}

const queryTaskLimits = `SELECT dl_limit, ul_limit, ratio_limit, seeding_time_limit FROM tasks WHERE id = ?`

// taskLimits reads a task's persisted limits.
func (h *TaskHandlers) taskLimits(ctx context.Context, id string) (patchedLimits, error) {
	var limits patchedLimits
	if err := h.db.GetContext(ctx, &limits, queryTaskLimits, id); err != nil {
		return patchedLimits{}, internalFailure(ctx, "read task limits", err)
	}

	return limits, nil
}

// buildTaskPatch validates the patchable fields and builds the store
// patch. Every field-level violation is collected into fieldErrs so one
// response can name them all; err is reserved for the internal failures a
// field error must not mask.
func (h *TaskHandlers) buildTaskPatch(
	ctx context.Context,
	body PatchTaskBody,
) (patch store.TaskPatch, fieldErrs []*huma.ErrorDetail, err error) {
	if body.Name != nil {
		if *body.Name == "" {
			fieldErrs = append(fieldErrs, &huma.ErrorDetail{Message: emptyNameDetail, Location: "body.name"})
		} else {
			patch.Name = body.Name
		}
	}

	if body.Category != nil {
		var id string
		qErr := h.db.GetContext(ctx, &id, queryCategoryIDByName, *body.Category)
		switch {
		case errors.Is(qErr, sql.ErrNoRows):
			fieldErrs = append(fieldErrs, &huma.ErrorDetail{
				Message:  fmt.Sprintf(unknownCategoryFormat, *body.Category),
				Location: "body.category",
			})
		case qErr != nil:
			return patch, nil, internalFailure(ctx, "resolve patch category", qErr)
		default:
			patch.CategoryID = &id
		}
	}

	if body.DLLimit != nil {
		if *body.DLLimit < 0 {
			fieldErrs = append(fieldErrs, &huma.ErrorDetail{Message: negativeLimitDetail, Location: "body.dl_limit"})
		} else {
			patch.DLLimit = body.DLLimit
		}
	}
	if body.ULLimit != nil {
		if *body.ULLimit < 0 {
			fieldErrs = append(fieldErrs, &huma.ErrorDetail{Message: negativeLimitDetail, Location: "body.ul_limit"})
		} else {
			patch.ULLimit = body.ULLimit
		}
	}
	if body.RatioLimit != nil {
		if *body.RatioLimit < 0 {
			fieldErrs = append(fieldErrs, &huma.ErrorDetail{Message: negativeLimitDetail, Location: "body.ratio_limit"})
		} else {
			patch.RatioLimit = body.RatioLimit
		}
	}
	if body.SeedingTimeLimit != nil {
		if *body.SeedingTimeLimit < 0 {
			fieldErrs = append(fieldErrs, &huma.ErrorDetail{Message: negativeLimitDetail, Location: "body.seeding_time_limit"})
		} else {
			patch.SeedingTimeLimit = body.SeedingTimeLimit
		}
	}

	patch.Sequential = body.Sequential

	return patch, fieldErrs, nil
}

// applyLiveRateLimits pushes new rate limits to the engine that holds the
// task without restarting it: both of aria2's changeOption limits are on
// its safe list (doc 06 section 4.3). A task the admission pass has not
// handed to an engine yet gets its limits at admission time instead.
// ErrNotSupported is not a failure: the persisted limit then applies the
// next time the engine starts the task.
func (h *TaskHandlers) applyLiveRateLimits(ctx context.Context, task store.Task, down, up *int64) error {
	if task.EngineRef == nil || (down == nil && up == nil) {
		return nil
	}

	e, ok := h.engines.Get(task.Engine)
	if !ok {
		return engineUnavailable(task.Engine)
	}

	err := e.SetRateLimits(ctx, engineTaskID(task.Engine, task.EngineRef), down, up)
	if errors.Is(err, engine.ErrNotSupported) {
		return nil
	}
	if err != nil {
		return Problem(SlugEngineUnavailable, http.StatusServiceUnavailable, detailEngineFailed)
	}

	return nil
}

// replaceTaskTags rewrites the task's task_tags rows in one transaction:
// the whole set is replaced, so an empty slice clears every link
// (doc 05 section 5.5). The tag rows themselves are created on demand
// before the transaction starts.
func (h *TaskHandlers) replaceTaskTags(ctx context.Context, taskID string, names []string) error {
	if err := h.ensureTags(ctx, names); err != nil {
		return fmt.Errorf("ensure tags: %w", err)
	}

	tx, err := h.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tag rewrite of task %q: %w", taskID, err)
	}
	// Rolls back on any early return; after Commit this is sql.ErrTxDone,
	// which is the expected outcome and not worth a warning.
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			logFromContext(ctx).Warn("rollback of tag rewrite failed", slog.String("task_id", taskID), slog.Any("err", err))
		}
	}()

	if _, err := tx.ExecContext(ctx, `DELETE FROM task_tags WHERE task_id = ?`, taskID); err != nil {
		return fmt.Errorf("clear tags of task %q: %w", taskID, err)
	}
	for _, name := range names {
		var tagID string
		if err := tx.GetContext(ctx, &tagID, queryTagIDByName, name); err != nil {
			return fmt.Errorf("resolve tag %q: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, queryInsertTaskTag, taskID, tagID); err != nil {
			return fmt.Errorf("link tag %q: %w", name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tag rewrite of task %q: %w", taskID, err)
	}

	return nil
}
