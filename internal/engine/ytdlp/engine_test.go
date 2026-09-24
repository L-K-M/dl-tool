// Unit coverage of the adapter's invariants with a stubbed yt-dlp: the
// capability declaration, the ErrNotSupported/ErrNotFound contract,
// ownership of tasks and processes, pause/resume respawn semantics and
// the rate-limit store the next spawn will carry.
package ytdlp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/engine"
)

// newTestEngine builds an adapter over the given stub binary with all
// scratch state under t.TempDir.
func newTestEngine(t *testing.T, binary string) *Engine {
	t.Helper()
	e := NewEngine(testConfig(t, binary), testLogger())
	require.NoError(t, e.Connect(context.Background()))
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	return e
}

func TestCapabilitiesAreExactlyThree(t *testing.T) {
	e := newTestEngine(t, "/nonexistent/yt-dlp")
	assert.Equal(t,
		[]engine.Capability{engine.CapMediaSite, engine.CapPushEvents, engine.CapRename},
		e.Capabilities())
}

// Every optional surface the plan does not declare for yt-dlp refuses
// with ErrNotSupported and leaves the recorded task untouched — the
// declared-capability gate in the task files is what makes "without
// changing task state" verifiable.
func TestUnsupportedMethodsReturnErrNotSupported(t *testing.T) {
	e := newTestEngine(t, "/nonexistent/yt-dlp")
	id := addPausedTask(t, e)
	before, err := e.Get(context.Background(), id)
	require.NoError(t, err)

	for name, call := range map[string]func() error{
		"SetFiles":    func() error { return e.SetFiles(context.Background(), id, []int{0}, nil) },
		"SetLocation": func() error { return e.SetLocation(context.Background(), id, "/elsewhere") },
		"SetCategory": func() error { return e.SetCategory(context.Background(), id, "c") },
		"SetShareLimits": func() error {
			ratio, mins := 1.0, int64(60)
			return e.SetShareLimits(context.Background(), id, &ratio, &mins)
		},
	} {
		assert.ErrorIs(t, call(), engine.ErrNotSupported, name)
	}

	after, err := e.Get(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// Every id-keyed method rejects an id the adapter did not mint — the
// one error shape the shared suite pins.
func TestUnknownIDIsNotFound(t *testing.T) {
	e := newTestEngine(t, "/nonexistent/yt-dlp")
	id := "ytdlp:01DOESNOTEXIST"
	down := int64(1024)

	for name, call := range map[string]func() error{
		"Get":           func() error { _, err := e.Get(context.Background(), id); return err },
		"Files":         func() error { _, err := e.Files(context.Background(), id); return err },
		"Pause":         func() error { return e.Pause(context.Background(), id) },
		"Resume":        func() error { return e.Resume(context.Background(), id) },
		"Remove":        func() error { return e.Remove(context.Background(), id) },
		"Rename":        func() error { return e.Rename(context.Background(), id, "%(title)s") },
		"SetRateLimits": func() error { return e.SetRateLimits(context.Background(), id, &down, nil) },
	} {
		assert.ErrorIs(t, call(), engine.ErrNotFound, name)
	}
}

// addPausedTask adds a StartPaused media URL: recorded without a spawn,
// so no binary is needed.
func addPausedTask(t *testing.T, e *Engine) string {
	t.Helper()
	id, err := e.Add(context.Background(), engine.AddRequest{
		URIs:        []string{"https://media.example/watch?v=abc"},
		SaveDir:     t.TempDir(),
		StartPaused: true,
	})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(id, "ytdlp:"))
	return id
}

// mediaURLScript is a stub that answers --version, emits two progress
// lines and then sleeps until killed — enough to drive spawn, progress
// and pause/resume without a real download.
const mediaURLScript = `#!/bin/sh
for arg in "$@"; do
	case "$arg" in --version) echo 2026.01.01; exit 0;; esac
done
echo '{"status":"downloading","downloaded":512,"total":2048,"est":null,"speed":1024,"eta":2,"frag":null,"frags":null,"file":null}'
sleep 0.1
echo '{"status":"downloading","downloaded":1024,"total":2048,"est":null,"speed":1024,"eta":1,"frag":null,"frags":null,"file":"/tmp/x.mp4"}'
exec sleep 60
`

func TestAddStartPausedRecordsWithoutSpawning(t *testing.T) {
	e := newTestEngine(t, "/nonexistent/yt-dlp")
	id := addPausedTask(t, e)

	info, err := e.Get(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, engine.StatePaused, info.State)
	assert.Equal(t, engine.NameYtDlp, info.Engine)
	assert.Nil(t, info.CompletedAt)

	infos, err := e.List(context.Background())
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Equal(t, id, infos[0].ID)
}

// Spawn, pause, resume and remove: the watcher reports progress, Pause
// kills the process and marks the record paused, Resume respawns from
// the stored request and Remove drops the task — the lifecycle the
// contract suite checks against a real binary.
func TestLifecycleSpawnPauseResumeRemove(t *testing.T) {
	e := newTestEngine(t, writeStub(t, mediaURLScript))
	saveDir := t.TempDir()

	events, err := e.Events(context.Background())
	require.NoError(t, err)

	id, err := e.Add(context.Background(), engine.AddRequest{
		URIs:    []string{"https://media.example/watch?v=abc"},
		SaveDir: saveDir,
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		info, err := e.Get(context.Background(), id)
		return err == nil && info.State == engine.StateDownloading && info.CompletedBytes == 1024
	}, 5*time.Second, 20*time.Millisecond)

	e.mu.Lock()
	proc := e.tasks[id].proc
	e.mu.Unlock()
	require.NotNil(t, proc)
	require.NoError(t, e.Pause(context.Background(), id))
	// Pause is a kill, not a state rewrite: the stub process is dead
	// before Cancel returns, or Pause would have errored on the grace
	// timeout. ExitCode -1 is a signal death, not a natural exit.
	require.NotNil(t, proc.Cmd.ProcessState)
	assert.Equal(t, -1, proc.Cmd.ProcessState.ExitCode(), "pause must kill the process")
	info, err := e.Get(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, engine.StatePaused, info.State)
	assert.Zero(t, info.DownloadRate)
	assert.Nil(t, info.ETASeconds)

	require.NoError(t, e.Resume(context.Background(), id))
	require.Eventually(t, func() bool {
		info, err := e.Get(context.Background(), id)
		return err == nil && info.State == engine.StateDownloading && info.CompletedBytes >= 1024
	}, 5*time.Second, 20*time.Millisecond)

	e.mu.Lock()
	proc = e.tasks[id].proc
	e.mu.Unlock()
	require.NotNil(t, proc)
	require.NoError(t, e.Remove(context.Background(), id))
	require.NotNil(t, proc.Cmd.ProcessState)
	assert.Equal(t, -1, proc.Cmd.ProcessState.ExitCode(), "remove must kill the process")
	_, err = e.Get(context.Background(), id)
	assert.ErrorIs(t, err, engine.ErrNotFound)

	saw := map[engine.EventKind]bool{}
	deadline := time.Now().Add(5 * time.Second)
	for !saw[engine.EventAdded] || !saw[engine.EventProgress] || !saw[engine.EventPaused] || !saw[engine.EventRemoved] {
		require.False(t, time.Now().After(deadline), "timed out waiting for events, saw %v", saw)
		select {
		case ev := <-events:
			saw[ev.Kind] = true
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// A zero exit settles the task completed and folds the --print-to-file
// document into the reported info.
func TestCompletionAppliesInfoDocument(t *testing.T) {
	e := newTestEngine(t, writeStub(t, `#!/bin/sh
info=""
while [ $# -gt 0 ]; do
	if [ "$1" = "--print-to-file" ]; then shift 2; info="$1"; continue; fi
	shift
done
echo '{"status":"downloading","downloaded":2048,"total":2048,"est":null,"speed":1024,"eta":0,"frag":null,"frags":null,"file":"/tmp/x.mp4"}'
printf '{"id":"abc","title":"Stub Video","filename":"/tmp/x.mp4","filesize":2048,"timestamp":1700000000}' > "$info"
echo '{"status":"finished","downloaded":null,"total":2048,"est":null,"speed":null,"eta":null,"frag":null,"frags":null,"file":null}'
exit 0
`))
	saveDir := t.TempDir()

	id, err := e.Add(context.Background(), engine.AddRequest{
		URIs:    []string{"https://media.example/watch?v=abc"},
		SaveDir: saveDir,
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		info, err := e.Get(context.Background(), id)
		return err == nil && info.State == engine.StateCompleted
	}, 5*time.Second, 20*time.Millisecond)

	info, err := e.Get(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, "Stub Video", info.Name)
	assert.NotNil(t, info.CompletedAt)
	assert.Zero(t, info.DownloadRate)
}

// A nonzero exit settles error with the ClassifyExit code, so the
// adapter reports engine_unavailable for exit 100.
func TestErrorExitReportsClassifiedCode(t *testing.T) {
	e := newTestEngine(t, writeStub(t, "#!/bin/sh\necho boom >&2\nexit 100\n"))

	id, err := e.Add(context.Background(), engine.AddRequest{
		URIs:    []string{"https://media.example/watch?v=abc"},
		SaveDir: t.TempDir(),
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		info, err := e.Get(context.Background(), id)
		return err == nil && info.State == engine.StateError
	}, 5*time.Second, 20*time.Millisecond)

	info, err := e.Get(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, "engine_unavailable", info.ErrorCode)
	assert.Equal(t, "boom", info.ErrorMessage)
}

// The stored limits reach only the next spawn: SetRateLimits records a
// per-task and a global value, the spawn combines them into the minimum
// and a running process is never re-limited (section 10.1).
func TestSetRateLimitsStoresForNextSpawn(t *testing.T) {
	e := newTestEngine(t, "/nonexistent/yt-dlp")
	id := addPausedTask(t, e)

	_, err := e.pendingDownloadLimit(id)
	require.NoError(t, err)

	perTask, global := int64(2_000_000), int64(1_000_000)
	require.NoError(t, e.SetRateLimits(context.Background(), id, &perTask, nil))
	require.NoError(t, e.SetRateLimits(context.Background(), "", &global, nil))

	got, err := e.pendingDownloadLimit(id)
	require.NoError(t, err)
	assert.Equal(t, global, got, "the tighter global limit wins")
	got, err = e.pendingDownloadLimit("")
	require.NoError(t, err)
	assert.Equal(t, global, got)

	perTask = 500_000
	require.NoError(t, e.SetRateLimits(context.Background(), id, &perTask, nil))
	got, err = e.pendingDownloadLimit(id)
	require.NoError(t, err)
	assert.Equal(t, perTask, got, "the tighter per-task limit wins")
}

// A negative limit is a caller bug: it is rejected outright rather than
// stored into the argv of the next spawn, for both the global and the
// per-task form.
func TestSetRateLimitsRejectsNegative(t *testing.T) {
	e := newTestEngine(t, "/nonexistent/yt-dlp")
	id := addPausedTask(t, e)

	negative := int64(-1)
	assert.Error(t, e.SetRateLimits(context.Background(), id, &negative, nil))
	assert.Error(t, e.SetRateLimits(context.Background(), "", &negative, nil))

	got, err := e.pendingDownloadLimit(id)
	require.NoError(t, err)
	assert.Zero(t, got)
}

// Remove drops the record and the info document — dl-tool bookkeeping,
// not payload — while a payload file the download wrote is retained.
func TestRemoveDropsTaskAndDeletesInfoDocumentOnly(t *testing.T) {
	e := newTestEngine(t, writeStub(t, `#!/bin/sh
info=""
while [ $# -gt 0 ]; do
	if [ "$1" = "--print-to-file" ]; then shift 2; info="$1"; continue; fi
	shift
done
printf '{"id":"abc","title":"Stub Video","filename":"/tmp/x.mp4","filesize":2048}' > "$info"
exec sleep 60
`))
	saveDir := t.TempDir()
	payload := filepath.Join(saveDir, "payload.bin")
	require.NoError(t, os.WriteFile(payload, []byte("partial"), 0o644))

	id, err := e.Add(context.Background(), engine.AddRequest{
		URIs:    []string{"https://media.example/watch?v=abc"},
		SaveDir: saveDir,
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(filepath.Join(saveDir, InfoJSONName))
		return statErr == nil
	}, 5*time.Second, 20*time.Millisecond)

	require.NoError(t, e.Remove(context.Background(), id))
	_, err = e.Get(context.Background(), id)
	assert.ErrorIs(t, err, engine.ErrNotFound)
	_, err = os.Stat(filepath.Join(saveDir, InfoJSONName))
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(payload)
	assert.NoError(t, err, "payload data is always retained")
}

// Files reports exactly one selected entry — yt-dlp's whole output
// model — built from the recorded info.
func TestFilesReturnsSingleEntry(t *testing.T) {
	e := newTestEngine(t, "/nonexistent/yt-dlp")
	id := addPausedTask(t, e)

	files, err := e.Files(context.Background(), id)
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, 0, files[0].Index)
	assert.True(t, files[0].Selected)
	assert.Nil(t, files[0].Priority)
}

// Rename stores the -o template for the next spawn and rejects a
// template that escapes SaveDir.
func TestRenameStoresTemplateForNextSpawn(t *testing.T) {
	e := newTestEngine(t, "/nonexistent/yt-dlp")
	id := addPausedTask(t, e)

	require.NoError(t, e.Rename(context.Background(), id, "%(uploader)s - %(title)s.%(ext)s"))
	assert.Equal(t, "%(uploader)s - %(title)s.%(ext)s", e.tasks[id].req.Filename)

	assert.Error(t, e.Rename(context.Background(), id, "../escape"))
}

// List reports only the tasks this adapter minted — nothing a foreign
// yt-dlp process could leak into.
func TestListReportsOnlyMintedTasks(t *testing.T) {
	e := newTestEngine(t, "/nonexistent/yt-dlp")
	id1 := addPausedTask(t, e)
	id2 := addPausedTask(t, e)

	infos, err := e.List(context.Background())
	require.NoError(t, err)
	require.Len(t, infos, 2)
	assert.ElementsMatch(t, []string{id1, id2}, []string{infos[0].ID, infos[1].ID})
	assert.Less(t, infos[0].ID, infos[1].ID, "the snapshot is sorted and stable")
}

// Health reports the stub's --version output, and ErrUnavailable when
// the binary is absent — the missing-binary signal Connect never raises.
func TestHealthReportsVersionAndUnavailable(t *testing.T) {
	e := newTestEngine(t, writeStub(t, mediaURLScript))
	version, err := e.Health(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "2026.01.01", version)

	broken := NewEngine(testConfig(t, "/nonexistent/yt-dlp"), testLogger())
	_, err = broken.Health(context.Background())
	assert.ErrorIs(t, err, engine.ErrUnavailable)
}

// Accepts matches nothing while the T088 extractor cache is deferred:
// the mechanism the plan prescribed does not exist, so the media-lane
// answer is a plain false and Route keeps nil.
func TestAcceptsMatchesNothing(t *testing.T) {
	e := newTestEngine(t, "/nonexistent/yt-dlp")
	assert.False(t, e.Accepts("https://www.youtube.com/watch?v=abc"))
	assert.False(t, e.Accepts("magnet:?xt=urn:btih:abc"))
}

// Concurrent subscribers each see the pushed feed and are dropped on
// ctx cancel without closing the engine.
func TestEventsFanOutToSubscribers(t *testing.T) {
	e := newTestEngine(t, writeStub(t, mediaURLScript))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := make([]map[engine.EventKind]bool, 2)
	for i := range 2 {
		events, err := e.Events(ctx)
		require.NoError(t, err)
		seen[i] = map[engine.EventKind]bool{}
		wg.Add(1)
		go func(m map[engine.EventKind]bool, ch <-chan engine.TaskEvent) {
			defer wg.Done()
			for ev := range ch {
				mu.Lock()
				m[ev.Kind] = true
				mu.Unlock()
			}
		}(seen[i], events)
	}

	_, err := e.Add(context.Background(), engine.AddRequest{
		URIs:    []string{"https://media.example/watch?v=abc"},
		SaveDir: t.TempDir(),
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return seen[0][engine.EventAdded] && seen[1][engine.EventAdded]
	}, 5*time.Second, 20*time.Millisecond)
	cancel()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for i, m := range seen {
		assert.True(t, m[engine.EventAdded], fmt.Sprintf("subscriber %d", i))
	}
}
