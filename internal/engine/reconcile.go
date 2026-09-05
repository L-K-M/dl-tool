// Reconciliation: the sweep that keeps the tasks table in step with the
// engines dl-tool owns. In-memory state never survives a restart — the
// database is the only source of truth, and the engine's live list is the
// only source of transfer state — so T026 reconciles the two at boot and on
// every poll (docs/17-operations-and-runbook.md section 1.6, stage S10).

package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/L-K-M/dl-tool/internal/store"
)

// CodeTaskReconciled is the task_events code of every row the reconciler
// writes: one per state it adopted from an engine report, and one per
// transfer it re-submitted after the engine lost the handle. Declared here,
// next to its only emitter, per docs/14-conventions.md section 4.
const CodeTaskReconciled = "task.reconciled"

// magnetInfohashPrefix rebuilds a submit URI from a bare infohash, the
// qBittorrent re-submission path of docs/17-operations-and-runbook.md
// section 1.6 ("re-add by infohash").
const magnetInfohashPrefix = "magnet:?xt=urn:btih:"

// TaskWriter is the store surface the reconciler needs; internal/store's
// *TaskStore satisfies it. The engine package owns the interface — the store
// stays a leaf that imports nothing from here (docs/03-architecture.md
// section 5.2, layering rule) — so the row shapes it names (Reconcilable,
// Progress) live in the store package and are referenced from this one.
type TaskWriter interface {
	// ListNonTerminalByEngine returns engine_ref -> task for one engine,
	// skipping completed, removed and error tasks and tasks with no handle
	// yet.
	ListNonTerminalByEngine(ctx context.Context, engineName string) (map[string]store.Reconcilable, error)
	UpdateProgress(ctx context.Context, id string, p store.Progress) error
	SetEngineRef(ctx context.Context, id, engineRef string) error
	Transition(ctx context.Context, id, next, code, message string) error
	AppendEvent(ctx context.Context, taskID, level, code, message string, detail any) error
}

// The interface is satisfied by the concrete store, not by assertion in a
// comment: a signature drift in either package fails here at compile time.
var _ TaskWriter = (*store.TaskStore)(nil)

// Reconciler keeps the tasks table in step with the engines dl-tool owns.
// It is the only writer of engine-sourced task state: every counter, rate
// and state change that originates in an engine lands through one of its
// sweeps, joined on the (engine, engine_ref) pair.
type Reconciler struct {
	registry *Registry
	tasks    TaskWriter
	poll     time.Duration
	log      *slog.Logger
}

// NewReconciler wires the registry to the task store. poll is the sweep
// interval; 1s in production. log is the loop's logger — the composition
// root passes its own, so sweeps never bypass it through slog.Default();
// nil falls back to the default for direct constructions such as tests.
func NewReconciler(reg *Registry, ts TaskWriter, poll time.Duration, log *slog.Logger) *Reconciler {
	if log == nil {
		log = slog.Default()
	}
	return &Reconciler{registry: reg, tasks: ts, poll: poll, log: log}
}

// Boot runs one full sweep before the HTTP listener opens, over the
// non-terminal tasks only: every known engine_ref is written back, a task in
// downloading, seeding or checking whose handle has vanished is re-submitted
// from its stored source with resume semantics, a queued or paused task is
// left alone, and every unknown handle is ignored. An unreachable engine is
// a warning, not a Boot failure — it is retried on the next poll and no task
// state changes on its account.
func (r *Reconciler) Boot(ctx context.Context) error {
	for _, name := range r.registry.Names() {
		e, ok := r.registry.Get(name)
		if !ok {
			continue // Names and Get disagree only mid-Register; skip it.
		}
		if err := r.sweepEngine(ctx, name, e); err != nil {
			return err
		}
	}

	return nil
}

// Run drives the poll loop: one Boot per tick until ctx is cancelled. It
// is owned by the component that started it and stops with that context.
// The boot sweep itself is the composition root's — it calls Boot once
// before the listener opens (docs/17-operations-and-runbook.md section
// 1.6) — so the loop starts with a tick, not with a sweep: an immediate
// re-sweep would only duplicate the one that just ran and race the engine
// registrations that may still follow construction, the same reason the
// sync hub's loop is ticker-first. A failed sweep logs at warn and
// retries on the next tick: at the default Info level a silent loop would
// hide exactly the outage an operator must see — stale state served by a
// healthy-looking process — and a transient error must not retire the
// reconciler for the process lifetime.
func (r *Reconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.poll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.Boot(ctx); err != nil {
				// A cancelled context is the owner shutting the loop down, not
				// an outage: the warn below is reserved for sweeps that fail
				// on a live context.
				if ctx.Err() != nil {
					return ctx.Err()
				}
				r.log.Warn("reconciliation sweep failed; retrying on the next tick", "error", err)
			}
		}
	}
}

// sweepEngine reconciles one engine against the tasks that name it. A
// store-wide failure (the listing below) aborts the sweep and surfaces; an
// engine failure is a warning, and a per-task failure — a row deleted
// mid-sweep, a state the store refuses — is logged and skipped, because by
// this point the engine List has already succeeded and the remaining tasks
// still deserve their writes. A cancelled context aborts the sweep quietly:
// the caller is tearing down (the boot budget expiring, a shutdown), the
// next sweep retries, and no task may be failed on its account.
func (r *Reconciler) sweepEngine(ctx context.Context, name string, e Engine) error {
	listed, err := e.List(ctx)
	if err != nil {
		// Cancellation is not an outage either: a shutdown or an expired
		// boot budget surfaces the engine's context error, never a fake
		// "unreachable" warning — the same policy as every other branch.
		if ctx.Err() != nil {
			// Attribution over a bare ctx.Err(): Boot sweeps several engines,
			// and a boot-budget expiry must name the engine that consumed it;
			// %w keeps errors.Is(err, context.Canceled) true.
			return fmt.Errorf("engine %q: %w: %v", name, ctx.Err(), err)
		}
		r.log.Warn("engine unreachable, retrying on the next sweep", "engine", name, "error", err)
		return nil
	}

	known, err := r.tasks.ListNonTerminalByEngine(ctx, name)
	if err != nil {
		return fmt.Errorf("reconcile engine %q: %w", name, err)
	}

	// Pass 1 — the engine's live list. Every reported handle joins against
	// the map on the engine_ref value.
	seen := make(map[string]bool, len(listed))
	for i := range listed {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		info := listed[i]
		ref := bareHandle(name, info.ID)
		task, knownHandle := known[ref]
		if !knownHandle {
			// A transfer dl-tool did not create: foreign, and this drop is
			// the whole of ADR-0017 (exclusive control of engines). One
			// rule, no options and no setting: it never enters tasks,
			// counts toward no limit, and is never paused, relocated or
			// deleted. Detection is by handle alone, and the check only
			// filters — it creates nothing.
			continue
		}
		seen[ref] = true

		if err := r.writeBack(ctx, task, info); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.log.Error("reconcile write-back failed; continuing with the remaining tasks",
				"engine", name, "task_id", task.ID, "error", err)
		}
	}

	// Pass 2 — the handles the engine no longer reports, in a stable order
	// so the writes and their events are deterministic.
	vanished := make([]string, 0, len(known))
	for ref := range known {
		if !seen[ref] {
			vanished = append(vanished, ref)
		}
	}
	slices.Sort(vanished)

	for _, ref := range vanished {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		task := known[ref]
		// An aria2 GID never survives a daemon restart, so a vanished
		// handle is the expected path, not a failure. Only a task mid-
		// transfer is re-submitted; queued and paused tasks are the
		// admission pass's to start (T098), and a terminal task never
		// reaches this map at all.
		if !resubmittable(task.State) {
			continue
		}
		if err := r.resubmit(ctx, name, e, task); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.log.Error("re-submission failed; continuing with the remaining tasks",
				"engine", name, "task_id", task.ID, "engine_ref", ref, "error", err)
		}
	}

	return nil
}

// writeBack adopts one engine-reported task: the counters always, the state
// only when it differs, so an unchanged task produces no event and no delta
// and a 1 Hz poll of a quiet queue writes nothing but progress.
func (r *Reconciler) writeBack(ctx context.Context, task store.Reconcilable, info TaskInfo) error {
	if err := r.tasks.UpdateProgress(ctx, task.ID, store.Progress{
		TotalBytes:     info.TotalBytes,
		CompletedBytes: info.CompletedBytes,
		UploadedBytes:  info.UploadedBytes,
		DownloadRate:   info.DownloadRate,
		UploadRate:     info.UploadRate,
		ETASeconds:     info.ETASeconds,
	}); err != nil {
		return fmt.Errorf("reconcile task %q: %w", task.ID, err)
	}

	if task.State == string(info.State) {
		return nil
	}

	err := r.tasks.Transition(ctx, task.ID, string(info.State), CodeTaskReconciled,
		fmt.Sprintf("reconciler adopted engine state %q", info.State))
	// A post-processing state (extracting, moving) is owned by the jobs
	// table, not the engine, and the engine may legally report a state the
	// row cannot move to while a job holds it. The engine's word is not
	// wrong and the row is not wrong; the move is simply not legal now, so
	// it is skipped with a warning rather than poisoning the sweep.
	if errors.Is(err, store.ErrIllegalTransition) {
		r.log.Warn("reconciler cannot adopt engine state",
			"task_id", task.ID, "from", task.State, "to", string(info.State))
		return nil
	}

	return err
}

// resubmit hands a vanished transfer back to its engine from the stored
// source with resume semantics, adopts the new handle and records the
// reconciliation. A refused re-submission errors that one task and the
// sweep moves on — the refusal is a per-task outcome, not an engine-wide
// one, and the other vanished tasks still deserve their writes.
func (r *Reconciler) resubmit(ctx context.Context, name string, e Engine, task store.Reconcilable) error {
	req, ok := resubmitRequest(task)
	if !ok {
		// No stored source and no infohash: nothing to re-submit from. The
		// task keeps its state and the warning recurs once per sweep, which
		// is the honest signal for a row the operator must look at.
		r.log.Warn("cannot re-submit task: no stored source or infohash",
			"task_id", task.ID, "engine", name)
		return nil
	}

	newID, err := e.Add(ctx, req)
	if err != nil {
		// A caller-side cancellation — the boot budget expiring, a shutdown —
		// is not an engine refusal: the task keeps its state and the next
		// sweep retries. Only a live context's failure errors the task.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return r.failResubmit(ctx, task, name, err)
	}

	if err := r.tasks.SetEngineRef(ctx, task.ID, bareHandle(name, newID)); err != nil {
		// The transfer now exists engine-side while the row still names the
		// vanished handle, so the next sweep would add it again — once per
		// sweep, every duplicate foreign under ADR-0017 and untouchable.
		// Compensate by removing the transfer this sweep itself just created
		// (its own Add receipt, not a foreign one), on a context that survives
		// the cancellation that may have caused the failure — and carries its
		// own deadline, because WithoutCancel also drops the budget a Boot
		// was running under, and no engine is owed an unbounded wait. If even
		// the removal fails, name the handle so the operator can remove it.
		removeCtx, cancelRemove := context.WithTimeout(context.WithoutCancel(ctx), compensateRemoveBudget)
		defer cancelRemove()
		if removeErr := e.Remove(removeCtx, newID); removeErr != nil {
			r.log.Error("re-submitted but could not record the new handle, and the compensating removal failed; the transfer is stranded engine-side",
				"task_id", task.ID, "engine", name, "engine_ref", bareHandle(name, newID),
				"error", err, "remove_error", removeErr)
		} else {
			r.log.Error("re-submitted but could not record the new handle; removed the new transfer so the next sweep cannot duplicate it",
				"task_id", task.ID, "engine", name, "engine_ref", bareHandle(name, newID), "error", err)
		}
		return fmt.Errorf("reconcile task %q: %w", task.ID, err)
	}

	// The ref is committed, so the reconciliation itself is durable: a
	// failure to append its audit event must not fail the sweep or roll
	// anything back — the handle is adopted either way, and the next sweep
	// would find it healthy and never re-emit this event. Warn and move on.
	if err := r.tasks.AppendEvent(ctx, task.ID, "info", CodeTaskReconciled,
		"engine lost the transfer; re-submitted with resume semantics",
		map[string]string{"engine": name, "engine_ref": bareHandle(name, newID)}); err != nil {
		r.log.Warn("re-submitted but failed to record the reconciliation event",
			"task_id", task.ID, "engine", name, "error", err)
	}

	return nil
}

// failResubmit records a refused re-submission: the task moves to error
// carrying engine.unavailable, one event, and nothing else — SetEngineRef is
// never called, so the stale handle stays for the operator to see.
func (r *Reconciler) failResubmit(ctx context.Context, task store.Reconcilable, name string, cause error) error {
	err := r.tasks.Transition(ctx, task.ID, string(StateError), store.CodeEngineUnavailable,
		fmt.Sprintf("engine %q refused the re-submission: %v", name, cause))
	if err != nil {
		return fmt.Errorf("reconcile task %q: %w", task.ID, err)
	}

	return nil
}

// resubmitRequest rebuilds the engine submission from the stored identity:
// the source URI when dl-tool kept one, the infohash as a magnet otherwise
// (the qBittorrent path). resumeExtra carries the aria2 --continue option;
// engines without such an option ignore it.
func resubmitRequest(task store.Reconcilable) (AddRequest, bool) {
	switch {
	case task.SourceURI != nil && *task.SourceURI != "":
		return AddRequest{URIs: []string{*task.SourceURI}, SaveDir: task.Destination, Extra: resumeExtra()}, true
	case task.InfohashV1 != nil && *task.InfohashV1 != "":
		return AddRequest{URIs: []string{magnetInfohashPrefix + *task.InfohashV1}, SaveDir: task.Destination, Extra: resumeExtra()}, true
	default:
		return AddRequest{}, false
	}
}

// resumeExtra is the engine-specific escape hatch the re-submission uses for
// aria2's --continue (docs/17-operations-and-runbook.md section 1.6).
func resumeExtra() map[string]string {
	return map[string]string{"continue": "true"}
}

// compensateRemoveBudget bounds the compensating removal of a transfer
// whose handle could not be recorded. It runs on a context that survives
// the caller's cancellation, so nothing else bounds it.
const compensateRemoveBudget = 10 * time.Second

// resubmittable reports whether a vanished handle is re-submitted: only a
// transfer the engine was actively holding. queued and paused tasks are left
// alone — the admission controller (T098) submits them when a slot frees.
func resubmittable(state string) bool {
	switch state {
	case string(StateDownloading), string(StateSeeding), string(StateChecking):
		return true
	default:
		return false
	}
}

// bareHandle strips the engine namespace from an engine task id, yielding
// the value tasks.engine_ref stores: "aria2:<gid>" -> "<gid>".
func bareHandle(engineName, id string) string {
	return strings.TrimPrefix(id, engineName+":")
}
