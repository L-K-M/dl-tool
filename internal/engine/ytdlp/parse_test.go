package ytdlp

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/L-K-M/dl-tool/internal/engine"
)

func int64ptr(v int64) *int64 { return &v }

func TestParseProgressLineDecodesEveryField(t *testing.T) {
	line := `{"status":"downloading","downloaded":512,"total":2048,"est":1536,"speed":100,"eta":15,"frag":2,"frags":8,"file":"video.mp4"}`
	p, ok := ParseProgressLine([]byte(line))
	if !ok {
		t.Fatalf("ParseProgressLine ok = false for %s", line)
	}
	want := Progress{
		Status:        "downloading",
		Downloaded:    int64ptr(512),
		Total:         int64ptr(2048),
		Estimate:      int64ptr(1536),
		Speed:         int64ptr(100),
		ETA:           int64ptr(15),
		FragmentIndex: int64ptr(2),
		FragmentCount: int64ptr(8),
		Filename:      "video.mp4",
	}
	if diff := cmp.Diff(want, p); diff != "" {
		t.Fatalf("Progress mismatch (-want +got):\n%s", diff)
	}
}

// The acceptance chain of docs/06-download-engines.md §7.3: TotalBytes falls
// back total -> est -> nil and is never set to 0 as a guess; fragments are the
// percentage fallback for live, HLS and DASH sources that report neither size.
func TestProgressTotalFallbackChain(t *testing.T) {
	for _, tc := range []struct {
		name      string
		line      string
		wantTotal *int64
		wantPct   float64
	}{
		{
			name:      "exact total wins over estimate",
			line:      `{"status":"downloading","downloaded":100,"total":400,"est":300,"speed":10,"eta":30,"frag":1,"frags":4,"file":"a.mp4"}`,
			wantTotal: int64ptr(400),
			wantPct:   -1,
		},
		{
			name:      "null total falls back to estimate",
			line:      `{"status":"downloading","downloaded":100,"total":null,"est":300,"speed":null,"eta":null,"frag":null,"frags":null,"file":"a.mp4"}`,
			wantTotal: int64ptr(300),
			wantPct:   -1,
		},
		{
			name:      "both null leaves TotalBytes nil and yields the fragment ratio",
			line:      `{"status":"downloading","downloaded":100,"total":null,"est":null,"speed":null,"eta":null,"frag":1,"frags":4,"file":"a.m3u8"}`,
			wantTotal: nil,
			wantPct:   0.25,
		},
		{
			name:      "no size and no fragments yields nil and -1",
			line:      `{"status":"downloading","downloaded":100,"total":null,"est":null,"speed":null,"eta":null,"frag":null,"frags":null,"file":"live.mp4"}`,
			wantTotal: nil,
			wantPct:   -1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := ParseProgressLine([]byte(tc.line))
			if !ok {
				t.Fatalf("ParseProgressLine ok = false for %s", tc.line)
			}
			var info engine.TaskInfo
			p.Apply(&info)
			if diff := cmp.Diff(tc.wantTotal, info.TotalBytes); diff != "" {
				t.Fatalf("TotalBytes mismatch (-want +got):\n%s", diff)
			}
			if info.TotalBytes != nil && *info.TotalBytes == 0 {
				t.Fatal("TotalBytes was set to 0 as a guess")
			}
			if got := p.PercentFallback(); got != tc.wantPct {
				t.Fatalf("PercentFallback = %v, want %v", got, tc.wantPct)
			}
		})
	}
}

func TestPercentFallbackRejectsDegenerateCounters(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Progress
	}{
		{"zero count", Progress{Status: "downloading", FragmentIndex: int64ptr(1), FragmentCount: int64ptr(0)}},
		{"negative index", Progress{Status: "downloading", FragmentIndex: int64ptr(-1), FragmentCount: int64ptr(4)}},
		{"index only", Progress{Status: "downloading", FragmentIndex: int64ptr(1)}},
		{"count only", Progress{Status: "downloading", FragmentCount: int64ptr(4)}},
	} {
		if got := tc.p.PercentFallback(); got != -1 {
			t.Fatalf("%s: PercentFallback = %v, want -1", tc.name, got)
		}
	}
	if got := (Progress{Status: "downloading", FragmentIndex: int64ptr(4), FragmentCount: int64ptr(4)}).PercentFallback(); got != 1 {
		t.Fatalf("PercentFallback at the last fragment = %v, want 1", got)
	}
}

func TestProgressApplyMapsStatusAndFields(t *testing.T) {
	for _, tc := range []struct {
		status    string
		wantState engine.TaskState
	}{
		{"downloading", engine.StateDownloading},
		{"finished", engine.StateCompleted},
		{"error", engine.StateError},
	} {
		var info engine.TaskInfo
		(Progress{Status: tc.status}).Apply(&info)
		if info.State != tc.wantState {
			t.Fatalf("status %q -> State %q, want %q", tc.status, info.State, tc.wantState)
		}
	}

	var info engine.TaskInfo
	(Progress{
		Status:     "downloading",
		Downloaded: int64ptr(512),
		Speed:      int64ptr(100),
		ETA:        int64ptr(15),
		Filename:   "video.mp4",
	}).Apply(&info)
	if info.CompletedBytes != 512 || info.DownloadRate != 100 {
		t.Fatalf("Apply bytes/rate = (%d, %d), want (512, 100)", info.CompletedBytes, info.DownloadRate)
	}
	if info.ETASeconds == nil || *info.ETASeconds != 15 {
		t.Fatalf("ETASeconds = %v, want 15", info.ETASeconds)
	}
	if info.ContentPath != "video.mp4" {
		t.Fatalf("ContentPath = %q, want %q", info.ContentPath, "video.mp4")
	}

	// A terminal line reports eta:null — "unknown" — so it clears the ETA a
	// downloading line left behind instead of letting it go stale.
	(Progress{Status: "finished", Downloaded: int64ptr(2048), Total: int64ptr(2048), Filename: "video.mp4"}).Apply(&info)
	if info.ETASeconds != nil {
		t.Fatalf("ETASeconds = %v after a finished line with null eta, want nil", *info.ETASeconds)
	}
}

// yt-dlp prints warnings to stdout even on success; they must be skipped,
// not parsed (docs/06-download-engines.md §7.3).
func TestProgressSkipsWarningLine(t *testing.T) {
	for _, line := range []string{
		"WARNING: [youtube] Falling back on generic information extractor",
		"ERROR: unable to download video data: HTTP Error 403: Forbidden",
		"[download]   4.2% of ~10.00MiB at 100.00KiB/s ETA 00:15",
		"",
		"   ",
		"NA",
	} {
		if p, ok := ParseProgressLine([]byte(line)); ok {
			t.Fatalf("ParseProgressLine(%q) = (%+v, true), want ok=false", line, p)
		}
	}
}

func TestProgressSkipsUnknownStatus(t *testing.T) {
	for _, status := range []string{"postprocess", "alive", "NA", "", "Downloading"} {
		line := `{"status":"` + status + `","downloaded":0,"total":null,"est":null,"speed":null,"eta":null,"frag":null,"frags":null,"file":"a.mp4"}`
		if p, ok := ParseProgressLine([]byte(line)); ok {
			t.Fatalf("ParseProgressLine status %q = (%+v, true), want ok=false", status, p)
		}
	}
}

// A scan interleaved with a warning line, a blank line and an unknown-status
// line still emits one event per accepted line and closes at EOF.
func TestScanProgressEmitsEventsAndSkipsNoise(t *testing.T) {
	stream := strings.Join([]string{
		`{"status":"downloading","downloaded":100,"total":400,"est":null,"speed":10,"eta":30,"frag":null,"frags":null,"file":"a.mp4"}`,
		`WARNING: [youtube] unable to extract nsig deciphering`,
		``,
		`{"status":"bogey","downloaded":0,"total":null,"est":null,"speed":null,"eta":null,"frag":null,"frags":null,"file":"a.mp4"}`,
		`{"status":"error","downloaded":100,"total":null,"est":null,"speed":null,"eta":null,"frag":null,"frags":null,"file":"a.mp4"}`,
		`{"status":"finished","downloaded":400,"total":400,"est":null,"speed":null,"eta":null,"frag":null,"frags":null,"file":"a.mp4"}`,
	}, "\n") + "\n"

	var events []engine.TaskEvent
	for ev := range ScanProgress("ytdlp:01JTEST", strings.NewReader(stream)) {
		events = append(events, ev)
	}
	wantKinds := []engine.EventKind{engine.EventProgress, engine.EventError, engine.EventCompleted}
	var gotKinds []engine.EventKind
	for _, ev := range events {
		gotKinds = append(gotKinds, ev.Kind)
	}
	if diff := cmp.Diff(wantKinds, gotKinds); diff != "" {
		t.Fatalf("event kinds mismatch (-want +got):\n%s", diff)
	}
	for i, ev := range events {
		if ev.TaskID != "ytdlp:01JTEST" || ev.Info == nil || ev.Info.ID != "ytdlp:01JTEST" {
			t.Fatalf("event %d carries task %q info %+v, want ytdlp:01JTEST", i, ev.TaskID, ev.Info)
		}
	}
	if events[0].Info.State != engine.StateDownloading || events[0].Info.CompletedBytes != 100 {
		t.Fatalf("progress event info = %+v, want downloading at 100 bytes", events[0].Info)
	}
	if events[1].Info.State != engine.StateError {
		t.Fatalf("error event info = %+v, want error state", events[1].Info)
	}
	if events[2].Info.State != engine.StateCompleted || events[2].Info.TotalBytes == nil || *events[2].Info.TotalBytes != 400 {
		t.Fatalf("finished event info = %+v, want completed with 400 total bytes", events[2].Info)
	}
}

// A template line can exceed bufio's 64 KiB default token size when the
// filename is long; the 1 MiB buffer must let it through.
func TestScanProgressAcceptsLongLine(t *testing.T) {
	long := strings.Repeat("x", 512*1024)
	line := `{"status":"downloading","downloaded":1,"total":2,"est":null,"speed":null,"eta":null,"frag":null,"frags":null,"file":"` + long + `"}`
	var n int
	for range ScanProgress("ytdlp:01J", strings.NewReader(line+"\n")) {
		n++
	}
	if n != 1 {
		t.Fatalf("events = %d, want 1 for a >64 KiB line", n)
	}
}

// A line over the 1 MiB cap is a scanner failure, not a clean EOF: the scan
// must surface one EventError instead of silently truncating the rest of the
// stream.
func TestScanProgressReportsScannerError(t *testing.T) {
	oversized := strings.Repeat("x", progressLineMax)
	valid := `{"status":"finished","downloaded":2,"total":2,"est":null,"speed":null,"eta":null,"frag":null,"frags":null,"file":"a.mp4"}`
	stream := oversized + "\n" + valid + "\n"
	var events []engine.TaskEvent
	for ev := range ScanProgress("ytdlp:01J", strings.NewReader(stream)) {
		events = append(events, ev)
	}
	if len(events) != 1 || events[0].Kind != engine.EventError {
		t.Fatalf("events = %+v, want a single EventError", events)
	}
	if events[0].Info == nil || events[0].Info.State != engine.StateError || events[0].Info.ErrorCode != "unknown" {
		t.Fatalf("scanner-error info = %+v, want error state with code unknown", events[0].Info)
	}
}

func TestParseInfoDocument(t *testing.T) {
	raw := `{"id":"abc123","title":"Some Video","filename":"/data/media/Some Video [abc123].mp4","filesize":12345,"filesize_approx":null,"duration":61,"extractor":"youtube","webpage_url":"https://example.org/watch?v=abc123","timestamp":1758000000}`
	i, err := ParseInfoDocument([]byte(raw))
	if err != nil {
		t.Fatalf("ParseInfoDocument: %v", err)
	}
	want := Info{
		ID:         "abc123",
		Title:      "Some Video",
		Filename:   "/data/media/Some Video [abc123].mp4",
		FileSize:   int64ptr(12345),
		Duration:   int64ptr(61),
		Extractor:  "youtube",
		WebpageURL: "https://example.org/watch?v=abc123",
		Timestamp:  int64ptr(1758000000),
	}
	if diff := cmp.Diff(want, i); diff != "" {
		t.Fatalf("Info mismatch (-want +got):\n%s", diff)
	}
	if _, err := ParseInfoDocument([]byte("not json")); err == nil {
		t.Fatal("ParseInfoDocument of invalid JSON returned nil error")
	}

	// A guess-only document must decode filesize_approx through the JSON tag,
	// not just through a hand-built struct.
	rawGuess := `{"id":"abc123","title":"Some Video","filename":"/data/media/g.mp4","filesize":null,"filesize_approx":999,"duration":61,"extractor":"youtube","webpage_url":"https://example.org/watch?v=abc123","timestamp":1758000000}`
	guess, err := ParseInfoDocument([]byte(rawGuess))
	if err != nil {
		t.Fatalf("ParseInfoDocument guess: %v", err)
	}
	if guess.FileSizeApx == nil || *guess.FileSizeApx != 999 {
		t.Fatalf("FileSizeApx = %v, want 999 decoded from filesize_approx", guess.FileSizeApx)
	}
}

func TestInfoDocumentFillsContentPath(t *testing.T) {
	var info engine.TaskInfo
	Info{
		Title:     "Some Video",
		Filename:  "/data/media/Some Video [abc123].mp4",
		FileSize:  int64ptr(12345),
		Timestamp: int64ptr(1758000000),
	}.Apply(&info)
	if info.Name != "Some Video" {
		t.Fatalf("Name = %q, want %q", info.Name, "Some Video")
	}
	if info.ContentPath != "/data/media/Some Video [abc123].mp4" {
		t.Fatalf("ContentPath = %q", info.ContentPath)
	}
	if info.TotalBytes == nil || *info.TotalBytes != 12345 {
		t.Fatalf("TotalBytes = %v, want 12345", info.TotalBytes)
	}
	if info.CreatedAt == nil || !info.CreatedAt.Equal(time.Unix(1758000000, 0).UTC()) {
		t.Fatalf("CreatedAt = %v, want %v", info.CreatedAt, time.Unix(1758000000, 0).UTC())
	}
}

// filesize is null on a guess-only document: filesize_approx fills
// TotalBytes, and both absent leaves it nil rather than guessing 0.
func TestInfoDocumentSizeFallback(t *testing.T) {
	var approx engine.TaskInfo
	Info{FileSizeApx: int64ptr(999)}.Apply(&approx)
	if approx.TotalBytes == nil || *approx.TotalBytes != 999 {
		t.Fatalf("TotalBytes = %v, want 999 from filesize_approx", approx.TotalBytes)
	}
	var none engine.TaskInfo
	Info{}.Apply(&none)
	if none.TotalBytes != nil {
		t.Fatalf("TotalBytes = %v, want nil when both sizes are absent", *none.TotalBytes)
	}
}

// docs/06-download-engines.md §7.4, plus -1 for a process signalled by
// Runner.Cancel.
func TestClassifyExitTable(t *testing.T) {
	privateTail := "ERROR: [youtube] abc123: Private video. Sign in if you've been granted access to this video"
	for _, tc := range []struct {
		name      string
		code      int
		stderr    string
		wantState engine.TaskState
		wantCode  string
		wantRetry bool
	}{
		{"success", 0, "", engine.StateCompleted, "", false},
		{"stopped early on purpose", 101, "", engine.StateCompleted, "", false},
		{"option error", 2, "usage: yt-dlp [OPTIONS] URL [URL...]", engine.StateError, "unknown", false},
		{"restart for update", 100, "", engine.StateError, "engine_unavailable", false},
		{"private video", 1, privateTail, engine.StateError, "private_video", false},
		{"private video, other extractor phrasing", 1, "ERROR: This video is private.", engine.StateError, "private_video", false},
		{"generic error", 1, "ERROR: unable to download video data: HTTP Error 403: Forbidden", engine.StateError, "unknown", true},
		{"signalled by Cancel", -1, "", engine.StatePaused, "", false},
		{"undocumented code", 42, "", engine.StateError, "unknown", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyExit(tc.code, tc.stderr)
			if got.State != tc.wantState || got.ErrorCode != tc.wantCode || got.Retryable != tc.wantRetry {
				t.Fatalf("ClassifyExit(%d, %q) = %+v, want state=%s code=%q retryable=%v",
					tc.code, tc.stderr, got, tc.wantState, tc.wantCode, tc.wantRetry)
			}
			if got.State == engine.StateError && tc.stderr != "" && got.ErrorMessage != strings.TrimSpace(tc.stderr) {
				t.Fatalf("ErrorMessage = %q, want the stderr tail %q", got.ErrorMessage, tc.stderr)
			}
		})
	}
}

// Every ErrorCode the mapper can produce must be a tasks.error_code value of
// docs/04-data-model.md §4.2: sweeping every exit code and a hostile stderr
// tail proves the produced set is exactly {unknown, engine_unavailable,
// private_video}, all three of which are in the enum.
func TestClassifyExitProducesOnlyDocumentedErrorCodes(t *testing.T) {
	produced := map[string]bool{}
	for code := -128; code <= 255; code++ {
		for _, tail := range []string{"", "ERROR: Private video. Sign in", "ERROR: boom"} {
			got := ClassifyExit(code, tail)
			if got.State != engine.StateError && got.ErrorCode != "" {
				t.Fatalf("ClassifyExit(%d, %q) set ErrorCode %q on non-error state %s", code, tail, got.ErrorCode, got.State)
			}
			if got.State == engine.StateError {
				produced[got.ErrorCode] = true
			}
		}
	}
	for _, documented := range []string{"unknown", "engine_unavailable", "private_video"} {
		if !produced[documented] {
			t.Fatalf("documented code %q never produced", documented)
		}
		delete(produced, documented)
	}
	for code := range produced {
		t.Fatalf("produced undocumented error_code %q", code)
	}
}
