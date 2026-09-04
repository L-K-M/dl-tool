package store

import (
	"crypto/rand"
	"time"

	"github.com/oklog/ulid/v2"
)

const (
	PrefixUser                = "usr_"
	PrefixSession             = "ses_"
	PrefixAPIToken            = "tok_"
	PrefixSetting             = "set_"
	PrefixEngine              = "eng_"
	PrefixCategory            = "cat_"
	PrefixTag                 = "tag_"
	PrefixTaskTracker         = "ttr_"
	PrefixTaskEvent           = "evt_"
	PrefixIndexer             = "idx_"
	PrefixSearchJob           = "sch_"
	PrefixSearchResult        = "res_"
	PrefixFeed                = "fed_"
	PrefixFeedItem            = "itm_"
	PrefixTask                = "tsk_"
	PrefixNotificationChannel = "ntf_"
	PrefixRule                = "rul_"
	PrefixRuleMatch           = "mat_"
	PrefixJob                 = "job_"
	PrefixBandwidthCell       = "bws_"
	PrefixUIPref              = "uip_"
	PrefixWatchFolder         = "wfd_"
	PrefixTaskFile            = "tfi_"
)

// NewID returns a table prefix followed by a Crockford base32 ULID.
func NewID(prefix string) string {
	id := ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader)

	return prefix + id.String()
}

// Task is one row of the tasks table. Column names and types are owned by
// docs/04-data-model.md section 3.3; the columns not present here take their
// DDL defaults on insert and are written by the tasks that own them (T020,
// T026, T036, T074).
type Task struct {
	ID             string  `db:"id" json:"id"`
	Engine         string  `db:"engine" json:"engine"`
	EngineRef      *string `db:"engine_ref" json:"engine_ref"`
	SourceKind     string  `db:"source_kind" json:"source_kind"`
	SourceURI      *string `db:"source_uri" json:"source_uri"`
	Name           string  `db:"name" json:"name"`
	InfohashV1     *string `db:"infohash_v1" json:"infohash_v1"`
	InfohashV2     *string `db:"infohash_v2" json:"infohash_v2"`
	State          string  `db:"state" json:"state"`
	ErrorCode      *string `db:"error_code" json:"error_code"`
	ErrorMessage   *string `db:"error_message" json:"error_message"`
	Destination    string  `db:"destination" json:"destination"`
	ContentPath    *string `db:"content_path" json:"content_path"`
	CategoryID     *string `db:"category_id" json:"category_id"`
	TotalBytes     *int64  `db:"total_bytes" json:"total_bytes"`
	CompletedBytes int64   `db:"completed_bytes" json:"completed_bytes"`
	UploadedBytes  int64   `db:"uploaded_bytes" json:"uploaded_bytes"`
	DownloadRate   int64   `db:"download_rate" json:"download_rate"`
	UploadRate     int64   `db:"upload_rate" json:"upload_rate"`
	ETASeconds     *int64  `db:"eta_seconds" json:"eta_seconds"`
	Sequential     int     `db:"sequential" json:"sequential"`
	QueuePosition  *int64  `db:"queue_position" json:"queue_position"`
	AddedAt        int64   `db:"added_at" json:"added_at"`
	StartedAt      *int64  `db:"started_at" json:"started_at"`
	CompletedAt    *int64  `db:"completed_at" json:"completed_at"`
	CreatedAt      int64   `db:"created_at" json:"created_at"`
	UpdatedAt      int64   `db:"updated_at" json:"updated_at"`
}

// TaskEvent is one row of the task_events table (docs/04-data-model.md
// section 3.3). Code is a dotted i18n key, never a formatted value
// (docs/14-conventions.md section 4).
type TaskEvent struct {
	ID         string  `db:"id" json:"id"`
	TaskID     string  `db:"task_id" json:"task_id"`
	At         int64   `db:"at" json:"at"`
	Level      string  `db:"level" json:"level"`
	Code       string  `db:"code" json:"code"`
	Message    string  `db:"message" json:"message"`
	DetailJSON *string `db:"detail_json" json:"detail_json"`
	CreatedAt  int64   `db:"created_at" json:"created_at"`
	UpdatedAt  int64   `db:"updated_at" json:"updated_at"`
}
