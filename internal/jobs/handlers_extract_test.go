package jobs

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/store"
)

// sevenzipPath locates a 7zz for the suite: the configured env var first,
// then the usual binary names. Tests skip rather than fail on a host
// without one — the image carries the pinned RAR-capable build, so CI
// exercises the real decoder.
func sevenzipPath(t *testing.T) string {
	t.Helper()

	if path := os.Getenv("DLTOOL_SEVENZIP_PATH"); path != "" {
		return path
	}
	for _, name := range []string{"7zz", "7zzs", "7z"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}

	t.Skip("no 7zz binary available")
	return ""
}

// archiveFixture is one candidate payload of TestExtractsAllSixFormats.
type archiveFixture struct {
	ext   string
	build func(t *testing.T, path string)
}

// memberName is the single member every synthesized fixture carries; the
// checked-in .rar fixture holds hello.txt instead.
const memberName = "dir/inner.txt"
const memberBody = "dl-tool extract fixture payload"

func memberArchiveFormats() []archiveFixture {
	return []archiveFixture{
		{"zip", func(t *testing.T, path string) {
			var buf bytes.Buffer
			zw := zip.NewWriter(&buf)
			w, err := zw.Create(memberName)
			require.NoError(t, err)
			_, err = w.Write([]byte(memberBody))
			require.NoError(t, err)
			require.NoError(t, zw.Close())
			require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
		}},
		{"tar", func(t *testing.T, path string) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			require.NoError(t, tw.WriteHeader(&tar.Header{
				Name: memberName, Mode: 0o644, Size: int64(len(memberBody)),
			}))
			_, err := tw.Write([]byte(memberBody))
			require.NoError(t, err)
			require.NoError(t, tw.Close())
			require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
		}},
		{"gz", func(t *testing.T, path string) {
			var buf bytes.Buffer
			gw := gzip.NewWriter(&buf)
			gw.Name = "inner.txt"
			_, err := gw.Write([]byte(memberBody))
			require.NoError(t, err)
			require.NoError(t, gw.Close())
			require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
		}},
		{"tgz", func(t *testing.T, path string) {
			var inner bytes.Buffer
			tw := tar.NewWriter(&inner)
			require.NoError(t, tw.WriteHeader(&tar.Header{
				Name: memberName, Mode: 0o644, Size: int64(len(memberBody)),
			}))
			_, err := tw.Write([]byte(memberBody))
			require.NoError(t, err)
			require.NoError(t, tw.Close())

			var buf bytes.Buffer
			gw := gzip.NewWriter(&buf)
			_, err = gw.Write(inner.Bytes())
			require.NoError(t, err)
			require.NoError(t, gw.Close())
			require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
		}},
		{"7z", func(t *testing.T, path string) {
			dir := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "dir"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, memberName), []byte(memberBody), 0o644))
			cmd := exec.Command(sevenzipPath(t), "a", "-y", path, filepath.Join(dir, "dir"))
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "7zz a failed: %s", out)
		}},
		{"rar", func(t *testing.T, path string) {
			// 7-Zip never writes RAR, so the fixture is checked in — a real
			// RAR5 archive minted by the upstream RAR trial binary
			// (ADR-0021). It contains hello.txt, 25 bytes.
			data, err := os.ReadFile(filepath.Join("testdata", "fixture.rar"))
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0o644))
		}},
	}
}

// newCompletedTask inserts one task row already in completed whose payload
// is the archive at path, and returns its id.
func newCompletedTask(t *testing.T, db *sqlx.DB, destination, archivePath string) string {
	t.Helper()

	tasks := store.NewTaskStore(db)
	task, err := tasks.Create(t.Context(), store.Task{
		Engine:      "aria2",
		SourceKind:  "http",
		Name:        filepath.Base(archivePath),
		State:       "completed",
		Destination: destination,
		ContentPath: &archivePath,
	})
	require.NoError(t, err)

	return task.ID
}

func extractJobFor(taskID string) store.Job {
	return store.Job{ID: "job_test_" + taskID, Kind: JobKindExtract, TaskID: &taskID}
}

// unzipProgress reads the gauge column directly — it is not part of the
// Task projection.
func unzipProgress(t *testing.T, db *sqlx.DB, taskID string) int64 {
	t.Helper()
	var progress int64
	require.NoError(t, db.GetContext(
		t.Context(), &progress,
		`SELECT COALESCE(unzip_progress, -1) FROM tasks WHERE id = ?`, taskID,
	))
	return progress
}

func taskErrorCode(t *testing.T, db *sqlx.DB, taskID string) string {
	t.Helper()
	var code *string
	require.NoError(t, db.GetContext(
		t.Context(), &code, `SELECT error_code FROM tasks WHERE id = ?`, taskID,
	))
	if code == nil {
		return ""
	}
	return *code
}

func taskErrorDetail(t *testing.T, db *sqlx.DB, taskID string) string {
	t.Helper()
	var msg *string
	require.NoError(t, db.GetContext(
		t.Context(), &msg, `SELECT error_message FROM tasks WHERE id = ?`, taskID,
	))
	if msg == nil {
		return ""
	}
	return *msg
}

func taskState(t *testing.T, db *sqlx.DB, taskID string) string {
	t.Helper()
	var state string
	require.NoError(t, db.GetContext(
		t.Context(), &state, `SELECT state FROM tasks WHERE id = ?`, taskID,
	))
	return state
}

func eventCodes(t *testing.T, db *sqlx.DB, taskID string) []string {
	t.Helper()
	var codes []string
	require.NoError(t, db.SelectContext(
		t.Context(), &codes,
		`SELECT code FROM task_events WHERE task_id = ? ORDER BY at`, taskID,
	))
	return codes
}

func TestExtractsAllSixFormats(t *testing.T) {
	sevenzipPath(t)

	for _, fixture := range memberArchiveFormats() {
		t.Run(fixture.ext, func(t *testing.T) {
			db := newTestDB(t)
			dest := t.TempDir()
			archive := filepath.Join(dest, "payload."+fixture.ext)
			fixture.build(t, archive)

			taskID := newCompletedTask(t, db, dest, archive)
			handler := NewExtractHandler(store.NewTaskStore(db), sevenzipPath(t))
			require.NoError(t, handler.Handle(t.Context(), extractJobFor(taskID)))

			assert.Equal(t, "completed", taskState(t, db, taskID), "error_message: %s", taskErrorDetail(t, db, taskID))
			assert.Equal(t, int64(100), unzipProgress(t, db, taskID))

			// The verified tree lands in a directory named after the
			// archive stem, beside the archive itself.
			stem := strings.TrimSuffix(filepath.Base(archive), filepath.Ext(archive))
			out := filepath.Join(dest, stem)
			var wantName, wantBody string
			switch fixture.ext {
			case "rar":
				wantName, wantBody = "hello.txt", ""
			case "gz":
				// 7zz names a bare gzip's member from the stored header
				// name (the archive stem only when the header omits it).
				wantName, wantBody = "inner.txt", memberBody
			default:
				wantName, wantBody = memberName, memberBody
			}
			extracted := filepath.Join(out, wantName)
			info, err := os.Lstat(extracted)
			require.NoError(t, err, "extracted member missing at %s", extracted)
			assert.True(t, info.Mode().IsRegular())
			assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
			if fixture.ext == "rar" {
				// fixture.rar's hello.txt is pinned at 25 bytes — drift
				// in the checked-in fixture must fail here.
				assert.Equal(t, int64(25), info.Size())
			}
			if wantBody != "" {
				data, err := os.ReadFile(extracted)
				require.NoError(t, err)
				assert.Equal(t, wantBody, string(data))
			}

			// A .tgz must yield the tar's contents, not the .tar shell.
			if fixture.ext == "tgz" {
				entries, err := os.ReadDir(out)
				require.NoError(t, err)
				require.Len(t, entries, 1)
				assert.Equal(t, "dir", entries[0].Name())
			}

			assert.NoDirExists(t, filepath.Join(dest, stem+".tar"))

			// No staging directory survives a clean run.
			leftovers, err := filepath.Glob(filepath.Join(dest, ".dl-tool-extract-*"))
			require.NoError(t, err)
			assert.Empty(t, leftovers)

			codes := eventCodes(t, db, taskID)
			assert.Contains(t, codes, eventExtractStarted)
			assert.Contains(t, codes, eventExtractCompleted)
		})
	}
}

func TestProgressReaches100(t *testing.T) {
	bin := sevenzipPath(t)

	// A multi-megabyte payload keeps the extractor busy long enough for the
	// shrunken poll interval to land real mid-run gauge writes. The data is
	// random so the zip is roughly as large as its contents and stays far
	// under the 10x total-size cap.
	restore := extractPollInterval
	extractPollInterval = time.Millisecond
	t.Cleanup(func() { extractPollInterval = restore })

	db := newTestDB(t)
	dest := t.TempDir()
	archive := filepath.Join(dest, "big.zip")

	body := make([]byte, 4<<20)
	_, err := rand.Read(body)
	require.NoError(t, err)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := range 4 {
		w, err := zw.Create(fmt.Sprintf("file-%02d.bin", i))
		require.NoError(t, err)
		_, err = w.Write(body)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	require.NoError(t, os.WriteFile(archive, buf.Bytes(), 0o644))

	taskID := newCompletedTask(t, db, dest, archive)
	handler := NewExtractHandler(store.NewTaskStore(db), bin)

	// Sample the gauge while the handler runs: "advancing" is only proven
	// by a value strictly between the handler's own 0 and 100 writes.
	var seen sync.Map
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var progress int64
			err := db.GetContext(
				context.Background(), &progress,
				`SELECT COALESCE(unzip_progress, -1) FROM tasks WHERE id = ?`, taskID,
			)
			if err == nil {
				seen.Store(progress, struct{}{})
			}
			// ~5x the 1 ms writer cadence so the loops cannot alias.
			time.Sleep(200 * time.Microsecond)
		}
	}()
	require.NoError(t, handler.Handle(t.Context(), extractJobFor(taskID)))
	close(stop)
	<-sampled

	assert.Equal(t, int64(100), unzipProgress(t, db, taskID), "error_message: %s", taskErrorDetail(t, db, taskID))
	assert.Equal(t, "completed", taskState(t, db, taskID))

	var intermediate bool
	seen.Range(func(key, _ any) bool {
		if p := key.(int64); p > 0 && p < 100 {
			intermediate = true
			return false
		}
		return true
	})
	assert.True(t, intermediate, "no intermediate unzip_progress sample observed")
}

func TestTruncatedArchiveIsInvalid(t *testing.T) {
	bin := sevenzipPath(t)

	db := newTestDB(t)
	dest := t.TempDir()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(memberName)
	require.NoError(t, err)
	_, err = w.Write([]byte(memberBody))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	archive := filepath.Join(dest, "broken.zip")
	require.NoError(t, os.WriteFile(archive, buf.Bytes()[:buf.Len()/3], 0o644))

	taskID := newCompletedTask(t, db, dest, archive)
	handler := NewExtractHandler(store.NewTaskStore(db), bin)
	require.NoError(t, handler.Handle(t.Context(), extractJobFor(taskID)))

	assert.Equal(t, "error", taskState(t, db, taskID))
	assert.Equal(t, codeExtractFailedInvalid, taskErrorCode(t, db, taskID))
	assert.Contains(t, eventCodes(t, db, taskID), eventExtractFailed)

	// Nothing was written and no staging directory is left behind.
	entries, err := os.ReadDir(dest)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the archive itself may remain")
}

func TestZipSlipMemberRejectedInPassOne(t *testing.T) {
	bin := sevenzipPath(t)

	db := newTestDB(t)
	dest := t.TempDir()
	archive := filepath.Join(dest, "evil.zip")

	// archive/zip does not sanitise member names — WriteHeader stores the
	// traversal verbatim.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("../escape.txt")
	require.NoError(t, err)
	_, err = w.Write([]byte("escaped"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, os.WriteFile(archive, buf.Bytes(), 0o644))

	taskID := newCompletedTask(t, db, dest, archive)
	handler := NewExtractHandler(store.NewTaskStore(db), bin)
	require.NoError(t, handler.Handle(t.Context(), extractJobFor(taskID)))

	assert.Equal(t, "error", taskState(t, db, taskID))
	assert.Equal(t, codeExtractFailedInvalid, taskErrorCode(t, db, taskID))
	assert.NoFileExists(t, filepath.Join(dest, "..", "escape.txt"))
	assert.NoFileExists(t, filepath.Join(filepath.Dir(dest), "escape.txt"))
	entries, err := os.ReadDir(dest)
	require.NoError(t, err)
	require.Len(t, entries, 1, "pass 1 rejects before anything is written")
}

func TestSymlinkMemberRejected(t *testing.T) {
	bin := sevenzipPath(t)

	db := newTestDB(t)
	dest := t.TempDir()
	archive := filepath.Join(dest, "linked.zip")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	header := &zip.FileHeader{Name: "link", Method: zip.Store}
	header.SetMode(0o777 | os.ModeSymlink)
	w, err := zw.CreateHeader(header)
	require.NoError(t, err)
	_, err = w.Write([]byte("/etc/passwd"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, os.WriteFile(archive, buf.Bytes(), 0o644))

	taskID := newCompletedTask(t, db, dest, archive)
	handler := NewExtractHandler(store.NewTaskStore(db), bin)
	require.NoError(t, handler.Handle(t.Context(), extractJobFor(taskID)))

	assert.Equal(t, "error", taskState(t, db, taskID))
	assert.Equal(t, codeExtractFailedInvalid, taskErrorCode(t, db, taskID))
	entries, err := os.ReadDir(dest)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestTarPrivilegedMembersRejectedInPassOne(t *testing.T) {
	bin := sevenzipPath(t)

	// tar reports member kinds through the Mode field, not Attributes:
	// a setuid file or a FIFO must die in pass 1 before anything writes.
	cases := []struct {
		name   string
		header *tar.Header
	}{
		{"setuid", &tar.Header{Name: "suid.bin", Typeflag: tar.TypeReg, Mode: 0o4755, Size: 1}},
		{"fifo", &tar.Header{Name: "pipe", Typeflag: tar.TypeFifo, Mode: 0o644}},
		{"symlink", &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			dest := t.TempDir()
			archive := filepath.Join(dest, "hostile.tar")

			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			require.NoError(t, tw.WriteHeader(tc.header))
			if tc.header.Typeflag == tar.TypeReg {
				_, err := tw.Write([]byte("x"))
				require.NoError(t, err)
			}
			require.NoError(t, tw.WriteHeader(&tar.Header{
				Name: memberName, Mode: 0o644, Size: int64(len(memberBody)),
			}))
			_, err := tw.Write([]byte(memberBody))
			require.NoError(t, err)
			require.NoError(t, tw.Close())
			require.NoError(t, os.WriteFile(archive, buf.Bytes(), 0o644))

			taskID := newCompletedTask(t, db, dest, archive)
			handler := NewExtractHandler(store.NewTaskStore(db), bin)
			require.NoError(t, handler.Handle(t.Context(), extractJobFor(taskID)))

			assert.Equal(t, "error", taskState(t, db, taskID))
			assert.Equal(t, codeExtractFailedInvalid, taskErrorCode(t, db, taskID))
			entries, err := os.ReadDir(dest)
			require.NoError(t, err)
			assert.Len(t, entries, 1, "pass 1 rejects before anything is written")
		})
	}
}

func TestExtractReRunAfterSuccessIsIdempotent(t *testing.T) {
	bin := sevenzipPath(t)

	// The worker reschedules a job row stranded in running by a crash
	// between Handle's return and the row's done mark; the second run
	// finds its own output in place and must not fail the task.
	db := newTestDB(t)
	dest := t.TempDir()
	archive := filepath.Join(dest, "payload.zip")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(memberName)
	require.NoError(t, err)
	_, err = w.Write([]byte(memberBody))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, os.WriteFile(archive, buf.Bytes(), 0o644))

	taskID := newCompletedTask(t, db, dest, archive)
	handler := NewExtractHandler(store.NewTaskStore(db), bin)
	require.NoError(t, handler.Handle(t.Context(), extractJobFor(taskID)))
	require.Equal(t, "completed", taskState(t, db, taskID))

	require.NoError(t, handler.Handle(t.Context(), extractJobFor(taskID)))
	assert.Equal(t, "completed", taskState(t, db, taskID), "error_message: %s", taskErrorDetail(t, db, taskID))
	assert.Equal(t, "", taskErrorCode(t, db, taskID))

	extracted := filepath.Join(dest, "payload", memberName)
	data, err := os.ReadFile(extracted)
	require.NoError(t, err)
	assert.Equal(t, memberBody, string(data))
}

func TestCompressionBombHitsTheCap(t *testing.T) {
	bin := sevenzipPath(t)

	db := newTestDB(t)
	dest := t.TempDir()
	archive := filepath.Join(dest, "bomb.zip")

	body := bytes.Repeat([]byte{0}, 4<<20) // 4 MiB of zeros
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("zeros.bin")
	require.NoError(t, err)
	_, err = w.Write(body)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, os.WriteFile(archive, buf.Bytes(), 0o644))

	taskID := newCompletedTask(t, db, dest, archive)
	handler := NewExtractHandler(store.NewTaskStore(db), bin)
	handler.caps = Caps{TotalUncompressedBytes: 1 << 20} // 1 MiB
	require.NoError(t, handler.Handle(t.Context(), extractJobFor(taskID)))

	assert.Equal(t, "error", taskState(t, db, taskID))
	assert.Equal(t, codeExtractFailedQuota, taskErrorCode(t, db, taskID))
	entries, err := os.ReadDir(dest)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestNestedArchiveIsNotRecursed(t *testing.T) {
	bin := sevenzipPath(t)

	db := newTestDB(t)
	dest := t.TempDir()

	// inner.zip is a plain payload member, not a gzip layer: the recipe
	// extracts exactly one level and leaves it alone.
	var innerBuf bytes.Buffer
	iz := zip.NewWriter(&innerBuf)
	w, err := iz.Create("deep.txt")
	require.NoError(t, err)
	_, err = w.Write([]byte("deep"))
	require.NoError(t, err)
	require.NoError(t, iz.Close())

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err = zw.Create("inner.zip")
	require.NoError(t, err)
	_, err = w.Write(innerBuf.Bytes())
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	archive := filepath.Join(dest, "outer.zip")
	require.NoError(t, os.WriteFile(archive, buf.Bytes(), 0o644))

	taskID := newCompletedTask(t, db, dest, archive)
	handler := NewExtractHandler(store.NewTaskStore(db), bin)
	require.NoError(t, handler.Handle(t.Context(), extractJobFor(taskID)))

	assert.Equal(t, "completed", taskState(t, db, taskID))
	// The nested archive sits inside the output verbatim, unopened.
	assert.FileExists(t, filepath.Join(dest, "outer", "inner.zip"))
	assert.NoDirExists(t, filepath.Join(dest, "outer", "inner"))
}

func TestFailedExtractJobIsRedispatched(t *testing.T) {
	db := newTestDB(t)
	dest := t.TempDir()
	archive := filepath.Join(dest, "payload.zip")
	require.NoError(t, os.WriteFile(archive, []byte("pk"), 0o644))

	setBoolSetting(t, db, settingAutoExtract, true)

	taskID := newCompletedTask(t, db, dest, archive)
	chain := NewChain(db, store.NewTaskStore(db))
	require.NoError(t, chain.OnCompleted(t.Context(), taskID))

	// The first run's job row exhausts its retries; the operator retries
	// the download, the task reaches completed again, and the chain owes
	// the new completion a fresh run on the same (kind, task_id) row.
	_, err := db.ExecContext(
		t.Context(),
		`UPDATE jobs SET state = 'failed', attempts = max_attempts WHERE kind = ? AND task_id = ?`,
		JobKindExtract, taskID,
	)
	require.NoError(t, err)

	require.NoError(t, chain.OnCompleted(t.Context(), taskID))

	var state string
	require.NoError(t, db.GetContext(
		t.Context(), &state,
		`SELECT state FROM jobs WHERE kind = ? AND task_id = ?`, JobKindExtract, taskID,
	))
	assert.Equal(t, "pending", state, "a failed extract row is reset for the retried completion")
}

func TestSameStemCollisionFails(t *testing.T) {
	bin := sevenzipPath(t)

	// dest/payload already holds a different archive's output; the staged
	// tree does not match it, so the run must fail loudly rather than
	// report the other payload as this task's extraction.
	db := newTestDB(t)
	dest := t.TempDir()
	archive := filepath.Join(dest, "payload.zip")
	require.NoError(t, os.MkdirAll(filepath.Join(dest, "payload"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dest, "payload", "other.txt"), []byte("not ours"), 0o644))

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(memberName)
	require.NoError(t, err)
	_, err = w.Write([]byte(memberBody))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, os.WriteFile(archive, buf.Bytes(), 0o644))

	taskID := newCompletedTask(t, db, dest, archive)
	handler := NewExtractHandler(store.NewTaskStore(db), bin)
	require.NoError(t, handler.Handle(t.Context(), extractJobFor(taskID)))

	assert.Equal(t, "error", taskState(t, db, taskID))
	assert.Equal(t, codeExtractFailed, taskErrorCode(t, db, taskID))
	// The foreign directory is untouched.
	data, err := os.ReadFile(filepath.Join(dest, "payload", "other.txt"))
	require.NoError(t, err)
	assert.Equal(t, "not ours", string(data))
	assert.NoFileExists(t, filepath.Join(dest, "payload", memberName))
}

func TestSameExtractedTreeRequiresAnExactMatch(t *testing.T) {
	staged := t.TempDir()
	target := t.TempDir()

	stage := func() {
		require.NoError(t, os.MkdirAll(filepath.Join(staged, "sub"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(staged, "sub", "a.txt"), []byte("a"), 0o644))
	}
	populate := func(dir string) {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "a.txt"), []byte("a"), 0o644))
	}
	stage()
	populate(target)

	same, err := sameExtractedTree(staged, target)
	require.NoError(t, err)
	assert.True(t, same)

	// A superset target is a polluted home, not a re-run: the extra file
	// has no staged counterpart, so the trees differ.
	extra := t.TempDir()
	populate(extra)
	require.NoError(t, os.WriteFile(filepath.Join(extra, "stray.txt"), []byte("x"), 0o644))
	same, err = sameExtractedTree(staged, extra)
	require.NoError(t, err)
	assert.False(t, same)

	// A symlink where the staged tree has a regular file is a type swap,
	// not a match — even though both occupy the same relative path.
	swapped := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(swapped, "sub"), 0o755))
	require.NoError(t, os.Symlink("/etc/passwd", filepath.Join(swapped, "sub", "a.txt")))
	same, err = sameExtractedTree(staged, swapped)
	require.NoError(t, err)
	assert.False(t, same)
}

func TestListMembersHonoursCancellation(t *testing.T) {
	bin := sevenzipPath(t)

	dest := t.TempDir()
	archive := filepath.Join(dest, "payload.zip")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(memberName)
	require.NoError(t, err)
	_, err = w.Write([]byte(memberBody))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, os.WriteFile(archive, buf.Bytes(), 0o644))

	// A cancelled context is an abort, not a verdict on the archive.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = ListMembers(ctx, bin, archive)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrInvalidArchive)
}

func TestAutoExtractDefaultsOff(t *testing.T) {
	db := newTestDB(t)
	dest := t.TempDir()
	archive := filepath.Join(dest, "payload.zip")
	require.NoError(t, os.WriteFile(archive, []byte("not even a real zip"), 0o644))

	taskID := newCompletedTask(t, db, dest, archive)
	chain := NewChain(db, store.NewTaskStore(db))
	require.NoError(t, chain.OnCompleted(t.Context(), taskID))

	var count int
	require.NoError(t, db.GetContext(
		t.Context(), &count,
		`SELECT COUNT(*) FROM jobs WHERE kind = ? AND task_id = ?`, JobKindExtract, taskID,
	))
	assert.Equal(t, 0, count, "auto_extract defaults to false: no job may be enqueued")
}

func TestChainEnqueuesAndHandlerRuns(t *testing.T) {
	bin := sevenzipPath(t)

	db := newTestDB(t)
	dest := t.TempDir()
	archive := filepath.Join(dest, "payload.zip")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(memberName)
	require.NoError(t, err)
	_, err = w.Write([]byte(memberBody))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, os.WriteFile(archive, buf.Bytes(), 0o644))

	setBoolSetting(t, db, settingAutoExtract, true)

	tasks := store.NewTaskStore(db)
	chain := NewChain(db, tasks)

	// Install the hook exactly as main.go does, so the completed →
	// extracting → completed leg re-enters the chain through the store.
	// SetCompletedHook is process-global: do not add t.Parallel() to this
	// package without serializing hook installation.
	store.SetCompletedHook(func(ctx context.Context, taskID string) {
		require.NoError(t, chain.OnCompleted(ctx, taskID))
	})
	t.Cleanup(func() { store.SetCompletedHook(nil) })

	taskID := newCompletedTask(t, db, dest, archive)

	// A completed task reaches the chain through the transition hook, the
	// same path engine reconciliation takes.
	require.NoError(t, tasks.Transition(t.Context(), taskID, "seeding", "test.setup", ""))
	require.NoError(t, tasks.Transition(t.Context(), taskID, "completed", "test.setup", ""))

	var job store.Job
	require.NoError(t, db.GetContext(
		t.Context(), &job,
		`SELECT id, kind, task_id, payload_json, state, attempts, max_attempts,
		        run_after, locked_at, last_error
		   FROM jobs WHERE kind = ? AND task_id = ?`, JobKindExtract, taskID,
	))
	assert.Equal(t, JobKindExtract, job.Kind)
	assert.Equal(t, "pending", job.State)

	// A second completion is idempotent on (kind, task_id).
	require.NoError(t, tasks.Transition(t.Context(), taskID, "checking", "test.setup", ""))
	require.NoError(t, tasks.Transition(t.Context(), taskID, "completed", "test.setup", ""))
	var jobCount int
	require.NoError(t, db.GetContext(
		t.Context(), &jobCount,
		`SELECT COUNT(*) FROM jobs WHERE kind = ? AND task_id = ?`, JobKindExtract, taskID,
	))
	assert.Equal(t, 1, jobCount)

	handler := NewExtractHandler(tasks, bin)
	require.NoError(t, handler.Handle(t.Context(), job))
	assert.Equal(t, "completed", taskState(t, db, taskID))
	assert.FileExists(t, filepath.Join(dest, "payload", memberName))
}

func TestAutoRemoveDeletesRowKeepsFiles(t *testing.T) {
	db := newTestDB(t)
	dest := t.TempDir()
	payload := filepath.Join(dest, "keep-me.txt")
	require.NoError(t, os.WriteFile(payload, []byte("stays"), 0o644))

	setBoolSetting(t, db, settingAutoRemoveOnComplete, true)

	taskID := newCompletedTask(t, db, dest, payload)
	chain := NewChain(db, store.NewTaskStore(db))
	require.NoError(t, chain.OnCompleted(t.Context(), taskID))

	var count int
	require.NoError(t, db.GetContext(
		t.Context(), &count, `SELECT COUNT(*) FROM tasks WHERE id = ?`, taskID,
	))
	assert.Equal(t, 0, count, "auto_remove_on_complete deletes the row")
	assert.FileExists(t, payload, "downloaded data stays on disk")
}

func setBoolSetting(t *testing.T, db *sqlx.DB, key string, value bool) {
	t.Helper()
	_, err := db.ExecContext(
		t.Context(),
		`INSERT INTO settings (id, key, value_json, created_at, updated_at)
		 VALUES (?, ?, ?, 0, 0)
		 ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json`,
		"set_test_"+key, key, fmt.Sprintf("%t", value),
	)
	require.NoError(t, err)
}

func TestAutoRemoveAfterExtractChain(t *testing.T) {
	bin := sevenzipPath(t)

	db := newTestDB(t)
	dest := t.TempDir()
	archive := filepath.Join(dest, "payload.zip")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(memberName)
	require.NoError(t, err)
	_, err = w.Write([]byte(memberBody))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, os.WriteFile(archive, buf.Bytes(), 0o644))

	setBoolSetting(t, db, settingAutoExtract, true)
	setBoolSetting(t, db, settingAutoRemoveOnComplete, true)

	tasks := store.NewTaskStore(db)
	chain := NewChain(db, tasks)
	store.SetCompletedHook(func(ctx context.Context, taskID string) {
		require.NoError(t, chain.OnCompleted(ctx, taskID))
	})
	t.Cleanup(func() { store.SetCompletedHook(nil) })

	taskID := newCompletedTask(t, db, dest, archive)
	require.NoError(t, chain.OnCompleted(t.Context(), taskID))

	// Emulate the worker's claim on the real row: it is running while
	// Handle is in flight, which is what the chain sees on the handler's
	// success leg.
	_, err = db.ExecContext(
		t.Context(),
		`UPDATE jobs SET state = 'running' WHERE kind = ? AND task_id = ?`,
		JobKindExtract, taskID,
	)
	require.NoError(t, err)

	var job store.Job
	require.NoError(t, db.GetContext(
		t.Context(), &job,
		`SELECT id, kind, task_id, payload_json, state, attempts, max_attempts,
		        run_after, locked_at, last_error
		   FROM jobs WHERE kind = ? AND task_id = ?`, JobKindExtract, taskID,
	))

	handler := NewExtractHandler(tasks, bin)
	require.NoError(t, handler.Handle(t.Context(), job))

	// The completed transition the handler's return leg makes re-entered
	// the chain, which ran the auto-remove tail.
	var count int
	require.NoError(t, db.GetContext(
		t.Context(), &count, `SELECT COUNT(*) FROM tasks WHERE id = ?`, taskID,
	))
	assert.Equal(t, 0, count)
	assert.FileExists(t, filepath.Join(dest, "payload", memberName))
	assert.FileExists(t, archive)
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name    string
		members []Member
		want    error
	}{
		{"absolute path", []Member{{Path: "/etc/cron.d/x", Size: 1}}, ErrInvalidArchive},
		{"dotdot", []Member{{Path: "../escape", Size: 1}}, ErrInvalidArchive},
		{"mid-path dotdot", []Member{{Path: "a/../../escape", Size: 1}}, ErrInvalidArchive},
		{"backslash traversal", []Member{{Path: `..\escape`, Size: 1}}, ErrInvalidArchive},
		{"backslash mid-path dotdot", []Member{{Path: `a\..\escape`, Size: 1}}, ErrInvalidArchive},
		{"symlink mode", []Member{{Path: "link", Size: 1, Attributes: " lrwxrwxrwx"}}, ErrInvalidArchive},
		{"fifo mode", []Member{{Path: "pipe", Size: 0, Attributes: " prw-r--r--"}}, ErrInvalidArchive},
		{"device mode", []Member{{Path: "dev", Size: 0, Attributes: " crw-r--r--"}}, ErrInvalidArchive},
		{"setuid mode", []Member{{Path: "suid", Size: 1, Attributes: " -rwsr-xr-x"}}, ErrInvalidArchive},
		{"unsanitisable name", []Member{{Path: "con.txt", Size: 1}}, ErrInvalidArchive},
		{"member count", make([]Member, defaultMemberCount+1), ErrCapExceeded},
		{"single member cap", []Member{{Path: "big", Size: defaultSingleMemberBytes + 1}}, ErrCapExceeded},
		{"total cap", []Member{{Path: "a", Size: 100}, {Path: "b", Size: 100}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(tc.members, Caps{}.withDefaults(1<<20))
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
		})
	}
}
