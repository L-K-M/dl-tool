package aria2

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/L-K-M/dl-tool/internal/engine"
)

var update = flag.Bool("update", false, "rewrite .golden.json files from current parser output")

func strPtr(s string) *string { return &s }

// TestToState walks the state table of docs/06-download-engines.md §4.6 row
// by row. The unknown-status row lives in TestToStateUnknownStatusWarnsOnce
// so its warning can be captured instead of printed.
func TestToState(t *testing.T) {
	tests := []struct {
		name   string
		result statusResult
		want   engine.TaskState
	}{
		{"verifyIntegrityPending present beats active", statusResult{Status: statusActive, VerifyIntegrityPending: strPtr(aria2True)}, engine.StateChecking},
		{"verifiedLength present beats active", statusResult{Status: statusActive, VerifiedLength: strPtr("1024")}, engine.StateChecking},
		{"verifyIntegrityPending present on waiting", statusResult{Status: statusWaiting, VerifyIntegrityPending: strPtr(aria2True)}, engine.StateChecking},
		{"active seeder is seeding", statusResult{Status: statusActive, Seeder: strPtr(aria2True)}, engine.StateSeeding},
		{"active non-seeder is downloading", statusResult{Status: statusActive, Seeder: strPtr("false")}, engine.StateDownloading},
		{"active without seeder key is downloading", statusResult{Status: statusActive}, engine.StateDownloading},
		{"waiting is queued", statusResult{Status: statusWaiting}, engine.StateQueued},
		{"paused is paused", statusResult{Status: statusPaused}, engine.StatePaused},
		{"complete is completed", statusResult{Status: statusComplete}, engine.StateCompleted},
		{"error is error", statusResult{Status: statusError}, engine.StateError},
		{"removed is removed", statusResult{Status: statusRemoved}, engine.StateRemoved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toState(tt.result); got != tt.want {
				t.Errorf("toState(%+v) = %q, want %q", tt.result, got, tt.want)
			}
		})
	}
}

// recordHandler collects slog records so a test can count what was logged.
type recordHandler struct {
	records []slog.Record
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordHandler) warns() []slog.Record {
	var out []slog.Record
	for _, r := range h.records {
		if r.Level == slog.LevelWarn {
			out = append(out, r)
		}
	}
	return out
}

// An unknown status maps to queued and emits exactly one warning carrying
// the engine attribute (docs/06-download-engines.md §4.6, fallback row).
func TestToStateUnknownStatusWarnsOnce(t *testing.T) {
	h := &recordHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	got := toState(statusResult{GID: "abc123", Status: "bogus"})
	if got != engine.StateQueued {
		t.Fatalf("toState(unknown status) = %q, want %q", got, engine.StateQueued)
	}

	warns := h.warns()
	if len(warns) != 1 {
		t.Fatalf("toState(unknown status) emitted %d warnings, want exactly 1", len(warns))
	}

	var engineAttr string
	warns[0].Attrs(func(a slog.Attr) bool {
		if a.Key == "engine" {
			engineAttr = a.Value.String()
		}
		return true
	})
	if engineAttr != engine.NameAria2 {
		t.Errorf("warning engine attribute = %q, want %q", engineAttr, engine.NameAria2)
	}
}

// TestToErrorCode walks the errorCode table of docs/06-download-engines.md
// §4.7 row by row.
func TestToErrorCode(t *testing.T) {
	tests := []struct {
		name string
		code string
		want string
	}{
		{"absent key", "", ""},
		{"0 means success", "0", ""},
		{"1 unknown error", "1", errCodeUnknown},
		{"2 timeout", "2", errCodeTimeout},
		{"3 resource not found", "3", errCodeBrokenLink},
		{"4 resource-not-found count reached", "4", errCodeBrokenLink},
		{"5 download speed too slow", "5", errCodeTimeout},
		{"6 network problem", "6", errCodeBrokenLink},
		{"7 unfinished downloads", "7", errCodeUnknown},
		{"8 resume not supported", "8", errCodeNotSupportedType},
		{"9 not enough disk space", "9", errCodeDiskFull},
		{"10 piece length differed", "10", errCodeUnknown},
		{"11 same file downloading", "11", errCodeTorrentDuplicate},
		{"12 same infohash downloading", "12", errCodeTorrentDuplicate},
		{"13 is the first code past the documented range", "13", errCodeUnknown},
		{"codes keep going up", "32", errCodeUnknown},
		{"unparsable is still an error", "abc", errCodeUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toErrorCode(tt.code); got != tt.want {
				t.Errorf("toErrorCode(%q) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

// toFileEntries subtracts one from aria2's 1-based index and leaves Priority
// nil, because aria2 has no numeric per-file priority
// (docs/06-download-engines.md §1.1).
func TestToFileEntries(t *testing.T) {
	got := toFileEntries([]fileEntry{
		{Index: "1", Path: "/data/a.iso", Length: "100", CompletedLength: "40", Selected: aria2True},
		{Index: "3", Path: "/data/b.iso", Length: "200", CompletedLength: "0", Selected: "false"},
	})
	want := []engine.FileEntry{
		{Index: 0, Path: "/data/a.iso", Size: 100, Completed: 40, Selected: true, Priority: nil},
		{Index: 2, Path: "/data/b.iso", Size: 200, Completed: 0, Selected: false, Priority: nil},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("toFileEntries mismatch (-want +got):\n%s", diff)
	}
	for _, e := range got {
		if e.Priority != nil {
			t.Errorf("file %d Priority = %v, want nil", e.Index, *e.Priority)
		}
	}

	if got := toFileEntries(nil); got != nil {
		t.Errorf("toFileEntries(nil) = %v, want nil", got)
	}
}

// TestToTaskInfo pins the field-level rules of docs/06-download-engines.md
// §4.4 that the golden file cannot express: pointer absence, the pending
// total, infohash case and content-path selection.
func TestToTaskInfo(t *testing.T) {
	t.Run("id is engine-namespaced", func(t *testing.T) {
		info := toTaskInfo(statusResult{GID: "2089b05ecca3d829", Status: statusWaiting})
		if info.ID != "aria2:2089b05ecca3d829" {
			t.Errorf("ID = %q, want %q", info.ID, "aria2:2089b05ecca3d829")
		}
		if info.Engine != engine.NameAria2 {
			t.Errorf("Engine = %q, want %q", info.Engine, engine.NameAria2)
		}
	})

	t.Run("total is unknown while 0 or absent", func(t *testing.T) {
		for _, total := range []string{"", "0"} {
			info := toTaskInfo(statusResult{Status: statusWaiting, TotalLength: total})
			if info.TotalBytes != nil {
				t.Errorf("TotalBytes for totalLength %q = %v, want nil", total, *info.TotalBytes)
			}
		}
	})

	t.Run("infohash is lowercased and v2 stays empty", func(t *testing.T) {
		info := toTaskInfo(statusResult{
			Status:   statusActive,
			InfoHash: strPtr("0123456789ABCDEF0123456789ABCDEF01234567"),
		})
		if want := "0123456789abcdef0123456789abcdef01234567"; info.InfohashV1 != want {
			t.Errorf("InfohashV1 = %q, want %q", info.InfohashV1, want)
		}
		// aria2 has no BEP 52 support, so InfohashV2 is always empty.
		if info.InfohashV2 != "" {
			t.Errorf("InfohashV2 = %q, want empty", info.InfohashV2)
		}
	})

	t.Run("seeds and peers come from numSeeders and connections", func(t *testing.T) {
		info := toTaskInfo(statusResult{Status: statusActive, NumSeeders: strPtr("7"), Connections: strPtr("12")})
		if info.NumSeeds == nil || *info.NumSeeds != 7 {
			t.Errorf("NumSeeds = %v, want 7", info.NumSeeds)
		}
		if info.NumPeers == nil || *info.NumPeers != 12 {
			t.Errorf("NumPeers = %v, want 12", info.NumPeers)
		}

		absent := toTaskInfo(statusResult{Status: statusActive})
		if absent.NumSeeds != nil || absent.NumPeers != nil {
			t.Errorf("absent keys: NumSeeds = %v, NumPeers = %v, want both nil", absent.NumSeeds, absent.NumPeers)
		}
	})

	t.Run("content path is the first selected file", func(t *testing.T) {
		info := toTaskInfo(statusResult{
			Status: statusActive,
			Files: []fileEntry{
				{Index: "1", Path: "/data/skip.bin", Selected: "false"},
				{Index: "2", Path: "/data/want.bin", Selected: aria2True},
			},
		})
		if info.ContentPath != "/data/want.bin" {
			t.Errorf("ContentPath = %q, want %q", info.ContentPath, "/data/want.bin")
		}

		none := toTaskInfo(statusResult{Status: statusActive})
		if none.ContentPath != "" {
			t.Errorf("ContentPath without files = %q, want empty", none.ContentPath)
		}
	})

	t.Run("error fields come from the errorCode and errorMessage keys", func(t *testing.T) {
		info := toTaskInfo(statusResult{
			Status:       statusError,
			ErrorCode:    strPtr("9"),
			ErrorMessage: strPtr("No space left on device"),
		})
		if info.ErrorCode != errCodeDiskFull {
			t.Errorf("ErrorCode = %q, want %q", info.ErrorCode, errCodeDiskFull)
		}
		if info.ErrorMessage != "No space left on device" {
			t.Errorf("ErrorMessage = %q", info.ErrorMessage)
		}

		ok := toTaskInfo(statusResult{Status: statusComplete, ErrorCode: strPtr("0"), ErrorMessage: strPtr("")})
		if ok.ErrorCode != "" {
			t.Errorf("ErrorCode for code 0 = %q, want empty", ok.ErrorCode)
		}
	})
}

// rpcResponse is the JSON-RPC envelope around one tellStatus result; the
// committed fixture is the raw daemon response, envelope included.
type rpcResponse struct {
	Result statusResult `json:"result"`
}

// TestToTaskInfoGolden decodes the recorded aria2 1.37.0 response and diffs
// the normalised TaskInfo against the committed golden file
// (docs/13-testing-and-verification.md §5).
func TestToTaskInfoGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/aria2_tellstatus_1.37.0.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var resp rpcResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	got := toTaskInfo(resp.Result)

	golden := "testdata/aria2_tellstatus_1.37.0.golden.json"
	if *update {
		writeGolden(t, golden, got)
	}
	want := loadGolden(t, golden)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("mismatch (-want +got):\n%s", diff)
	}
}

func writeGolden(t *testing.T, path string, info engine.TaskInfo) {
	t.Helper()
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write golden: %v", err)
	}
}

func loadGolden(t *testing.T, path string) engine.TaskInfo {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var info engine.TaskInfo
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	return info
}
