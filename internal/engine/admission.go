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
	// A root the map does not carry resolves to fsx.DefaultMinFreeBytes;
	// an explicit 0 disables the floor for that root.
	MinFree map[string]int64
	// Roots are the configured data roots (DLTOOL_DATA_ROOTS): the floor
	// of a candidate is the floor of the root that owns its destination.
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
	// both, so a parked task never starves behind newer queued ones.
	SelectQueuedCandidates(ctx context.Context, limit int) ([]Candidate, error)
	// SumRemainingByDestination returns the committed-but-unwritten bytes
	// per destination over the counted active states.
	SumRemainingByDestination(ctx context.Context) (map[string]int64, error)
	// PauseWithCode lands the disk-full pause atomically: state, stamp and
	// event in one transaction.
	PauseWithCode(ctx context.Context, id string, pause store.CodedPause) error
	// ClearHoldCode clears a hold stamp unless the task is paused — the
	// release cleanup's guarded form of SetErrorCode("", "").
	ClearHoldCode(ctx context.Context, id string) error
	Get(ctx context.Context, id string) (store.Task, error)
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
// neither a limit nor a filesystem the database does not reflect yet.
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
	// A store failure aborts the pass — the next tick retries — while a
	// stat failure inside the gate fails open: a queue must not wedge on a
	// transient filesystem answer.
	gate, err := a.spaceGate(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("admission pass: %w", err)
	}

	released := make([]string, 0, len(candidates))
	for _, cand := range candidates {
		if ctx.Err() != nil {
			return released, ctx.Err()
		}

		if cand.State == string(StatePaused) {
			// A task the disk-space guard parked. Space comes first, and it
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
				continue
			}
			if held, message := p.Limits.Blocked(counts, cand.Engine); held {
				// Space came back but the slot did not. The row keeps its
				// disk_full stamp and stays parked — un-pausing into a held
				// slot would only re-park it — so an operator chasing the
				// stamp deserves the real reason in the log.
				a.log.Debug("admission pass: disk space recovered; parked task now waits on a concurrency slot",
					"task_id", cand.ID, "engine", cand.Engine, "hold", message)
				continue
			}
		} else {
			if held, message := p.Limits.Blocked(counts, cand.Engine); held {
				// The stamp is the only write a held task gets: the state
				// stays queued and the guarded SetErrorCode keeps a re-stamp
				// of the same sentence silent.
				a.stampHeld(ctx, cand, ErrorCodeConcurrencyLimit, message)
				continue
			}
			if held, message := gate.holds(cand); held {
				a.stampHeld(ctx, cand, ErrorCodeDiskFull, message)
				continue
			}
		}

		if err := a.release(ctx, cand); err != nil {
			if ctx.Err() != nil {
				return released, ctx.Err()
			}
			a.releaseFailed(ctx, cand, err)
			continue
		}

		// The release has promised both a slot and the candidate's remaining
		// bytes; spend each in memory so the later candidates of this same
		// pass see both gone.
		counts.Total++
		counts.ByEngine[cand.Engine]++
		gate.commit(cand)
		released = append(released, cand.ID)
	}

	return released, nil
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
	spaces  map[string]fsx.Space // filesystem id -> statfs answer, read once per pass
	warned  map[string]bool      // filesystem ids whose read failure was logged this pass
	log     *slog.Logger
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

	gate := &spaceGate{policy: p, commits: make(map[string]int64, len(perDestination)), spaces: make(map[string]fsx.Space), warned: make(map[string]bool), log: a.log}
	for destination, remaining := range perDestination {
		fsID, err := fsx.FilesystemID(destination)
		if err != nil {
			a.log.Warn("admission pass could not pool a destination's committed bytes", "destination", destination, "error", err)
			continue
		}
		gate.commits[fsID] += remaining
	}

	return gate, nil
}

// holdMessage is the error_message a queued space hold carries. It is a
// fixed sentence on purpose: SetErrorCode dedupes on the exact (code,
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
// not reflect yet.
func (g *spaceGate) commit(cand store.Candidate) {
	fsID, err := fsx.FilesystemID(cand.Destination)
	if err != nil {
		// The release already happened; the next pass re-derives the pool
		// from the store, where the task now counts as active. Keyed
		// "unidentified" like the reservation reads, so a mount that
		// cannot be identified warns once per pass, not once per released
		// candidate on it.
		g.warnOnce("unidentified", cand.Destination, fmt.Errorf("could not commit a released task's bytes: %w", err))
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
	fsID, err := fsx.FilesystemID(destination)
	if err != nil {
		// One key for every identification failure: the climb reaches "/",
		// so a failure means no destination in the process can be identified
		// — one warn per pass, not one per destination.
		g.warnOnce("unidentified", destination, err)
		return fsx.Reservation{}, false
	}

	space, ok := g.spaces[fsID]
	if !ok {
		space, err = fsx.FreeSpace(destination)
		if err != nil {
			g.warnOnce("space:"+fsID, destination, err)
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
// A vanished task is expected mid-pass; anything else is a warn the next
// pass repeats. The guarded SetErrorCode keeps a re-stamp of the same
// sentence silent, so a 1 Hz pass neither churns the row nor feeds the
// sync deltas.
func (a *Admitter) stampHeld(ctx context.Context, cand store.Candidate, code, message string) {
	if err := a.tasks.SetErrorCode(ctx, cand.ID, code, message); err != nil && !errors.Is(err, store.ErrNotFound) {
		a.log.Warn("admission pass could not stamp a held task", "task_id", cand.ID, "error", err)
	}
}

// diskFullMessage is the error_message every disk_full stamp carries — a
// fixed sentence, like holdMessage, so the guarded SetErrorCode's
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
		return fmt.Errorf("pause disk-full task %q: %w", id, err)
	}

	switch {
	case task.State == string(StatePaused) && task.ErrorCode != nil && *task.ErrorCode == ErrorCodeDiskFull:
		// Already parked by the guard — an engine that reports disk_full
		// twice, or a write path racing the first pause. The guarded
		// SetErrorCode keeps a repeat silent.
		if err := a.tasks.SetErrorCode(ctx, id, ErrorCodeDiskFull, diskFullMessage); err != nil {
			return fmt.Errorf("pause disk-full task %q: %w", id, err)
		}

		return nil
	case task.State == string(StatePaused):
		return fmt.Errorf("pause disk-full task %q: the task is paused without %q; an operator pause must not gain the stamp",
			id, ErrorCodeDiskFull)
	case task.State == string(StateDownloading), task.State == string(StateChecking),
		task.State == string(StateExtracting), task.State == string(StateMoving):
		// The counted active states: a transfer whose write path can fail
		// with ENOSPC. Everything else — queued, seeding, error, completed,
		// removed — has no write to park.
	default:
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
	// adapter strips its own namespace again. A failure here is a
	// warning, never a reason to keep the row active: the engine-side
	// transfer will surface its own error and the reconciler records it.
	if task.EngineRef != nil {
		if e, ok := a.registry.Get(task.Engine); ok {
			if err := e.Pause(ctx, namespacedHandle(task.Engine, *task.EngineRef)); err != nil {
				a.log.Warn("could not pause the engine-side transfer after ENOSPC",
					"task_id", id, "engine", task.Engine, "error", err)
			}
		} else {
			// No engine to contact — the row-level pause below is the whole
			// reaction, and the next sweep records the engine's absence.
			a.log.Warn("engine of a disk-full task is not registered",
				"task_id", id, "engine", task.Engine)
		}
	}

	return a.tasks.PauseWithCode(ctx, id, store.CodedPause{
		EventCode:    store.CodeTaskPaused,
		EventMessage: pauseDiskFullEventMessage,
		ErrorCode:    ErrorCodeDiskFull,
		ErrorMessage: diskFullMessage,
		FromStates:   diskFullPauseSources,
	})
}

// diskFullPauseSources are the states a disk-full pause may land from —
// the counted active states, the same set the read-side switch above
// allow-lists, so the atomic write re-checks what the caller read and a
// task that moved on in between is left untouched.
var diskFullPauseSources = []string{
	string(StateDownloading), string(StateChecking), string(StateExtracting), string(StateMoving),
}

// releaseFailed must not report it as a refusal.
type storeWriteError struct{ cause error }

func (e storeWriteError) Error() string { return "admission store write: " + e.cause.Error() }

func (e storeWriteError) Unwrap() error { return e.cause }

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
		err := e.Resume(ctx, namespacedHandle(cand.Engine, *cand.EngineRef))
		switch {
		case err == nil:
			if err := a.markReleased(ctx, cand.ID); err != nil {
				return storeWriteError{cause: err}
			}
			return nil
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

		return storeWriteError{cause: err}
	}

	if err := a.markReleased(ctx, cand.ID); err != nil {
		return storeWriteError{cause: err}
	}

	return nil
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
// 3.3). One home for the join, so the pass's resume, the disk-full pause
// and the API actions cannot drift apart in how they address an engine.
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
