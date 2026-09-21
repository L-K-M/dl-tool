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
	"sync"

	"github.com/L-K-M/dl-tool/internal/store"
)

// The two settings keys of docs/11-config-reference.md section 5 the
// governor owns. Both are integer bytes per second; an absent row is the
// documented default of 0, unlimited.
const (
	settingDownloadRateLimit = "download_rate_limit"
	settingUploadRateLimit   = "upload_rate_limit"
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

	mu      sync.Mutex
	current RateLimits
}

// NewGovernor returns the governor over the shared registry and the
// settings rows the stored limits live in.
func NewGovernor(reg *Registry, st *store.SettingsStore) *Governor {
	return &Governor{reg: reg, settings: st}
}

// Current returns the limits last applied — recorded only when every
// registered engine accepted the fan-out, so a failed apply is never
// mistaken for a landed one and a caller comparing against Current still
// retries it.
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
// errors.Join, so one unreachable daemon never blocks the others. The mutex
// serialises whole applies — two concurrent fan-outs cannot interleave —
// while the engines inside one apply run in parallel, so an engine that
// hangs until the context ends cannot starve the ones behind it of the
// shared deadline.
func (g *Governor) ApplyGlobal(ctx context.Context, l RateLimits) error {
	g.mu.Lock()
	defer g.mu.Unlock()

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
		g.current = l
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
	if g.settings == nil {
		return errors.New("engine: governor has no settings store")
	}

	down, err := g.settings.GetInt64(ctx, settingDownloadRateLimit, 0)
	if err != nil {
		return fmt.Errorf("engine: load %s: %w", settingDownloadRateLimit, err)
	}
	up, err := g.settings.GetInt64(ctx, settingUploadRateLimit, 0)
	if err != nil {
		return fmt.Errorf("engine: load %s: %w", settingUploadRateLimit, err)
	}

	return g.ApplyGlobal(ctx, RateLimits{Down: down, Up: up})
}
