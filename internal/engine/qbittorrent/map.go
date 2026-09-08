package qbittorrent

import (
	"log/slog"
	"strings"
	"time"

	"github.com/L-K-M/dl-tool/internal/engine"
)

// torrentJSON is one element of torrents/info and one value of the
// sync/maindata torrents object (docs/06-download-engines.md sections 5.5
// and 5.4). Field names are the daemon's own; two wiki errors are corrected
// here: the tri-state key is `private`, not `isPrivate`, and
// `infohash_v1`/`infohash_v2` exist in 5.2.3.
type torrentJSON struct {
	Hash          string  `json:"hash"`
	InfohashV1    string  `json:"infohash_v1"`
	InfohashV2    string  `json:"infohash_v2"`
	HasMetadata   bool    `json:"has_metadata"`
	Name          string  `json:"name"`
	State         string  `json:"state"`
	Progress      float64 `json:"progress"`
	Size          int64   `json:"size"`       // selected files only
	TotalSize     int64   `json:"total_size"` // including unselected
	Completed     int64   `json:"completed"`
	Uploaded      int64   `json:"uploaded"`
	DlSpeed       int64   `json:"dlspeed"`
	UpSpeed       int64   `json:"upspeed"`
	ETA           int64   `json:"eta"`
	Ratio         float64 `json:"ratio"`
	SavePath      string  `json:"save_path"`
	ContentPath   string  `json:"content_path"`
	Category      string  `json:"category"`
	Tags          string  `json:"tags"` // comma-concatenated
	NumSeeds      int     `json:"num_seeds"`
	NumComplete   int     `json:"num_complete"`
	NumLeechs     int     `json:"num_leechs"`
	NumIncomplete int     `json:"num_incomplete"`
	AddedOn       int64   `json:"added_on"`
	CompletionOn  int64   `json:"completion_on"`
	DlLimit       int64   `json:"dl_limit"`
	UpLimit       int64   `json:"up_limit"`
	SeqDl         bool    `json:"seq_dl"`
	AutoTMM       bool    `json:"auto_tmm"`
	Private       *bool   `json:"private"` // tri-state: null until metadata arrives
}

// The 5.2.3 serialiser emits exactly these state strings
// (docs/06-download-engines.md section 5.6); pausedDL, pausedUP and
// allocating come from 4.x and the stale wiki, and stay for compatibility.
const (
	stateError             = "error"
	stateMissingFiles      = "missingFiles"
	stateUploading         = "uploading"
	stateForcedUP          = "forcedOP"
	stateStalledUP         = "stalledUP"
	stateStoppedUP         = "stoppedUP"
	statePausedUP          = "pausedUP"
	stateQueuedUP          = "queuedUP"
	stateCheckingUP        = "checkingUP"
	stateCheckingResume    = "checkingResumeData"
	stateCheckingDL        = "checkingDL"
	stateMoving            = "moving"
	stateDownloading       = "downloading"
	stateForcedDL          = "forcedDL"
	stateMetaDL            = "metaDL"
	stateForcedMetaDL      = "forcedMetaDL"
	stateStalledDL         = "stalledDL"
	stateStoppedDL         = "stoppedDL"
	statePausedDL          = "pausedDL"
	stateQueuedDL          = "queuedDL"
	stateAllocating        = "allocating"
	progressComplete       = 1.0
	errCodeUnknownTorrents = "unknown"
)

// normaliseState maps a qBittorrent state onto the canonical TaskState of
// docs/06-download-engines.md section 5.6. progress is needed because
// pausedUP/stoppedUP is completed at progress == 1 and paused otherwise. An
// unrecognised state — `unknown` included — returns engine.StateQueued and
// logs one warning; it never returns an error and never panics.
func normaliseState(state string, progress float64) engine.TaskState {
	switch state {
	case stateDownloading, stateMetaDL, stateForcedDL, stateForcedMetaDL, stateStalledDL:
		return engine.StateDownloading

	case stateUploading, stateForcedUP, stateStalledUP:
		return engine.StateSeeding

	case stateQueuedDL, stateQueuedUP, stateAllocating:
		return engine.StateQueued

	case statePausedDL, stateStoppedDL:
		return engine.StatePaused

	case statePausedUP, stateStoppedUP:
		if progress >= progressComplete {
			return engine.StateCompleted
		}
		return engine.StatePaused

	case stateCheckingDL, stateCheckingUP, stateCheckingResume, stateMoving:
		return engine.StateChecking

	case stateError, stateMissingFiles:
		return engine.StateError

	default:
		slog.Warn("qbittorrent: unknown torrent state, treating as queued",
			"engine", engine.NameQBittorrent, "state", state)
		return engine.StateQueued
	}
}

// toTaskInfo projects one torrentJSON onto engine.TaskInfo. ID is
// "qbittorrent:" + Hash, TotalBytes is nil while HasMetadata is false, and
// InfohashV1/InfohashV2 come from the infohash_v1/infohash_v2 keys, never
// from Hash.
func toTaskInfo(t torrentJSON) engine.TaskInfo {
	info := engine.TaskInfo{
		ID:             engine.NameQBittorrent + ":" + t.Hash,
		Engine:         engine.NameQBittorrent,
		Name:           t.Name,
		State:          normaliseState(t.State, t.Progress),
		CompletedBytes: t.Completed,
		UploadedBytes:  t.Uploaded,
		DownloadRate:   t.DlSpeed,
		UploadRate:     t.UpSpeed,
		ETASeconds:     etaSeconds(t.ETA),
		SaveDir:        t.SavePath,
		ContentPath:    t.ContentPath,
		InfohashV1:     strings.ToLower(t.InfohashV1),
		InfohashV2:     strings.ToLower(t.InfohashV2),
		NumSeeds:       &t.NumSeeds,
		NumPeers:       &t.NumLeechs,
		Ratio:          &t.Ratio,
		CreatedAt:      secondsToTime(t.AddedOn),
		CompletedAt:    secondsToTime(t.CompletionOn),
	}

	if t.HasMetadata {
		// `size` counts the selected files only — the bytes this task is
		// still expected to move; `total_size` includes unselected ones.
		info.TotalBytes = &t.Size
	}

	if info.State == engine.StateError {
		// torrents/info carries no error text; the state is all the daemon
		// volunteers, so the code is the generic one.
		info.ErrorCode = errCodeUnknownTorrents
	}
	return info
}

// etaSeconds nils qBittorrent's "unknown ETA" sentinel (docs/06 section 5.5).
func etaSeconds(eta int64) *int64 {
	if eta >= etaSentinel || eta < 0 {
		return nil
	}
	return &eta
}

// secondsToTime converts a Unix-seconds timestamp to UTC, nil on the zero
// and negative values the daemon uses for "never".
func secondsToTime(sec int64) *time.Time {
	if sec <= 0 {
		return nil
	}
	t := time.Unix(sec, 0).UTC()
	return &t
}

// splitTags splits the comma-concatenated tags string of torrents/info,
// dropping empty segments.
func splitTags(tags string) []string {
	parts := strings.Split(tags, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			values = append(values, value)
		}
	}
	return values
}
