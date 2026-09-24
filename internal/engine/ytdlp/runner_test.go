package ytdlp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
