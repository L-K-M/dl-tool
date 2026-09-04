package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/fsx"
	"github.com/L-K-M/dl-tool/internal/store"
	"github.com/L-K-M/dl-tool/internal/uri"
)

const (
	// maxCreateURIs is the per-submission cap of doc 05 section 5.2. The
	// maxItems schema tag enforces it for over-long bodies; the handler
	// enforces it for empty ones, which no schema tag can express.
	maxCreateURIs = 50

	operationCreateTasks = "create-tasks"

	queryCategoryIDByName = `SELECT id FROM categories WHERE name = ?`
	queryInsertTag        = `INSERT INTO tags (id, name, created_at, updated_at)
VALUES (?, ?, ?, ?) ON CONFLICT(name) DO NOTHING`
	queryTagIDByName   = `SELECT id FROM tags WHERE name = ?`
	queryInsertTaskTag = `INSERT INTO task_tags (task_id, tag_id) VALUES (?, ?) ON CONFLICT DO NOTHING`

	emptySubmissionDetail = "the submission holds no uri; send between 1 and 50"
	allRejectedDetail     = "every uri in the submission was rejected; see rejected[] for the per-uri reasons"
	unknownCategoryFormat = "category %q does not exist"
	engineUnavailableFmt  = "the %s engine is required for this submission but is not registered"
	uriRejectedDetail     = "the uri scheme is not supported in v1"
	engineRefusesURIFmt   = "engine %q does not accept this uri"
)

// CreateTasksBody is the JSON body of POST /tasks. The multipart form is
// added by T033.
type CreateTasksBody struct {
	URIs            []string        `json:"uris"             maxItems:"50" doc:"One entry per download; http(s), ftp(s), sftp, magnet, bare infohash and the obfuscated schemes"`
	Destination     string          `json:"destination,omitempty" doc:"Must resolve inside a configured data root; defaults to the first root"`
	Category        string          `json:"category,omitempty" doc:"Category name; must already exist"`
	Tags            []string        `json:"tags,omitempty" doc:"Tag names; created on demand"`
	Paused          bool            `json:"paused,omitempty" doc:"Create in paused instead of queued"`
	Sequential      bool            `json:"sequential,omitempty"`
	CreateSubfolder bool            `json:"create_subfolder,omitempty" doc:"Place content in <destination>/<manifest name>/ (applied by the admission pass)"`
	FTPCredentials  *FTPCredentials `json:"ftp_credentials,omitempty" doc:"Used for this request's ftp, ftps and sftp URIs only; never returned"`
	ExtractPassword string          `json:"extract_password,omitempty" doc:"Stored for auto-extract; never returned"`
	Engine          string          `json:"engine,omitempty" enum:"aria2,qbittorrent,ytdlp" doc:"Overrides the routing table when that engine accepts the URI"`
}

// CreateTasksInput is the operation input carrying CreateTasksBody.
type CreateTasksInput struct {
	Body CreateTasksBody
}

// FTPCredentials is used for this request's ftp, ftps and sftp URIs only and
// is never returned.
type FTPCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// RejectedURI is one entry of rejected[]; type is a slug from the registry
// in doc 05 section 1.3.
type RejectedURI struct {
	URI    string `json:"uri"`
	Type   string `json:"type"`
	Detail string `json:"detail"`
}

// CreateTasksOutput carries HTTP 201 whenever at least one task was created.
type CreateTasksOutput struct {
	Status int `json:"-" enum:"201" doc:"Created"`
	Body   struct {
		Created  []TaskDTO     `json:"created" doc:"The full Task objects of the created tasks"`
		Rejected []RejectedURI `json:"rejected" doc:"One entry per URI the submission refused; empty when everything was created"`
	}
}

// TaskDTO is the canonical Task object of doc 05 section 3. Timestamps are
// RFC 3339; sizes and rates are bytes and bytes per second; unknown is null.
type TaskDTO struct {
	ID                   string   `json:"id"`
	Engine               string   `json:"engine"`
	SourceKind           string   `json:"source_kind"`
	SourceURI            *string  `json:"source_uri"`
	InfohashV1           *string  `json:"infohash_v1"`
	InfohashV2           *string  `json:"infohash_v2"`
	Name                 string   `json:"name"`
	State                string   `json:"state"`
	ErrorCode            *string  `json:"error_code"`
	ErrorMessage         *string  `json:"error_message"`
	Destination          string   `json:"destination"`
	RequestedDestination *string  `json:"requested_destination"`
	ContentPath          *string  `json:"content_path"`
	Category             *string  `json:"category"`
	Tags                 []string `json:"tags"`
	TotalBytes           *int64   `json:"total_bytes"`
	CompletedBytes       int64    `json:"completed_bytes"`
	UploadedBytes        int64    `json:"uploaded_bytes"`
	Progress             float64  `json:"progress"`
	DownloadRate         int64    `json:"download_rate"`
	UploadRate           int64    `json:"upload_rate"`
	ETASeconds           *int64   `json:"eta_seconds"`
	Ratio                float64  `json:"ratio"`
	TotalPeers           int      `json:"total_peers"`
	ConnectedSeeders     int      `json:"connected_seeders"`
	ConnectedLeechers    int      `json:"connected_leechers"`
	DLLimit              int64    `json:"dl_limit"`
	ULLimit              int64    `json:"ul_limit"`
	RatioLimit           *float64 `json:"ratio_limit"`
	SeedingTimeLimit     *int64   `json:"seeding_time_limit"`
	Sequential           bool     `json:"sequential"`
	QueuePosition        *int64   `json:"queue_position"`
	UnzipProgress        *int     `json:"unzip_progress"`
	FileCount            *int     `json:"file_count"`
	AddedAt              string   `json:"added_at"`
	StartedAt            *string  `json:"started_at"`
	CompletedAt          *string  `json:"completed_at"`
	UpdatedAt            string   `json:"updated_at"`
}

// TaskHandlers owns the /tasks collection operations. T020 lands the create
// operation; the list, patch, action and file operations arrive with their
// own tasks.
type TaskHandlers struct {
	db      *sqlx.DB
	tasks   *store.TaskStore
	engines *engine.Registry
	roots   []string
}

// NewTaskHandlers builds the task handlers. db is the store the task rows
// are written through; engines is the routing-time availability table — a
// URI whose routed engine is not registered answers 503; roots is
// DLTOOL_DATA_ROOTS in configured order.
func NewTaskHandlers(db *sqlx.DB, engines *engine.Registry, roots []string) *TaskHandlers {
	return &TaskHandlers{db: db, tasks: store.NewTaskStore(db), engines: engines, roots: roots}
}

// registerOperations mounts the /tasks operations on the Huma API;
// Server.registerOperations is the call site.
func (h *TaskHandlers) registerOperations(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID: operationCreateTasks,
		Method:      http.MethodPost,
		Path:        "/tasks",
		Summary:     "Create tasks from submitted URIs",
		Description: "Creates one queued task per accepted URI and reports the refused ones in rejected[]. Partial success is normal. No engine is contacted: the admission pass owns Engine.Add.",
		Tags:        []string{"tasks"},
		Security:    credentialRequired,
	}, h.CreateTasks)
}

// plannedTask is one accepted URI with its routing decided, ready to insert.
type plannedTask struct {
	normalized uri.Normalized
	engine     string
}

// CreateTasks accepts up to 50 URIs, normalises and routes each one,
// resolves the destination inside a configured root and inserts one tasks
// row per accepted URI. It never hands a task to an engine: the admission
// pass (T098) is the only caller of Engine.Add, so the concurrency limits
// govern a new task exactly as they govern a resumed one.
func (h *TaskHandlers) CreateTasks(ctx context.Context, in *CreateTasksInput) (*CreateTasksOutput, error) {
	// Shape validation runs before any other work, so a malformed submission
	// can create no row and touch no engine.
	if len(in.Body.URIs) == 0 || len(in.Body.URIs) > maxCreateURIs {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, emptySubmissionDetail)
	}

	destination, err := fsx.ResolveDestination(h.roots, in.Body.Destination)
	if err != nil {
		return nil, destinationRejected(in.Body.Destination)
	}

	categoryID, err := h.resolveCategory(ctx, in.Body.Category)
	if err != nil {
		return nil, err
	}

	planned, rejected, err := h.planURIs(in.Body)
	if err != nil {
		return nil, err
	}
	if len(planned) == 0 {
		// Every URI refused: the top-level detail carries the first
		// rejection's reason — for an ed2k-only submission exactly the
		// message of doc 06 section 2 row 7.
		detail := allRejectedDetail
		if len(rejected) > 0 {
			detail = rejected[0].Detail
		}

		return nil, Problem(SlugUnsupportedScheme, http.StatusUnprocessableEntity, detail)
	}

	created := make([]TaskDTO, 0, len(planned))
	for _, p := range planned {
		dto, err := h.insertPlanned(ctx, p, &in.Body, destination, categoryID)
		if err != nil {
			return nil, err
		}
		created = append(created, dto)
	}

	output := &CreateTasksOutput{Status: http.StatusCreated}
	output.Body.Created = created
	output.Body.Rejected = rejected

	return output, nil
}

// planURIs normalises and routes every URI, collecting a rejection for each
// refused one. It returns an error only for the whole-request failures: an
// explicit engine that is not registered, or a routed engine that is not.
func (h *TaskHandlers) planURIs(body CreateTasksBody) ([]plannedTask, []RejectedURI, error) {
	planned := make([]plannedTask, 0, len(body.URIs))
	rejected := []RejectedURI{}

	for _, raw := range body.URIs {
		n, err := uri.Normalize(raw)
		if err != nil {
			rejected = append(rejected, rejectURI(raw, err))

			continue
		}

		// MediaMatcher stays nil until the T088 ADR lands; until then a
		// media URL simply routes to aria2 (IMPLEMENTING.md, open items).
		engineName, err := engine.Route(n, nil)
		if err != nil {
			rejected = append(rejected, rejectURI(raw, err))

			continue
		}

		// An explicit engine overrides the router only when that engine
		// accepts the URI (doc 06 section 2); otherwise this one URI is
		// refused, not the whole request.
		if body.Engine != "" && body.Engine != engineName {
			chosen, ok := h.engines.Get(body.Engine)
			if !ok {
				return nil, nil, engineUnavailable(body.Engine)
			}
			if !chosen.Accepts(n.URI) {
				rejection := RejectedURI{
					URI:    raw,
					Type:   SlugUnsupportedScheme,
					Detail: fmt.Sprintf(engineRefusesURIFmt, body.Engine),
				}
				rejected = append(rejected, rejection)

				continue
			}
			engineName = body.Engine
		}

		if _, ok := h.engines.Get(engineName); !ok {
			return nil, nil, engineUnavailable(engineName)
		}

		planned = append(planned, plannedTask{normalized: n, engine: engineName})
	}

	return planned, rejected, nil
}

// insertPlanned persists one planned task, links its tags and renders the
// response DTO.
func (h *TaskHandlers) insertPlanned(
	ctx context.Context,
	p plannedTask,
	body *CreateTasksBody,
	destination string,
	categoryID *string,
) (TaskDTO, error) {
	n := p.normalized

	state := string(engine.StateQueued)
	if body.Paused {
		state = string(engine.StatePaused)
	}

	// The stored source is the server-only engine/recovery source: it may
	// embed the request's FTP credentials, which the admission pass puts
	// into engine.AddRequest.Extra (docs/04-data-model.md section 3.3). The
	// API object below carries only the stripped display URI, so no
	// password is ever returned or logged.
	source := n.URI
	if body.FTPCredentials != nil && (n.Kind == uri.KindFTP || n.Kind == uri.KindSFTP) {
		source = embedCredentials(n.URI, *body.FTPCredentials)
	}

	task, err := h.tasks.Create(ctx, store.Task{
		Engine:      p.engine,
		SourceKind:  string(n.Kind),
		SourceURI:   &source,
		Name:        displayName(n),
		InfohashV1:  stringOrNil(n.InfohashV1),
		InfohashV2:  stringOrNil(n.InfohashV2),
		State:       state,
		Destination: destination,
		CategoryID:  categoryID,
		Sequential:  boolToInt(body.Sequential),
	})
	if err != nil {
		return TaskDTO{}, internalFailure(ctx, "create task", err)
	}

	if err := h.linkTags(ctx, task.ID, body.Tags); err != nil {
		return TaskDTO{}, internalFailure(ctx, "link tags", err)
	}

	var category *string
	if body.Category != "" {
		category = &body.Category
	}

	return newTaskDTO(task, n.URI, category, body.Tags), nil
}

// resolveCategory maps a category name to its id. The category must already
// exist (doc 05 section 5.2); an unknown name is a validation failure before
// any row is written. The categories store arrives with the settings tasks;
// until then this read is the only category access the create path needs.
func (h *TaskHandlers) resolveCategory(ctx context.Context, name string) (*string, error) {
	if name == "" {
		return nil, nil
	}

	var id string
	err := h.db.GetContext(ctx, &id, queryCategoryIDByName, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Problem(
			SlugValidationFailed,
			http.StatusUnprocessableEntity,
			fmt.Sprintf(unknownCategoryFormat, name),
		)
	}
	if err != nil {
		return nil, internalFailure(ctx, "resolve category", err)
	}

	return &id, nil
}

// linkTags creates tag rows on demand and links them to the task (doc 05
// section 5.2). The tag store arrives with its own task; until then these
// two statements are the only tag access the create path needs.
func (h *TaskHandlers) linkTags(ctx context.Context, taskID string, names []string) error {
	now := time.Now().UnixMilli()

	for _, name := range names {
		if _, err := h.db.ExecContext(ctx, queryInsertTag, store.NewID(store.PrefixTag), name, now, now); err != nil {
			return fmt.Errorf("create tag %q: %w", name, err)
		}

		var tagID string
		if err := h.db.GetContext(ctx, &tagID, queryTagIDByName, name); err != nil {
			return fmt.Errorf("resolve tag %q: %w", name, err)
		}

		if _, err := h.db.ExecContext(ctx, queryInsertTaskTag, taskID, tagID); err != nil {
			return fmt.Errorf("link tag %q: %w", name, err)
		}
	}

	return nil
}

// rejectURI renders one rejected[] entry for a URI that normalising or
// routing refused. The detail carries the sentinel's reason alone — for ed2k
// exactly "ed2k is not supported in v1" (doc 06 section 2 row 7).
func rejectURI(raw string, err error) RejectedURI {
	detail := uriRejectedDetail
	if reason, ok := strings.CutPrefix(err.Error(), uri.ErrUnsupportedScheme.Error()+": "); ok {
		detail = reason
	}

	return RejectedURI{URI: raw, Type: SlugUnsupportedScheme, Detail: detail}
}

// destinationRejected renders the 403 of doc 05 section 1.3's example: the
// requested destination echoed in detail, plus the field-level error.
func destinationRejected(requested string) error {
	return &huma.ErrorModel{
		Type:   SlugPathRejected,
		Title:  http.StatusText(http.StatusForbidden),
		Status: http.StatusForbidden,
		Detail: fmt.Sprintf("destination %q resolves outside every configured data root", requested),
		Errors: []*huma.ErrorDetail{{
			Message:  "must resolve inside a configured root",
			Location: "body.destination",
			Value:    requested,
		}},
	}
}

// engineUnavailable is the 503 a submission gets when its routed engine is
// not registered at all — routing is resolved here even though the
// submission is not handed over (doc 05 section 5.2).
func engineUnavailable(name string) error {
	return Problem(SlugEngineUnavailable, http.StatusServiceUnavailable, fmt.Sprintf(engineUnavailableFmt, name))
}

// embedCredentials rebuilds the engine source with the request's FTP
// userinfo. Only tasks.source_uri ever carries it; the response DTO carries
// the stripped display URI instead.
func embedCredentials(displayURI string, creds FTPCredentials) string {
	u, err := url.Parse(displayURI)
	if err != nil {
		return displayURI
	}
	u.User = url.UserPassword(creds.Username, creds.Password)

	return u.String()
}

// displayName derives the task's display name: a magnet's dn parameter when
// present, else the last path segment, else the host, else the URI itself.
func displayName(n uri.Normalized) string {
	if n.DisplayName != "" {
		return n.DisplayName
	}

	u, err := url.Parse(n.URI)
	if err != nil {
		return n.URI
	}
	if base := path.Base(u.Path); base != "." && base != "/" && base != "" {
		return base
	}
	if u.Host != "" {
		return u.Host
	}

	return n.URI
}

// newTaskDTO renders one canonical Task object from a stored row. display is
// the API-safe source URI — never the row's server-only source_uri, which
// may embed FTP credentials. tags is the display order: the names as
// submitted, empty when untagged.
func newTaskDTO(t store.Task, display string, category *string, tags []string) TaskDTO {
	sourceURI := display
	dto := TaskDTO{
		ID:                t.ID,
		Engine:            t.Engine,
		SourceKind:        t.SourceKind,
		SourceURI:         &sourceURI,
		InfohashV1:        t.InfohashV1,
		InfohashV2:        t.InfohashV2,
		Name:              t.Name,
		State:             t.State,
		Destination:       t.Destination,
		Category:          category,
		Tags:              tags,
		TotalBytes:        t.TotalBytes,
		CompletedBytes:    t.CompletedBytes,
		UploadedBytes:     t.UploadedBytes,
		Progress:          0,
		DownloadRate:      t.DownloadRate,
		UploadRate:        t.UploadRate,
		ETASeconds:        t.ETASeconds,
		Ratio:             0,
		TotalPeers:        0,
		ConnectedSeeders:  0,
		ConnectedLeechers: 0,
		DLLimit:           0,
		ULLimit:           0,
		Sequential:        t.Sequential == 1,
		AddedAt:           time.UnixMilli(t.AddedAt).UTC().Format(time.RFC3339),
		UpdatedAt:         time.UnixMilli(t.UpdatedAt).UTC().Format(time.RFC3339),
	}
	if tags == nil {
		dto.Tags = []string{}
	}

	return dto
}

// stringOrNil maps an empty string to nil for the nullable pointer columns.
func stringOrNil(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

// boolToInt maps a boolean onto the 0/1 integer columns.
func boolToInt(b bool) int {
	if b {
		return 1
	}

	return 0
}
