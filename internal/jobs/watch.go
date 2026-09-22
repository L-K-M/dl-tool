// The watch-folder loader (T083): a .torrent dropped into an enabled
// watch_folders row's directory becomes a task through the ordinary
// creation path within one poll interval. Registration uses inotify on
// Linux and falls back to polling when it is unavailable; the portable
// build polls always (docs/05-api-contract.md section 15, FR-043).
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/L-K-M/dl-tool/internal/store"
	"github.com/L-K-M/dl-tool/internal/uri"
)

// SkipReason is the closed vocabulary of docs/05-api-contract.md section
// 15 — every skipped file carries exactly one of these and no other
// string.
type SkipReason string

const (
	SkipNotATorrent   SkipReason = "not_a_torrent"
	SkipAlreadyLoaded SkipReason = "already_loaded"
	SkipUnreadable    SkipReason = "unreadable"
	SkipDuplicate     SkipReason = "torrent_duplicate"
	SkipPathRejected  SkipReason = "path_rejected"
)

// ScanResult is the body of POST /watch-folders/{id}/scan.
type ScanResult struct {
	Scanned   int           `json:"scanned"`
	Created   []string      `json:"created"`
	Skipped   []SkippedFile `json:"skipped"`
	ElapsedMS int64         `json:"elapsed_ms"`
}

// SkippedFile is one scanned entry that produced no task.
type SkippedFile struct {
	File   string     `json:"file"`
	Reason SkipReason `json:"reason"`
}

// TaskCreator is the T020 creation path, injected so the watcher never
// re-implements it. name is the file's base name — it feeds the rejection
// display and the task-name fallback, exactly as an UploadedFile part name
// does.
type TaskCreator interface {
	CreateFromTorrent(ctx context.Context, name string, blob []byte, dest, category string) (taskID string, err error)
}

// ErrTorrentDuplicate and ErrDestinationRejected are the classified
// failures the TaskCreator contract defines; ScanOnce maps them with
// errors.Is onto SkipDuplicate and SkipPathRejected. Any other error is a
// hand-off failure: the file stays in place and the scan records it in the
// folder's last_error through TouchWatchFolder.
var (
	ErrTorrentDuplicate    = errors.New("jobs: a task for this torrent already exists")
	ErrDestinationRejected = errors.New("jobs: destination resolves outside the data roots")
)

// torrentFileSuffix is the extension an entry must carry to be a load
// candidate; everything else is not_a_torrent.
const torrentFileSuffix = ".torrent"

// defaultWatchPollInterval is the sweep cadence when a folder's
// poll_interval_s is unset or below 1 — the API validates >= 1, the store
// does not, so the loader enforces the floor itself.
const defaultWatchPollInterval = 10 * time.Second

// folderWatcher is the platform registration for one folder. C fires each
// time the folder wants a scan and Close releases the registration.
type folderWatcher interface {
	C() <-chan time.Time
	Close() error
}

// newOSWatcher is the platform hook. The default is the polling
// implementation; the Linux build replaces it in an init() with the
// inotify implementation, so no dependency outside the standard library is
// added. An implementation may report a registration failure alongside a
// non-nil fallback watcher: Run logs the failure and watches through it.
var newOSWatcher = newPollWatcher

// pollWatcher is the portable implementation: a scan per tick.
type pollWatcher struct{ ticker *time.Ticker }

func newPollWatcher(folder store.WatchFolder) (folderWatcher, error) {
	return &pollWatcher{ticker: time.NewTicker(pollInterval(folder))}, nil
}

func (p *pollWatcher) C() <-chan time.Time { return p.ticker.C }

func (p *pollWatcher) Close() error {
	p.ticker.Stop()
	return nil
}

// pollInterval resolves the folder's sweep cadence: poll_interval_s when
// it is at least 1, the 10-second default otherwise.
func pollInterval(folder store.WatchFolder) time.Duration {
	if folder.PollIntervalS < 1 {
		return defaultWatchPollInterval
	}

	return time.Duration(folder.PollIntervalS) * time.Second
}

// Watcher loads torrents from watch folders.
type Watcher struct {
	settings *store.SettingsStore
	creator  TaskCreator
	// scanMu guards scans, the per-folder lock map ScanOnce takes before
	// touching a row's loaded set — the poll loop and an explicit scan
	// (T107's POST /watch-folders/{id}/scan) can race the same folder, and
	// MarkWatchFolderLoaded is a read-modify-write on one settings key.
	scanMu sync.Mutex
	scans  map[string]*sync.Mutex
	// log is set by Scheduler.WithWatcher to the scheduler's logger;
	// slog.Default is the fallback outside that attach.
	log *slog.Logger
}

// NewWatcher builds the loader over the settings store and the injected
// task-creation path.
func NewWatcher(st *store.SettingsStore, creator TaskCreator) *Watcher {
	return &Watcher{settings: st, creator: creator, scans: map[string]*sync.Mutex{}}
}

func (w *Watcher) logger() *slog.Logger {
	if w.log == nil {
		return slog.Default()
	}

	return w.log
}

// Run watches every enabled folder until ctx ends. It calls newOSWatcher
// for each folder; when registration fails it falls back to a time.Ticker
// at the folder's poll_interval_s. Both paths call ScanOnce and nothing
// else. The folder list is read once at start — a row created while Run
// is live is picked up on restart — and ScanOnce's per-folder lock keeps
// a scan from running concurrently with itself on the same row.
func (w *Watcher) Run(ctx context.Context) error {
	folders, err := w.settings.ListEnabledWatchFolders(ctx)
	if err != nil {
		return fmt.Errorf("jobs: list enabled watch folders: %w", err)
	}

	var wg sync.WaitGroup
	for _, folder := range folders {
		fw, err := newOSWatcher(folder)
		if err != nil {
			w.logger().WarnContext(ctx, "watch folder registration failed; polling instead",
				"path", folder.Path, "err", err)
			if fw == nil {
				fw, err = newPollWatcher(folder)
				if err != nil {
					w.logger().ErrorContext(ctx, "watch folder polling setup failed",
						"path", folder.Path, "err", err)
					continue
				}
			}
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			w.watchFolder(ctx, folder, fw)
		}()
	}
	wg.Wait()

	return nil
}

// watchFolder is the per-folder scan loop: one sweep at start so a file
// dropped while the process was down loads without waiting out a full
// interval, then one per watcher signal until ctx ends.
func (w *Watcher) watchFolder(ctx context.Context, folder store.WatchFolder, fw folderWatcher) {
	defer func() {
		if err := fw.Close(); err != nil && ctx.Err() == nil {
			w.logger().WarnContext(ctx, "watch folder close failed", "path", folder.Path, "err", err)
		}
	}()

	w.scanLogged(ctx, folder.ID)

	c := fw.C()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c:
			w.scanLogged(ctx, folder.ID)
		}
	}
}

// scanLogged runs one sweep; a failure is logged rather than returned —
// the loop has no retry channel of its own and the folder's last_error
// already carries what happened.
func (w *Watcher) scanLogged(ctx context.Context, folderID string) {
	if _, err := w.ScanOnce(ctx, folderID); err != nil && ctx.Err() == nil {
		w.logger().ErrorContext(ctx, "watch folder scan failed", "folder_id", folderID, "err", err)
	}
}

// ScanOnce scans exactly one watch folder and returns what it did. It is
// synchronous and idempotent: a file already loaded is skipped with
// SkipAlreadyLoaded, never loaded twice. A disabled folder still scans on
// demand — the scan is what the button does, not the schedule.
func (w *Watcher) ScanOnce(ctx context.Context, folderID string) (ScanResult, error) {
	folder, err := w.settings.GetWatchFolder(ctx, folderID)
	if err != nil {
		return ScanResult{}, err
	}
	if w.creator == nil {
		return ScanResult{}, errors.New("jobs: watcher has no task creator")
	}

	// One scan per folder at a time: the sweep and an explicit scan can
	// race, and the loaded set is a read-modify-write.
	lock := w.folderLock(folder.ID)
	lock.Lock()
	defer lock.Unlock()

	started := time.Now()
	result := ScanResult{Created: []string{}, Skipped: []SkippedFile{}}

	entries, err := os.ReadDir(folder.Path)
	if err != nil {
		w.touch(ctx, folder.ID, err)
		return result, fmt.Errorf("jobs: read watch folder %s: %w", folder.Path, err)
	}

	category := ""
	if folder.Category != nil {
		category = *folder.Category
	}

	var lastErr error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Scanned++
		name := entry.Name()

		if !strings.HasSuffix(name, torrentFileSuffix) {
			result.Skipped = append(result.Skipped, SkippedFile{File: name, Reason: SkipNotATorrent})
			continue
		}
		path := filepath.Join(folder.Path, name)

		// A .torrent-named directory, fifo or socket can never parse and a
		// fifo's ReadFile would block the sweep — anything not a regular
		// file is unreadable at this layer. readdir reports DT_UNKNOWN on
		// some filesystems (XFS without ftype, some NFS/CIFS mounts), so an
		// unknown type falls back to lstat before the file is declared
		// unreadable; a symlink stays unreadable either way.
		if !entry.Type().IsRegular() {
			info, statErr := entry.Info()
			if statErr != nil || !info.Mode().IsRegular() {
				result.Skipped = append(result.Skipped, SkippedFile{File: name, Reason: SkipUnreadable})
				continue
			}
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			result.Skipped = append(result.Skipped, SkippedFile{File: name, Reason: SkipUnreadable})
			continue
		}
		manifest, err := uri.InspectTorrent(blob)
		if err != nil {
			result.Skipped = append(result.Skipped, SkippedFile{File: name, Reason: SkipNotATorrent})
			continue
		}

		// The loaded set keys on the identity magnetFromManifest prefers:
		// the v1 hash when present, the v2 hash otherwise.
		infohash := manifest.InfohashV1
		if infohash == "" {
			infohash = manifest.InfohashV2
		}
		loaded, err := w.settings.WatchFolderLoaded(ctx, folder.ID, infohash)
		if err != nil {
			w.touch(ctx, folder.ID, err)
			return result, fmt.Errorf("jobs: read loaded set of watch folder %s: %w", folder.ID, err)
		}
		if loaded {
			result.Skipped = append(result.Skipped, SkippedFile{File: name, Reason: SkipAlreadyLoaded})
			continue
		}

		taskID, err := w.creator.CreateFromTorrent(ctx, name, blob, folder.Destination, category)
		switch {
		case err == nil:
			// The mark lands after the creator returned a task id, so a
			// re-scan — in this process or after a restart — answers
			// already_loaded. A mark failure cannot uncreate the task; it
			// is recorded as the folder's last_error, and the next sweep's
			// torrent_duplicate is the backstop.
			if err := w.settings.MarkWatchFolderLoaded(ctx, folder.ID, infohash); err != nil {
				lastErr = errors.Join(lastErr, fmt.Errorf("record loaded infohash of %s: %w", name, err))
			}
			// The source is unlinked only after the creator accepted it,
			// and never for a duplicate: delete_after_load never races a
			// hand-off.
			if folder.DeleteAfterLoad != 0 {
				if err := os.Remove(path); err != nil {
					lastErr = errors.Join(lastErr, fmt.Errorf("unlink %s: %w", name, err))
				}
			}
			result.Created = append(result.Created, taskID)
		case errors.Is(err, ErrDestinationRejected):
			result.Skipped = append(result.Skipped, SkippedFile{File: name, Reason: SkipPathRejected})
		case errors.Is(err, ErrTorrentDuplicate):
			result.Skipped = append(result.Skipped, SkippedFile{File: name, Reason: SkipDuplicate})
		default:
			// A hand-off failure leaves the file in place and lands in
			// last_error — the skipped vocabulary has no reason for it.
			lastErr = errors.Join(lastErr, fmt.Errorf("%s: %w", name, err))
		}
	}

	w.touch(ctx, folder.ID, lastErr)
	result.ElapsedMS = time.Since(started).Milliseconds()

	return result, nil
}

// folderLock returns the mutex serializing scans of one folder.
func (w *Watcher) folderLock(folderID string) *sync.Mutex {
	w.scanMu.Lock()
	defer w.scanMu.Unlock()
	lk, ok := w.scans[folderID]
	if !ok {
		lk = &sync.Mutex{}
		w.scans[folderID] = lk
	}

	return lk
}

// watchLastErrorMax bounds the text a sweep writes into last_error — one
// joined line per failing file would grow the column without limit on a
// folder full of hand-off failures.
const watchLastErrorMax = 4096

// touch records the scan outcome on the folder row; a nil lastErr clears
// last_error. A touch failure is logged, not raised — the scan result
// already carries what happened.
func (w *Watcher) touch(ctx context.Context, folderID string, lastErr error) {
	text := ""
	if lastErr != nil {
		text = lastErr.Error()
		if len(text) > watchLastErrorMax {
			text = text[:watchLastErrorMax] + "..."
		}
	}
	if err := w.settings.TouchWatchFolder(ctx, folderID, time.Now().UnixMilli(), text); err != nil && ctx.Err() == nil {
		w.logger().WarnContext(ctx, "watch folder touch failed", "folder_id", folderID, "err", err)
	}
}
