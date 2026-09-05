// Admission control: dl-tool is the only admission controller
// (docs/03-architecture.md section 6.4). No engine can see past its own
// queue, so the engines' own limits are raised out of the way
// (docs/06-download-engines.md section 9.4) and this pass decides alone
// which queued task reaches an engine: it counts the started tasks in
// total and per engine, walks the queue in creation order, and releases a
// task only while every applicable limit still has headroom. A task held
// by a limit is never rejected — it stays queued carrying
// concurrency_limit and starts on its own once a slot frees
// (docs/05-api-contract.md section 5.11).

package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/L-K-M/dl-tool/internal/store"
)

// ErrorCodeConcurrencyLimit is the tasks.error_code of a task held in
// queued by a concurrency limit. It is never a creation-time rejection:
// POST /tasks accepts the task, the pass stamps the code on whatever it
// cannot release, and the stamp is cleared the moment a slot frees.
const ErrorCodeConcurrencyLimit = "concurrency_limit"

// candidatesUnbounded is the candidate limit the pass selects: every
// queued task, because a held task must carry concurrency_limit wherever
// it sits in the queue, not only at the head.
const candidatesUnbounded = 0

// Limits are the two max_active_* settings keys of
// docs/11-config-reference.md section 5. 0 means unlimited. The bandwidth
// pair is RateLimits (T079); these two types are distinct on purpose.
type Limits struct {
	MaxActiveTotal     int
	MaxActivePerEngine int
}

// ActiveCounts is one snapshot of the counted set: tasks in state
// downloading, checking, extracting or moving. Tasks in state seeding are
// excluded from every count — the exclusion is CountActive's, in SQL, so
// no reader can count a seed list against a new download.
type ActiveCounts = store.ActiveCounts

// Candidate is one queued task considered for release. EngineRef is nil
// when the task has never been handed to an engine.
type Candidate = store.Candidate

// AdmissionStore is the store surface the admitter needs; internal/store's
// *TaskStore satisfies it. As with TaskWriter, the engine package owns the
// interface and the store stays a leaf that imports nothing from here
// (docs/03-architecture.md section 5.2, layering rule) — the row shapes it
// names live in the store package and reach this one through the aliases
// above.
type AdmissionStore interface {
	CountActive(ctx context.Context) (ActiveCounts, error)
	// SelectQueuedCandidates returns queued tasks in process_order, oldest
	// added_at first.
	SelectQueuedCandidates(ctx context.Context, limit int) ([]Candidate, error)
	Transition(ctx context.Context, id, next, code, message string) error
	SetErrorCode(ctx context.Context, id, errorCode, message string) error
	SetEngineRef(ctx context.Context, id, engineRef string) error
}

// The interface is satisfied by the concrete store, not by assertion in a
// comment: a signature drift in either package fails here at compile time.
var _ AdmissionStore = (*store.TaskStore)(nil)

// Admitter releases queued tasks while every applicable limit has
// headroom. It is the only caller of Engine.Add for a queued task, and of
// Engine.Resume for a task the admission pass itself parked — every other
// resume path requeues and leaves the release to the pass.
type Admitter struct {
	registry *Registry
	tasks    AdmissionStore
	tick     time.Duration
	log      *slog.Logger
}

// NewAdmitter wires the registry to the task store. tick is Run's pass
// interval. The loop logs through slog.Default(); the composition root's
// own logger can be installed when Run gains its call site (T099 wires the
// pass with the disk-space gate that shares it).
func NewAdmitter(reg *Registry, ts AdmissionStore, tick time.Duration) *Admitter {
	return &Admitter{registry: reg, tasks: ts, tick: tick, log: slog.Default()}
}

// Blocked reports whether one more task on engineName would exceed a
// non-zero limit, and returns the operator-facing message naming the
// binding one — the error_message of a held task and the per-id detail of
// a blocked resume are the same sentence (docs/05-api-contract.md section
// 5.11). A zero limit never blocks: 0 means unlimited in that dimension.
func (l Limits) Blocked(c ActiveCounts, engineName string) (bool, string) {
	if l.MaxActiveTotal > 0 && c.Total >= l.MaxActiveTotal {
		return true, fmt.Sprintf("%d of %d slots in use", c.Total, l.MaxActiveTotal)
	}
	if l.MaxActivePerEngine > 0 && c.ByEngine[engineName] >= l.MaxActivePerEngine {
		return true, fmt.Sprintf("%d of %d %s slots in use", c.ByEngine[engineName], l.MaxActivePerEngine, engineName)
	}

	return false, ""
}

// Pass runs one admission pass and returns the ids it released. It is
// idempotent and safe to run concurrently with the reconciler: the
// store's guarded updates decide every write, so a candidate the
// reconciler moved underneath the pass simply fails its transition and
// stays for the next tick. The counts are read once and incremented
// in memory after each release, so one pass cannot admit past a limit the
// database does not reflect yet.
func (a *Admitter) Pass(ctx context.Context, l Limits) ([]string, error) {
	counts, err := a.tasks.CountActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("admission pass: %w", err)
	}

	candidates, err := a.tasks.SelectQueuedCandidates(ctx, candidatesUnbounded)
	if err != nil {
		return nil, fmt.Errorf("admission pass: %w", err)
	}

	released := make([]string, 0, len(candidates))
	for _, cand := range candidates {
		if ctx.Err() != nil {
			return released, ctx.Err()
		}

		if held, message := l.Blocked(counts, cand.Engine); held {
			// The stamp is the only write a held task gets: the state
			// stays queued and the guarded SetErrorCode keeps a re-stamp
			// of the same sentence silent.
			if err := a.tasks.SetErrorCode(ctx, cand.ID, ErrorCodeConcurrencyLimit, message); err != nil && !errors.Is(err, store.ErrNotFound) {
				a.log.Warn("admission pass could not stamp a held task", "task_id", cand.ID, "error", err)
			}
			continue
		}

		if err := a.release(ctx, cand); err != nil {
			if ctx.Err() != nil {
				return released, ctx.Err()
			}
			a.releaseFailed(ctx, cand, err)
			continue
		}

		counts.Total++
		counts.ByEngine[cand.Engine]++
		released = append(released, cand.ID)
	}

	return released, nil
}

// Run drives Pass on a ticker until ctx is cancelled. load reads the
// limits each tick — settings change at runtime, so the pass must not
// cache them — and a failing load is a warning and a retry, never the
// loop's end: an admission outage must not outlive its cause. The loop is
// ticker-first, like the reconciler's, so constructing an Admitter never
// implies a pass.
func (a *Admitter) Run(ctx context.Context, load func(context.Context) (Limits, error)) error {
	ticker := time.NewTicker(a.tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			limits, err := load(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				a.log.Warn("admission pass could not load the limits; retrying on the next tick", "error", err)
				continue
			}
			if _, err := a.Pass(ctx, limits); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				a.log.Warn("admission pass failed; retrying on the next tick", "error", err)
			}
		}
	}
}

// errNoSubmission marks a candidate whose row carries neither a stored
// source URI nor an infohash, so no Engine.Add request can be built.
var errNoSubmission = errors.New("engine: queued task has no submission source")

// release hands one candidate to its engine and records the release: Add
// when the task has never been handed over, Resume when it holds a handle
// — and Add again when the engine no longer knows that handle, the same
// self-healing the reconciler applies to a handle that vanished
// mid-transfer. The state move and the error-code clear are the release's
// own writes; SetEngineRef writes the acceptance event in the same
// transaction as the handle.
func (a *Admitter) release(ctx context.Context, cand store.Candidate) error {
	e, ok := a.registry.Get(cand.Engine)
	if !ok {
		return fmt.Errorf("%w: %s is not registered", ErrUnavailable, cand.Engine)
	}

	if cand.EngineRef != nil {
		err := e.Resume(ctx, cand.Engine+":"+*cand.EngineRef)
		switch {
		case err == nil:
			return a.markReleased(ctx, cand.ID)
		case errors.Is(err, ErrNotFound):
			// The engine lost the handle (an aria2 daemon restart, for
			// example); fall through to Add with resume semantics.
		default:
			return err
		}
	}

	req, ok := admissionRequest(cand)
	if !ok {
		return errNoSubmission
	}

	newID, err := e.Add(ctx, req)
	if err != nil {
		return err
	}

	if err := a.tasks.SetEngineRef(ctx, cand.ID, bareHandle(cand.Engine, newID)); err != nil {
		// The transfer now exists engine-side while the row still carries
		// no handle, so the next pass would add it again — and every
		// duplicate would be foreign under ADR-0017 and untouchable.
		// Compensate by removing the transfer this pass itself just
		// created (its own Add receipt, not a foreign one), on a context
		// that survives the cancellation that may have caused the failure
		// and carries its own deadline. If even the removal fails, name
		// the handle so the operator can remove it.
		removeCtx, cancelRemove := context.WithTimeout(context.WithoutCancel(ctx), compensateRemoveBudget)
		defer cancelRemove()
		if removeErr := e.Remove(removeCtx, newID); removeErr != nil {
			a.log.Error("released but could not record the handle, and the compensating removal failed; the transfer is stranded engine-side",
				"task_id", cand.ID, "engine", cand.Engine, "engine_ref", bareHandle(cand.Engine, newID),
				"error", err, "remove_error", removeErr)
		} else {
			a.log.Warn("released but could not record the handle; removed the new transfer so the next pass cannot duplicate it",
				"task_id", cand.ID, "engine", cand.Engine, "error", err)
		}

		return err
	}

	return a.markReleased(ctx, cand.ID)
}

// markReleased moves the released task to downloading — the task.resumed
// event of the release — and clears any stale concurrency_limit, so a
// started task never carries the code that held it
// (docs/05-api-contract.md section 5.11).
func (a *Admitter) markReleased(ctx context.Context, id string) error {
	if err := a.tasks.Transition(ctx, id, string(StateDownloading), store.CodeTaskResumed, "released by the admission pass"); err != nil {
		return err
	}

	return a.tasks.SetErrorCode(ctx, id, "", "")
}

// releaseFailed records one release the pass could not complete. An
// unreachable or unregistered engine is an outage, not a refusal — the
// candidate stays queued and the next tick retries, exactly the
// reconciler's policy for an engine that is down. A refusal on a live
// context is the task_events vocabulary's engine.rejected moment
// (internal/store/events.go): the task moves to error carrying the
// refusal, and error -> queued remains the retry path.
func (a *Admitter) releaseFailed(ctx context.Context, cand store.Candidate, cause error) {
	switch {
	case errors.Is(cause, errNoSubmission):
		// Nothing to hand the engine: a queued row without a stored source
		// or infohash. The reconciler answers the same shape with a
		// recurring warning; the row is the operator's to look at.
		a.log.Warn("queued task has no source to release", "task_id", cand.ID, "engine", cand.Engine)
	case errors.Is(cause, ErrUnavailable):
		a.log.Warn("engine unreachable at hand-off; retrying on the next tick",
			"task_id", cand.ID, "engine", cand.Engine, "error", cause)
	default:
		err := a.tasks.Transition(ctx, cand.ID, string(StateError), store.CodeEngineRejected,
			fmt.Sprintf("engine %q refused the task: %v", cand.Engine, cause))
		if err != nil {
			a.log.Error("could not record an engine refusal",
				"task_id", cand.ID, "engine", cand.Engine, "cause", cause, "error", err)
		}
	}
}

// admissionRequest rebuilds the engine submission from the stored
// identity, the reconciler's resubmitRequest shape: the source URI when
// dl-tool kept one, the infohash as a magnet otherwise. resumeExtra
// carries aria2's --continue so a re-add resumes partial data; engines
// without such an option ignore it.
func admissionRequest(cand store.Candidate) (AddRequest, bool) {
	switch {
	case cand.SourceURI != nil && *cand.SourceURI != "":
		return AddRequest{URIs: []string{*cand.SourceURI}, SaveDir: cand.Destination, Extra: resumeExtra()}, true
	case cand.InfohashV1 != nil && *cand.InfohashV1 != "":
		return AddRequest{URIs: []string{magnetInfohashPrefix + *cand.InfohashV1}, SaveDir: cand.Destination, Extra: resumeExtra()}, true
	default:
		return AddRequest{}, false
	}
}
