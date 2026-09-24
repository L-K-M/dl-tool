// The engine.Engine implementation of the media lane
// (docs/06-download-engines.md section 7): one supervised yt-dlp
// subprocess per task, owned end to end. The tasks map is the whole
// ownership model — a transfer exists only while this process minted
// its id, so a foreign yt-dlp process can never surface, which is how
// the exclusive-control rule of section 8 holds for an engine with no
// daemon to query.
package ytdlp

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/L-K-M/dl-tool/internal/engine"
)

// eventsBuffer caps one Events subscriber's queue; a full buffer drops
// the event rather than stall the process watcher, the same fan-out
// policy the qBittorrent adapter documents.
const eventsBuffer = 64

// Engine is the media lane. It declares exactly three capabilities —
// media_site, rename and push_events — because yt-dlp has no per-file
// model, no categories, no tags, no sequential mode and no share limits.
type Engine struct {
	runner *Runner
	log    *slog.Logger

	mu      sync.Mutex
	tasks   map[string]*taskRecord // keyed by the engine-namespaced id
	subs    map[chan engine.TaskEvent]struct{}
	closed  bool
	limitBS int64 // global bytes/second applied to the next spawn; 0 means unlimited
}

// taskRecord is everything the engine keeps about one minted task: the
// submission a respawn re-issues, the live process while it runs and
// the latest normalised info Get and List report.
type taskRecord struct {
	req      engine.AddRequest
	info     engine.TaskInfo
	proc     *Proc // nil while paused, finished or failed
	limitBS  int64 // per-task bytes/second for the next spawn; 0 means unset
	removing bool  // Remove claimed the record; a respawn must not slip between its cancel and its delete
}

var _ engine.Engine = (*Engine)(nil)

// NewEngine builds the adapter over the Runner of T087. cfg carries the
// resolved environment values: DLTOOL_YTDLP_PATH, DLTOOL_JS_RUNTIME_PATH
// and the archive directory.
func NewEngine(cfg Config, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		runner: New(cfg, log),
		log:    log,
		tasks:  make(map[string]*taskRecord),
		subs:   make(map[chan engine.TaskEvent]struct{}),
	}
}

// Name is the engine key every id is namespaced by.
func (e *Engine) Name() string { return engine.NameYtDlp }

// Capabilities reports the declared set, sorted and stable.
func (e *Engine) Capabilities() []engine.Capability {
	return []engine.Capability{engine.CapMediaSite, engine.CapPushEvents, engine.CapRename}
}

// Accepts reports whether a yt-dlp extractor claims the URI. The cache
// that would answer it is the deferred T088 — the mechanism the plan
// prescribed does not exist (docs/06-download-engines.md section 7.2) —
// so until its ADR lands nothing matches here and Route's nil
// mediaMatch sends a media URL to aria2.
func (e *Engine) Accepts(string) bool { return false }

// Connect readies the lane. The extractor cache it was meant to load is
// the deferred T088, so there is nothing to load; a missing binary is
// Health's report, never a Connect failure.
func (e *Engine) Connect(context.Context) error { return nil }

// Close kills every live process and closes every subscriber channel.
// A process already reaped by its watcher answers ErrNotFound, which is
// not a close failure.
func (e *Engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	for ch := range e.subs {
		close(ch)
		delete(e.subs, ch)
	}
	live := make([]string, 0, len(e.tasks))
	for id, rec := range e.tasks {
		if rec.proc != nil {
			live = append(live, id)
		}
	}
	e.mu.Unlock()

	var errs []error
	for _, id := range live {
		if err := e.runner.Cancel(id); err != nil && !errors.Is(err, engine.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Health runs "<binary> --version" and reports the version string, or
// ErrUnavailable when the binary is absent or refuses — the missing-
// binary signal Connect never raises.
func (e *Engine) Health(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, e.runner.cfg.BinaryPath, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("ytdlp: %q --version: %w: %w", e.runner.cfg.BinaryPath, engine.ErrUnavailable, err)
	}
	version, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(version), nil
}

// Add mints an engine-namespaced id and records the task. StartPaused
// records it paused with no process; otherwise the spawn runs now and
// the watcher goroutine owns its progress stream. A spawn failure drops
// the record — a refused add leaves no handle behind.
func (e *Engine) Add(ctx context.Context, req engine.AddRequest) (string, error) {
	if len(req.Blob) > 0 {
		return "", engine.ErrNotSupported
	}
	if len(req.URIs) != 1 {
		return "", fmt.Errorf("ytdlp: add takes exactly one URI, got %d", len(req.URIs))
	}

	id := engineIDPrefix + ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()
	rec := &taskRecord{req: req, info: engine.TaskInfo{
		ID:      id,
		Engine:  engine.NameYtDlp,
		Name:    req.URIs[0],
		State:   engine.StatePaused,
		SaveDir: req.SaveDir,
	}}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return "", errors.New("ytdlp: engine closed")
	}
	e.tasks[id] = rec
	var proc *Proc
	if !req.StartPaused {
		var err error
		if proc, err = e.spawnLocked(ctx, id, rec); err != nil {
			delete(e.tasks, id)
			e.mu.Unlock()
			return "", err
		}
	}
	snap := rec.info
	e.mu.Unlock()

	if proc != nil {
		go e.watch(id, proc)
	}
	e.emit(engine.TaskEvent{TaskID: id, Kind: engine.EventAdded, Info: &snap})
	return id, nil
}

// List reports every minted task, in id order so consecutive calls are
// byte-stable. e.tasks is the only source: a yt-dlp process this engine
// did not start has no record and is never reported (section 8).
func (e *Engine) List(context.Context) ([]engine.TaskInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	infos := make([]engine.TaskInfo, 0, len(e.tasks))
	for _, rec := range e.tasks {
		infos = append(infos, rec.info)
	}
	slices.SortFunc(infos, func(a, b engine.TaskInfo) int { return strings.Compare(a.ID, b.ID) })
	return infos, nil
}

// Get reports one minted task, or ErrNotFound for an id the adapter did
// not mint — including a live foreign process.
func (e *Engine) Get(_ context.Context, id string) (engine.TaskInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	rec, ok := e.tasks[id]
	if !ok {
		return engine.TaskInfo{}, fmt.Errorf("ytdlp: %s: %w", id, engine.ErrNotFound)
	}
	return rec.info, nil
}

// Files reports the single output the lane produces: yt-dlp has no
// per-file model, so every task is one entry — Index 0, always
// selected, Priority nil — shaped from the recorded info.
func (e *Engine) Files(_ context.Context, id string) ([]engine.FileEntry, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	rec, ok := e.tasks[id]
	if !ok {
		return nil, fmt.Errorf("ytdlp: %s: %w", id, engine.ErrNotFound)
	}

	entry := engine.FileEntry{Index: 0, Selected: true, Completed: rec.info.CompletedBytes}
	if rec.info.TotalBytes != nil {
		entry.Size = *rec.info.TotalBytes
	}
	if rec.info.ContentPath != "" {
		if rel, err := filepath.Rel(rec.info.SaveDir, rec.info.ContentPath); err == nil && !strings.HasPrefix(rel, "..") {
			entry.Path = rel
		}
	}
	return []engine.FileEntry{entry}, nil
}

// Pause kills the process through Runner.Cancel and marks the task
// paused; the partial file plus the download archive are what makes the
// respawn a resume (section 10.1). Pausing a paused task is a no-op;
// pausing a terminal one is an error, not a state rewrite.
func (e *Engine) Pause(_ context.Context, id string) error {
	e.mu.Lock()
	rec, ok := e.tasks[id]
	if !ok || rec.removing {
		e.mu.Unlock()
		return fmt.Errorf("ytdlp: %s: %w", id, engine.ErrNotFound)
	}
	proc := rec.proc
	state := rec.info.State
	e.mu.Unlock()

	if proc == nil {
		if state == engine.StatePaused {
			return nil
		}
		return fmt.Errorf("ytdlp pause %s: task is %s", id, state)
	}

	if err := e.runner.Cancel(id); err != nil && !errors.Is(err, engine.ErrNotFound) {
		return fmt.Errorf("ytdlp pause %s: %w", id, err)
	}
	e.finishProc(id, proc, Outcome{State: engine.StatePaused})
	return nil
}

// Resume respawns a paused task with the stored AddRequest. A running
// task is a no-op; a respawn clears the terminal fields the last exit
// left so a retry after error does not present stale codes.
func (e *Engine) Resume(ctx context.Context, id string) error {
	e.mu.Lock()
	rec, ok := e.tasks[id]
	if !ok || rec.removing {
		e.mu.Unlock()
		return fmt.Errorf("ytdlp: %s: %w", id, engine.ErrNotFound)
	}
	if rec.proc != nil {
		e.mu.Unlock()
		return nil
	}
	rec.info.ErrorCode, rec.info.ErrorMessage = "", ""
	rec.info.DownloadRate, rec.info.ETASeconds = 0, nil
	rec.info.CompletedAt = nil
	proc, err := e.spawnLocked(ctx, id, rec)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	snap := rec.info
	e.mu.Unlock()

	go e.watch(id, proc)
	e.emit(engine.TaskEvent{TaskID: id, Kind: engine.EventStarted, Info: &snap})
	return nil
}

// Remove kills the process if it is live, drops the record and deletes
// the info document — dl-tool's own bookkeeping, not payload data,
// which Remove always retains. The record stays when the cancel fails,
// so the caller can retry against a still-owned transfer.
func (e *Engine) Remove(_ context.Context, id string) error {
	e.mu.Lock()
	rec, ok := e.tasks[id]
	if !ok || rec.removing {
		e.mu.Unlock()
		return fmt.Errorf("ytdlp: %s: %w", id, engine.ErrNotFound)
	}
	rec.removing = true
	proc := rec.proc
	e.mu.Unlock()

	if proc != nil {
		if err := e.runner.Cancel(id); err != nil && !errors.Is(err, engine.ErrNotFound) {
			e.mu.Lock()
			if cur, live := e.tasks[id]; live {
				cur.removing = false
			}
			e.mu.Unlock()
			return fmt.Errorf("ytdlp remove %s: %w", id, err)
		}
	}

	e.mu.Lock()
	delete(e.tasks, id)
	e.mu.Unlock()

	// Spawn always writes the document at <SaveDir>/.dl-tool-info.json.
	infoPath := filepath.Join(rec.info.SaveDir, InfoJSONName)
	if err := os.Remove(infoPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		e.log.Warn("ytdlp info document not removed", slog.String("task_id", id), slog.String("error", err.Error()))
	}
	e.emit(engine.TaskEvent{TaskID: id, Kind: engine.EventRemoved})
	return nil
}

// SetFiles is unsupported: yt-dlp has no per-file model.
func (e *Engine) SetFiles(context.Context, string, []int, map[int]int) error {
	return engine.ErrNotSupported
}

// SetLocation is unsupported: a spawn binds its output to SaveDir.
func (e *Engine) SetLocation(context.Context, string, string) error {
	return engine.ErrNotSupported
}

// SetCategory is unsupported: yt-dlp has no categories.
func (e *Engine) SetCategory(context.Context, string, string) error {
	return engine.ErrNotSupported
}

// SetShareLimits is unsupported: yt-dlp has no share limits.
func (e *Engine) SetShareLimits(context.Context, string, *float64, *int64) error {
	return engine.ErrNotSupported
}

// Rename stores the -o template for the next spawn — the only point a
// media task's output name can change, since a running process's argv
// is fixed (docs/06-download-engines.md section 1, rename row).
func (e *Engine) Rename(_ context.Context, id, name string) error {
	if err := rejectEscapingOutputTemplate(name); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	rec, ok := e.tasks[id]
	if !ok {
		return fmt.Errorf("ytdlp: %s: %w", id, engine.ErrNotFound)
	}
	rec.req.Filename = name
	return nil
}

// SetRateLimits stores the download value for the next spawn. A running
// process is never re-limited, per docs/06-download-engines.md
// section 10.1; the upload direction is always ignored because yt-dlp
// uploads nothing. An empty id stores the global limit the next spawn
// of any task combines with its own.
func (e *Engine) SetRateLimits(_ context.Context, id string, down, _ *int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if id == "" {
		if down != nil {
			e.limitBS = *down
		}
		return nil
	}
	rec, ok := e.tasks[id]
	if !ok {
		return fmt.Errorf("ytdlp: %s: %w", id, engine.ErrNotFound)
	}
	if down != nil {
		rec.limitBS = *down
	}
	return nil
}

// Events subscribes to the lane's pushed feed — one event per accepted
// progress line, plus lifecycle transitions — on a buffered channel
// that closes when ctx is cancelled or the engine is closed. A full
// buffer drops rather than stalls the watcher; events are hints and
// List is the authoritative snapshot.
func (e *Engine) Events(ctx context.Context) (<-chan engine.TaskEvent, error) {
	ch := make(chan engine.TaskEvent, eventsBuffer)

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		close(ch)
		return ch, nil
	}
	e.subs[ch] = struct{}{}
	e.mu.Unlock()

	go func() {
		<-ctx.Done()
		e.mu.Lock()
		if _, ok := e.subs[ch]; ok {
			delete(e.subs, ch)
			close(ch)
		}
		e.mu.Unlock()
	}()
	return ch, nil
}

// spawnLocked starts the process for a record with no live proc; the
// caller holds e.mu. The spawn runs under context.WithoutCancel: the
// context becomes the process's lifetime through exec.CommandContext,
// so the caller's request or sweep deadline must not kill it — only
// Pause, Remove and Close cancel a spawn.
func (e *Engine) spawnLocked(ctx context.Context, id string, rec *taskRecord) (*Proc, error) {
	if e.closed {
		return nil, errors.New("ytdlp: engine closed")
	}
	proc, err := e.runner.Spawn(context.WithoutCancel(ctx), id, rec.req, e.effectiveLimitLocked(rec))
	if err != nil {
		return nil, err
	}
	rec.proc = proc
	rec.info.State = engine.StateDownloading
	return proc, nil
}

// effectiveLimitLocked combines the per-task and global stores into the
// single value the next spawn carries: the smallest non-zero of the
// two, 0 meaning unlimited. Caller holds e.mu.
func (e *Engine) effectiveLimitLocked(rec *taskRecord) int64 {
	limit := e.limitBS
	if rec.limitBS > 0 && (limit == 0 || rec.limitBS < limit) {
		limit = rec.limitBS
	}
	return limit
}

// pendingDownloadLimit is the suite's DownloadLimitReadback backing: a
// subprocess has no daemon to query, so it reports the exact value the
// next spawn will carry — the global store for id "", the per-task
// effective minimum otherwise — which the confirmed rate-limit flag
// (T113) will translate into argv.
func (e *Engine) pendingDownloadLimit(id string) (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if id == "" {
		return e.limitBS, nil
	}
	rec, ok := e.tasks[id]
	if !ok {
		return 0, fmt.Errorf("ytdlp: %s: %w", id, engine.ErrNotFound)
	}
	return e.effectiveLimitLocked(rec), nil
}

// watch drains one process's progress stream into the record and the
// subscriber feed, then settles the exit through ClassifyExit. It is
// the only writer of progress deltas: Pause, Remove and Close act on
// the process, never on the stream.
func (e *Engine) watch(id string, proc *Proc) {
	for ev := range ScanProgress(id, proc.Stdout) {
		e.mu.Lock()
		rec, ok := e.tasks[id]
		if !ok || rec.proc != proc || ev.Info == nil {
			e.mu.Unlock()
			continue
		}
		mergeProgress(&rec.info, ev.Info)
		snap := rec.info
		e.mu.Unlock()
		e.emit(engine.TaskEvent{TaskID: id, Kind: ev.Kind, Info: &snap})
	}
	if err := proc.Stdout.Close(); err != nil {
		e.log.Warn("ytdlp stdout close failed", slog.String("task_id", id), slog.String("error", err.Error()))
	}

	code, err := e.runner.Wait(id)
	if errors.Is(err, engine.ErrNotFound) {
		// Cancel reaped it first: Pause, Remove or Close already owns the
		// transition and its event, so the watcher retires quietly.
		return
	}
	outcome := ClassifyExit(code, proc.stderr.String())
	if outcome.State == engine.StateError {
		e.log.Error("ytdlp process failed",
			slog.String("task_id", id),
			slog.Int("exit_code", code),
			slog.String("error_code", outcome.ErrorCode),
			slog.String("stderr", outcome.ErrorMessage))
	}
	e.finishProc(id, proc, outcome)
}

// finishProc applies the terminal outcome to the record exactly once:
// the caller that still finds rec.proc == proc owns the transition and
// emits its event, so a kill racing a natural exit cannot double-report.
// A completed run folds the --print-to-file document in before the
// event, so subscribers see the final name, size and timestamp.
func (e *Engine) finishProc(id string, proc *Proc, outcome Outcome) {
	e.mu.Lock()
	rec, ok := e.tasks[id]
	if !ok || rec.proc != proc {
		e.mu.Unlock()
		return
	}
	rec.proc = nil
	applyOutcome(&rec.info, outcome)
	if outcome.State == engine.StateCompleted {
		if doc, err := readInfoDocument(proc.InfoPath); err == nil {
			doc.Apply(&rec.info)
		} else {
			e.log.Warn("ytdlp info document unreadable",
				slog.String("task_id", id), slog.String("error", err.Error()))
		}
	}
	snap := rec.info
	e.mu.Unlock()

	e.emit(engine.TaskEvent{TaskID: id, Kind: eventKindFor(snap.State), Info: &snap})
}

// mergeProgress folds one progress-line delta into the stored info: the
// line owns state, counters, rate, ETA and the final filename, while
// fields it cannot know — name, SaveDir, timestamps — stay untouched.
// A scanner-failure EventError carries its code and message instead of
// a status.
func mergeProgress(dst *engine.TaskInfo, src *engine.TaskInfo) {
	dst.State = src.State
	dst.TotalBytes = src.TotalBytes
	dst.CompletedBytes = src.CompletedBytes
	dst.DownloadRate = src.DownloadRate
	dst.ETASeconds = src.ETASeconds
	if src.ContentPath != "" {
		dst.ContentPath = src.ContentPath
	}
	if src.ErrorCode != "" {
		dst.ErrorCode, dst.ErrorMessage = src.ErrorCode, src.ErrorMessage
	}
}

// applyOutcome lands a ClassifyExit verdict on the stored info: the
// error fields only on failure, and a frozen transfer's rate and ETA
// cleared on every terminal or paused stop — a paused task must not
// keep presenting the rate it died at (parse.go's Outcome contract).
func applyOutcome(info *engine.TaskInfo, outcome Outcome) {
	info.State = outcome.State
	info.ErrorCode = outcome.ErrorCode
	info.ErrorMessage = outcome.ErrorMessage
	info.DownloadRate = 0
	info.ETASeconds = nil
	if outcome.State == engine.StateCompleted {
		now := time.Now().UTC()
		info.CompletedAt = &now
	}
}

// readInfoDocument loads and decodes the --print-to-file JSON a
// completed process left at <SaveDir>/.dl-tool-info.json.
func readInfoDocument(path string) (Info, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Info{}, err
	}
	return ParseInfoDocument(raw)
}

// eventKindFor projects a settled state onto the event vocabulary:
// paused, completed and error name their own kinds, everything else is
// progress of one living transfer.
func eventKindFor(state engine.TaskState) engine.EventKind {
	switch state {
	case engine.StatePaused:
		return engine.EventPaused
	case engine.StateCompleted:
		return engine.EventCompleted
	case engine.StateError:
		return engine.EventError
	default:
		return engine.EventProgress
	}
}

// emit fans one event out to every subscriber without blocking: a slow
// consumer loses deltas, never the watcher. The send runs under e.mu so
// it cannot race a subscriber's close.
func (e *Engine) emit(ev engine.TaskEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for ch := range e.subs {
		select {
		case ch <- ev:
		default:
			e.log.Warn("ytdlp event dropped: subscriber buffer full",
				slog.String("task_id", ev.TaskID), slog.String("kind", string(ev.Kind)))
		}
	}
}
