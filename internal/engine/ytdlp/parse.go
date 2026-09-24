// The JSON side of the yt-dlp lane (docs/06-download-engines.md §7.3, §7.4):
// the newline-delimited --progress-template stream on stdout and the
// --print-to-file info document. Upstream is explicit that nothing else is a
// stable interface: "Your program should avoid parsing the normal stdout
// since they may change in future versions. Instead, they should use options
// such as -J, --print, --progress-template, --exec to create console output
// that you can reliably reproduce and parse." Normal stdout is therefore
// never parsed: only the JSON the template emits is decoded here, and a
// yt-dlp warning printed to stdout is skipped rather than breaking the scan.

package ytdlp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/L-K-M/dl-tool/internal/engine"
)

// The three status values yt-dlp reports on a progress line; any other value
// means the line is ignored, per the upstream field table ("Check this first
// and ignore unknown values.").
const (
	progressStatusDownloading = "downloading"
	progressStatusFinished    = "finished"
	progressStatusError       = "error"
)

// progressLineMax caps one template line at 1 MiB so a hostile or malformed
// child cannot grow the scanner without bound.
const progressLineMax = 1 << 20

// Progress is one decoded --progress-template line. Every numeric field is a
// pointer because yt-dlp emits None for an unknown value, rendered as JSON
// null by the `|null` template defaults; see docs/06-download-engines.md §7.3.
// The JSON keys must match the runner's progressTemplate one-for-one.
type Progress struct {
	Status        string `json:"status"` // "downloading" | "finished" | "error"; ignore anything else
	Downloaded    *int64 `json:"downloaded"`
	Total         *int64 `json:"total"`
	Estimate      *int64 `json:"est"`
	Speed         *int64 `json:"speed"`
	ETA           *int64 `json:"eta"`
	FragmentIndex *int64 `json:"frag"`
	FragmentCount *int64 `json:"frags"`
	Filename      string `json:"file"` // always present
}

// ParseProgressLine decodes one line. ok is false for a blank line, a
// non-JSON warning line, or a status value outside the three documented ones;
// the caller then ignores the line.
func ParseProgressLine(line []byte) (p Progress, ok bool) {
	if len(bytes.TrimSpace(line)) == 0 {
		return Progress{}, false
	}
	if err := json.Unmarshal(line, &p); err != nil {
		return Progress{}, false
	}
	switch p.Status {
	case progressStatusDownloading, progressStatusFinished, progressStatusError:
		return p, true
	}
	return Progress{}, false
}

// Apply folds p into info. State follows status: downloading ->
// StateDownloading, finished -> StateCompleted, error -> StateError.
// TotalBytes falls back Total -> Estimate -> nil; it is never set to 0 as a
// guess, because live, HLS and DASH sources routinely report neither size.
func (p Progress) Apply(info *engine.TaskInfo) {
	switch p.Status {
	case progressStatusDownloading:
		info.State = engine.StateDownloading
	case progressStatusFinished:
		info.State = engine.StateCompleted
	case progressStatusError:
		info.State = engine.StateError
	}
	if p.Downloaded != nil {
		info.CompletedBytes = *p.Downloaded
	}
	info.TotalBytes = p.Total
	if info.TotalBytes == nil {
		info.TotalBytes = p.Estimate
	}
	if p.Speed != nil {
		info.DownloadRate = *p.Speed
	} else if p.Status != progressStatusDownloading {
		// Terminal lines emit speed:null; a finished or errored task is not
		// transferring, so freezing the last downloading rate would be wrong.
		// Mid-download a null keeps the last known value: int64 has no
		// "unknown" representation.
		info.DownloadRate = 0
	}
	// A null eta means "unknown", including on the terminal finished/error
	// lines, so it clears a previously known value rather than going stale.
	info.ETASeconds = p.ETA
	if p.Filename != "" {
		info.ContentPath = p.Filename
	}
}

// PercentFallback returns fragment_index/fragment_count as a 0..1 ratio when
// neither Total nor Estimate is known, and -1 when no fragment counters are
// present either.
func (p Progress) PercentFallback() float64 {
	if p.Total != nil || p.Estimate != nil {
		return -1
	}
	if p.FragmentIndex == nil || p.FragmentCount == nil || *p.FragmentIndex < 0 || *p.FragmentCount <= 0 {
		return -1
	}
	if r := float64(*p.FragmentIndex) / float64(*p.FragmentCount); r < 1 {
		return r
	}
	return 1
}

// Info is the subset of the --print-to-file document dl-tool reads.
type Info struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Filename    string `json:"filename"`
	FileSize    *int64 `json:"filesize"`
	FileSizeApx *int64 `json:"filesize_approx"`
	Duration    *int64 `json:"duration"`
	Extractor   string `json:"extractor"`
	WebpageURL  string `json:"webpage_url"`
	Timestamp   *int64 `json:"timestamp"` // Unix seconds
}

// ParseInfoDocument decodes <SaveDir>/.dl-tool-info.json.
func ParseInfoDocument(raw []byte) (Info, error) {
	var i Info
	if err := json.Unmarshal(raw, &i); err != nil {
		return Info{}, fmt.Errorf("ytdlp info document: %w", err)
	}
	return i, nil
}

// Apply fills Name, ContentPath, TotalBytes and CreatedAt from the final info
// document. TotalBytes falls back filesize -> filesize_approx -> nil.
func (i Info) Apply(info *engine.TaskInfo) {
	if i.Title != "" {
		info.Name = i.Title
	}
	if i.Filename != "" {
		info.ContentPath = i.Filename
	}
	info.TotalBytes = i.FileSize
	if info.TotalBytes == nil {
		info.TotalBytes = i.FileSizeApx
	}
	if i.Timestamp != nil {
		t := time.Unix(*i.Timestamp, 0).UTC()
		info.CreatedAt = &t
	}
}

// Outcome is the normalised result of one exited process.
type Outcome struct {
	State        engine.TaskState
	ErrorCode    string // a tasks.error_code value; "" when State is not StateError
	ErrorMessage string
	Retryable    bool
}

// ClassifyExit maps an exit code plus the captured stderr tail to an Outcome,
// per docs/06-download-engines.md §7.4:
//
//	0   -> completed
//	101 -> completed  (stopped early on purpose, e.g. --break-on-existing)
//	2   -> error, unknown            (the caller logs the full argv)
//	100 -> error, engine_unavailable (never retry)
//	1   -> error, private_video when stderr says so, otherwise unknown
//	-1  -> paused    (the process was signalled by Runner.Cancel)
//
// Only tasks.error_code values of docs/04-data-model.md §4.2 are produced.
// Retryable is false for failures a retry cannot change — engine_unavailable,
// private_video and the deterministic option error of exit 2 — and true for
// a generic or undocumented failure, which is usually transient.
func ClassifyExit(exitCode int, stderrTail string) Outcome {
	tail := strings.TrimSpace(stderrTail)
	switch exitCode {
	case 0, 101:
		return Outcome{State: engine.StateCompleted}
	case -1:
		// ExitCode() reports -1 for any signal, not only the kill sent by
		// Runner.Cancel: a foreign SIGKILL (e.g. an OOM kill) lands here too
		// and presents as paused. Cancel is the only intended signaler in
		// this deployment.
		return Outcome{State: engine.StatePaused}
	case 100:
		return Outcome{State: engine.StateError, ErrorCode: "engine_unavailable", ErrorMessage: tail}
	case 2:
		return Outcome{State: engine.StateError, ErrorCode: "unknown", ErrorMessage: tail}
	case 1:
		// Extractor phrasings differ: YouTube reports "Private video. Sign in
		// ..." while others report "This video is private".
		if low := strings.ToLower(tail); strings.Contains(low, "private video") || strings.Contains(low, "video is private") {
			return Outcome{State: engine.StateError, ErrorCode: "private_video", ErrorMessage: tail}
		}
	}
	return Outcome{State: engine.StateError, ErrorCode: "unknown", ErrorMessage: tail, Retryable: true}
}

// ScanProgress consumes a reader of newline-delimited --progress-template
// output and emits one TaskEvent per accepted line: EventProgress for
// "downloading", EventCompleted for "finished" and EventError for "error".
// A scanner failure (a line over the 1 MiB cap or a pipe read error) is
// surfaced as a final EventError carrying error_code "unknown" so it cannot
// masquerade as a clean end of stream. It closes the returned channel when
// the reader is exhausted; the caller must drain the channel until it closes,
// because abandoning it blocks the goroutine on send.
func ScanProgress(taskID string, r io.Reader) <-chan engine.TaskEvent {
	ch := make(chan engine.TaskEvent)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), progressLineMax)
		for sc.Scan() {
			p, ok := ParseProgressLine(sc.Bytes())
			if !ok {
				continue
			}
			info := &engine.TaskInfo{ID: taskID, Engine: engine.NameYtDlp}
			p.Apply(info)
			ch <- engine.TaskEvent{TaskID: taskID, Kind: progressEventKind(p.Status), Info: info}
		}
		if err := sc.Err(); err != nil {
			info := &engine.TaskInfo{
				ID:           taskID,
				Engine:       engine.NameYtDlp,
				State:        engine.StateError,
				ErrorCode:    "unknown",
				ErrorMessage: "progress stream: " + err.Error(),
			}
			ch <- engine.TaskEvent{TaskID: taskID, Kind: engine.EventError, Info: info}
		}
	}()
	return ch
}

func progressEventKind(status string) engine.EventKind {
	switch status {
	case progressStatusFinished:
		return engine.EventCompleted
	case progressStatusError:
		return engine.EventError
	default:
		return engine.EventProgress
	}
}
