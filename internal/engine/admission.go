// Admission control: dl-tool is the only admission controller
// (docs/03-architecture.md section 6.4). No engine can see past its own
// queue, so the engines' own limits are raised out of the way
// (docs/06-download-engines.md section 9.4) and this pass decides alone
// which queued task reaches an engine: it counts the started tasks in
// total and per engine, walks the queue in creation order, consults the
// two concurrency limits and the disk-space reservation of FR-047, and
// releases a task only while every applicable gate still has headroom. A
// task held by a limit is never rejected — it stays queued carrying
// concurrency_limit, a task held by space carries disk_full, and both
// start on their own once the hold lifts
// (docs/05-api-contract.md section 5.11).

package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/L-K-M/dl-tool/internal/fsx"
	"github.com/L-K-M/dl-tool/internal/store"
)

// ErrorCodeConcurrencyLimit is the tasks.error_code of a task held in
// queued by a concurrency limit. It is never a creation-time rejection:
// POST /tasks accepts the task, the pass stamps the code on whatever it
// cannot release, and the stamp is cleared the moment a slot frees.
const ErrorCodeConcurrencyLimit = "concurrency_limit"

// ErrorCodeDiskFull is the tasks.error_code of a task the disk could not
// hold: a write that failed with ENOSPC (FR-048, paused) or a candidate
// the reservation will not admit (FR-047, held in queued). The aria2
// mapping produces the same value from aria2 errorCode 9, so an
// engine-reported disk failure and dl-tool's own reservation speak one
// vocabulary.
const ErrorCodeDiskFull = "disk_full"

// candidatesUnbounded is the candidate limit the pass selects: every
// queued task, because a held task must carry concurrency_limit wherever
// it sits in the queue, not only at the head. math.MaxInt rather than 0
// stays correct even under a store that interpolates the limit into a
// SQL LIMIT clause, where 0 would select no rows.
const candidatesUnbounded = math.MaxInt

// Limits are the two max_active_* settings keys of
// docs/11-config-reference.md section 5. 0 means unlimited. The bandwidth
// pair is RateLimits (T079); these two types are distinct on purpose.
type Limits struct {
	MaxActiveTotal     int
	MaxActivePerEngine int
}

// Policy is one tick's admission policy: the concurrency limits plus the
// disk-reservation settings the space gate consults (FR-047). Run's load
// closure re-reads all of it every tick because PATCH /settings may change
// any of it at runtime.
type Policy struct {
	Limits Limits
	// MinFree maps a data-root path to its min_free_space floor in bytes.
	// Readers must use the two-value lookup — floor, ok := MinFree[root]:
	// !ok resolves to fsx.DefaultMinFreeBytes, an explicit stored 0
	// disables the floor for that root. A plain MinFree[root] read returns
	// 0 for an absent root and would silently disable its floor; fsx.Floor
	// is the one lookup the gate uses.
	MinFree map[string]int64
	// Roots are the configured data roots (DLTOOL_DATA_ROOTS): the floor
	// of a candidate is the floor of the longest root that owns its
	// destination as a separator-bounded path prefix — /data does not own
	// /database. A destination no root owns resolves to the default floor.
	Roots []string
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
	// SelectQueuedCandidates returns queued tasks and paused tasks carrying
	// disk_full in process_order, oldest added_at first — one ordering over
	// both, so a parked task never starves behind newer queued ones. The
	// paused rows are meant to be only pauses the guard itself landed; an
	// operator pause must clear the stamp (T127's action-layer takeover)
	// so the pass can never mistake a deliberate pause for a parked one.
	SelectQueuedCandidates(ctx context.Context, limit int) ([]Candidate, error)
	// SumRemainingByDestination returns the committed-but-unwritten bytes
	// per destination over the counted active states.
	SumRemainingByDestination(ctx context.Context) (map[string]int64, error)
	// PauseWithCode lands the disk-full pause atomically: state, stamp and
	// event in one transaction.
	PauseWithCode(ctx context.Context, id string, pause store.CodedPause) error
	// ClearHoldCode clears a hold stamp unless the task is paused, and
	// only ever a hold stamp — a paused row or a real failure's own code
	// survives; a missing id is the store's not-found error.
	ClearHoldCode(ctx context.Context, id string) error
	// Get returns the task identified by id, or the store's not-found
	// error for an unknown id — never a zero Task with a nil error.
	Get(ctx context.Context, id string) (store.Task, error)
	Transition(ctx context.Context, id, next, code, message string) error
	// SetErrorCodeIfState stamps tasks.error_code only while the row is
	// still in the state the caller's snapshot read — a compare-and-set, so
	// a hold stamp can never mint the paused+disk_full resume token on a
	// row the operator moved after selection. A declined write reports
	// success; a missing id is the store's not-found error.
	SetErrorCodeIfState(ctx context.Context, id, expectedState, errorCode, message string) error
	// ClaimParkedDiskFull revalidates the persisted paused+disk_full pair
	// with one guarded no-op write and reports whether it took the row —
	// the pass's claim before a parked candidate's first engine call. A
	// missing id is the store's not-found error; a declined write is an
	// overtaken candidate, never an engine rejection.
	ClaimParkedDiskFull(ctx context.Context, id string) (bool, error)
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
// interval; a non-positive one is a composition bug and panics here, at
// construction, rather than inside the loop goroutine where
// time.NewTicker's generic panic would land far from the misconfigured
// call site. log is the loop's logger — the composition root passes its
// own, so passes never bypass it through slog.Default(); nil falls back to
// the default for direct constructions such as tests.
func NewAdmitter(reg *Registry, ts AdmissionStore, tick time.Duration, log *slog.Logger) *Admitter {
	if tick <= 0 {
		panic(fmt.Sprintf("engine: admission tick must be positive, got %s", tick))
	}
	if log == nil {
		log = slog.Default()
	}

	return &Admitter{registry: reg, tasks: ts, tick: tick, log: log}
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
// in memory after each release, and the reservations are read once and
// committed in memory after each release, so one pass can admit past
// neither a limit nor a reservation the database does not reflect yet.
// A paused disk_full candidate (FR-048) is released the same way — the
// space gate first, the limits second — so its partial data is resumed,
// never restarted.
func (a *Admitter) Pass(ctx context.Context, p Policy) ([]string, error) {
	counts, err := a.tasks.CountActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("admission pass: %w", err)
	}

	candidates, err := a.tasks.SelectQueuedCandidates(ctx, candidatesUnbounded)
	if err != nil {
		return nil, fmt.Errorf("admission pass: %w", err)
	}

	// One reservation pool per filesystem, built before the walk (FR-047).
	// A store failure aborts the pass — the next tick retries. A stat
	// failure fails open for queued candidates (`holds`: a queue must not
	// wedge on a transient filesystem answer) but closed for parked ones
	// (`holdsParked`: the stamp stays and the task stays paused, so a
	// parked transfer is never resumed against an unreadable filesystem).
	gate, err := a.spaceGate(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("admission pass: %w", err)
	}

	released := make([]string, 0, len(candidates))
	storeErrs := 0
	for _, cand := range candidates {
		if ctx.Err() != nil {
			return released, ctx.Err()
		}

		// The task-operation lease is taken without waiting, before either
		// hold gate or stampHeld: a busy task — one an operator action is
		// holding — is skipped quietly and reselected on a later pass, so
		// one slow action cannot head-of-line block the tick and no slot or
		// reservation is spent on it. Try can only answer the busy error,
		// which is the skip, never a failure.
		releaseLease, err := a.registry.AcquireTaskOp(ctx, cand.ID, TaskOpTry)
		if err != nil {
			continue
		}

		// One lease at a time: the holder of the candidate's iteration,
		// released before the pass walks on.
		outcome, err := a.processCandidate(ctx, cand, p, counts, gate)
		releaseLease()
		if err != nil {
			if ctx.Err() != nil {
				return released, ctx.Err()
			}

			// One candidate's store error must not starve the candidates
			// behind it: selection order is stable, so a pass that aborts
			// on error would reselect the same row and die on it again
			// every tick while everything behind it waits forever — the
			// same head-of-line blocking the busy-skip above prevents for
			// busy candidates, now also prevented on the error path.
			// Skip it, log it, retry it on a later pass; only context
			// cancellation aborts the pass.
			a.log.Warn("admission pass: skipping candidate after a store error",
				"task_id", cand.ID, "error", err)

			storeErrs++
			continue
		}
		if outcome == outcomeHeld {
			continue
		}

		// A release that recorded an engine handle has promised both a
		// slot and the candidate's remaining bytes — whether it finished
		// (outcomeReleased) or parked mid-flight with the transfer running
		// (outcomeSpent): spend each in memory so the later candidates of
		// this same pass see both gone. A queued candidate that arrived
		// already holding an engine handle was counted in this pass's
		// snapshot — spending it again would charge one transfer two
		// slots.
		if cand.State != string(StateQueued) || cand.EngineRef == nil {
			counts.Total++
			counts.ByEngine[cand.Engine]++
			gate.commit(cand)
		}
		if outcome == outcomeReleased {
			released = append(released, cand.ID)
		}
	}

	// A pass that skipped candidates but released others is healthy
	// enough — one broken row is the skip case above. A pass that skipped
	// candidates and released nothing is indistinguishable from an idle
	// queue without this error, and a full store outage must not read as
	// one. Run logs it and retries on the next tick, as it does for the
	// top-of-pass read failures.
	if storeErrs > 0 && len(released) == 0 {
		return released, fmt.Errorf("admission pass: skipped %d candidate(s) after store errors", storeErrs)
	}

	return released, nil
}

// processCandidate evaluates one selected candidate while the pass
// holds its task-operation lease, and reports whether the pass released
// it — the boolean the caller gates its in-memory slot and reservation
// spend on. The lease spans the revalidation read, the hold gates and
// stamp writes, the parked pair's claim, the engine calls, the release
// writes and release-failure handling, so no second operation on this
// task can interleave with any of them.
//
// The revalidation read is the first thing under the lease: the
// candidate is a snapshot from the start of the pass, and the row must
// still carry what selected it — queued for a queued snapshot,
// paused+disk_full for a parked one. A row that moved, was cleared or
// vanished is an overtaken candidate and aborts quietly: no engine
// call, no row write, no event, and nothing spent. Queued snapshots
// take the lease too, because an operator pause of a queued row (T127)
// clears its hold stamp and moves it out of the pass's reach the same
// way.
// releaseOutcome is what one candidate cost the pass. outcomeHeld spent
// nothing — no release, no engine call that took. outcomeReleased is a
// finished release: the task is downloading. outcomeSpent is the middle
// shape: the release recorded an engine handle but could not finish — a
// pending file selection, a failed first start, an ambiguous store write
// over a row that may hold a live transfer — so the slot and bytes are
// owned even though the row is not released.
type releaseOutcome int

const (
	outcomeHeld releaseOutcome = iota
	outcomeReleased
	outcomeSpent
)

func (a *Admitter) processCandidate(ctx context.Context, cand store.Candidate, p Policy, counts ActiveCounts, gate *spaceGate) (releaseOutcome, error) {
	current, err := a.tasks.Get(ctx, cand.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return outcomeHeld, nil
		}

		return outcomeHeld, fmt.Errorf("revalidate %s: %w", cand.ID, err)
	}
	if !candidateStillCurrent(cand, current) {
		return outcomeHeld, nil
	}
	// The candidate's persisted intent is re-read under the lease: an
	// operator PATCH that landed between selection and this point is the
	// intent the release owes the engine, not the pass-start snapshot's.
	cand.SelectFiles = current.SelectFiles
	cand.DLLimit = current.DLLimit
	cand.ULLimit = current.ULLimit

	if cand.State == string(StatePaused) {
		// A task the disk-space guard parked. The stamp is the whole
		// attribution: an operator pause landing on an already-parked row
		// is an idempotent no-op that keeps the stamp, and this pass
		// would resume what the operator parked. Clearing the stamp on an
		// operator pause is the action layer's takeover — T127's, not
		// this pass's. Space comes first, and it
		// fails closed: while the filesystem does not admit — or cannot
		// be read at all — the stamp stays and the task stays paused.
		// Resuming on an unreadable answer would ping-pong the transfer
		// against ENOSPC every tick, and a parked task loses nothing by
		// waiting one more tick. A limit that also blocks leaves it
		// exactly as it is — disk_full is why it is paused, and a slot is
		// only the second thing it will need. The release below resumes
		// the parked transfer through its stored handle (Engine.Resume
		// is aria2's unpause); Add is reached only when the engine lost
		// the handle, and then with resume semantics — never a duplicate.
		if held, message := gate.holdsParked(cand); held {
			a.stampHeld(ctx, cand, ErrorCodeDiskFull, message)

			return outcomeHeld, nil
		}
		if held, message := p.Limits.Blocked(counts, cand.Engine); held {
			// Space came back but the slot did not. The row keeps its
			// disk_full stamp — paused+disk_full is the selection token,
			// so re-stamping concurrency_limit would orphan the task
			// from the candidate query and it would never be re-examined
			// — and stays parked: un-pausing into a held slot would only
			// re-park it, so an operator chasing the stamp deserves the
			// real reason in the log.
			a.log.Debug("admission pass: disk space recovered; parked task now waits on a concurrency slot",
				"task_id", cand.ID, "engine", cand.Engine, "hold", message)

			return outcomeHeld, nil
		}
	} else if cand.EngineRef == nil {
		if held, message := p.Limits.Blocked(counts, cand.Engine); held {
			// The stamp is the only write a held task gets: the state
			// stays queued and the guarded SetErrorCode keeps a re-stamp
			// of the same sentence silent.
			a.stampHeld(ctx, cand, ErrorCodeConcurrencyLimit, message)

			return outcomeHeld, nil
		}
		if held, message := gate.holds(cand); held {
			a.stampHeld(ctx, cand, ErrorCodeDiskFull, message)

			return outcomeHeld, nil
		}
	}
	// A queued candidate already holding an engine handle is mid-release —
	// a pending file selection or an interrupted first start — and its slot
	// and bytes are already spent: re-gating it could stamp a hold over a
	// running transfer, so it goes straight to the release's resume path.

	// A parked candidate is claimed immediately before its first engine
	// call: one guarded no-op write over the exact paused+disk_full pair
	// the candidate was selected by. Together with the lease this closes
	// the selection-to-engine window — a clear that landed before the
	// lease or between the read above and this write declines the claim,
	// and one that comes after it waits on the lease until the engine
	// call and the release writes are done. A declined or vanished pair
	// is an overtaken candidate, not an engine rejection: (false, nil),
	// no engine call, no row or event. The claim stores nothing, so a
	// process exit after it and before the engine call needs no recovery.
	// Queued candidates take no claim write: their release mechanics are
	// unchanged.
	if cand.State == string(StatePaused) {
		taken, err := a.tasks.ClaimParkedDiskFull(ctx, cand.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return outcomeHeld, nil
			}

			return outcomeHeld, fmt.Errorf("claim %s: %w", cand.ID, err)
		}
		if !taken {
			return outcomeHeld, nil
		}
	}

	if err := a.release(ctx, cand); err != nil {
		if ctx.Err() != nil {
			return outcomeHeld, ctx.Err()
		}
		a.releaseFailed(ctx, cand, err)

		switch {
		case errors.Is(err, errSelectionPending), errors.Is(err, errReleaseIncomplete):
			// The release recorded a live engine handle: the slot is
			// spent even though the row is not released.
			return outcomeSpent, nil
		default:
			var storeWrite storeWriteError
			if errors.As(err, &storeWrite) && cand.EngineRef != nil {
				// An ambiguous store write over a handle-holding row —
				// the transfer may still exist engine-side, so the
				// conservative read spends the slot rather than
				// over-admitting behind it.
				return outcomeSpent, nil
			}
			return outcomeHeld, nil
		}
	}

	return outcomeReleased, nil
}

// candidateStillCurrent reports whether the row still carries what the
// candidate snapshot selected it by: the queued state for a queued
// snapshot, the paused+disk_full pair for a parked one. Anything else —
// moved, cleared, operator-paused — is an overtaken candidate.
func candidateStillCurrent(cand store.Candidate, current store.Task) bool {
	if cand.State == string(StatePaused) {
		return current.State == string(StatePaused) && current.ErrorCode != nil && *current.ErrorCode == ErrorCodeDiskFull
	}

	return current.State == string(StateQueued)
}

// Run drives Pass on a ticker until ctx is cancelled. load reads the
// policy each tick — settings change at runtime, so the pass must not
// cache them — and a failing load is a warning and a retry, never the
// loop's end: an admission outage must not outlive its cause. The loop is
// ticker-first, like the reconciler's, so constructing an Admitter never
// implies a pass.
func (a *Admitter) Run(ctx context.Context, load func(context.Context) (Policy, error)) error {
	ticker := time.NewTicker(a.tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			policy, err := load(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				a.log.Warn("admission pass could not load the policy; retrying on the next tick", "error", err)
				continue
			}
			if _, err := a.Pass(ctx, policy); err != nil {
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

// spaceGate is the pass's disk-reservation table (FR-047): one pool of
// committed-but-unwritten bytes per filesystem, shared by every
// destination on that mount, beside the live statfs answer of each
// filesystem the walk touches.
type spaceGate struct {
	policy  Policy
	commits map[string]int64     // filesystem id -> committed bytes of the counted active tasks
	ids     map[string]string    // destination -> filesystem id, resolved once per pass
	spaces  map[string]fsx.Space // filesystem id -> statfs answer, read once per pass
	// filesystem ids whose statfs failed this pass: a failure is cached
	// like a success, so one broken mount costs one statfs per pass, not
	// one per candidate on it.
	unreadable map[string]bool
	warned     map[string]bool // filesystem ids whose read failure was logged this pass
	log        *slog.Logger
}

// spaceGate builds the pass's reservation table: the store's
// per-destination sums folded into one pool per filesystem. A destination
// whose filesystem cannot be identified is skipped with a warn — its bytes
// go uncounted, the honest cost of never wedging the queue on a stat
// failure.
func (a *Admitter) spaceGate(ctx context.Context, p Policy) (*spaceGate, error) {
	perDestination, err := a.tasks.SumRemainingByDestination(ctx)
	if err != nil {
		return nil, err
	}

	gate := &spaceGate{policy: p, commits: make(map[string]int64, len(perDestination)), ids: make(map[string]string, len(perDestination)), spaces: make(map[string]fsx.Space), unreadable: make(map[string]bool), warned: make(map[string]bool), log: a.log}
	for destination, remaining := range perDestination {
		fsID, ok := gate.filesystemOf(destination)
		if !ok {
			continue
		}
		gate.commits[fsID] += remaining
	}

	return gate, nil
}

// filesystemOf resolves a destination to its filesystem id once per pass —
// the ancestor climb is not cheap, and a destination shared by k
// candidates is climbed once, not k times. A destination whose filesystem
// cannot be identified warns once per pass and caches the miss (the empty
// sentinel), so a persistently broken mount costs one climb per
// destination per pass, not one per candidate; its bytes go uncounted,
// the honest cost of never wedging the queue on a stat failure.
func (g *spaceGate) filesystemOf(destination string) (string, bool) {
	if fsID, cached := g.ids[destination]; cached {
		return fsID, fsID != ""
	}

	fsID, err := fsx.FilesystemID(destination)
	if err != nil {
		// One key for every identification failure: the climb reaches "/",
		// so a failure means no destination in the process can be identified
		// — one warn per pass, not one per destination.
		g.warnOnce("unidentified", destination, err)
		// The one-warn key assumes the failure is process-wide; a failure
		// scoped to a single path would still be silently uncounted, so
		// every failing destination after the first is at least visible at
		// debug.
		g.log.Debug("admission pass cannot identify a destination's filesystem",
			"destination", destination, "error", err)
		g.ids[destination] = ""
		return "", false
	}
	g.ids[destination] = fsID

	return fsID, true
}

// holdMessage is the error_message a queued space hold carries. It is a
// fixed sentence on purpose: SetErrorCodeIfState dedupes on the exact (code,
// message) pair, so a message carrying the live free/committed/floor
// numbers would be new on every tick and re-stamp the row — and feed the
// sync deltas — once per second per held task. The numbers go to the
// debug log instead, where a tick's worth of detail costs nothing. A
// parked task is stamped with diskFullMessage instead — the same sentence
// PauseDiskFull writes — so the pass and the pause never alternate two
// sentences on one row.
const holdMessage = "not enough free space beside the committed bytes and the floor; the task starts once space returns"

// holds reports whether the candidate's filesystem refuses its remaining
// bytes (FR-047). A filesystem that cannot be identified or read fails
// OPEN — a queued task must not be held hostage by a transient stat
// failure — and the numbers land in the debug log for the operator who
// is asking why nothing starts.
func (g *spaceGate) holds(cand store.Candidate) (bool, string) {
	reservation, ok := g.reservation(cand.Destination)
	if !ok {
		return false, ""
	}

	remaining := remainingBytes(cand)
	if reservation.Admits(remaining) {
		return false, ""
	}

	g.log.Debug("space gate held a candidate",
		"task_id", cand.ID, "destination", cand.Destination,
		"remaining_bytes", remaining, "free_bytes", reservation.FreeBytes,
		"committed_bytes", reservation.CommittedBytes, "min_free_bytes", reservation.MinFreeBytes)

	return true, holdMessage
}

// holdsParked is holds for a paused disk_full candidate, and it fails
// CLOSED: an unreadable filesystem holds the parked task one more tick
// instead of resuming it into the ENOSPC it was parked for.
func (g *spaceGate) holdsParked(cand store.Candidate) (bool, string) {
	reservation, ok := g.reservation(cand.Destination)
	remaining := remainingBytes(cand)
	if !ok {
		g.log.Debug("space gate cannot read a parked task's filesystem; holding it one more tick",
			"task_id", cand.ID, "destination", cand.Destination)

		return true, diskFullMessage
	}

	if reservation.Admits(remaining) {
		return false, ""
	}

	g.log.Debug("space gate keeps holding a parked task",
		"task_id", cand.ID, "destination", cand.Destination,
		"remaining_bytes", remaining, "free_bytes", reservation.FreeBytes,
		"committed_bytes", reservation.CommittedBytes, "min_free_bytes", reservation.MinFreeBytes)

	return true, diskFullMessage
}

// commit spends one released candidate's remaining bytes on its
// filesystem's pool, so the later candidates of the same pass see them
// promised — one pass cannot over-commit a filesystem the database does
// not reflect yet. A destination whose filesystem cannot be identified
// commits nothing: the release already happened, and the next pass
// re-derives the pool from the store, where the task now counts as active.
func (g *spaceGate) commit(cand store.Candidate) {
	fsID, ok := g.filesystemOf(cand.Destination)
	if !ok {
		return
	}
	g.commits[fsID] += remainingBytes(cand)
}

// warnOnce reports a filesystem read failure at most once per pass per
// filesystem, however many candidates sit on it: a persistently broken
// mount must not turn the 1 Hz pass into a per-candidate warn flood —
// the same tick-churn discipline the fixed hold sentence keeps.
func (g *spaceGate) warnOnce(fsID, destination string, err error) {
	if g.warned[fsID] {
		return
	}
	g.warned[fsID] = true
	g.log.Warn("admission pass cannot read a destination's filesystem", "destination", destination, "error", err)
}

// reservation returns the candidate's filesystem's reservation, with the
// floor of the root that owns the destination — a candidate under a root
// the min_free_space map does not name gets the 2 GiB default. ok is false
// when the filesystem cannot be identified or read.
func (g *spaceGate) reservation(destination string) (fsx.Reservation, bool) {
	fsID, ok := g.filesystemOf(destination)
	if !ok {
		return fsx.Reservation{}, false
	}

	if g.unreadable[fsID] {
		return fsx.Reservation{}, false
	}

	space, ok := g.spaces[fsID]
	if !ok {
		var err error
		space, err = fsx.FreeSpace(destination)
		if err != nil {
			g.warnOnce("space:"+fsID, destination, err)
			g.unreadable[fsID] = true
			return fsx.Reservation{}, false
		}
		g.spaces[fsID] = space
	}

	return fsx.Reservation{
		FilesystemID:   fsID,
		FreeBytes:      space.FreeBytes,
		CommittedBytes: g.commits[fsID],
		MinFreeBytes:   fsx.Floor(g.policy.MinFree, rootOf(g.policy.Roots, destination)),
	}, true
}

// remainingBytes is the candidate's committed-but-unwritten share: 0
// while the total is unknown, never negative.
func remainingBytes(cand store.Candidate) int64 {
	if cand.TotalBytes == nil {
		return 0
	}
	if remaining := *cand.TotalBytes - cand.CompletedBytes; remaining > 0 {
		return remaining
	}

	return 0
}

// rootOf returns the configured data root that owns destination — the
// longest matching root, so nested roots resolve to the innermost — or ""
// when destination lies under no configured root, in which case
// fsx.Floor's 2 GiB default applies: an unrouted destination is never
// promised the whole disk. A trailing slash in the configured spelling
// is trimmed so the match cannot silently fail; the policy loader
// normalises Roots and MinFree keys to the same clean form, so the
// trimmed answer is also the map's key. The root "/" keeps its spelling
// — a whole-filesystem root owns every absolute destination.
func rootOf(roots []string, destination string) string {
	best := ""
	for _, root := range roots {
		// Every trailing slash, not just one: a root spelled "/data//"
		// must still own "/data/x" — only its spelling differs.
		trimmed := strings.TrimRight(root, "/")
		if trimmed == "" {
			trimmed = "/"
		}
		if len(trimmed) > len(best) && withinRoot(destination, trimmed) {
			best = trimmed
		}
	}

	return best
}

// withinRoot reports whether path is root itself or a path under it — a
// segment-wise comparison, so /database is not under /data and everything
// absolute is under "/".
func withinRoot(path, root string) bool {
	if root == "/" {
		return strings.HasPrefix(path, "/")
	}

	return path == root || strings.HasPrefix(path, root+"/")
}

// stampHeld writes one hold code on a candidate the pass cannot release.
// The candidate is a snapshot from the start of the pass, so the write is
// a compare-and-set on that state: an operator action that moved the row
// after selection declines the stamp silently — a paused row stamped
// disk_full is exactly the pair the next pass resumes guard-parked tasks
// by, and minting it on a user-paused row would un-pause what the user
// parked (the same attribution limit T127 takes over at the action
// layer). A vanished task is expected mid-pass; anything else is a warn
// the next pass repeats. The pair guard keeps a re-stamp of the same
// sentence silent, so a 1 Hz pass neither churns the row nor feeds the
// sync deltas.
func (a *Admitter) stampHeld(ctx context.Context, cand store.Candidate, code, message string) {
	if err := a.tasks.SetErrorCodeIfState(ctx, cand.ID, cand.State, code, message); err != nil && !errors.Is(err, store.ErrNotFound) {
		a.log.Warn("admission pass could not stamp a held task", "task_id", cand.ID, "error", err)
	}
}

// diskFullMessage is the error_message every disk_full stamp carries — a
// fixed sentence, like holdMessage, so the guarded SetErrorCodeIfState's
// identical-pair no-op holds across repeats. The failing write's own text
// goes to the log beside the pause, not onto the row.
const diskFullMessage = "no space left on device; the task resumes once space returns"

// pauseDiskFullEventMessage is the one task_events row a disk-full pause
// writes, beside its transition.
const pauseDiskFullEventMessage = "paused by the disk-space guard: no space left on device"

// PauseDiskFull reacts to ENOSPC on a running task (FR-048): the transfer
// is paused engine-side, and the row lands paused carrying disk_full with
// exactly one task_events row — state, stamp and event commit in one
// transaction, so no concurrent hold-code clear can split the pause from
// its stamp. Nothing is unlinked: every partially downloaded byte stays
// on disk, so the next admission pass resumes the same file rather than
// restarting it. The write paths that can observe a raw ENOSPC call this
// with the failing error; fsx.IsENOSPC decides which errors qualify.
// Only the counted active states may pause, and the atomic write
// re-checks them, so a task that moved on between the caller's read and
// the landing is left untouched. A task already parked carrying disk_full
// only refreshes its stamp; a task parked for any other reason — an
// operator pause — is refused, because the admission pass would later
// read the stamp as its own and silently un-pause what the user parked.
func (a *Admitter) PauseDiskFull(ctx context.Context, id string, cause error) error {
	task, err := a.tasks.Get(ctx, id)
	if err != nil {
		// A vanished task is nothing left to pause — the same
		// vanish-tolerance the branches below keep.
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}

		return fmt.Errorf("pause disk-full task %q: %w", id, err)
	}

	switch {
	case task.State == string(StatePaused) && task.ErrorCode != nil && *task.ErrorCode == ErrorCodeDiskFull:
		// Already parked by the guard — an engine that reports disk_full
		// twice, or a write path racing the first pause. The guarded
		// SetErrorCodeIfState keeps a repeat silent, and a task that vanished
		// mid-pause is nothing left to stamp or stop.
		// The engine-side stop runs first, like the fresh path below: a
		// store failure must not leave the transfer writing against a disk
		// that just reported ENOSPC.
		if cause != nil {
			// A repeat can carry different information than the first
			// report — a different mount, a quota error — so it is logged
			// with the same message the fresh path uses, greppable together.
			a.log.Warn("pausing task after a write failed with ENOSPC",
				"task_id", id, "engine", task.Engine, "error", cause)
		}
		a.pauseEngineSide(ctx, id, task)

		if err := a.tasks.SetErrorCodeIfState(ctx, id, string(StatePaused), ErrorCodeDiskFull, diskFullMessage); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}

			return fmt.Errorf("pause disk-full task %q: %w", id, err)
		}

		return nil
	case task.State == string(StatePaused):
		return fmt.Errorf("pause disk-full task %q: the task is paused without %q; an operator pause must not gain the stamp",
			id, ErrorCodeDiskFull)
	case slices.Contains(diskFullPauseSources(), task.State):
		// The counted active states: a transfer whose write path can fail
		// with ENOSPC. Everything else — queued, seeding, error, completed,
		// removed — has no write to park.
	default:
		// A queued row can briefly own a live engine transfer — the pass
		// resumes or submits it engine-side before it lands the active
		// transition — so stop the writes even when the row-level pause is
		// refused: a disk-full report's first duty is that the daemon stops
		// writing bytes the disk just rejected.
		if task.State == string(StateQueued) {
			if cause != nil {
				// The row-level pause is refused, so this stop is the whole
				// visible reaction — it gets the cause the other branches log.
				a.log.Warn("pausing the engine-side transfer of a queued task after ENOSPC; the row-level pause is refused until the task turns active",
					"task_id", id, "engine", task.Engine, "error", cause)
			}
			a.pauseEngineSide(ctx, id, task)
		}

		return fmt.Errorf("pause disk-full task %q: a task in state %q cannot pause", id, task.State)
	}

	if cause != nil {
		a.log.Warn("pausing task after a write failed with ENOSPC",
			"task_id", id, "engine", task.Engine, "error", cause)
	}

	// Pause the transfer engine-side first, so the daemon stops writing
	// bytes dl-tool has just decided the disk cannot hold. The handle is
	// the engine-namespaced form — "aria2:<gid>", the TaskInfo.ID shape
	// the API actions pass (docs/04-data-model.md section 3.3); the
	// adapter strips its own namespace again.
	a.pauseEngineSide(ctx, id, task)

	// A vanished task is nothing left to pause, the same vanish-tolerance
	// the already-parked branch above keeps: the row is gone, so there is
	// no stamp to land and no transfer to stop.
	if err := a.tasks.PauseWithCode(ctx, id, store.CodedPause{
		EventCode:    store.CodeTaskPaused,
		EventMessage: pauseDiskFullEventMessage,
		ErrorCode:    ErrorCodeDiskFull,
		ErrorMessage: diskFullMessage,
		FromStates:   diskFullPauseSources(),
	}); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("pause disk-full task %q: %w", id, err)
	}

	return nil
}

// pauseEngineSide stops the transfer at its engine after ENOSPC. A
// failure is a warning, never a reason to keep the row active: the
// engine-side transfer will surface its own error and the reconciler
// records it.
func (a *Admitter) pauseEngineSide(ctx context.Context, id string, task store.Task) {
	if task.EngineRef == nil {
		return
	}

	e, ok := a.registry.Get(task.Engine)
	if !ok {
		// No engine to contact — the row-level pause is the whole reaction,
		// and the next sweep records the engine's absence.
		a.log.Warn("engine of a disk-full task is not registered",
			"task_id", id, "engine", task.Engine)
		return
	}

	if err := e.Pause(ctx, namespacedHandle(task.Engine, *task.EngineRef)); err != nil {
		a.log.Warn("could not pause the engine-side transfer after ENOSPC",
			"task_id", id, "engine", task.Engine, "error", err)
	}
}

// diskFullPauseSources returns the states a disk-full pause may land
// from — the counted active states. The read-side switch above consumes
// the list and the atomic write re-checks it, so the two can never drift
// apart, and a task that moved on in between is left untouched. A fresh
// slice per call keeps the shared set immutable — no caller can append to
// or zero the one list both gates read.
func diskFullPauseSources() []string {
	return []string{
		string(StateDownloading), string(StateChecking), string(StateExtracting), string(StateMoving),
	}
}

// releaseFailed must not report it as a refusal.
type storeWriteError struct{ cause error }

func (e storeWriteError) Error() string { return "admission store write: " + e.cause.Error() }

func (e storeWriteError) Unwrap() error { return e.cause }

// release hands one candidate to its engine and records the release: Add
// when the task has never been handed over, Resume when it holds a handle
// — and Add again when the engine lost that handle. A stopped disk-full
// result also needs Add: aria2 cannot unpause it, so Get first proves the
// owned result stopped for that condition before the same resume-safe
// submission path replaces its handle. The persisted intent — the
// select_files document and the dl_limit/ul_limit a PATCH may have
// written — applies on the engine handle before the transfer starts,
// because an engine that never saw the creation cannot have them. The
// state move and error-code clear are the release's own writes;
// SetEngineRef writes the acceptance event in the same transaction as
// the handle.
func (a *Admitter) release(ctx context.Context, cand store.Candidate) error {
	e, ok := a.registry.Get(cand.Engine)
	if !ok {
		return fmt.Errorf("%w: %s is not registered", ErrUnavailable, cand.Engine)
	}

	sel, err := decodeSelectionIntent(cand.SelectFiles)
	if err != nil {
		// A document the store cannot parse is operator damage, not a
		// reason to strand the task: admit without it and say so.
		a.log.Warn("stored file selection is unreadable; releasing without it",
			"task_id", cand.ID, "engine", cand.Engine, "error", err)
	}

	if cand.EngineRef != nil {
		handle := namespacedHandle(cand.Engine, *cand.EngineRef)
		// Inspect the handle before touching it: a vanished one — or an
		// aria2 stopped result, which reports as a queryable error state —
		// takes the re-add path below rather than an intent call it would
		// only fault on. aria2's stopped results answer SetRateLimits and
		// SetFiles with a generic wrong-state fault, not ErrNotFound, so
		// sending the persisted intent first would misread the shape as a
		// refusal and error a task that was meant to be re-added.
		info, getErr := e.Get(ctx, handle)
	handleCheck:
		switch {
		case errors.Is(getErr, ErrNotFound):
			// The engine lost the handle (an aria2 daemon restart, for
			// example); fall through to Add with resume semantics.
		case getErr != nil:
			return fmt.Errorf("inspect engine handle %q: %w", handle, getErr)
		case info.State == StateError:
			if info.ErrorCode != ErrorCodeDiskFull {
				// A stopped result carrying a different failure is not the
				// park-and-resume shape the re-add below exists for.
				return fmt.Errorf("engine %q holds a stopped result for %q: %s %s",
					cand.Engine, handle, info.ErrorCode, info.ErrorMessage)
			}
			// aria2 cannot unpause a stopped disk-full result; fall
			// through to Add with resume semantics.
		default:
			// A selection owed to a transfer that is already running —
			// the pass itself resumed it for metadata on an earlier tick —
			// must land on it stopped: aria2's select-file is not on
			// changeOption's active-safe list, so SetFiles faults on a
			// running GID even though the listing is there to apply. The
			// pause is also harmless on engines that take the call on a
			// live transfer, and the Resume below restarts it either way.
			if sel != nil && info.State == StateDownloading {
				if err := e.Pause(ctx, handle); err != nil {
					if errors.Is(err, ErrNotFound) {
						// The handle vanished between the check and the
						// call; fall through to Add with resume semantics.
						break handleCheck
					}
					return fmt.Errorf("pause %q so the pending file selection can land: %w", handle, err)
				}
			}
			// The engine-side transfer is stopped, so the persisted
			// limits and selection land before any byte moves.
			intentErr := applyPersistedIntent(ctx, e, handle, cand.DLLimit, cand.ULLimit, sel)
			var resumeErr error
			if intentErr == nil {
				resumeErr = e.Resume(ctx, handle)
			}
			switch {
			case errors.Is(intentErr, ErrNotFound) || errors.Is(resumeErr, ErrNotFound):
				// The handle died between the check and the call; fall
				// through to Add with resume semantics.
			case errors.Is(intentErr, errSelectionPending):
				// The engine cannot list the files yet — a stopped
				// torrent fetches its metadata only while running, so
				// holding it stopped would deadlock the selection. Let it
				// run and keep the pass's ownership: a queued row is never
				// adopted by the reconciler, so a parked candidate is
				// requeued first and the next pass retries through this
				// same path until the listing exists.
				a.requeuePendingSelection(ctx, cand)
				a.resumeForMetadata(ctx, e, handle, cand)
				return intentErr
			case intentErr != nil:
				return intentErr
			case resumeErr == nil:
				if err := a.markReleased(ctx, cand.ID); err != nil {
					return storeWriteError{cause: err}
				}
				return nil
			default:
				return resumeErr
			}
		}
	}

	req, ok := admissionRequest(cand, sel)
	if !ok {
		return errNoSubmission
	}

	newID, err := e.Add(ctx, req)
	if err != nil {
		return err
	}

	// The persisted intent lands on the fresh transfer before the release
	// is recorded — engines that cannot take a selection or a limit at
	// add time (qBittorrent's WebAPI ignores AddRequest.SelectFiles) get
	// it here, and StartPaused held the transfer stopped so no byte moved
	// first. A refusal errors the task and removes the transfer this pass
	// just created, so nothing orphaned keeps running; a pending listing,
	// a vanished-fresh handle or an outage is a wait instead — the handle
	// is recorded and the next pass retries through the resume path.
	intentErr := applyPersistedIntent(ctx, e, newID, cand.DLLimit, cand.ULLimit, sel)
	if intentErr != nil && !errors.Is(intentErr, errSelectionPending) &&
		!errors.Is(intentErr, ErrUnavailable) && !errors.Is(intentErr, ErrNotFound) {
		a.removeStrandedTransfer(ctx, e, cand, newID, "refused the persisted limits or file selection", intentErr)
		return intentErr
	}

	if err := a.tasks.SetEngineRef(ctx, cand.ID, bareHandle(cand.Engine, newID)); err != nil {
		// The transfer now exists engine-side while the row still carries
		// no handle, so the next pass would add it again — and every
		// duplicate would be foreign under ADR-0017 and untouchable.
		// Compensate by removing the transfer this pass itself just
		// created (its own Add receipt, not a foreign one). If even the
		// removal fails, name the handle so the operator can remove it.
		a.removeStrandedTransfer(ctx, e, cand, newID, "released but could not record the handle", err)

		return storeWriteError{cause: err}
	}

	if errors.Is(intentErr, errSelectionPending) {
		// Run the fresh transfer for its metadata and keep the row
		// queued: the reconciler never adopts a queued row, and the next
		// pass applies the selection through the resume path. A parked
		// candidate re-adds through this same branch — the requeue is what
		// keeps its paused row from being adopted to downloading out from
		// under the retry.
		if a.requeuePendingSelection(ctx, cand) {
			a.resumeForMetadata(ctx, e, newID, cand)
		}
		return intentErr
	}
	if intentErr != nil {
		return fmt.Errorf("%w: %v", errReleaseIncomplete, intentErr)
	}

	if req.StartPaused {
		if err := e.Resume(ctx, newID); err != nil {
			// The handle is recorded, so a failed first start is not a
			// loss: the next pass re-enters through the resume path, where
			// the intent re-states idempotently before Resume.
			return fmt.Errorf("%w: %v", errReleaseIncomplete, err)
		}
	}

	if err := a.markReleased(ctx, cand.ID); err != nil {
		return storeWriteError{cause: err}
	}

	return nil
}

// requeuePendingSelection returns a parked candidate whose file listing
// is still pending to queued: the release keeps an engine handle it owes
// a selection to, and queued is the one state the reconciler never
// adopts engine state over — left paused, the sweep would adopt the
// metadata fetch's downloading report and the selection would never be
// retried. Queued candidates skip the write. The bool says whether the
// caller may run the transfer: a parked row whose requeue failed must
// not move, because running it would let the sweep adopt the row before
// the next pass can retry.
func (a *Admitter) requeuePendingSelection(ctx context.Context, cand store.Candidate) bool {
	if cand.State != string(StatePaused) {
		return true
	}
	if err := a.tasks.Transition(ctx, cand.ID, string(StateQueued), store.CodeTaskResumed,
		"file selection waits for the engine's file listing; requeued for retry"); err != nil {
		a.log.Warn("could not requeue the parked task whose file listing is pending",
			"task_id", cand.ID, "engine", cand.Engine, "error", err)
		return false
	}
	return true
}

// resumeForMetadata starts a transfer whose file selection is waiting on
// an empty listing: a stopped torrent fetches its metadata only while
// running, so holding it stopped would deadlock the selection. A failure
// is a warning, never a release outcome — the next pass retries both the
// selection and the start through the same path.
func (a *Admitter) resumeForMetadata(ctx context.Context, e Engine, handle string, cand store.Candidate) {
	if err := e.Resume(ctx, handle); err != nil {
		a.log.Warn("could not run the transfer whose file listing is pending",
			"task_id", cand.ID, "engine", cand.Engine, "error", err)
	}
}

// removeStrandedTransfer compensates a hand-off failure by removing the
// transfer this pass itself just created (its own Add receipt, not a
// foreign one), on a context that survives the cancellation that may
// have caused the failure and carries its own deadline — WithoutCancel
// also drops the budget a Boot was running under, and no engine is owed
// an unbounded wait.
func (a *Admitter) removeStrandedTransfer(ctx context.Context, e Engine, cand store.Candidate, newID, why string, cause error) {
	removeCtx, cancelRemove := context.WithTimeout(context.WithoutCancel(ctx), compensateRemoveBudget)
	defer cancelRemove()
	if removeErr := e.Remove(removeCtx, newID); removeErr != nil {
		a.log.Error(why+", and the compensating removal failed; the transfer is stranded engine-side",
			"task_id", cand.ID, "engine", cand.Engine, "engine_ref", bareHandle(cand.Engine, newID),
			"error", cause, "remove_error", removeErr)
	} else {
		a.log.Warn(why+"; removed the new transfer so the next pass cannot duplicate it",
			"task_id", cand.ID, "engine", cand.Engine, "error", cause)
	}
}

// clearHoldCode clears one task's hold stamp, warning with consequence
// when the clear fails — the shared body of markReleased's and
// clearStaleStamp's cleanups. A vanished task is expected mid-pass;
// anything else is a warn naming what the leftover stamp means.
func (a *Admitter) clearHoldCode(ctx context.Context, id, consequence string) {
	if err := a.tasks.ClearHoldCode(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		a.log.Warn(consequence, "task_id", id, "error", err)
	}
}

// markReleased moves the released task to downloading — the task.resumed
// event of the release — and clears any stale hold code, so a started task
// never carries the code that held it (docs/05-api-contract.md section
// 5.11). The clear is guarded on the row not being paused: a concurrent
// disk-full pause may have parked the task between the two writes, and
// wiping its stamp would strand it outside the pass's selection (FR-048).
// The release is complete once the transition lands: a failed stamp clear
// is a warning, never an error handed back — a returned error would route
// the healthy downloading task into releaseFailed and mislabel it as
// refused. Nothing else clears the stamp afterwards: the pass selects
// only queued and disk_full-paused candidates, so it never revisits a
// downloading row — the residual code rides the row until the task queues
// again or an operator acts.
func (a *Admitter) markReleased(ctx context.Context, id string) error {
	if err := a.tasks.Transition(ctx, id, string(StateDownloading), store.CodeTaskResumed, "released by the admission pass"); err != nil {
		return err
	}

	a.clearHoldCode(ctx, id, "released but could not clear the stale hold stamp; it stays on the downloading row until requeue or operator action")

	return nil
}

// namespacedHandle renders the engine task id from the stored bare ref —
// the TaskInfo.ID shape, "aria2:<gid>" (docs/04-data-model.md section
// 3.3). It is the one home for the join inside this package: the pass's
// resume and the disk-full pause share it. The API action layer renders
// the same shape at its own call site (internal/api/tasks_actions.go);
// the shape is pinned by the adapter, which strips its own namespace
// again, and routing that call site through this helper belongs to the
// task that owns the file (T127), not to a cross-package export made
// speculatively.
func namespacedHandle(engineName, ref string) string {
	return engineName + ":" + ref
}

// releaseFailed records one release the pass could not complete. An
// unreachable or unregistered engine is an outage, not a refusal — the
// candidate stays queued and the next tick retries, exactly the
// reconciler's policy for an engine that is down; the adapters wrap their
// transport failures in ErrUnavailable, so the branch sees every outage.
// A failure of the pass's own writes is the same patience: the cause is
// dl-tool's storage, never an engine decision. A refusal on a live
// context — an engine-phase error that is none of the above — is the
// task_events vocabulary's engine.rejected moment
// (internal/store/events.go): the task moves to error carrying the
// refusal, and error -> queued remains the retry path. The three
// staying-queued branches also drop a stale hold stamp: the hold no
// longer applies to this task, and an operator reading a held code would
// chase slots instead of the down engine or the failing write.
func (a *Admitter) releaseFailed(ctx context.Context, cand store.Candidate, cause error) {
	var storeWrite storeWriteError
	switch {
	case errors.As(cause, &storeWrite):
		a.clearStaleStamp(ctx, cand.ID)
		a.log.Warn("admission store write failed; retrying on the next tick",
			"task_id", cand.ID, "engine", cand.Engine, "error", cause)
	case errors.Is(cause, errNoSubmission):
		// Nothing to hand the engine: a queued row without a stored source
		// or infohash. The reconciler answers the same shape with a
		// recurring warning; the row is the operator's to look at.
		a.clearStaleStamp(ctx, cand.ID)
		a.log.Warn("queued task has no source to release", "task_id", cand.ID, "engine", cand.Engine)
	case errors.Is(cause, ErrUnavailable):
		a.clearStaleStamp(ctx, cand.ID)
		a.log.Warn("engine unreachable at hand-off; retrying on the next tick",
			"task_id", cand.ID, "engine", cand.Engine, "error", cause)
	case errors.Is(cause, errSelectionPending):
		a.clearStaleStamp(ctx, cand.ID)
		// The row holds a live engine handle and the pass retries every
		// tick: routine, so a debug line, not a per-tick warning.
		a.log.Debug("file selection waits for the engine's file listing; retrying on the next tick",
			"task_id", cand.ID, "engine", cand.Engine, "error", cause)
	case errors.Is(cause, errReleaseIncomplete):
		a.clearStaleStamp(ctx, cand.ID)
		a.log.Debug("release left incomplete; retrying on the next tick",
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

// clearStaleStamp drops a hold stamp a candidate no longer deserves — its
// release failed for a reason that is not the hold. Best effort: a
// vanished task is expected mid-pass, anything else is a warn the next
// pass repeats.
func (a *Admitter) clearStaleStamp(ctx context.Context, id string) {
	a.clearHoldCode(ctx, id, "could not clear a stale hold stamp; the next pass repeats the clear")
}

// admissionRequest rebuilds the engine submission from the stored
// identity, the reconciler's resubmitRequest shape: the source URI when
// dl-tool kept one, the infohash as a magnet otherwise. resumeExtra
// carries aria2's --continue so a re-add resumes partial data; engines
// without such an option ignore it. The persisted selection rides
// AddRequest.SelectFiles for the engines that honour it at add time —
// applyPersistedIntent covers the rest. Any persisted intent starts the
// add paused: an engine that ignores SelectFiles and the limits at add
// time would otherwise run files the selection excludes or at rates the
// task does not allow before applyPersistedIntent lands.
func admissionRequest(cand store.Candidate, sel *store.SelectionIntent) (AddRequest, bool) {
	var selectFiles []int
	if sel != nil {
		selectFiles = sel.Indices
	}
	startPaused := sel != nil || cand.DLLimit > 0 || cand.ULLimit > 0

	switch {
	case cand.SourceURI != nil && *cand.SourceURI != "":
		return AddRequest{URIs: []string{*cand.SourceURI}, SaveDir: cand.Destination, SelectFiles: selectFiles, StartPaused: startPaused, Extra: resumeExtra()}, true
	case cand.InfohashV1 != nil && *cand.InfohashV1 != "":
		return AddRequest{URIs: []string{magnetInfohashPrefix + *cand.InfohashV1}, SaveDir: cand.Destination, SelectFiles: selectFiles, StartPaused: startPaused, Extra: resumeExtra()}, true
	default:
		return AddRequest{}, false
	}
}

// errSelectionPending reports that the engine cannot apply a file
// selection yet because its file listing is still empty — a magnet whose
// metadata has not arrived. It is a wait, not a refusal: the release
// keeps the row queued and the next pass retries, the same patience an
// unreachable engine gets.
var errSelectionPending = errors.New("engine: the task's file listing is not available yet")

// errReleaseIncomplete marks a release that recorded a fresh handle but
// could not finish the hand-off — the persisted intent or the first start
// failed transiently. The row stays queued with its engine_ref, so the
// reconciler leaves it to the pass and the next tick resumes the release
// where it stopped.
var errReleaseIncomplete = errors.New("engine: the release did not complete; the next pass resumes it")

// decodeSelectionIntent parses the stored select_files document; a nil
// or empty column means the task carries no create-time selection.
func decodeSelectionIntent(raw *string) (*store.SelectionIntent, error) {
	if raw == nil || *raw == "" {
		return nil, nil
	}

	sel, err := store.DecodeSelectionIntent(*raw)
	if err != nil {
		return nil, err
	}

	return &sel, nil
}

// applyPersistedIntent lands the candidate's stored intent on one engine
// handle: the per-task rate limits a PATCH wrote before admission, then
// the persisted file selection. Both calls run before the transfer is
// allowed to move a byte — the callers order them ahead of Resume and
// markReleased. The reconciler's re-submission runs the same pair on the
// fresh handle, which is why they are package functions, not admitter
// methods.
func applyPersistedIntent(ctx context.Context, e Engine, handle string, dlLimit, ulLimit int64, sel *store.SelectionIntent) error {
	if err := applyPersistedRateLimits(ctx, e, handle, dlLimit, ulLimit); err != nil {
		return err
	}

	return applyPersistedSelection(ctx, e, handle, sel)
}

// applyPersistedRateLimits pushes the stored limits to the handle; a
// zero limit means unset, so it is never sent — the engine's own default
// stands. ErrNotSupported is not a failure, the live-PATCH rule
// (internal/api/tasks_actions.go applyLiveRateLimits): an engine that
// cannot take per-task limits simply runs unlimited.
func applyPersistedRateLimits(ctx context.Context, e Engine, handle string, dlLimit, ulLimit int64) error {
	var down, up *int64
	if dlLimit > 0 {
		down = &dlLimit
	}
	if ulLimit > 0 {
		up = &ulLimit
	}
	if down == nil && up == nil {
		return nil
	}

	err := e.SetRateLimits(ctx, handle, down, up)
	if err == nil || errors.Is(err, ErrNotSupported) {
		return nil
	}

	return fmt.Errorf("apply persisted rate limits: %w", err)
}

// applyPersistedSelection hands the stored selection to the engine
// through SetFiles — the post-add application for the engines whose add
// cannot take one (qBittorrent's WebAPI), and the re-statement for those
// that can. The priorities map reaches only engines declaring
// per_file_priority: aria2's SetFiles rejects it outright, and the
// create-time gate already kept high and maximum off a select-only
// engine, so dropping the map loses nothing the engine could honour.
// A SetFiles the engine refuses while its listing is still empty is
// errSelectionPending; a refusal on a populated listing is a real
// rejection. ErrNotSupported is silent: validation admitted only capable
// engines, and a capability the registry lost since cannot be held for.
func applyPersistedSelection(ctx context.Context, e Engine, handle string, sel *store.SelectionIntent) error {
	if sel == nil || !slices.Contains(e.Capabilities(), CapPerFileSelect) {
		return nil
	}

	var priorities map[int]int
	if slices.Contains(e.Capabilities(), CapPerFilePriority) {
		priorities = sel.Priorities
	}

	err := e.SetFiles(ctx, handle, sel.Indices, priorities)
	if err == nil {
		return nil
	}

	// The listing decides what a SetFiles failure means. An engine that
	// cannot enumerate files yet — a metadata-less magnet, whose Files
	// may answer ErrNotFound (aria2) as readily as an empty list — has
	// nothing to apply the selection against, so the release waits. An
	// unreachable engine is an outage, not a wait. Every other listing
	// failure is a fault, not a wait either: the transfer is running for
	// its metadata, so waiting forever would let it download every file
	// the selection excludes — pause it so the refusal path below errors
	// the task over a stopped transfer, never a running one. And a
	// refusal on a populated listing is real — including ErrNotSupported,
	// which an empty indices slice draws from aria2 ("select nothing" is
	// inexpressible there): surfacing it refuses the task honestly
	// instead of silently downloading every file.
	entries, filesErr := e.Files(ctx, handle)
	switch {
	case errors.Is(filesErr, ErrNotFound) || filesErr == nil && len(entries) == 0:
		return errSelectionPending
	case filesErr != nil:
		if pauseErr := e.Pause(ctx, handle); pauseErr != nil && !errors.Is(pauseErr, ErrNotFound) {
			return fmt.Errorf("pause the transfer after the file listing failed (%w): %v", filesErr, pauseErr)
		}
		return fmt.Errorf("check the file listing after a refused selection: %w", filesErr)
	}

	return fmt.Errorf("apply persisted file selection: %w", err)
}
