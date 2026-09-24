package ytdlp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/L-K-M/dl-tool/internal/engine"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig(t *testing.T, binaryPath string) Config {
	t.Helper()
	return Config{
		BinaryPath:    binaryPath,
		JSRuntimePath: "/usr/bin/node",
		ArchiveDir:    filepath.Join(t.TempDir(), "archives"),
	}
}

func TestArgvGoldenPlainRequest(t *testing.T) {
	cfg := Config{BinaryPath: "/usr/local/bin/yt-dlp", JSRuntimePath: "/usr/bin/node", ArchiveDir: "/config/archives"}
	req := engine.AddRequest{
		URIs:    []string{"https://example.org/watch?v=abc123"},
		SaveDir: "/data/media",
	}
	got := Argv(cfg, req, "/config/archives/01JB0Q7M8WQ0F1R2S3T4V5W6X7.txt", "/data/media/.dl-tool-info.json", 0)
	want := []string{
		"--no-colors", "--newline", "--no-playlist",
		"--paths", "/data/media",
		"--output", "%(title)s [%(id)s].%(ext)s",
		"--download-archive", "/config/archives/01JB0Q7M8WQ0F1R2S3T4V5W6X7.txt",
		"--progress-template",
		`download:{"status":"%(progress.status)s","downloaded":%(progress.downloaded_bytes|0)d,"total":%(progress.total_bytes|null)j,"est":%(progress.total_bytes_estimate|null)j,"speed":%(progress.speed|null)j,"eta":%(progress.eta|null)j,"frag":%(progress.fragment_index|null)j,"frags":%(progress.fragment_count|null)j,"file":"%(progress.filename)s"}`,
		"--print-to-file", "%()j", "/data/media/.dl-tool-info.json",
		"--no-simulate",
		"--",
		"https://example.org/watch?v=abc123",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Argv mismatch (-want +got):\n%s", diff)
	}
}

func TestArgvUsesFilenameAsOutput(t *testing.T) {
	req := engine.AddRequest{
		URIs:     []string{"https://example.org/v/1"},
		SaveDir:  "/data/media",
		Filename: "%(uploader)s - %(title)s.%(ext)s",
	}
	args := Argv(Config{}, req, "/a.txt", "/i.json", 0)
	idx := slices.Index(args, "--output")
	if idx < 0 || idx+1 >= len(args) {
		t.Fatalf("argv lacks --output value: %v", args)
	}
	if args[idx+1] != req.Filename {
		t.Fatalf("--output = %q, want %q", args[idx+1], req.Filename)
	}
}

func TestArgvPutsURILast(t *testing.T) {
	req := engine.AddRequest{URIs: []string{"https://example.org/v/1"}, SaveDir: "/data/media"}
	args := Argv(Config{}, req, "/a.txt", "/i.json", 4096)
	if args[len(args)-1] != req.URIs[0] {
		t.Fatalf("last arg = %q, want URI %q", args[len(args)-1], req.URIs[0])
	}
	if args[len(args)-2] != "--" {
		t.Fatalf("arg before URI = %q, want end-of-options marker \"--\"", args[len(args)-2])
	}
}

func TestArgvGuardsDashPrefixedURI(t *testing.T) {
	// A URI starting with "-" must land after the end-of-options marker, not be
	// parsed as a yt-dlp flag (ADR-0018 invariant: never -U or --update-to).
	for _, uri := range []string{"--update-to=nightly", "--simulate", "-x"} {
		req := engine.AddRequest{URIs: []string{uri}, SaveDir: "/data/media"}
		args := Argv(Config{}, req, "/a.txt", "/i.json", 0)
		if args[len(args)-1] != uri || args[len(args)-2] != "--" {
			t.Fatalf("uri %q not guarded by \"--\": %v", uri, args)
		}
	}
}

func TestArgvRejectsShellComposition(t *testing.T) {
	for _, uri := range []string{
		"https://example.org/v?a=1;rm -rf /",
		"$(id)",
	} {
		req := engine.AddRequest{URIs: []string{uri}, SaveDir: "/data/media"}
		args := Argv(Config{}, req, "/a.txt", "/i.json", 0)
		if args[len(args)-1] != uri {
			t.Fatalf("uri %q not preserved as last element: %v", uri, args)
		}
		n := 0
		for _, arg := range args {
			if arg == uri {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("uri %q appears %d times in %v", uri, n, args)
		}
		for _, arg := range args {
			if arg == "sh" || arg == "bash" || arg == "-c" ||
				strings.HasPrefix(arg, "sh ") || strings.HasPrefix(arg, "bash ") {
				t.Fatalf("argv %v contains shell invocation element %q", args, arg)
			}
		}
	}
}

func writeStub(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("yt-dlp shell stubs require a POSIX /bin/sh")
	}
	path := filepath.Join(t.TempDir(), "yt-dlp-stub")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func TestSpawnAndCancel(t *testing.T) {
	r := New(testConfig(t, writeStub(t, "exec sleep 60")), testLogger())
	req := engine.AddRequest{URIs: []string{"https://example.org/v/1"}, SaveDir: t.TempDir()}

	start := time.Now()
	p, err := r.Spawn(context.Background(), "ytdlp:01JB0Q7M8WQ0F1R2S3T4V5W6X7", req, 0)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Spawn did not return before the process exited")
	}
	if got := r.Live(); !slices.Equal(got, []string{p.ID}) {
		t.Fatalf("Live() = %v, want [%s]", got, p.ID)
	}

	if err := r.Cancel(p.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// Cancel returned only after the reaper saw the process die: a signalled
	// exit reports -1, proving the OS process was actually killed and not
	// merely deregistered.
	if p.exitCode != -1 || p.Cmd.ProcessState == nil || p.Cmd.ProcessState.ExitCode() != -1 {
		t.Fatalf("stub process not signalled: exitCode=%d state=%v", p.exitCode, p.Cmd.ProcessState)
	}
	if err := p.Stdout.Close(); err != nil {
		t.Fatalf("close stdout: %v", err)
	}

	waited := make(chan error, 1)
	go func() {
		_, err := r.Wait(p.ID)
		waited <- err
	}()
	select {
	case err := <-waited:
		if !errors.Is(err, engine.ErrNotFound) {
			t.Fatalf("Wait after Cancel returned %v, want ErrNotFound (entry removed by Cancel)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return within 10 s of Cancel")
	}

	if got := r.Live(); len(got) != 0 {
		t.Fatalf("Live() = %v, want empty", got)
	}
	if err := r.Cancel(p.ID); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("second Cancel = %v, want ErrNotFound", err)
	}
}

func TestCancelUnknownIDIsNotFound(t *testing.T) {
	r := New(testConfig(t, "/nonexistent/yt-dlp"), testLogger())
	if err := r.Cancel("ytdlp:01JUNKNOWN0000000000000000"); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("Cancel = %v, want ErrNotFound", err)
	}
}

func TestWaitUnknownIDIsNotFound(t *testing.T) {
	r := New(testConfig(t, "/nonexistent/yt-dlp"), testLogger())
	if _, err := r.Wait("ytdlp:01JUNKNOWN0000000000000000"); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("Wait = %v, want ErrNotFound", err)
	}
}

func TestWaitPropagatesExitCode(t *testing.T) {
	r := New(testConfig(t, writeStub(t, "exit 3")), testLogger())
	req := engine.AddRequest{URIs: []string{"https://example.org/v/1"}, SaveDir: t.TempDir()}
	p, err := r.Spawn(context.Background(), "ytdlp:01JB0Q7M8WQ0F1R2S3T4V5W6X7", req, 0)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	code, err := r.Wait(p.ID)
	if code != 3 {
		t.Fatalf("Wait exit code = %d, want 3 (err %v)", code, err)
	}
	if got := r.Live(); len(got) != 0 {
		t.Fatalf("Live() = %v, want empty after Wait", got)
	}
	if err := p.Stdout.Close(); err != nil {
		t.Fatalf("close stdout: %v", err)
	}
}

func TestStdoutReadsToEOFAcrossProcessExit(t *testing.T) {
	r := New(testConfig(t, writeStub(t, "echo line1\nexec sleep 60")), testLogger())
	req := engine.AddRequest{URIs: []string{"https://example.org/v/1"}, SaveDir: t.TempDir()}
	p, err := r.Spawn(context.Background(), "ytdlp:01JB0Q7M8WQ0F1R2S3T4V5W6X7", req, 0)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Reading the first line doubles as the ready signal: it returns only once
	// the stub's echo has run, so the kill below cannot race the write.
	br := bufio.NewReader(p.Stdout)
	line, err := br.ReadString('\n')
	if err != nil || line != "line1\n" {
		t.Fatalf("first line = (%q, %v), want (%q, nil)", line, err, "line1\n")
	}
	if err := r.Cancel(p.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read stdout across process exit: %v", err)
	}
	if len(rest) != 0 {
		t.Fatalf("stdout tail = %q, want clean EOF", rest)
	}
	if err := p.Stdout.Close(); err != nil {
		t.Fatalf("close stdout: %v", err)
	}
}

func TestSpawnWithoutURIErrors(t *testing.T) {
	r := New(testConfig(t, "/nonexistent/yt-dlp"), testLogger())
	req := engine.AddRequest{SaveDir: t.TempDir()}
	if _, err := r.Spawn(context.Background(), "ytdlp:01J", req, 0); err == nil {
		t.Fatal("Spawn with no URI succeeded, want error")
	}
	if got := r.Live(); len(got) != 0 {
		t.Fatalf("Live() = %v, want empty", got)
	}
}

func TestArchivePathStripsEnginePrefix(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{ArchiveDir: dir}, testLogger())
	got := r.ArchivePath("ytdlp:01JB0Q7M8WQ0F1R2S3T4V5W6X7")
	want := filepath.Join(dir, "01JB0Q7M8WQ0F1R2S3T4V5W6X7.txt")
	if got != want {
		t.Fatalf("ArchivePath = %q, want %q", got, want)
	}
}

func TestArchivePathStaysInsideArchiveDir(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{ArchiveDir: dir}, testLogger())
	for _, id := range []string{"ytdlp:../evil", "ytdlp:nested/path", "ytdlp:", "ytdlp:.."} {
		got := r.ArchivePath(id)
		if filepath.Dir(got) != dir {
			t.Fatalf("ArchivePath(%q) = %q, want a file directly inside %q", id, got, dir)
		}
	}
}

func TestBoundedBufferKeepsTail(t *testing.T) {
	b := &boundedBuffer{limit: 8}
	if n, err := b.Write([]byte("0123456789ABCDEF")); err != nil || n != 16 {
		t.Fatalf("Write = (%d, %v), want (16, nil)", n, err)
	}
	if got := b.String(); got != "89ABCDEF" {
		t.Fatalf("buffer = %q, want tail %q", got, "89ABCDEF")
	}
	if n, err := b.Write([]byte("tail")); err != nil || n != 4 {
		t.Fatalf("Write = (%d, %v), want (4, nil)", n, err)
	}
	if got := b.String(); got != "CDEFtail" {
		t.Fatalf("buffer = %q, want tail %q", got, "CDEFtail")
	}
}

func TestSpawnRejectsEscapingFilename(t *testing.T) {
	r := New(testConfig(t, "/nonexistent/yt-dlp"), testLogger())
	for _, filename := range []string{"/tmp/x.%(ext)s", "../../x.%(ext)s", "~/x.%(ext)s", "a/../x.%(ext)s"} {
		req := engine.AddRequest{URIs: []string{"https://example.org/v/1"}, SaveDir: t.TempDir(), Filename: filename}
		if _, err := r.Spawn(context.Background(), "ytdlp:01J", req, 0); err == nil {
			t.Fatalf("Spawn with Filename %q succeeded, want rejection", filename)
		}
	}
	if got := r.Live(); len(got) != 0 {
		t.Fatalf("Live() = %v, want empty", got)
	}
}
