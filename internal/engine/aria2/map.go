// Package aria2 decodes the aria2 JSON-RPC wire format into engine.TaskInfo,
// engine.TaskState and engine.FileEntry. Parsing is pure: no network, no
// daemon. The tables below are docs/06-download-engines.md sections 4.4, 4.6
// and 4.7 reproduced row for row.
package aria2

import (
	"log/slog"
	"strconv"
	"strings"

	"github.com/L-K-M/dl-tool/internal/engine"
)

// statusResult is one aria2.tellStatus result. Scalar values arrive as JSON strings, including the
// numeric ones, so every number is parsed. files, uris and followedBy are arrays; bitfield,
// followedBy, belongsTo, verifiedLength and verifyIntegrityPending are conditionally absent.
type statusResult struct {
	GID                    string      `json:"gid"`
	Status                 string      `json:"status"`
	TotalLength            string      `json:"totalLength"`
	CompletedLength        string      `json:"completedLength"`
	UploadLength           string      `json:"uploadLength"`
	DownloadSpeed          string      `json:"downloadSpeed"`
	UploadSpeed            string      `json:"uploadSpeed"`
	Dir                    string      `json:"dir"`
	Files                  []fileEntry `json:"files"`
	ErrorCode              *string     `json:"errorCode"`
	ErrorMessage           *string     `json:"errorMessage"`
	InfoHash               *string     `json:"infoHash"`
	NumSeeders             *string     `json:"numSeeders"`
	Seeder                 *string     `json:"seeder"`
	Connections            *string     `json:"connections"`
	FollowedBy             []string    `json:"followedBy"`
	VerifiedLength         *string     `json:"verifiedLength"`
	VerifyIntegrityPending *string     `json:"verifyIntegrityPending"`
}

// fileEntry is one aria2.getFiles element. Index starts at 1 in aria2 and is exposed 0-based.
type fileEntry struct {
	Index           string `json:"index"`
	Path            string `json:"path"`
	Length          string `json:"length"`
	CompletedLength string `json:"completedLength"`
	Selected        string `json:"selected"`
}

// aria2 tellStatus status values (docs/06-download-engines.md §4.6).
const (
	statusActive   = "active"
	statusWaiting  = "waiting"
	statusPaused   = "paused"
	statusError    = "error"
	statusComplete = "complete"
	statusRemoved  = "removed"
)

// aria2True is aria2's string encoding of boolean true ("true"), used by
// seeder, selected and the other boolean-ish wire fields.
const aria2True = "true"

// tasks.error_code values the aria2 mapping can produce
// (docs/06-download-engines.md §4.7).
const (
	errCodeBrokenLink       = "broken_link"
	errCodeTimeout          = "timeout"
	errCodeNotSupportedType = "not_supported_type"
	errCodeDiskFull         = "disk_full"
	errCodeTorrentDuplicate = "torrent_duplicate"
	errCodeUnknown          = "unknown"
)

// parseInt64 decodes one numeric scalar: aria2 emits every scalar as a JSON
// string, digits included. An unparsable value is bad data, not a failure of
// the mapping: it becomes 0 and is logged at debug.
func parseInt64(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		slog.Debug("aria2: unparsable numeric scalar", "engine", engine.NameAria2, "value", s, "error", err)
		return 0
	}
	return n
}

// toState applies the table of docs/06-download-engines.md §4.6. The checking row is evaluated
// first because it is a key-presence test, not a status value. An unknown status returns
// engine.StateQueued and logs one warning.
func toState(r statusResult) engine.TaskState {
	switch {
	case r.VerifyIntegrityPending != nil || r.VerifiedLength != nil:
		return engine.StateChecking
	case r.Status == statusActive && r.Seeder != nil && *r.Seeder == aria2True:
		return engine.StateSeeding
	}

	switch r.Status {
	case statusActive:
		return engine.StateDownloading
	case statusWaiting:
		return engine.StateQueued
	case statusPaused:
		return engine.StatePaused
	case statusComplete:
		return engine.StateCompleted
	case statusError:
		return engine.StateError
	case statusRemoved:
		return engine.StateRemoved
	default:
		slog.Warn("aria2: unknown tellStatus status, treating as queued",
			"engine", engine.NameAria2, "gid", r.GID, "status", r.Status)
		return engine.StateQueued
	}
}

// toErrorCode maps an aria2 errorCode string to a tasks.error_code value. "0" and "" return "".
func toErrorCode(code string) string {
	if code == "" || code == "0" {
		return ""
	}

	switch n := parseInt64(code); n {
	case 3, 4, 6:
		return errCodeBrokenLink
	case 2, 5:
		return errCodeTimeout
	case 8:
		return errCodeNotSupportedType
	case 9:
		return errCodeDiskFull
	case 11, 12:
		return errCodeTorrentDuplicate
	default:
		// 1, 7, 10 and every code from 13 upward; an unparsable code parsed
		// to 0 and lands here too, which is the honest answer for a value
		// the table does not know.
		return errCodeUnknown
	}
}

// toTaskInfo normalises one tellStatus result. It never returns an error: an unparsable numeric
// scalar becomes 0 and is logged at debug.
func toTaskInfo(r statusResult) engine.TaskInfo {
	info := engine.TaskInfo{
		ID:             engine.NameAria2 + ":" + r.GID,
		Engine:         engine.NameAria2,
		State:          toState(r),
		TotalBytes:     toTotalBytes(r.TotalLength),
		CompletedBytes: parseInt64(r.CompletedLength),
		UploadedBytes:  parseInt64(r.UploadLength),
		DownloadRate:   parseInt64(r.DownloadSpeed),
		UploadRate:     parseInt64(r.UploadSpeed),
		SaveDir:        r.Dir,
		ContentPath:    firstSelectedPath(r.Files),
		// InfohashV2 stays empty: aria2 has no BEP 52 support.
	}

	if r.ErrorCode != nil {
		info.ErrorCode = toErrorCode(*r.ErrorCode)
	}
	if r.ErrorMessage != nil {
		info.ErrorMessage = *r.ErrorMessage
	}
	if r.InfoHash != nil {
		info.InfohashV1 = strings.ToLower(*r.InfoHash)
	}
	if r.NumSeeders != nil {
		n := int(parseInt64(*r.NumSeeders))
		info.NumSeeds = &n
	}
	if r.Connections != nil {
		n := int(parseInt64(*r.Connections))
		info.NumPeers = &n
	}

	return info
}

// toTotalBytes parses totalLength, treating an absent key or a "0" as
// unknown rather than as a zero-byte download: aria2 reports "0" while the
// metadata that carries the size is still pending
// (docs/06-download-engines.md §4.4).
func toTotalBytes(s string) *int64 {
	if s == "" {
		return nil
	}
	n := parseInt64(s)
	if n == 0 {
		return nil
	}
	return &n
}

// firstSelectedPath returns the path of the first selected file, which
// dl-tool surfaces as the task's content path, or "" when no selected file
// is known yet.
func firstSelectedPath(fs []fileEntry) string {
	for _, f := range fs {
		if f.Selected == aria2True {
			return f.Path
		}
	}
	return ""
}

// toFileEntries converts getFiles elements, subtracting one from every aria2 index and leaving
// Priority nil, because aria2 has no numeric per-file priority.
func toFileEntries(fs []fileEntry) []engine.FileEntry {
	if len(fs) == 0 {
		return nil
	}

	entries := make([]engine.FileEntry, 0, len(fs))
	for _, f := range fs {
		entries = append(entries, engine.FileEntry{
			Index:     int(parseInt64(f.Index)) - 1,
			Path:      f.Path,
			Size:      parseInt64(f.Length),
			Completed: parseInt64(f.CompletedLength),
			Selected:  f.Selected == aria2True,
			Priority:  nil,
		})
	}
	return entries
}
