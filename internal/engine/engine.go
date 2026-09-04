// Package engine declares the single Engine interface every download adapter
// implements, the capability and state vocabularies, and the protocol router
// that assigns a submission to an engine (docs/06-download-engines.md
// sections 1-2).
package engine

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotSupported = errors.New("engine: capability not supported") // optional method, Capability absent
	ErrNotFound     = errors.New("engine: task not found")
	ErrUnavailable  = errors.New("engine: daemon unreachable or session refused")
)

// TaskState is the canonical state. Same values as tasks.state (DB) and `state` (API).
// No adapter ever returns StateExtracting or StateMoving: those two belong to
// dl-tool's post-processing jobs, which overwrite the engine state while a job
// holds the task.
type TaskState string

const (
	StateQueued      TaskState = "queued"
	StateDownloading TaskState = "downloading"
	StateSeeding     TaskState = "seeding"
	StatePaused      TaskState = "paused"
	StateChecking    TaskState = "checking"
	StateExtracting  TaskState = "extracting"
	StateMoving      TaskState = "moving"
	StateCompleted   TaskState = "completed"
	StateError       TaskState = "error"
	StateRemoved     TaskState = "removed"
)

// Capability is a declared, not guessed, engine feature: adapters list what
// they can do so the UI can grey out the rest.
type Capability string

const (
	CapHTTP            Capability = "http"
	CapFTP             Capability = "ftp"
	CapSFTP            Capability = "sftp"
	CapBitTorrent      Capability = "bittorrent"
	CapMagnet          Capability = "magnet"
	CapMetalink        Capability = "metalink"
	CapMediaSite       Capability = "media_site"
	CapNZB             Capability = "nzb"
	CapPerFileSelect   Capability = "per_file_select"   // can (de)select files
	CapPerFilePriority Capability = "per_file_priority" // can set a numeric per-file priority
	CapCategories      Capability = "categories"
	CapTags            Capability = "tags"
	CapSequential      Capability = "sequential"
	CapSetLocation     Capability = "set_location"
	CapRename          Capability = "rename"
	CapShareLimits     Capability = "share_limits"
	CapSearch          Capability = "search"
	CapRSSRules        Capability = "rss_rules"
	CapBTV2            Capability = "bt_v2"
	CapPushEvents      Capability = "push_events"
)

// AddRequest is the engine-independent submission. Exactly one of URIs or Blob is set.
type AddRequest struct {
	URIs        []string // http/ftp/sftp/magnet/metalink URLs
	Blob        []byte   // raw .torrent / .metalink bytes
	BlobKind    string   // "torrent" | "metalink" | "nzb"
	SaveDir     string   // absolute, already validated by internal/fsx
	Filename    string   // rename / output-template override
	Category    string
	Tags        []string
	StartPaused bool
	Sequential  bool
	SelectFiles []int             // file indices to download; nil means all
	Extra       map[string]string // engine-specific escape hatch, never surfaced in the API
}

// FileEntry is one file of an engine task.
type FileEntry struct {
	Index     int    // 0-based in every adapter
	Path      string // relative to SaveDir
	Size      int64
	Completed int64
	Selected  bool
	Priority  *int // 0=skip 1=normal 6=high 7=maximum (§1.1); nil when the engine has no priorities
}

// TaskInfo is one engine task, normalised. Pointer fields are nil when the engine does not know yet.
type TaskInfo struct {
	ID             string // engine-namespaced, e.g. "aria2:2089b05ecca3d829"
	Engine         string
	Name           string
	State          TaskState
	TotalBytes     *int64 // nil while metadata is still unknown
	CompletedBytes int64
	UploadedBytes  int64
	DownloadRate   int64 // bytes/second
	UploadRate     int64 // bytes/second
	ETASeconds     *int64
	SaveDir        string
	ContentPath    string // absolute path to the finished file or directory; "" if unknown
	ErrorCode      string // a tasks.error_code value; "" when no error
	ErrorMessage   string
	InfohashV1     string // lowercase hex, 40 chars; "" if not a torrent
	InfohashV2     string // lowercase hex, 64 chars; "" unless v2 or hybrid
	NumSeeds       *int
	NumPeers       *int
	Ratio          *float64
	CreatedAt      *time.Time
	CompletedAt    *time.Time
}

// EventKind is the vocabulary of the engine task events carried by a TaskEvent.
type EventKind string

const (
	EventAdded     EventKind = "added"
	EventStarted   EventKind = "started"
	EventProgress  EventKind = "progress"
	EventPaused    EventKind = "paused"
	EventCompleted EventKind = "completed"
	EventError     EventKind = "error"
	EventRemoved   EventKind = "removed"
)

// TaskEvent is one engine task event, pushed by the engine where possible and
// synthesised by a polling loop otherwise.
type TaskEvent struct {
	TaskID string
	Kind   EventKind
	Info   *TaskInfo // nil for EventRemoved
}

// Engine is the one interface every adapter in internal/engine/<name>/ implements.
// context.Context is the first parameter of every I/O method; error is the last
// return value; an unsupported optional method returns ErrNotSupported and
// mutates nothing.
type Engine interface {
	Name() string               // "aria2" | "qbittorrent" | "ytdlp"
	Capabilities() []Capability // declared, sorted, stable
	Accepts(uri string) bool    // true if this engine should handle this URI; used by the router

	Connect(ctx context.Context) error
	Close() error
	Health(ctx context.Context) (version string, err error) // ErrUnavailable when down

	Add(ctx context.Context, req AddRequest) (id string, err error)
	List(ctx context.Context) ([]TaskInfo, error)
	Get(ctx context.Context, id string) (TaskInfo, error) // ErrNotFound when absent
	Files(ctx context.Context, id string) ([]FileEntry, error)

	Pause(ctx context.Context, id string) error
	Resume(ctx context.Context, id string) error
	Remove(ctx context.Context, id string) error // always retain payload data

	// Optional: return ErrNotSupported when the backing Capability is absent,
	// and mutate nothing.
	SetFiles(ctx context.Context, id string, selected []int, priorities map[int]int) error
	// Optional: return ErrNotSupported when the backing Capability is absent,
	// and mutate nothing.
	SetLocation(ctx context.Context, id, path string) error
	// Optional: return ErrNotSupported when the backing Capability is absent,
	// and mutate nothing.
	Rename(ctx context.Context, id, name string) error
	// Optional: return ErrNotSupported when the backing Capability is absent,
	// and mutate nothing.
	SetCategory(ctx context.Context, id, category string) error
	// SetRateLimits applies bytes/second. id == "" means the global limit; a nil direction is
	// left unchanged; 0 means unlimited. An adapter without the backing Capability returns
	// ErrNotSupported and mutates nothing.
	SetRateLimits(ctx context.Context, id string, down, up *int64) error
	// Optional: return ErrNotSupported when the backing Capability is absent,
	// and mutate nothing.
	SetShareLimits(ctx context.Context, id string, ratio *float64, seedMinutes *int64) error

	// Events pushes where possible (aria2 WebSocket notifications), else runs a polling loop that
	// yields the same TaskEvent shape (qBittorrent sync/maindata rid deltas). The channel closes
	// when ctx is cancelled or the engine is closed.
	Events(ctx context.Context) (<-chan TaskEvent, error)
}
