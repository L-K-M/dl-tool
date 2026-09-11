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
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/fsx"
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

	// detailTaskOpBusy is the pause action's answer when its wait for the
	// shared task-operation lease outlives pauseLeaseWait: contention, not
	// failure — the per-id current-state failure type, never
	// engine-unavailable.
	detailTaskOpBusy = "another task operation is in progress; retry"

	emptyNameDetail     = "the display name cannot be empty"
	patchFailedDetail   = "the patch holds values that failed validation"
	negativeLimitDetail = "the limit cannot be negative; 0 means unlimited"

	// detailSequentialUnsupported is the 422 of a sequential patch
	// against an engine that declares no such capability: the persisted
	// flag could never be honoured, so the request is refused before any
	// engine call or store write (doc 05 section 5.5).
	detailSequentialUnsupported = "the task's engine does not declare sequential download"

	// detailMutatorUnsupported is the 422 of a patch field whose
	// engine-side setter the registered engine does not carry — the
	// narrowed-interface refusal of tagMutator and sequentialEngine.
	detailMutatorUnsupported = "the task's engine does not expose this capability"

	// eventTaskMoved is the task_events code of the moving transition a
	// relocated task enters (doc 05 section 5.5).
	eventTaskMoved   = "task.moved"
	messageTaskMoved = "destination changed; the engine was told the new location"

	// The two settings keys of the concurrency limits and their defaults
	// (docs/11-config-reference.md section 5). The initial migration seeds
	// both keys, so a missing row is a fresh or hand-edited database.
	settingMaxActiveTotal     = "max_active_total"
	settingMaxActivePerEngine = "max_active_per_engine"
	defaultMaxActiveTotal     = 5
	defaultMaxActivePerEngine = 3

	queryConcurrencySettings = `SELECT key, value_json FROM settings WHERE key IN (?, ?)`
)

// pauseLeaseWait is the operator-response budget a pause action spends
// waiting for the shared task-operation lease (T128's registry table):
// deliberately shorter than an adapter's worst-case RPC sequence, so a
// slow healthy holder — an admission release mid-flight — makes the
// action ask for a retry instead of holding the request open. The lease
// wait runs under this timeout, never the request context alone.
const pauseLeaseWait = 5 * time.Second

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
// replaces the whole set — an empty array clears it. Destination tells the
// engine the new location and enters moving; the cross-filesystem move
// itself is owned by T076.
type PatchTaskInput struct {
	ID   string        `path:"id" doc:"The tsk_ id of the task"`
	Body PatchTaskBody `json:"-"`
}

// PatchTaskBody is the JSON body of PATCH /tasks/{id}.
type PatchTaskBody struct {
	Name             *string  `json:"name,omitempty"           doc:"Display name only; files on disk are not renamed"`
	Destination      *string  `json:"destination,omitempty"     doc:"Moves the data; must resolve inside a configured root; the task enters moving"`
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

// tagMutator is implemented by an engine that can replace a task's whole
// tag set in one call. Declared here, at the consumer, so the Engine
// interface of docs/06-download-engines.md section 1 stays unchanged —
// the same pattern as trackerEngine and peerEngine.
type tagMutator interface {
	SetTags(ctx context.Context, id string, tags []string) error
}

// sequentialEngine is implemented by an engine that can flip a task's
// sequential download flag; declared at the consumer for the same reason
// as tagMutator.
type sequentialEngine interface {
	SetSequential(ctx context.Context, id string, sequential bool) error
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

	// One concurrency snapshot serves a whole resume batch: the action
	// requeues and the admission pass (T098) releases, so the per-id
	// headroom answer is advisory and one read is exact enough for every
	// id of the request.
	var snap *concurrencySnapshot
	if in.Body.Action == actionResume {
		snap, err = h.loadConcurrencySnapshot(ctx)
		if err != nil {
			return nil, err
		}
	}

	results := make([]ActionResult, 0, len(in.Body.IDs))
	for _, id := range in.Body.IDs {
		task, ok := tasks[id]
		if !ok {
			results = append(results, actionFailure(id, SlugNotFound, detailTaskNotFound))

			continue
		}
		results = append(results, h.applyAction(ctx, task, in.Body.Action, snap))
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
// needs neither an engine round-trip nor an event. snap carries the
// concurrency snapshot of a resume batch and is nil for every other action.
func (h *TaskHandlers) applyAction(
	ctx context.Context,
	task actionTask,
	action string,
	snap *concurrencySnapshot,
) ActionResult {
	// recheck rides the optional capability interface instead of the base
	// Engine interface, and its transition happens only once an engine
	// that can re-verify accepted the request.
	if action == actionRecheck {
		return h.recheck(ctx, task)
	}

	// Pause takes its own path: the operator pause joins the admission
	// pass's task-operation lease before it trusts its snapshot, because a
	// task the disk-space guard parked must change hands — stamp and all —
	// rather than be resumed by the next pass (T127).
	if action == actionPause {
		return h.pauseAction(ctx, task)
	}

	target, code, message := actionOutcome(action)

	// Resume requeues the task whatever the headroom, then answers from
	// the snapshot; it never contacts an engine (doc 05 section 5.7).
	if action == actionResume {
		return h.resumeAction(ctx, task, target, code, message, snap)
	}

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

// pauseAction is the pause action's own path (T127). An operator pause
// on a task the disk-space guard parked (paused + disk_full) takes
// ownership of the row: without the takeover the admission pass selects
// the row by its stamp alone and silently resumes what the operator
// parked. The action therefore joins the shared task-operation lease in
// waiting mode under a bounded budget — its clear lands before the
// pass's guarded claim, follows a completed release, or fails clearly
// when the budget expires — then reloads the row the holder left
// behind and holds the lease through the engine call, the state write
// and the hold-stamp clear. An already-paused row keeps its idempotent
// shape: no engine round-trip, no second pause event, then the clear.
// An active row keeps the engine-first behaviour and its one event,
// then the clear wipes whatever hold stamp rode the row.
func (h *TaskHandlers) pauseAction(ctx context.Context, task actionTask) ActionResult {
	target, code, message := actionOutcome(actionPause)

	// The lease wait runs under the named operator budget, never the
	// request context alone: a wait that outlives it returns the retry
	// outcome below with nothing touched — no engine call, no mutating
	// store call. The budget bounds only the wait: once acquired, the
	// lease lives until the deferred release below runs — the registry
	// ties the hold to that closure, not to waitCtx, whose deadline may
	// well expire while a slow engine call is still in flight under ctx.
	waitCtx, cancelWait := context.WithTimeout(ctx, pauseLeaseWait)
	defer cancelWait()

	// TaskOpWait can answer only the context's error here (a busy try is
	// the Try mode's answer), so every failure is the expired wait.
	releaseLease, err := h.engines.AcquireTaskOp(waitCtx, task.ID, engine.TaskOpWait)
	if err != nil {
		return actionFailure(task.ID, SlugValidationFailed, detailTaskOpBusy)
	}
	defer releaseLease()

	// The preloaded row predates the wait; reload under the lease so the
	// decision below reads what the holder left behind — a released row
	// goes through the ordinary engine pause, a still-parked one through
	// the idempotent branch.
	current, err := h.tasks.Get(ctx, task.ID)
	if errors.Is(err, store.ErrNotFound) {
		return actionFailure(task.ID, SlugNotFound, detailTaskNotFound)
	}
	if err != nil {
		logFromContext(ctx).Error("pause action could not reload the task",
			slog.String("task_id", task.ID), slog.Any("err", err))

		return actionFailure(task.ID, SlugInternal, detailActionFailed)
	}

	reloaded := actionTask{
		ID: current.ID, Engine: current.Engine, EngineRef: current.EngineRef, State: current.State,
	}

	if reloaded.State != target {
		// The engine call the action's table row names, unchanged from the
		// generic path: engine first, so an engine failure leaves the state
		// untouched. A task no engine holds yet (no engine_ref) applies to
		// dl-tool's row alone.
		e, err := h.engineFor(ctx, reloaded)
		if err == nil && e != nil {
			err = e.Pause(ctx, engineTaskID(reloaded.Engine, reloaded.EngineRef))
		}
		if err != nil {
			return engineFailure(ctx, task.ID, err)
		}

		outcome := h.transitionAction(ctx, reloaded, target, code, message)
		if !outcome.Ok {
			return outcome
		}
	}

	// The takeover itself: wipe a hold stamp off the paused row so the
	// admission pass — which selects candidates by the paused+disk_full
	// pair — can never resume the task the operator parked. No
	// task_events row belongs to the clear; the pause event (when one
	// landed) is the whole story.
	cleared, err := h.tasks.ClearPausedHoldCode(ctx, task.ID)
	if errors.Is(err, store.ErrNotFound) {
		return actionFailure(task.ID, SlugNotFound, detailTaskNotFound)
	}
	if err != nil {
		// The pause landed but the takeover did not: report the failure so
		// a retry — which finds the idempotent branch — tries the clear
		// again. Until it lands the pass may still resume the row.
		logFromContext(ctx).Error("pause landed but the hold-stamp clear failed; the admission pass may resume the task until a retry clears it",
			slog.String("task_id", task.ID), slog.Any("err", err))

		return actionFailure(task.ID, SlugInternal, detailActionFailed)
	}
	if cleared {
		logFromContext(ctx).Info("operator pause took over a guard-parked task", slog.String("task_id", task.ID))
	}

	return ActionResult{ID: task.ID, Ok: true}
}

// resumeAction requeues one task and reports whether a slot is free now:
// the admission pass (T098) owns Engine.Resume for a queued task, so the
// action itself contacts no engine. The requeue happens whatever the
// headroom — with no slot free the task stays queued and starts on its
// own once one frees, and the per-id outcome is ok:false
// /problems/concurrency-limit so the client knows it will not start now
// (docs/05-api-contract.md sections 5.7 and 5.11).
func (h *TaskHandlers) resumeAction(
	ctx context.Context,
	task actionTask,
	target, code, message string,
	snap *concurrencySnapshot,
) ActionResult {
	// Actions loads the snapshot for every resume batch, so a nil one is
	// a wiring bug in this file — fail the id rather than panic.
	if snap == nil {
		logFromContext(ctx).Error("resume action without a concurrency snapshot", slog.String("task_id", task.ID))

		return actionFailure(task.ID, SlugInternal, detailActionFailed)
	}

	// The nil guard above aside, an already-queued task needs no store
	// write — same-state resume is the idempotent success applyAction
	// documents — it only needs the headroom answer.
	if task.State != target {
		outcome := h.transitionAction(ctx, task, target, code, message)
		if !outcome.Ok {
			return outcome
		}
	}

	if blocked, detail := snap.limits.Blocked(snap.counts, task.Engine); blocked {
		return actionFailure(task.ID, SlugConcurrencyLimit, detail)
	}

	// This id's ok:true has promised one slot of the batch's headroom:
	// consume it so the later ids of the same request see it spent. The
	// admission pass re-derives the truth; the snapshot only has to be
	// self-consistent within the batch.
	snap.counts.Reserve(task.Engine)

	return ActionResult{ID: task.ID, Ok: true}
}

// concurrencySnapshot is one read of the counted set and the two limit
// settings — the headroom answer of a resume batch.
type concurrencySnapshot struct {
	counts engine.ActiveCounts
	limits engine.Limits
}

// settingRow is one (key, value_json) pair of the settings table.
type settingRow struct {
	Key       string `db:"key"`
	ValueJSON string `db:"value_json"`
}

// loadConcurrencySnapshot reads the counted set and the two concurrency
// settings keys. The defaults of docs/11-config-reference.md section 5
// cover a missing row; a value that is not an integer is a corrupt row
// and fails the request rather than guessing a limit.
func (h *TaskHandlers) loadConcurrencySnapshot(ctx context.Context) (*concurrencySnapshot, error) {
	counts, err := h.tasks.CountActive(ctx)
	if err != nil {
		return nil, internalFailure(ctx, "count active tasks", err)
	}

	query, args, err := sqlx.In(queryConcurrencySettings, settingMaxActiveTotal, settingMaxActivePerEngine)
	if err != nil {
		return nil, internalFailure(ctx, "read concurrency settings", err)
	}
	var rows []settingRow
	if err := h.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, internalFailure(ctx, "read concurrency settings", err)
	}

	limits := engine.Limits{
		MaxActiveTotal:     defaultMaxActiveTotal,
		MaxActivePerEngine: defaultMaxActivePerEngine,
	}
	for _, row := range rows {
		value, err := strconv.Atoi(row.ValueJSON)
		if err != nil || value < 0 {
			// A negative limit would silently mean unlimited everywhere
			// else; the write side rejects it, so the read side must too.
			return nil, internalFailure(ctx, "read concurrency settings",
				fmt.Errorf("key %s: want a non-negative integer, got %q", row.Key, row.ValueJSON))
		}
		switch row.Key {
		case settingMaxActiveTotal:
			limits.MaxActiveTotal = value
		case settingMaxActivePerEngine:
			limits.MaxActivePerEngine = value
		}
	}

	return &concurrencySnapshot{counts: counts, limits: limits}, nil
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
// name, destination, category, tags, per-task rate limits, share limits
// and the sequential flag. Omitted fields are untouched; a non-nil tags
// slice replaces the whole set. Every live engine application runs
// before any store write, so an engine that cannot take a change fails
// the request with nothing persisted. The response is the full updated
// Task object.
func (h *TaskHandlers) PatchTask(ctx context.Context, in *PatchTaskInput) (*GetTaskOutput, error) {
	task, err := h.tasks.Get(ctx, in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, Problem(SlugNotFound, http.StatusNotFound, detailTaskNotFound)
	}
	if err != nil {
		return nil, internalFailure(ctx, "get task for patch", err)
	}

	patch, fieldErrs, err := h.buildTaskPatch(ctx, task, in.Body)
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

	// The destination resolves against the data roots before anything
	// else touches it: a path outside every root is the 403 of doc 05
	// section 5.5, and the resolved value is what both the engine call
	// and the row write carry.
	destination := ""
	if in.Body.Destination != nil {
		destination, err = fsx.ResolveDestination(h.roots, *in.Body.Destination)
		if err != nil {
			return nil, destinationRejected(*in.Body.Destination)
		}
	}

	// The live applications run in the order of the mutator block below,
	// every one before the first store write: an engine that cannot take
	// a change fails the request with nothing persisted.
	if err := h.applyPatchMutators(ctx, task, in.Body, destination); err != nil {
		return nil, err
	}
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

	if in.Body.Destination != nil {
		if err := h.applyDestination(ctx, in.ID, destination, task.EngineRef != nil); err != nil {
			return nil, err
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

const (
	queryTaskLimits = `SELECT dl_limit, ul_limit, ratio_limit, seeding_time_limit FROM tasks WHERE id = ?`

	// querySetTaskDestination writes the destination a relocation
	// produced. It lives here, not in TaskPatch, because the store's patch
	// type carries no destination column and this task owns only the
	// files its table names.
	querySetTaskDestination = `UPDATE tasks SET destination = ?, updated_at = ? WHERE id = ?`
)

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
	task store.Task,
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

	// A persisted sequential flag an engine could never honour is a lie
	// the poller never corrects, so it is refused here — also for a task
	// no engine holds yet, because the flag would ride the admission. An
	// engine that is not registered escapes the gate: the mutator block
	// below answers that case with the 503 it deserves.
	if body.Sequential != nil {
		if e, ok := h.engines.Get(task.Engine); ok && !hasCapability(e, engine.CapSequential) {
			fieldErrs = append(fieldErrs, &huma.ErrorDetail{
				Message:  detailSequentialUnsupported,
				Location: "body.sequential",
			})
		} else {
			patch.Sequential = body.Sequential
		}
	}

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

// applyPatchMutators pushes the patched category, tags, sequential flag,
// share limits and destination to the engine that holds the task, in the
// order of docs/06-download-engines.md section 5.7's mutator table and
// only for the fields present in the body. Any failure aborts the patch
// with the 503 of doc 05 section 5.5 and leaves the row unchanged; a task
// no engine holds yet persists everything and applies it at admission
// time instead. The share limits are sent as the merge of the patch with
// the stored row — the daemon takes both limits on every call, and an
// omitted field is untouched, never reset to the global default.
func (h *TaskHandlers) applyPatchMutators(
	ctx context.Context,
	task store.Task,
	body PatchTaskBody,
	destination string,
) error {
	if task.EngineRef == nil {
		return nil
	}

	e, ok := h.engines.Get(task.Engine)
	if !ok {
		return engineUnavailable(task.Engine)
	}

	id := engineTaskID(task.Engine, task.EngineRef)

	if body.Category != nil {
		if err := e.SetCategory(ctx, id, *body.Category); err != nil {
			return patchEngineProblem(ctx, task.ID, err)
		}
	}

	if body.Tags != nil {
		tagger, ok := e.(tagMutator)
		if !ok {
			// The engine declares tags but carries no setter — the same
			// class of refusal as a missing capability.
			return Problem(SlugValidationFailed, http.StatusUnprocessableEntity, detailMutatorUnsupported)
		}
		if err := tagger.SetTags(ctx, id, body.Tags); err != nil {
			return patchEngineProblem(ctx, task.ID, err)
		}
	}

	if body.Sequential != nil {
		seq, ok := e.(sequentialEngine)
		if !ok {
			return Problem(SlugValidationFailed, http.StatusUnprocessableEntity, detailMutatorUnsupported)
		}
		if err := seq.SetSequential(ctx, id, *body.Sequential); err != nil {
			return patchEngineProblem(ctx, task.ID, err)
		}
	}

	if body.RatioLimit != nil || body.SeedingTimeLimit != nil {
		ratio, seed := body.RatioLimit, body.SeedingTimeLimit
		if ratio == nil || seed == nil {
			limits, err := h.taskLimits(ctx, task.ID)
			if err != nil {
				return err
			}
			if ratio == nil {
				ratio = limits.RatioLimit
			}
			if seed == nil {
				seed = limits.SeedingTimeLimit
			}
		}
		if err := e.SetShareLimits(ctx, id, ratio, seed); err != nil {
			return patchEngineProblem(ctx, task.ID, err)
		}
	}

	if destination != "" {
		if err := e.SetLocation(ctx, id, destination); err != nil {
			return patchEngineProblem(ctx, task.ID, err)
		}
	}

	return nil
}

// patchEngineProblem maps one mutator failure onto the patch's 503. An
// ErrNotSupported here is a hard failure — unlike a rate limit, a
// category, tag, share limit, flag or location the engine cannot take
// has no later moment to apply, so the request fails and the row stays
// untouched.
func patchEngineProblem(ctx context.Context, taskID string, err error) error {
	if !errors.Is(err, engine.ErrUnavailable) && !errors.Is(err, engine.ErrNotSupported) {
		logFromContext(ctx).Error("patch engine call failed", slog.String("task_id", taskID), slog.Any("err", err))
	}

	return Problem(SlugEngineUnavailable, http.StatusServiceUnavailable, detailEngineFailed)
}

// applyDestination writes the relocated destination and, for a task an
// engine holds, enters moving through the store's own transition — the
// same write T022's lifecycle actions use — so the event log records the
// move and the reconciler walks the row out of moving once the engine
// reports its post-move state (docs/04-data-model.md section 8.1). The
// write runs only after SetLocation succeeded, so an engine that could
// not take the new location leaves the row untouched. An unadmitted task
// owns no data yet and enters no state; a task whose state cannot enter
// moving keeps the new destination and answers 422 — the state machine
// refused the transition, not the relocation.
func (h *TaskHandlers) applyDestination(ctx context.Context, id, destination string, admitted bool) error {
	result, err := h.db.ExecContext(ctx, querySetTaskDestination, destination, time.Now().UnixMilli(), id)
	if err != nil {
		return internalFailure(ctx, "relocate task", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return internalFailure(ctx, "relocate task", err)
	}
	if affected == 0 {
		return Problem(SlugNotFound, http.StatusNotFound, detailTaskNotFound)
	}

	if !admitted {
		return nil
	}

	err = h.tasks.Transition(ctx, id, string(engine.StateMoving), eventTaskMoved, messageTaskMoved)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return Problem(SlugNotFound, http.StatusNotFound, detailTaskNotFound)
	case errors.Is(err, store.ErrIllegalTransition), errors.Is(err, store.ErrTransitionConflict):
		return Problem(SlugValidationFailed, http.StatusUnprocessableEntity, detailIllegalState)
	default:
		return internalFailure(ctx, "transition relocated task", err)
	}
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
