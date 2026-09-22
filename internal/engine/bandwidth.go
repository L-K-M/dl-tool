// The global bandwidth governor of docs/06-download-engines.md section 10:
// one owner for the process-wide download and upload limits, fanned out to
// every registered engine through Engine.SetRateLimits with an empty id.
// Bytes per second is the only unit on this path — 0 means unlimited and is
// pushed as the literal 0, never skipped — and no KB/s value exists anywhere
// in the code path, so no conversion appears here.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/L-K-M/dl-tool/internal/store"
)

// The settings keys of docs/11-config-reference.md section 5 the
// governor owns. All four are integer bytes per second; an absent
// download_rate_limit or upload_rate_limit row is the documented
// default of 0, unlimited, and an absent alternative pair is the
// documented default below.
const (
	settingDownloadRateLimit    = "download_rate_limit"
	settingUploadRateLimit      = "upload_rate_limit"
	settingAltDownloadRateLimit = "alt_download_rate_limit"
	settingAltUploadRateLimit   = "alt_upload_rate_limit"

	// The documented defaults of the alternative pair
	// (docs/11-config-reference.md section 5): the values a `2` cell
	// pushes while the operator never saved one.
	defaultAltDownloadRateLimit int64 = 5242880
	defaultAltUploadRateLimit   int64 = 1048576
)

// RateLimits is one direction pair in bytes per second. Zero means
// unlimited. There is no KB/s representation anywhere in dl-tool: doc 04
// section 1.4 makes bytes the only unit. The name is not Limits: admission
// already declares that type in this package for the max_active_*
// concurrency caps.
type RateLimits struct {
	Down int64
	Up   int64
}

// Mode is the active schedule cell's meaning. T081 sets it; the governor
// only needs the vocabulary now, and ModeDefault is the zero-value-free
// steady state every boot starts in.
type Mode string

const (
	ModeNoDownload  Mode = "no_download"
	ModeDefault     Mode = "default"
	ModeAlternative Mode = "alternative"
)

// Governor owns the global bandwidth state and is the only caller of
// Engine.SetRateLimits with an empty id. The registry is the enabled set:
// the composition root registers exactly the engines the environment
// configured, so every entry the governor iterates is one the operator
// enabled. It is safe for concurrent use.
type Governor struct {
	reg      *Registry
	settings *store.SettingsStore
	// tasks is the parked-set collaborator WithTasks attaches: the
	// TaskStore ApplyMode parks through and releases from. nil leaves
	// the fan-out working but fails a No Download cell closed — without
	// the store the parked ids could never be resumed.
	tasks *store.TaskStore

	// applyMu serialises whole applies — fan-outs and mode applies
	// alike; mu guards only current and mode, so Current and Mode never
	// stall behind an engine apply that runs to the caller's deadline.
	applyMu sync.Mutex
	mu      sync.Mutex
	current RateLimits
	mode    Mode
}

// NewGovernor returns the governor over the shared registry and the
// settings rows the stored limits live in.
func NewGovernor(reg *Registry, st *store.SettingsStore) *Governor {
	return &Governor{reg: reg, settings: st}
}

// WithTasks attaches the TaskStore ApplyMode uses for the parked set and
// returns g so the composition root chains it off NewGovernor. A nil
// store leaves the engine-side fan-out working, but ApplyMode rejects a
// No Download cell before pausing any engine — without the store the
// parked ids could never be resumed. Production always attaches it.
func (g *Governor) WithTasks(ts *store.TaskStore) *Governor {
	g.tasks = ts
	return g
}

// Mode returns the mode last applied — recorded only when the whole
// apply succeeded, so a failed cell is retried on the next tick instead
// of being mistaken for a landed one.
func (g *Governor) Mode() Mode {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.mode
}

// Current returns the limits last applied — recorded only when no engine
// rejected the fan-out, so a failed apply is never mistaken for a landed
// one and a caller comparing against Current still retries it. Engines
// skipped for ErrNotSupported, and an empty registry, count as success.
func (g *Governor) Current() RateLimits {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.current
}

// ApplyGlobal pushes l to every registered engine and then reads each one
// back to confirm where the adapter exposes a read-back surface. A read-back
// mismatch is logged at warn with the requested and observed values and does
// not fail the call. An engine returning ErrNotSupported is skipped. Errors
// from individual engines are wrapped with the engine name and joined with
// errors.Join, so one unreachable daemon never blocks the others. applyMu
// serialises whole applies — two concurrent fan-outs cannot interleave —
// while the engines inside one apply run in parallel, so an engine that
// hangs until the context ends cannot starve the ones behind it of the
// shared deadline.
func (g *Governor) ApplyGlobal(ctx context.Context, l RateLimits) error {
	g.applyMu.Lock()
	defer g.applyMu.Unlock()

	g.mu.Lock()
	mode := g.mode
	g.mu.Unlock()
	if mode == ModeAlternative {
		// A write during an alternative cell must not leave the
		// engines on the global pair until the cell changes —
		// re-apply the pair the cell names.
		return g.applyCellLimits(ctx, mode)
	}

	return g.applyGlobal(ctx, l)
}

// applyGlobal is ApplyGlobal's body, callable with applyMu already held
// so a mode apply can fan out inside its own serialisation.
func (g *Governor) applyGlobal(ctx context.Context, l RateLimits) error {
	var (
		wg    sync.WaitGroup
		errMu sync.Mutex
		errs  []error
	)
	for _, name := range g.reg.Names() {
		e, ok := g.reg.Get(name)
		if !ok {
			continue
		}

		wg.Add(1)
		go func(name string, e Engine) {
			defer wg.Done()

			down, up := l.Down, l.Up
			err := e.SetRateLimits(ctx, "", &down, &up)
			if errors.Is(err, ErrNotSupported) {
				return
			}
			if err != nil {
				errMu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				errMu.Unlock()
				return
			}

			g.readBack(ctx, e, name, l)
		}(name, e)
	}
	wg.Wait()

	if len(errs) == 0 {
		g.mu.Lock()
		g.current = l
		g.mu.Unlock()
	}

	return errors.Join(errs...)
}

// globalLimitReader is the read-back surface an adapter may implement: the
// daemon's configured global limits in bytes per second, queried — never
// echoed from the set request. qbittorrent.Client answers it over
// GET /api/v2/transfer/info, the verification docs/06 section 10.1 asks
// for. An adapter without the surface is still fanned out to; its set is
// synchronous, so a nil error already means the daemon took the value, and
// only the extra confirmation is missing.
type globalLimitReader interface {
	GlobalLimits(ctx context.Context) (down, up int64, err error)
}

// readBack confirms one engine's global pair landed by querying the daemon
// itself. A mismatch is a warn carrying both numbers, never an error: the
// fan-out already succeeded, and the observed values are the operator's
// signal that the daemon disagrees. An engine with no read-back surface
// records a debug line so the missing confirmation is visible rather than
// silently absent.
func (g *Governor) readBack(ctx context.Context, e Engine, name string, want RateLimits) {
	reader, ok := e.(globalLimitReader)
	if !ok {
		slog.DebugContext(ctx, "engine: no global rate-limit read-back surface",
			"engine", name)
		return
	}

	gotDown, gotUp, err := reader.GlobalLimits(ctx)
	if err != nil {
		slog.WarnContext(ctx, "engine: global rate limits could not be read back",
			"engine", name, "err", err)
		return
	}
	if gotDown != want.Down {
		slog.WarnContext(ctx, "engine: download rate limit read back different from what was sent",
			"engine", name, "direction", "download",
			"sent_bps", want.Down, "read_bps", gotDown)
	}
	if gotUp != want.Up {
		slog.WarnContext(ctx, "engine: upload rate limit read back different from what was sent",
			"engine", name, "direction", "upload",
			"sent_bps", want.Up, "read_bps", gotUp)
	}
}

// LoadAndApply reads download_rate_limit and upload_rate_limit from the
// settings table and calls ApplyGlobal. It is called once at boot; the
// settings write path (T092) calls it again after a write that touches
// either key — that call site does not exist yet.
func (g *Governor) LoadAndApply(ctx context.Context) error {
	l, err := g.loadLimits(ctx, settingDownloadRateLimit, settingUploadRateLimit, 0, 0)
	if err != nil {
		return err
	}

	return g.ApplyGlobal(ctx, l)
}

// loadLimits reads one direction pair from the settings table; def
// covers an absent row. A nil settings store is a wiring error — the
// governor was built without the rows its modes load.
func (g *Governor) loadLimits(ctx context.Context, downKey, upKey string, downDef, upDef int64) (RateLimits, error) {
	if g.settings == nil {
		return RateLimits{}, errors.New("engine: governor has no settings store")
	}

	down, err := g.settings.GetInt64(ctx, downKey, downDef)
	if err != nil {
		return RateLimits{}, fmt.Errorf("engine: load %s: %w", downKey, err)
	}
	up, err := g.settings.GetInt64(ctx, upKey, upDef)
	if err != nil {
		return RateLimits{}, fmt.Errorf("engine: load %s: %w", upKey, err)
	}

	return RateLimits{Down: down, Up: up}, nil
}

// ApplyMode makes m the active schedule-cell mode (T081, FR-091 and
// FR-093). ModeDefault pushes the stored global pair, ModeAlternative
// the stored alternative pair — alternative speed is not an engine
// feature, it is a second global value pushed through the same
// SetRateLimits calls, so it reaches HTTP, FTP, SFTP, BitTorrent and
// media-site tasks alike — and both then resume the parked set.
// ModeNoDownload pauses every task dl-tool started and parks it; the
// limits are not changed and no engine ever receives a near-zero rate.
// An unchanged mode is a no-op, so the minute tick is idempotent; a
// mode is recorded only when the whole apply succeeded.
func (g *Governor) ApplyMode(ctx context.Context, m Mode) error {
	g.applyMu.Lock()
	defer g.applyMu.Unlock()

	g.mu.Lock()
	applied := g.mode
	g.mu.Unlock()
	if applied == m {
		return nil
	}

	var err error
	switch m {
	case ModeNoDownload:
		err = g.parkAll(ctx)
	case ModeDefault, ModeAlternative:
		err = g.applyCellLimits(ctx, m)
	default:
		return fmt.Errorf("engine: unknown schedule mode %q", m)
	}
	if err != nil {
		return err
	}

	g.mu.Lock()
	g.mode = m
	g.mu.Unlock()

	return nil
}

// applyCellLimits fans out the limit pair a Default or Alternative cell
// names and then releases the parked set — the limits land first so a
// resumed task never runs one engine call under the stale pair.
func (g *Governor) applyCellLimits(ctx context.Context, m Mode) error {
	var l RateLimits
	var err error
	if m == ModeAlternative {
		l, err = g.loadLimits(
			ctx, settingAltDownloadRateLimit, settingAltUploadRateLimit,
			defaultAltDownloadRateLimit, defaultAltUploadRateLimit,
		)
	} else {
		l, err = g.loadLimits(ctx, settingDownloadRateLimit, settingUploadRateLimit, 0, 0)
	}
	if err != nil {
		return err
	}

	if err := g.applyGlobal(ctx, l); err != nil {
		return err
	}

	return g.resumeParked(ctx)
}

// pausableTask is one parked-set candidate: the task id's engine name
// and handle — nil when the admission pass has not handed the task to
// an engine yet — and whether the engine-side transfer may be running.
// live is true for the running states and for a queued row the
// admission pass still owns mid-release; a plain queued task's
// engine-side transfer is stopped by construction (the handle alone
// does not spend a slot), so its park is the row move alone — calling
// Pause on a stopped handle would only earn an adapter fault.
type pausableTask struct {
	engine string
	ref    *string
	live   bool
}

// pausableTasks enumerates the parked set's input: every task in
// downloading, checking or queued that dl-tool started. The running
// states come from the per-engine non-terminal listing — the same
// scan the reconciler's sweep uses — and the queued set from the
// admission pass's candidate listing, so a queued task holding no
// engine handle yet is parked too instead of downloading through the
// cell. Seeding tasks, the post-download states and tasks dl-tool did
// not create never appear: the tables only ever hold dl-tool's rows.
func (g *Governor) pausableTasks(ctx context.Context) (map[string]pausableTask, error) {
	pausable := make(map[string]pausableTask)

	queued, err := g.tasks.SelectQueuedCandidates(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf("engine: list queued tasks for the schedule park: %w", err)
	}
	for _, cand := range queued {
		// The candidate listing also carries paused tasks the disk-space
		// guard parked; only the queued belong to the schedule's set.
		if cand.State != "queued" {
			continue
		}
		pausable[cand.ID] = pausableTask{
			engine: cand.Engine, ref: cand.EngineRef, live: cand.AdmissionPending != 0,
		}
	}

	for _, name := range g.reg.Names() {
		byRef, err := g.tasks.ListNonTerminalByEngine(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("engine: list non-terminal tasks of %q for the schedule park: %w", name, err)
		}
		for _, row := range byRef {
			if row.State != "downloading" && row.State != "checking" {
				continue
			}
			// The listing is keyed on the engine handle; the parked set
			// records task ids.
			ref := row.EngineRef
			pausable[row.ID] = pausableTask{engine: name, ref: &ref, live: true}
		}
	}

	return pausable, nil
}

// parkAll runs the No Download cell: an engine-side pause for every
// candidate whose transfer may be live, then the ScheduleParked row
// move that joins it to the parked set. Engine first, so an engine
// failure leaves the state untouched — a task whose engine refuses is
// not parked and is retried on the next tick. A candidate that vanished
// or moved on between the scan and the park is skipped: its outcome —
// gone or already out of the pausable states — is the one the cell
// wanted. A nil TaskStore fails the cell closed before any engine is
// touched: parking without the store would strand ids nothing can
// resume.
func (g *Governor) parkAll(ctx context.Context) error {
	if g.tasks == nil {
		return errors.New("engine: governor has no task store; refusing a no-download cell")
	}

	pausable, err := g.pausableTasks(ctx)
	if err != nil {
		return err
	}

	var errs []error
	for id, task := range pausable {
		if task.ref != nil && task.live {
			e, ok := g.reg.Get(task.engine)
			if !ok {
				errs = append(errs, fmt.Errorf("task %s: %w: %s is not registered", id, ErrUnavailable, task.engine))
				continue
			}
			// The handle is the engine-namespaced form — "aria2:<gid>",
			// the TaskInfo.ID shape; the adapter strips its own
			// namespace. A handle the engine already forgot is gone, not
			// a pause failure: park the row and let the reconciler sort
			// out the rest.
			if err := e.Pause(ctx, namespacedHandle(task.engine, *task.ref)); err != nil && !errors.Is(err, ErrNotFound) {
				errs = append(errs, fmt.Errorf("task %s: %w", id, err))
				continue
			}
		}

		if err := g.tasks.ScheduleParked(ctx, id); err != nil &&
			!errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrIllegalTransition) &&
			!errors.Is(err, store.ErrTransitionConflict) {
			errs = append(errs, fmt.Errorf("task %s: %w", id, err))
		}
	}

	return errors.Join(errs...)
}

// resumeParked releases the parked set on a change away from
// ModeNoDownload: every id ListScheduleParked returns is requeued —
// the admission pass owns Engine.Resume for a queued task, the same
// split the operator resume keeps — and the transition appends
// task.schedule.resumed, taking the row out of the set. A parked row
// that moved on between the listing and the transition is skipped: it
// left the set by its own event and needs no release. With no
// TaskStore attached nothing was ever parked, so there is nothing to
// resume.
func (g *Governor) resumeParked(ctx context.Context) error {
	if g.tasks == nil {
		return nil
	}

	ids, err := g.tasks.ListScheduleParked(ctx)
	if err != nil {
		return fmt.Errorf("engine: list schedule-parked tasks: %w", err)
	}

	var errs []error
	for _, id := range ids {
		err := g.tasks.Transition(ctx, id, "queued", store.CodeTaskScheduleResumed, "resumed by the download schedule")
		switch {
		case err == nil:
		case errors.Is(err, store.ErrNotFound),
			errors.Is(err, store.ErrIllegalTransition),
			errors.Is(err, store.ErrTransitionConflict):
		default:
			errs = append(errs, fmt.Errorf("task %s: %w", id, err))
		}
	}

	return errors.Join(errs...)
}

// ApplyTask pushes a per-task limit to the engine that owns the task, in
// bytes per second (T082, FR-094). engineTaskID is the engine-namespaced
// id — "aria2:2089b05ecca3d829" — the TaskInfo.ID shape: the prefix
// selects the engine and the bare ref is what SetRateLimits receives. A
// nil direction is left unchanged at the engine; 0 means unlimited. The
// call must not restart, re-add or re-check the transfer — aria2's
// changeOption carries both max-*-limit keys on the safe list and
// qBittorrent's torrents/set*Limit endpoints touch nothing else
// (docs/06-download-engines.md section 10.1) — so it invokes no method
// but SetRateLimits. An adapter without the capability answers
// ErrNotSupported — for yt-dlp the recorded value applies at the next
// spawn — and an unregistered or unreachable engine answers
// ErrUnavailable; the caller keeps the stored value either way, so a
// later boot reconciliation re-pushes it. ApplyTask touches no governor
// state — Current and Mode are the global pair's bookkeeping — and so
// takes no lock: any governor over the same registry applies identically.
func (g *Governor) ApplyTask(ctx context.Context, engineTaskID string, down, up *int64) error {
	if down == nil && up == nil {
		return nil
	}

	name, ref, ok := strings.Cut(engineTaskID, ":")
	if !ok || name == "" || ref == "" {
		return fmt.Errorf("engine: %q is not an engine-namespaced task id", engineTaskID)
	}

	e, ok := g.reg.Get(name)
	if !ok {
		return fmt.Errorf("%w: %s is not registered", ErrUnavailable, name)
	}

	if err := e.SetRateLimits(ctx, ref, down, up); err != nil {
		return err
	}

	g.readBackTask(ctx, e, name, ref, down, up)
	return nil
}

// taskLimitReader is the optional per-task read-back surface: the
// daemon's configured limits for one transfer in bytes per second,
// queried — never echoed from the set request. No adapter exports it
// yet — qBittorrent confirms a per-task set against its maindata cache
// inside SetRateLimits, and aria2's changeOption is synchronous — so the
// read-back below records the same debug line the global one leaves for
// an engine without the surface.
type taskLimitReader interface {
	TaskLimits(ctx context.Context, id string) (down, up int64, err error)
}

// readBackTask confirms one transfer's limit pair landed where the
// adapter exposes a read-back surface. A mismatch is a warn carrying
// both numbers, never an error — the set already succeeded — and a nil
// direction was not sent, so it is not compared.
func (g *Governor) readBackTask(ctx context.Context, e Engine, name, ref string, down, up *int64) {
	reader, ok := e.(taskLimitReader)
	if !ok {
		slog.DebugContext(ctx, "engine: no per-task rate-limit read-back surface",
			"engine", name)
		return
	}

	gotDown, gotUp, err := reader.TaskLimits(ctx, ref)
	if err != nil {
		slog.WarnContext(ctx, "engine: per-task rate limits could not be read back",
			"engine", name, "task", ref, "err", err)
		return
	}
	if down != nil && gotDown != *down {
		slog.WarnContext(ctx, "engine: per-task download limit read back different from what was sent",
			"engine", name, "task", ref,
			"sent_bps", *down, "read_bps", gotDown)
	}
	if up != nil && gotUp != *up {
		slog.WarnContext(ctx, "engine: per-task upload limit read back different from what was sent",
			"engine", name, "task", ref,
			"sent_bps", *up, "read_bps", gotUp)
	}
}
