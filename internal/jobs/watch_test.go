package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/store"
	"github.com/L-K-M/dl-tool/internal/uri"
)

// watchPieces20 is one syntactically valid 20-byte pieces value, the same
// fixture shape uri's tests hash offline.
const watchPieces20 = "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"

// watchTorrentV1 is a complete single-file v1 torrent.
const watchTorrentV1 = "d8:announce35:http://tracker.example.com/announce" +
	"4:infod6:lengthi11e4:name9:hello.txt12:piece lengthi16384e6:pieces20:" + watchPieces20 + "ee"

// watchTorrentV2 is the same torrent with a different name, so it carries
// a different infohash — a second torrent beside the first.
const watchTorrentV2 = "d8:announce35:http://tracker.example.com/announce" +
	"4:infod6:lengthi11e4:name9:other.txt12:piece lengthi16384e6:pieces20:" + watchPieces20 + "ee"

// watchPiecesRoot32 is a syntactically valid BEP 52 pieces root.
const watchPiecesRoot32 = "\x11\x11\x11\x11\x11\x11\x11\x11\x11\x11\x11\x11\x11\x11\x11\x11" +
	"\x11\x11\x11\x11\x11\x11\x11\x11\x11\x11\x11\x11\x11\x11\x11\x11"

// watchTorrentV2Only is a meta-version-2 torrent with no pieces field:
// InfohashV1 is empty and InfohashV2 carries the identity.
const watchTorrentV2Only = "d4:infod9:file treed9:hello.txtd0:d6:lengthi11e11:pieces root32:" +
	watchPiecesRoot32 + "eee12:meta versioni2e4:name9:hello.txt12:piece lengthi16384eee"

// watchTorrentV2OnlyB is the same shape under a different name, so its
// v2 infohash differs — two distinct v2-only torrents must both load.
const watchTorrentV2OnlyB = "d4:infod9:file treed9:other.txtd0:d6:lengthi11e11:pieces root32:" +
	watchPiecesRoot32 + "eee12:meta versioni2e4:name9:other.txt12:piece lengthi16384eee"

// watchCreatorCall is one CreateFromTorrent invocation the fake recorded.
type watchCreatorCall struct {
	name     string
	blob     []byte
	dest     string
	category string
}

// fakeWatchCreator is the injected TaskCreator stand-in: it records every
// call and answers per-file or blanket errors, so a test pins which
// creator outcome maps to which skip reason.
type fakeWatchCreator struct {
	mu    sync.Mutex
	calls []watchCreatorCall
	errs  map[string]error
	fail  error
}

func (f *fakeWatchCreator) CreateFromTorrent(_ context.Context, name string, blob []byte, dest, category string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, watchCreatorCall{name: name, blob: blob, dest: dest, category: category})
	if err := f.errs[name]; err != nil {
		return "", err
	}
	if f.fail != nil {
		return "", f.fail
	}

	return "tsk_watch_" + strconv.Itoa(len(f.calls)), nil
}

func (f *fakeWatchCreator) recorded() []watchCreatorCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]watchCreatorCall(nil), f.calls...)
}

// insertWatchFolder writes one watch_folders row directly — the CRUD
// endpoints are T107's — and returns its id.
func insertWatchFolder(t *testing.T, db *sqlx.DB, path, destination string, enabled, deleteAfterLoad, pollIntervalS int) string {
	t.Helper()

	id := store.NewID(store.PrefixWatchFolder)
	now := time.Now().UnixMilli()
	_, err := db.ExecContext(t.Context(), `INSERT INTO watch_folders
		(id, path, enabled, destination, delete_after_load, poll_interval_s, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, path, enabled, destination, deleteAfterLoad, pollIntervalS, now, now)
	require.NoError(t, err)

	return id
}

// insertWatchCategory writes one categories row and returns its id, so a
// folder can carry a category through the join.
func insertWatchCategory(t *testing.T, db *sqlx.DB, name, savePath string) string {
	t.Helper()

	id := store.NewID(store.PrefixCategory)
	now := time.Now().UnixMilli()
	_, err := db.ExecContext(t.Context(),
		`INSERT INTO categories (id, name, save_path, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		id, name, savePath, now, now)
	require.NoError(t, err)

	return id
}

// runWatcher starts Run on a cancellable context; cleanup cancels and
// requires the clean drain.
func runWatcher(t *testing.T, watcher *Watcher) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- watcher.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("watcher.Run did not return after cancel")
		}
	})
}

func TestDroppedTorrentBecomesTask(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	dest := t.TempDir()
	folderID := insertWatchFolder(t, db, dir, dest, 1, 0, 1)

	creator := &fakeWatchCreator{}
	watcher := NewWatcher(store.NewSettingsStore(db), creator)
	runWatcher(t, watcher)

	// The drop lands after the initial sweep, so the loader proves the
	// within-one-interval bound: poll_interval_s is 1 and inotify — when
	// registered — delivers the event sooner.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.torrent"), []byte(watchTorrentV1), 0o644))

	waitFor(t, "dropped torrent handed to the creator", func() bool {
		return len(creator.recorded()) > 0
	})

	call := creator.recorded()[0]
	require.Equal(t, "hello.torrent", call.name)
	require.Equal(t, dest, call.dest)
	require.Equal(t, "", call.category)
	require.Equal(t, []byte(watchTorrentV1), call.blob)

	// delete_after_load is unset: the source stays, and the loaded set
	// and last_scan_at landed on the row — the touch closes the scan, so
	// it trails the creator call by a beat.
	_, err := os.Stat(filepath.Join(dir, "hello.torrent"))
	require.NoError(t, err, "delete_after_load is unset — the source must stay")

	st := store.NewSettingsStore(db)
	waitFor(t, "the scan stamps last_scan_at", func() bool {
		folder, err := st.GetWatchFolder(t.Context(), folderID)
		return err == nil && folder.LastScanAt != nil
	})
}

func TestDeleteAfterLoadOnlyOnSuccess(t *testing.T) {
	db := newTestDB(t)
	st := store.NewSettingsStore(db)

	t.Run("accepted", func(t *testing.T) {
		dir := t.TempDir()
		folderID := insertWatchFolder(t, db, dir, t.TempDir(), 1, 1, 10)
		path := filepath.Join(dir, "hello.torrent")
		require.NoError(t, os.WriteFile(path, []byte(watchTorrentV1), 0o644))

		result, err := NewWatcher(st, &fakeWatchCreator{}).ScanOnce(t.Context(), folderID)
		require.NoError(t, err)
		require.Len(t, result.Created, 1)
		_, statErr := os.Stat(path)
		require.ErrorIs(t, statErr, os.ErrNotExist, "delete_after_load unlinks after acceptance")
	})

	t.Run("handoff failure keeps the file", func(t *testing.T) {
		dir := t.TempDir()
		folderID := insertWatchFolder(t, db, dir, t.TempDir(), 1, 1, 10)
		path := filepath.Join(dir, "hello.torrent")
		require.NoError(t, os.WriteFile(path, []byte(watchTorrentV1), 0o644))

		creator := &fakeWatchCreator{fail: errors.New("engine offline")}
		result, err := NewWatcher(st, creator).ScanOnce(t.Context(), folderID)
		require.NoError(t, err)
		require.Empty(t, result.Created)
		require.Empty(t, result.Skipped, "a hand-off failure is not a skip — it lands in last_error")
		_, statErr := os.Stat(path)
		require.NoError(t, statErr, "a failed hand-off always leaves the file")

		folder, err := st.GetWatchFolder(t.Context(), folderID)
		require.NoError(t, err)
		require.NotNil(t, folder.LastError)
		require.Contains(t, *folder.LastError, "engine offline")
	})

	t.Run("duplicate keeps the file", func(t *testing.T) {
		dir := t.TempDir()
		folderID := insertWatchFolder(t, db, dir, t.TempDir(), 1, 1, 10)
		path := filepath.Join(dir, "hello.torrent")
		require.NoError(t, os.WriteFile(path, []byte(watchTorrentV1), 0o644))

		creator := &fakeWatchCreator{errs: map[string]error{"hello.torrent": ErrTorrentDuplicate}}
		result, err := NewWatcher(st, creator).ScanOnce(t.Context(), folderID)
		require.NoError(t, err)
		require.Empty(t, result.Created)
		_, statErr := os.Stat(path)
		require.NoError(t, statErr, "a SkipDuplicate file is never unlinked")
	})
}

func TestSecondScanSkipsLoaded(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	folderID := insertWatchFolder(t, db, dir, t.TempDir(), 1, 0, 10)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.torrent"), []byte(watchTorrentV1), 0o644))

	creator := &fakeWatchCreator{}
	watcher := NewWatcher(store.NewSettingsStore(db), creator)

	first, err := watcher.ScanOnce(t.Context(), folderID)
	require.NoError(t, err)
	require.Len(t, first.Created, 1)
	require.Empty(t, first.Skipped)

	second, err := watcher.ScanOnce(t.Context(), folderID)
	require.NoError(t, err)
	require.Empty(t, second.Created)
	require.Equal(t, []SkippedFile{{File: "hello.torrent", Reason: SkipAlreadyLoaded}}, second.Skipped)
	require.Len(t, creator.recorded(), 1, "nothing is imported twice")
}

func TestNonTorrentSkipped(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	folderID := insertWatchFolder(t, db, dir, t.TempDir(), 1, 0, 10)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "movie.part"), []byte("partial"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "junk.torrent"), []byte("not bencode"), 0o644))

	result, err := NewWatcher(store.NewSettingsStore(db), &fakeWatchCreator{}).ScanOnce(t.Context(), folderID)
	require.NoError(t, err)
	require.Empty(t, result.Created)
	require.Equal(t, []SkippedFile{
		{File: "junk.torrent", Reason: SkipNotATorrent},
		{File: "movie.part", Reason: SkipNotATorrent},
	}, result.Skipped)
}

func TestRequestedDestinationRecorded(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	// The folder's configured destination is handed to the creator
	// verbatim — resolution and the requested_destination echo are the
	// create path's job (the api adapter test pins the echo landing).
	dest := filepath.Join(t.TempDir(), "downloads")
	// The category's save_path differs on purpose: if the watcher ever
	// handed the category's path instead of the folder's destination, the
	// assertion below would catch it.
	categoryID := insertWatchCategory(t, db, "movies", filepath.Join(t.TempDir(), "category-save"))
	folderID := store.NewID(store.PrefixWatchFolder)
	now := time.Now().UnixMilli()
	_, err := db.ExecContext(t.Context(), `INSERT INTO watch_folders
		(id, path, enabled, destination, category_id, delete_after_load, poll_interval_s, created_at, updated_at)
		VALUES (?, ?, 1, ?, ?, 0, 10, ?, ?)`,
		folderID, dir, dest, categoryID, now, now)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.torrent"), []byte(watchTorrentV1), 0o644))

	creator := &fakeWatchCreator{}
	result, err := NewWatcher(store.NewSettingsStore(db), creator).ScanOnce(t.Context(), folderID)
	require.NoError(t, err)
	require.Len(t, result.Created, 1)

	call := creator.recorded()[0]
	require.Equal(t, dest, call.dest, "the folder's destination reaches the create path verbatim")
	require.Equal(t, "movies", call.category, "the joined category name reaches the create path")
}

func TestCreatorErrorsMapToSkipReasons(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	folderID := insertWatchFolder(t, db, dir, t.TempDir(), 1, 0, 10)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dup.torrent"), []byte(watchTorrentV1), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "jail.torrent"), []byte(watchTorrentV2), 0o644))

	creator := &fakeWatchCreator{errs: map[string]error{
		"dup.torrent":  ErrTorrentDuplicate,
		"jail.torrent": ErrDestinationRejected,
	}}
	result, err := NewWatcher(store.NewSettingsStore(db), creator).ScanOnce(t.Context(), folderID)
	require.NoError(t, err)
	require.Empty(t, result.Created)
	require.Equal(t, []SkippedFile{
		{File: "dup.torrent", Reason: SkipDuplicate},
		{File: "jail.torrent", Reason: SkipPathRejected},
	}, result.Skipped)
}

func TestDisabledFolderScansOnDemand(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	folderID := insertWatchFolder(t, db, dir, t.TempDir(), 0, 0, 10)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.torrent"), []byte(watchTorrentV1), 0o644))

	result, err := NewWatcher(store.NewSettingsStore(db), &fakeWatchCreator{}).ScanOnce(t.Context(), folderID)
	require.NoError(t, err)
	require.Len(t, result.Created, 1, "an explicit scan works on a disabled folder — it is what the button does")
}

func TestFallsBackToPolling(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	folderID := insertWatchFolder(t, db, dir, t.TempDir(), 1, 0, 1)

	// The platform registration failing must not stop the folder: Run
	// falls back to the poll-interval sweep.
	original := newOSWatcher
	newOSWatcher = func(store.WatchFolder) (folderWatcher, error) {
		return nil, errors.New("inotify unavailable")
	}
	t.Cleanup(func() { newOSWatcher = original })

	creator := &fakeWatchCreator{}
	watcher := NewWatcher(store.NewSettingsStore(db), creator)
	runWatcher(t, watcher)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.torrent"), []byte(watchTorrentV1), 0o644))
	waitFor(t, "polling fallback hands the dropped torrent to the creator", func() bool {
		return len(creator.recorded()) > 0
	})

	st := store.NewSettingsStore(db)
	waitFor(t, "the fallback scan stamps last_scan_at", func() bool {
		folder, err := st.GetWatchFolder(t.Context(), folderID)
		return err == nil && folder.LastScanAt != nil
	})
}

// TestLoadedSetSurvivesWatcherRestart pins the persistence half of
// already_loaded: a fresh Watcher over the same store still answers the
// reason, because the record lives in the settings table, not in memory.
func TestLoadedSetSurvivesWatcherRestart(t *testing.T) {
	db := newTestDB(t)
	st := store.NewSettingsStore(db)
	dir := t.TempDir()
	folderID := insertWatchFolder(t, db, dir, t.TempDir(), 1, 0, 10)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.torrent"), []byte(watchTorrentV1), 0o644))

	_, err := NewWatcher(st, &fakeWatchCreator{}).ScanOnce(t.Context(), folderID)
	require.NoError(t, err)

	creator := &fakeWatchCreator{}
	result, err := NewWatcher(st, creator).ScanOnce(t.Context(), folderID)
	require.NoError(t, err)
	require.Equal(t, []SkippedFile{{File: "hello.torrent", Reason: SkipAlreadyLoaded}}, result.Skipped)
	require.Empty(t, creator.recorded())
}

// TestLoadedSetKeysOnV1Hash pins the identity the loaded set consults:
// manifest.InfohashV1 when present, so the mark lands on the same hash
// magnetFromManifest rebuilds the source URI from.
func TestLoadedSetKeysOnV1Hash(t *testing.T) {
	db := newTestDB(t)
	st := store.NewSettingsStore(db)
	dir := t.TempDir()
	folderID := insertWatchFolder(t, db, dir, t.TempDir(), 1, 0, 10)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.torrent"), []byte(watchTorrentV1), 0o644))

	_, err := NewWatcher(st, &fakeWatchCreator{}).ScanOnce(t.Context(), folderID)
	require.NoError(t, err)

	manifest, err := uri.InspectTorrent([]byte(watchTorrentV1))
	require.NoError(t, err)
	loaded, err := st.WatchFolderLoaded(t.Context(), folderID, manifest.InfohashV1)
	require.NoError(t, err)
	require.True(t, loaded, "the v1 infohash is the loaded-set identity")
}

// TestLoadedSetKeysOnV2HashWhenNoV1 pins the fallback the v1 test cannot
// reach: a v2-only torrent has an empty InfohashV1, and the loaded set
// must key on InfohashV2 — otherwise every v2-only torrent after the
// first collides on the empty key and reads as already_loaded.
func TestLoadedSetKeysOnV2HashWhenNoV1(t *testing.T) {
	db := newTestDB(t)
	st := store.NewSettingsStore(db)
	dir := t.TempDir()
	folderID := insertWatchFolder(t, db, dir, t.TempDir(), 1, 0, 10)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.torrent"), []byte(watchTorrentV2Only), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.torrent"), []byte(watchTorrentV2OnlyB), 0o644))

	manifest, err := uri.InspectTorrent([]byte(watchTorrentV2Only))
	require.NoError(t, err)
	require.Empty(t, manifest.InfohashV1, "fixture must be v2-only")
	require.NotEmpty(t, manifest.InfohashV2)

	result, err := NewWatcher(st, &fakeWatchCreator{}).ScanOnce(t.Context(), folderID)
	require.NoError(t, err)
	require.Len(t, result.Created, 2, "both distinct v2-only torrents load — no empty-key collision")
	require.Empty(t, result.Skipped)

	loaded, err := st.WatchFolderLoaded(t.Context(), folderID, manifest.InfohashV2)
	require.NoError(t, err)
	require.True(t, loaded, "the v2 infohash is the loaded-set identity when v1 is empty")
}
