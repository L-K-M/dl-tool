// Package ytdlp runs yt-dlp as a supervised subprocess: one OS process per
// task, spawned by exec.CommandContext with a Go-built argument slice and
// killed on pause or remove (docs/06-download-engines.md §7). Per ADR-0018 the
// binary is pinned by version and hash in the image and this package never
// invokes -U or --update-to: freshness comes from the weekly image rebuild,
// not a runtime updater.
package ytdlp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/L-K-M/dl-tool/internal/engine"
)

// DefaultOutputTemplate is the -o value used when AddRequest.Filename is empty.
const DefaultOutputTemplate = "%(title)s [%(id)s].%(ext)s"

// InfoJSONName is the per-task file --print-to-file writes the final info document to.
const InfoJSONName = ".dl-tool-info.json"

// progressTemplate is the verbatim --progress-template value of
// docs/06-download-engines.md §7.3. Every optional numeric carries a `|null`
// default and the `j` conversion because yt-dlp renders an absent numeric as
// the bare literal `NA`, which is not JSON; T089 parses what this emits, so it
// must not drift from the documented string.
const progressTemplate = `download:{"status":"%(progress.status)s","downloaded":%(progress.downloaded_bytes|0)d,"total":%(progress.total_bytes|null)j,"est":%(progress.total_bytes_estimate|null)j,"speed":%(progress.speed|null)j,"eta":%(progress.eta|null)j,"frag":%(progress.fragment_index|null)j,"frags":%(progress.fragment_count|null)j,"file":"%(progress.filename)s"}`

const (
	// stderrBound caps the captured stderr of one process; diagnostics only
	// need the tail, and an unbounded buffer is a memory leak driven by the
	// child.
	stderrBound = 64 * 1024
	// cancelGrace is how long Cancel waits for the killed process to exit
	// before it stops waiting and removes the registry entry anyway.
	cancelGrace = 10 * time.Second
	// engineIDPrefix namespaces live ids, e.g. "ytdlp:01JB0Q7M8W…".
	engineIDPrefix = "ytdlp:"
)

// Config is the resolved runtime configuration of the media lane.
type Config struct {
	BinaryPath    string        // DLTOOL_YTDLP_PATH, default /usr/local/bin/yt-dlp
	JSRuntimePath string        // DLTOOL_JS_RUNTIME_PATH, default /usr/bin/node
	ArchiveDir    string        // <DLTOOL_CONFIG_DIR>/archives
	SpawnTimeout  time.Duration // hard ceiling on one process; 0 means no ceiling
}

// Argv builds the argument vector for one submission. It never returns a shell string.
// The URI is always the final element and is never interpolated into any other argument.
// rateLimitBytesPerSecond of 0 means unlimited and adds no flag; the flag itself is
// unconfirmed upstream and T113 owns it, so a non-zero value is accepted but not yet
// emitted.
func Argv(cfg Config, req engine.AddRequest, archivePath, infoJSONPath string, rateLimitBytesPerSecond int64) []string {
	output := req.Filename
	if output == "" {
		output = DefaultOutputTemplate
	}
	args := []string{
		"--no-colors", "--newline", "--no-playlist",
		"--paths", req.SaveDir,
		"--output", output,
		"--download-archive", archivePath,
		"--progress-template", progressTemplate,
		"--print-to-file", "%()j", infoJSONPath,
		"--no-simulate",
	}
	if len(req.URIs) > 0 {
		// "--" ends option parsing: a submitted URI that begins with "-" must
		// be treated as a URL, never consumed as a yt-dlp flag.
		args = append(args, "--", req.URIs[0])
	}
	return args
}

// boundedBuffer is an io.Writer that keeps at most the last limit bytes and
// reports a full write either way, so a chatty child never blocks on stderr.
type boundedBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if overflow := len(b.buf) - b.limit; overflow > 0 {
		b.buf = slices.Delete(b.buf, 0, overflow)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// Proc is one live yt-dlp process.
type Proc struct {
	ID        string // engine-namespaced, e.g. "ytdlp:01JB0Q7M8WQ0F1R2S3T4V5W6X7"
	Cmd       *exec.Cmd
	SaveDir   string
	InfoPath  string
	StartedAt time.Time
	// Stdout is the child's stdout, a line reader for T089. It yields a clean
	// EOF once the process exits; the consumer owns closing it and must drain
	// it promptly or the child blocks on a full pipe.
	Stdout *os.File

	cancel   context.CancelFunc
	done     chan struct{} // closed by the reaper goroutine once Cmd.Wait returns
	exitCode int
	waitErr  error
	stderr   *boundedBuffer
}

// Runner owns every live process. It is safe for concurrent use.
type Runner struct {
	cfg  Config
	log  *slog.Logger
	mu   sync.Mutex
	live map[string]*Proc
}

// New creates the runner and the download-archive directory. ArchiveDir is
// made with mode 0777 so the process umask decides the result, per
// docs/10-deployment-and-compose.md §4.
func New(cfg Config, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(cfg.ArchiveDir, 0o777); err != nil {
		log.Error("ytdlp archive directory not creatable", slog.String("error", err.Error()))
	}
	return &Runner{cfg: cfg, log: log, live: make(map[string]*Proc)}
}

// childEnv is the entire environment a spawned process sees: PATH, HOME and TZ
// forwarded from ours, nothing else. No inherited variable — engine secrets
// included — reaches the child.
func childEnv() []string {
	env := make([]string, 0, 3)
	for _, key := range []string{"PATH", "HOME", "TZ"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// Spawn starts one process for req under the caller-supplied engine-namespaced id and returns
// immediately. Stdout is returned as a line reader for T089; stderr is captured into a
// bounded buffer.
func (r *Runner) Spawn(ctx context.Context, id string, req engine.AddRequest, rateLimitBytesPerSecond int64) (*Proc, error) {
	if len(req.URIs) == 0 {
		return nil, fmt.Errorf("ytdlp spawn %s: request carries no URI", id)
	}
	saveDir, err := filepath.Abs(req.SaveDir)
	if err != nil {
		return nil, fmt.Errorf("ytdlp spawn %s: resolving save dir: %w", id, err)
	}
	req.SaveDir = saveDir
	if err := rejectEscapingOutputTemplate(req.Filename); err != nil {
		return nil, fmt.Errorf("ytdlp spawn %s: %w", id, err)
	}
	if err := os.MkdirAll(r.cfg.ArchiveDir, 0o777); err != nil {
		return nil, fmt.Errorf("ytdlp spawn %s: archive dir: %w", id, err)
	}
	infoPath := filepath.Join(req.SaveDir, InfoJSONName)
	args := Argv(r.cfg, req, r.ArchivePath(id), infoPath, rateLimitBytesPerSecond)

	var spawnCtx context.Context
	var cancel context.CancelFunc
	if r.cfg.SpawnTimeout > 0 {
		spawnCtx, cancel = context.WithTimeout(ctx, r.cfg.SpawnTimeout)
	} else {
		spawnCtx, cancel = context.WithCancel(ctx)
	}

	cmd := exec.CommandContext(spawnCtx, r.cfg.BinaryPath, args...)
	cmd.Dir = req.SaveDir
	cmd.Env = childEnv()
	// A raw os.Pipe, not StdoutPipe: exec documents that Wait must not run
	// while a StdoutPipe is still being read, and T089 drains this stream
	// across the child's exit. With an *os.File the fd is inherited directly
	// and the reader sees a clean EOF.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("ytdlp spawn %s: stdout pipe: %w", id, err)
	}
	cmd.Stdout = stdoutW
	stderr := &boundedBuffer{limit: stderrBound}
	cmd.Stderr = stderr

	// The registry check and insert share one critical section with Start so
	// two concurrent Spawns of the same id cannot both pass the check.
	r.mu.Lock()
	if _, dup := r.live[id]; dup {
		r.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("ytdlp spawn %s: %w", id, errors.Join(errors.New("id already running"), stdoutR.Close(), stdoutW.Close()))
	}
	if err := cmd.Start(); err != nil {
		r.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("ytdlp spawn %s: %w", id, errors.Join(err, stdoutR.Close(), stdoutW.Close()))
	}
	// The child inherited its own copy of the write end; the parent's copy
	// must close now or the reader never sees EOF.
	if err := stdoutW.Close(); err != nil {
		r.log.Warn("ytdlp stdout write-end close failed", slog.String("task_id", id), slog.String("error", err.Error()))
	}
	p := &Proc{
		ID:        id,
		Cmd:       cmd,
		SaveDir:   req.SaveDir,
		InfoPath:  infoPath,
		StartedAt: time.Now(),
		Stdout:    stdoutR,
		cancel:    cancel,
		done:      make(chan struct{}),
		stderr:    stderr,
	}
	r.live[id] = p
	r.mu.Unlock()

	go func() {
		err := cmd.Wait()
		cancel() // release the spawn context; the process is already gone
		p.waitErr = err
		p.exitCode = -1
		if err == nil {
			p.exitCode = 0
		} else if exitErr := new(exec.ExitError); errors.As(err, &exitErr) {
			p.exitCode = exitErr.ExitCode()
		}
		close(p.done)
	}()

	return p, nil
}

// rejectEscapingOutputTemplate refuses a Filename that would write outside the
// task's SaveDir: an absolute -o overrides --paths entirely, and ".."/"~"
// templates traverse out of it.
func rejectEscapingOutputTemplate(output string) error {
	if output == "" {
		return nil
	}
	if filepath.IsAbs(output) || filepath.VolumeName(output) != "" || strings.HasPrefix(output, "~") {
		return fmt.Errorf("output template escapes save dir: %q", output)
	}
	for _, seg := range strings.FieldsFunc(output, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return fmt.Errorf("output template escapes save dir: %q", output)
		}
	}
	return nil
}

// Cancel kills the process for id via its context. It is idempotent and returns
// engine.ErrNotFound when id is unknown.
func (r *Runner) Cancel(id string) error {
	r.mu.Lock()
	p, ok := r.live[id]
	r.mu.Unlock()
	if !ok {
		return engine.ErrNotFound
	}

	p.cancel()
	select {
	case <-p.done:
	case <-time.After(cancelGrace):
		// Keep the entry registered: the process may still be alive and Live
		// must not under-report it. The caller learns the kill did not take.
		r.log.Error("ytdlp process did not exit within cancel grace", slog.String("task_id", id))
		return fmt.Errorf("ytdlp cancel %s: process did not exit within %s", id, cancelGrace)
	}

	r.mu.Lock()
	delete(r.live, id)
	r.mu.Unlock()
	r.log.Info("ytdlp process cancelled", slog.String("task_id", id))
	return nil
}

// Wait blocks until the process for id exits and returns its exit code. A signalled process
// reports code -1. It returns engine.ErrNotFound when id is unknown.
func (r *Runner) Wait(id string) (exitCode int, err error) {
	r.mu.Lock()
	p, ok := r.live[id]
	r.mu.Unlock()
	if !ok {
		return 0, engine.ErrNotFound
	}

	<-p.done
	r.mu.Lock()
	delete(r.live, id)
	r.mu.Unlock()
	if p.waitErr != nil {
		r.log.Debug("ytdlp process exited non-zero",
			slog.String("task_id", id),
			slog.Int("exit_code", p.exitCode),
			slog.String("stderr", p.stderr.String()))
	}
	return p.exitCode, p.waitErr
}

// Live returns the ids of every running process, sorted.
func (r *Runner) Live() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Sorted(maps.Keys(r.live))
}

// ArchivePath returns <ArchiveDir>/<id>.txt with the engine prefix stripped from id.
// The stem is sanitized to filename characters so a malformed id can never
// traverse out of the archive directory.
func (r *Runner) ArchivePath(id string) string {
	stem := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, strings.TrimPrefix(id, engineIDPrefix))
	return filepath.Join(r.cfg.ArchiveDir, stem+".txt")
}
