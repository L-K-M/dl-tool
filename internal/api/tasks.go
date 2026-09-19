package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/fsx"
	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
	isync "github.com/L-K-M/dl-tool/internal/sync"
	"github.com/L-K-M/dl-tool/internal/uri"
)

const (
	// The per-submission URI cap is MaxURIs (submission.go): payload uris
	// plus every .txt line pool under it. The maxItems schema tag enforces
	// it for over-long JSON bodies; the handler enforces it for the merged
	// count and for empty submissions, which no schema tag can express.

	operationCreateTasks = "create-tasks"
	operationListTasks   = "list-tasks"
	operationGetTask     = "get-task"

	queryCategoryIDByName = `SELECT id FROM categories WHERE name = ?`
	queryInsertTag        = `INSERT INTO tags (id, name, created_at, updated_at)
VALUES (?, ?, ?, ?) ON CONFLICT(name) DO NOTHING`
	queryTagIDByName   = `SELECT id FROM tags WHERE name = ?`
	queryInsertTaskTag = `INSERT INTO task_tags (task_id, tag_id) VALUES (?, ?) ON CONFLICT DO NOTHING`

	// The store's Task row cannot carry requested_destination yet (its
	// column is owned by the store task that adds it), so the create path
	// records the echo itself, after the insert it belongs to. updated_at
	// is untouched: the write completes the insert, it is not a task change.
	querySetRequestedDestination = `UPDATE tasks SET requested_destination = ? WHERE id = ?`

	emptySubmissionDetail = "the submission holds no uri; send between 1 and 50"
	tooManyURIsFormat     = "the submission holds %d uris; send between 1 and %d"
	allRejectedDetail     = "every uri in the submission was rejected; see rejected[] for the per-uri reasons"

	// search_result_ids is its own source family (doc 05 section 9.2): it
	// never mixes with uris or file parts, and its rejected[] entries name
	// the res_ id, never the provider source they resolved to.
	mixedSourceFamiliesDetail = "search_result_ids cannot be combined with uris or file parts"
	tooManyResultsFormat      = "the submission holds %d search_result_ids; send between 1 and %d"
	searchResultGoneDetail    = "the search result is unknown or its search job is gone"
	duplicateResultDetail     = "the same search result id appears twice in this submission"
	noSearchResultsDetail     = "no search result id resolved to a stored result"
	searchResultDisplayPrefix = "search-result:"

	selectionNoManifestDetail = "select_files applies to a multi-file manifest; the submission holds none"
	selectionCapDetailFormat  = "the %s engine does not support selecting files"
	selectionPrioCapFormat    = "the %s engine does not support per-file priority"
	unknownCategoryFormat     = "category %q does not exist"
	engineUnavailableFmt      = "the %s engine is required for this submission but is not registered"
	uriRejectedDetail         = "the uri scheme is not supported in v1"
	// errorCodeSSRFBlocked is the tasks.error_code vocabulary row of
	// docs/04-data-model.md section 4.2 that a guard-blocked URI's task
	// carries; its fixed message is ssrfBlockedMessage.
	errorCodeSSRFBlocked = "ssrf_blocked"
	ssrfBlockedMessage   = "blocked by the SSRF guard"
	ssrfAllBlockedDetail = "every uri in the submission was refused by the ssrf guard; see rejected[] for the per-uri reasons"
	engineRefusesURIFmt  = "engine %q does not accept this uri"
	// duplicateDetail is the conflict detail of a duplicate torrent; the
	// full detail names the live task that holds the identity.
	duplicateDetail       = "a task for this torrent already exists"
	duplicateDetailFormat = "%s: %s"
	duplicateRepeatDetail = "this torrent appears twice in this submission"

	queryCategoryNamesByIDs = `SELECT id, name FROM categories WHERE id IN (?)`

	queryTagNamesByTaskIDs = `SELECT tt.task_id, t.name
FROM task_tags tt JOIN tags t ON t.id = tt.tag_id
WHERE tt.task_id IN (?)
ORDER BY t.name`
)

// CreateTasksBody is the JSON body of POST /tasks, and the payload part of
// its multipart form (without blob: the form's file parts carry the bytes).
type CreateTasksBody struct {
	URIs            []string               `json:"uris,omitempty"         maxItems:"50" doc:"One entry per download; http(s), ftp(s), sftp, magnet, bare infohash and the obfuscated schemes"`
	Destination     string                 `json:"destination,omitempty" doc:"Must resolve inside a configured data root; defaults to the first root"`
	Category        string                 `json:"category,omitempty" doc:"Category name; must already exist"`
	Tags            []string               `json:"tags,omitempty" doc:"Tag names; created on demand"`
	Paused          bool                   `json:"paused,omitempty" doc:"Create in paused instead of queued"`
	Sequential      bool                   `json:"sequential,omitempty"`
	CreateSubfolder bool                   `json:"create_subfolder,omitempty" doc:"Place content in <destination>/<manifest name>/ (applied by the admission pass)"`
	SelectFiles     []FileSelectionRequest `json:"select_files,omitempty" doc:"Applied to the first multi-file manifest; 422 when the routed engine lacks per_file_select"`
	FTPCredentials  *FTPCredentials        `json:"ftp_credentials,omitempty" doc:"Used for this request's ftp, ftps and sftp URIs only; never returned"`
	ExtractPassword string                 `json:"extract_password,omitempty" doc:"Stored for auto-extract; never returned"`
	Engine          string                 `json:"engine,omitempty" enum:"aria2,qbittorrent,ytdlp" doc:"Overrides the routing table when that engine accepts the URI"`
	// SearchResultIDs is the opaque-id source family of doc 05 section 9.2:
	// res_ ids from a search job, resolved to their stored provider source
	// server-side. It never combines with uris or file parts.
	SearchResultIDs []string `json:"search_result_ids,omitempty" maxItems:"50" doc:"Opaque res_ ids from a search job; resolved server-side, never mixed with uris or file parts"`
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
	URI string `json:"uri,omitempty"`
	// SearchResultID names a refused search_result_ids element instead of
	// uri — the resolved provider source never appears in a rejection.
	SearchResultID string `json:"search_result_id,omitempty"`
	Type           string `json:"type"`
	Detail         string `json:"detail"`
}

// CreateTasksOutput carries HTTP 201 when at least one task was created —
// and for a search_result_ids submission whose every id resolved yet was
// refused (already-committed duplicates): created[] is then empty and
// rejected[] carries the per-id reasons.
type CreateTasksOutput struct {
	Status int `json:"-" enum:"201" doc:"Created"`
	Body   struct {
		Created  []TaskDTO     `json:"created" doc:"The full Task objects of the created tasks"`
		Rejected []RejectedURI `json:"rejected" doc:"One entry per URI the submission refused; empty when everything was created"`
	}
}

// ListTasksInput is the query of GET /tasks (docs/05-api-contract.md
// section 5.1). Category and Tag are read from the raw query as well,
// because Huma cannot distinguish ?category= (the uncategorised filter)
// from an absent parameter.
type ListTasksInput struct {
	State    string `query:"state" enum:"all,active,checking,completed,downloading,error,extracting,inactive,moving,paused,queued,removed,seeding,stopped" doc:"A canonical state, or a sidebar filter: all, downloading, completed, active, inactive, stopped, error"`
	Category string `query:"category" doc:"Category name; the empty value selects uncategorised tasks"`
	Tag      string `query:"tag" doc:"Tag name; the empty value selects untagged tasks"`
	Q        string `query:"q" doc:"Case-insensitive substring of name"`
	Sort     string `query:"sort" doc:"Sort column with an optional leading - to reverse; default -added_at"`
	Limit    int    `query:"limit" minimum:"1" maximum:"500" default:"100" doc:"Page size"`
	Cursor   string `query:"cursor" doc:"Opaque page token from a previous response"`
}

// ListTasksOutput is the cursor pagination envelope of doc 05 section 1.4.
type ListTasksOutput struct {
	Body struct {
		Items      []TaskDTO `json:"items" doc:"Full Task objects, ordered by the requested sort"`
		NextCursor *string   `json:"next_cursor" doc:"Token for the next page; null on the last page"`
		Total      int       `json:"total" doc:"Rows matching the filter, ignoring the cursor"`
	}
}

// GetTaskInput addresses one task by id.
type GetTaskInput struct {
	ID string `path:"id" doc:"The tsk_ id of the task"`
}

// GetTaskOutput is one canonical Task object.
type GetTaskOutput struct {
	Body TaskDTO
}

// rawQueryKey carries the unparsed query string through a Huma operation
// middleware: Huma's parameter parsing treats an empty value as an absent
// parameter, and the list endpoint needs the difference.
type rawQueryKey struct{}

// stashRawQuery is the per-operation middleware that puts the raw query on
// the request context before Huma parses the input.
func stashRawQuery(ctx huma.Context, next func(huma.Context)) {
	next(huma.WithValue(ctx, rawQueryKey{}, ctx.URL().RawQuery))
}

// rawQueryValues returns the parsed query as sent, including keys Huma
// drops; ok is false outside the list operation.
func rawQueryValues(ctx context.Context) (url.Values, bool) {
	raw, ok := ctx.Value(rawQueryKey{}).(string)
	if !ok {
		return nil, false
	}

	// The partially decoded pairs are authoritative for presence even
	// when another pair failed to unescape.
	values, _ := url.ParseQuery(raw)

	return values, true
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
	AddedAt              string   `json:"added_at" format:"date-time"`
	StartedAt            *string  `json:"started_at" format:"date-time"`
	CompletedAt          *string  `json:"completed_at" format:"date-time"`
	UpdatedAt            string   `json:"updated_at" format:"date-time"`
}

// TaskHandlers owns the /tasks collection operations: create, list and
// get; the patch, action and file operations arrive with their own tasks.
type TaskHandlers struct {
	db       *sqlx.DB
	tasks    *store.TaskStore
	settings *store.SettingsStore
	engines  *engine.Registry
	roots    []string
	// guard and resolver are the SSRF preflight pair of T122: every
	// user-submitted transport URI is checked through them before its task
	// may reach an engine. server.go wires one shared guard built from
	// cfg.SSRFAllowPrivate and net.DefaultResolver; a nil pair fails closed.
	guard    *secure.Guard
	resolver secure.Resolver
}

// NewTaskHandlers builds the task handlers. db is the store the task rows
// are written through — the SettingsStore for the category read and the
// default_destination fallback is built over it here, like the TaskStore,
// so the signature and call site do not change; engines is the
// routing-time availability table — a URI whose routed engine is not
// registered answers 503; roots is DLTOOL_DATA_ROOTS in configured order.
// guard and resolver are the SSRF preflight pair: server.go passes one
// guard built once per server and the process resolver.
func NewTaskHandlers(db *sqlx.DB, engines *engine.Registry, roots []string, guard *secure.Guard, resolver secure.Resolver) *TaskHandlers {
	return &TaskHandlers{
		db:       db,
		tasks:    store.NewTaskStore(db),
		settings: store.NewSettingsStore(db),
		engines:  engines,
		roots:    roots,
		guard:    guard,
		resolver: resolver,
	}
}

// preflight validates one submitted URI. It returns nil when the guard does
// not govern the scheme, and a *secure.BlockedError otherwise.
func (h *TaskHandlers) preflight(ctx context.Context, rawURI string) error {
	return secure.PreflightURI(ctx, h.guard, h.resolver, rawURI)
}

// ssrfRejection is the rejected[] entry for a blocked URI. The detail carries
// the redacted URL only: a resolved address or a matched prefix goes to the
// log record, never to an API response.
func ssrfRejection(rawURI string) RejectedURI {
	return RejectedURI{
		URI:    secure.RedactURL(rawURI),
		Type:   SlugSSRFBlocked,
		Detail: "the URL resolved to a blocked address range",
	}
}

// registerOperations mounts the /tasks operations on the Huma API;
// Server.registerOperations is the call site.
func (h *TaskHandlers) registerOperations(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID: operationCreateTasks,
		Method:      http.MethodPost,
		Path:        "/tasks",
		Summary:     "Create tasks from submitted URIs",
		Description: "Creates one queued task per accepted URI and reports the refused ones in rejected[]. Beside application/json the operation accepts the multipart form of doc 05 section 5.2: one payload part with this JSON body, plus .torrent, .metalink and .txt file parts. Partial success is normal. No engine is contacted: the admission pass owns Engine.Add.",
		Tags:        []string{"tasks"},
		Security:    credentialRequired,
		RequestBody: multipartSubmissionBody(),
		// The form middleware translates multipart/form-data into the JSON
		// path and exposes the file parts on the request context.
		Middlewares: huma.Middlewares{acceptSubmissionForm},
	}, h.CreateTasks)

	huma.Register(hapi, huma.Operation{
		OperationID: operationListTasks,
		Method:      http.MethodGet,
		Path:        "/tasks",
		Summary:     "List, filter and sort tasks",
		Description: "Cursor-paginated task list. state accepts a canonical state or a sidebar filter; an empty category or tag selects the uncategorised and untagged tasks. A cursor is bound to the filter and sort that issued it.",
		Tags:        []string{"tasks"},
		Security:    credentialRequired,
		// A mistyped query key is never silently ignored
		// (docs/05-api-contract.md 5.1, step 8 of the task).
		RejectUnknownQueryParameters: true,
		// The raw query reaches the handler because Huma parses an empty
		// value (?category=) as an absent parameter, and the empty value is
		// the uncategorised filter.
		Middlewares: huma.Middlewares{stashRawQuery},
	}, h.ListTasks)

	huma.Register(hapi, huma.Operation{
		OperationID: operationGetTask,
		Method:      http.MethodGet,
		Path:        "/tasks/{id}",
		Summary:     "Read one task",
		Tags:        []string{"tasks"},
		Security:    credentialRequired,
		// Same strictness as the list: a mistyped query key is 422.
		RejectUnknownQueryParameters: true,
	}, h.GetTask)
}

// plannedTask is one accepted submission with its routing decided, ready to
// insert: a URI, an uploaded .torrent or an uploaded .metalink.
type plannedTask struct {
	normalized uri.Normalized
	engine     string
	// destination is the effective save directory: the resolved request
	// destination, or its subfolder once create_subfolder moved a
	// multi-file manifest (FR-008).
	destination string
	// name overrides the display name when the submission has no URI to
	// derive it from (a metalink part).
	name string
	// manifest is the parsed .torrent of an uploaded part; nil for every
	// other submission.
	manifest *uri.Manifest
	// selection is the create-time select_files intent when this task is
	// the submission's target; nil for every other planned task.
	selection *store.SelectionIntent
	// displaySource is the API-safe reference written to
	// source_display_uri — "search-result:<res_id>" for a search-result
	// submission, nil for every other family.
	displaySource *string
	// blocked marks a URI the SSRF preflight refused: its row is created
	// already terminal in error state so the refusal is inspectable, and
	// the URI joins rejected[], never created[].
	blocked bool
}

// CreateTasks accepts up to 50 sources — payload uris, the lines of .txt
// parts and one task per .torrent or .metalink part — normalises and routes
// each submission, resolves the destination inside a configured root and
// inserts one tasks row per accepted source. It never hands a task to an
// engine: the admission pass (T098) is the only caller of Engine.Add, so
// the concurrency limits govern a new task exactly as they govern a resumed
// one.
func (h *TaskHandlers) CreateTasks(ctx context.Context, in *CreateTasksInput) (*CreateTasksOutput, error) {
	// The form middleware stashed the parsed file parts on the request
	// context; a JSON request carries none. Their .txt lines join the
	// payload's uris and their blobs merge into the same submission list
	// the JSON path builds (task step 5); an unrecognised part is a
	// rejection, never a guess.
	txtURIs, blobs, rejected := processUploads(uploadedFilesFrom(ctx))

	uris := make([]string, 0, len(in.Body.URIs)+len(txtURIs))
	uris = append(uris, in.Body.URIs...)
	uris = append(uris, txtURIs...)

	searchResultIDs := in.Body.SearchResultIDs

	// Exactly one source family per request (doc 05 section 9.2):
	// search_result_ids never mixes with uris, blob parts or even a
	// rejected junk part — the form middleware's rejected[] entries count
	// as the file-part family.
	if len(searchResultIDs) > 0 && (len(uris) > 0 || len(blobs) > 0 || len(rejected) > 0) {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, mixedSourceFamiliesDetail)
	}

	// Shape validation runs before any other work, so a malformed submission
	// can create no row and touch no engine. The schema's maxItems tag
	// answers an over-long JSON body first; this branch owns the merged
	// count of payload uris and .txt lines, and the empty submission —
	// which a junk-only form is not: it gets the all-rejected answer below,
	// its rejected[] entry intact.
	if len(uris) == 0 && len(blobs) == 0 && len(rejected) == 0 && len(searchResultIDs) == 0 {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, emptySubmissionDetail)
	}
	if len(uris) > MaxURIs {
		return nil, Problem(
			SlugValidationFailed,
			http.StatusUnprocessableEntity,
			fmt.Sprintf(tooManyURIsFormat, len(uris), MaxURIs),
		)
	}
	// The schema's maxItems tag answers a JSON body first; this runtime
	// check is the same defense-in-depth the uris family gets — any path
	// that bypasses schema validation still faces the cap.
	if len(searchResultIDs) > MaxURIs {
		return nil, Problem(
			SlugValidationFailed,
			http.StatusUnprocessableEntity,
			fmt.Sprintf(tooManyResultsFormat, len(searchResultIDs), MaxURIs),
		)
	}

	category, err := h.resolveCategory(ctx, in.Body.Category)
	if err != nil {
		return nil, err
	}
	var categoryID *string
	if category != nil {
		categoryID = &category.ID
	}

	destination, err := h.resolveDestination(ctx, in.Body.Destination, category)
	if err != nil {
		return nil, err
	}

	// The within-submission duplicate set spans URI identities and torrent
	// infohashes alike: one submission cannot plan the same torrent twice.
	seenInfohashes := map[string]bool{}

	var planned []plannedTask
	// searchResultsResolved reports whether any res_ id resolved at all —
	// the all-fail 404 below is for a submission whose every id is unknown
	// or expired, distinct from resolved-then-refused ids, which keep the
	// partial-success 201.
	searchResultsResolved := false
	// allURIsBlocked is the create contract's "every URI was blocked"
	// case: every submitted URI earned an ssrf rejection, so the answer is
	// the 403 problem. A blocked URI mixed with other refusals stays the
	// all-rejected 422 of doc 05 section 5.2.
	allURIsBlocked := false
	if len(searchResultIDs) > 0 {
		var resRejected []RejectedURI
		planned, resRejected, searchResultsResolved, err = h.planSearchResults(
			ctx, searchResultIDs, in.Body.Engine, destination, seenInfohashes)
		if err != nil {
			return nil, err
		}
		rejected = append(rejected, resRejected...)
	} else {
		blobPlanned, blobRejected, err := h.planBlobParts(ctx, blobs, in.Body.Engine, destination, in.Body.CreateSubfolder, seenInfohashes)
		if err != nil {
			return nil, err
		}
		rejected = append(rejected, blobRejected...)

		mergedBody := in.Body
		mergedBody.URIs = uris
		uriPlanned, uriRejected, err := h.planURIs(ctx, mergedBody, destination, seenInfohashes)
		if err != nil {
			return nil, err
		}
		rejected = append(rejected, uriRejected...)
		planned = append(uriPlanned, blobPlanned...)

		ssrfRejections := 0
		for _, r := range uriRejected {
			if r.Type == SlugSSRFBlocked {
				ssrfRejections++
			}
		}
		allURIsBlocked = len(uris) > 0 && ssrfRejections == len(uris)
	}

	// select_files precedes every insert: a refusal creates nothing. The
	// resolved intent lands on the one task it addresses, and insertPlanned
	// persists it for the admission pass to apply.
	target, selection, err := h.validateSelection(ctx, &in.Body, planned)
	if err != nil {
		return nil, err
	}
	if selection != nil {
		planned[target].selection = selection
	}

	if len(planned) == 0 {
		if len(searchResultIDs) > 0 {
			if !searchResultsResolved {
				// Unknown and expired ids are indistinguishable, and the
				// answer carries no provider data — 404, no rejected[].
				return nil, Problem(SlugNotFound, http.StatusNotFound, noSearchResultsDetail)
			}
			// Every id resolved but each was refused (a duplicate already
			// committed is terminal success for the caller): 201 with an
			// empty created[] and the per-id rejected[] entries.
			output := &CreateTasksOutput{Status: http.StatusCreated}
			output.Body.Created = []TaskDTO{}
			output.Body.Rejected = rejected

			return output, nil
		}
		// Every submission refused: the top-level detail carries the first
		// rejection's reason — for an ed2k-only submission exactly the
		// message of doc 06 section 2 row 7.
		detail := allRejectedDetail
		if len(rejected) > 0 {
			detail = rejected[0].Detail
		}

		return nil, Problem(SlugUnsupportedScheme, http.StatusUnprocessableEntity, detail)
	}

	// Tag rows are created up-front, before any task insert, so a tag-name
	// conflict surfaces before the batch starts. Task inserts themselves can
	// still fail mid-batch (the infohash race duplicateInfohash notes), which
	// leaves a partial submission behind; wrap ensureTags plus the insert
	// loop in a transaction once the store grows a tx-bound Create.
	if err := h.ensureTags(ctx, in.Body.Tags); err != nil {
		return nil, internalFailure(ctx, "create tags", err)
	}

	created := make([]TaskDTO, 0, len(planned))
	for _, p := range planned {
		dto, err := h.insertPlanned(ctx, p, &in.Body, categoryID)
		if err != nil {
			return nil, err
		}
		// A blocked URI's row exists so the refusal is inspectable, but it
		// is a rejection, not a creation: rejected[] already carries it.
		if p.blocked {
			continue
		}
		created = append(created, dto)
	}
	if len(created) == 0 {
		if allURIsBlocked {
			// Every planned task was a blocked URI and nothing else went
			// wrong: the whole submission was refused by the guard, and
			// the answer is the 403 problem, not an empty 201.
			return nil, Problem(SlugSSRFBlocked, http.StatusForbidden, ssrfAllBlockedDetail)
		}
		// created is empty but a URI was refused for another reason (an
		// unsupported scheme, a routing miss): the submission shares the
		// all-refused 422 shape of the len(planned) == 0 branch above.
		detail := allRejectedDetail
		if len(rejected) > 0 {
			detail = rejected[0].Detail
		}

		return nil, Problem(SlugUnsupportedScheme, http.StatusUnprocessableEntity, detail)
	}

	output := &CreateTasksOutput{Status: http.StatusCreated}
	output.Body.Created = created
	output.Body.Rejected = rejected

	return output, nil
}

// planBlobParts turns the blob uploads of the form into planned tasks: one
// per .torrent or .metalink part, routed by the raw-bytes rows of the
// routing table (rows 2 and 6). An explicit engine override applies to URI
// submissions only: a blob upload carries no URI for Accepts to judge, so a
// mismatch with the table's route is a per-part rejection, never a silent
// re-route.
func (h *TaskHandlers) planBlobParts(
	ctx context.Context,
	blobs []uploadBlob,
	engineOverride, destination string,
	createSubfolder bool,
	seen map[string]bool,
) ([]plannedTask, []RejectedURI, error) {
	planned := []plannedTask{}
	rejected := []RejectedURI{}

	for _, blob := range blobs {
		engineName := engine.NameQBittorrent
		if blob.kind == uploadKindMetalink {
			engineName = engine.NameAria2
		}
		if engineOverride != "" && engineOverride != engineName {
			rejected = append(rejected, RejectedURI{
				URI:    blob.name,
				Type:   SlugUnsupportedScheme,
				Detail: fmt.Sprintf(engineRefusesURIFmt, engineOverride),
			})

			continue
		}
		if _, ok := h.engines.Get(engineName); !ok {
			return nil, nil, engineUnavailable(engineName)
		}

		if blob.kind == uploadKindMetalink {
			// A metalink has no URI identity to store: the row holds the
			// part's display name and the aria2 routing of the table's row 6,
			// and the admission pass owns the engine handoff.
			planned = append(planned, plannedTask{
				normalized:  uri.Normalized{Kind: uri.KindMetalink},
				engine:      engine.NameAria2,
				name:        blob.name,
				destination: destination,
			})

			continue
		}

		p, rejection, err := h.planTorrentPart(ctx, blob, destination, createSubfolder, seen)
		if err != nil {
			return nil, nil, err
		}
		if rejection != nil {
			rejected = append(rejected, *rejection)

			continue
		}
		planned = append(planned, *p)
	}

	return planned, rejected, nil
}

// planTorrentPart parses one uploaded .torrent into a planned task. Its
// stored source is the magnet the infohash rebuilds, so the admission pass
// and the reconciler can resubmit the task after the uploaded bytes are
// gone; its manifest drives create_subfolder and select_files. A duplicate
// infohash is a per-part conflict rejection, the URI path's own rule.
func (h *TaskHandlers) planTorrentPart(
	ctx context.Context,
	part uploadBlob,
	destination string,
	createSubfolder bool,
	seen map[string]bool,
) (*plannedTask, *RejectedURI, error) {
	// classifyUpload already proved the bytes are bencode; this parse
	// demands the full metainfo shape.
	manifest, err := uri.InspectTorrent(part.bytes)
	if err != nil {
		rejection := RejectedURI{URI: part.name, Type: SlugValidationFailed, Detail: sentinelDetail(err, uri.ErrNotTorrent)}

		return nil, &rejection, nil
	}

	normalized := uri.Normalized{
		Kind:        uri.KindTorrent,
		URI:         magnetFromManifest(manifest),
		DisplayName: manifest.Name,
		InfohashV1:  manifest.InfohashV1,
		InfohashV2:  manifest.InfohashV2,
	}

	duplicate, err := h.duplicateRejection(ctx, normalized, seen, part.name)
	if err != nil {
		return nil, nil, err
	}
	if duplicate != nil {
		return nil, duplicate, nil
	}
	markPlanned(seen, normalized)

	p := &plannedTask{
		normalized:  normalized,
		engine:      engine.NameQBittorrent,
		destination: destination,
		manifest:    &manifest,
	}

	// create_subfolder applies once the manifest is known (FR-008): a
	// multi-file manifest's content lands in <destination>/<manifest name>/,
	// sanitised and re-resolved against the roots.
	if createSubfolder && len(manifest.Files) > 1 {
		effective, err := subfolderDestination(h.roots, destination, manifest.Name, true)
		if err != nil {
			return nil, nil, destinationRejected(destination)
		}
		p.destination = effective
	}

	return p, nil, nil
}

// magnetFromManifest rebuilds the submit URI of an uploaded torrent from
// its infohash — the v1 hash when present, the v2 hash otherwise — plus the
// display name, the same identity a magnet submission would have carried.
func magnetFromManifest(m uri.Manifest) string {
	if m.InfohashV1 != "" {
		return "magnet:?xt=urn:btih:" + m.InfohashV1
	}

	return "magnet:?xt=urn:btmh:" + m.InfohashV2
}

// validateSelection enforces the create-time selection rules of doc 05
// section 5.2, before any row is written: select_files applies to the
// first multi-file manifest of the submission, the routed engine must
// declare per_file_select, and a high or maximum priority is 422 unless
// the engine declares per_file_priority (task step 7) — skip and normal
// are selection outcomes every per_file_select engine honours. It
// returns the index of the one task the entries address and the resolved
// intent; the caller attaches it so the row persists it for the
// admission pass, which owns Engine.Add.
func (h *TaskHandlers) validateSelection(ctx context.Context, body *CreateTasksBody, planned []plannedTask) (int, *store.SelectionIntent, error) {
	if len(body.SelectFiles) == 0 {
		return -1, nil, nil
	}

	target, engineName, fileCount, ok := selectionTarget(planned)
	if !ok {
		return -1, nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, selectionNoManifestDetail)
	}

	e, registered := h.engines.Get(engineName)
	if !registered {
		return -1, nil, engineUnavailable(engineName)
	}
	if !hasCapability(e, engine.CapPerFileSelect) {
		return -1, nil, Problem(
			SlugValidationFailed,
			http.StatusUnprocessableEntity,
			fmt.Sprintf(selectionCapDetailFormat, engineName),
		)
	}

	indices, priorities, err := applySelection(body.SelectFiles, fileCount)
	if err != nil {
		return -1, nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, sentinelDetail(err, ErrInvalidSelection))
	}

	if !hasCapability(e, engine.CapPerFilePriority) {
		for _, priority := range priorities {
			if priority == priorityHigh || priority == priorityMaximum {
				return -1, nil, Problem(
					SlugValidationFailed,
					http.StatusUnprocessableEntity,
					fmt.Sprintf(selectionPrioCapFormat, engineName),
				)
			}
		}
	}

	return target, &store.SelectionIntent{Indices: indices, Priorities: priorities}, nil
}

// selectionTarget finds the submission the select_files entries address
// (doc 05 section 5.2: the first multi-file manifest): an uploaded torrent
// known to hold more than one file wins; otherwise the first submission
// whose manifest cannot be known at create time — a magnet or torrent URI,
// an unparsed metalink part — which the engine judges at add time. Only
// known single-file submissions remain, and those cannot take a selection.
func selectionTarget(planned []plannedTask) (target int, engineName string, fileCount int, ok bool) {
	for i, p := range planned {
		if p.manifest != nil && len(p.manifest.Files) > 1 {
			return i, p.engine, len(p.manifest.Files), true
		}
	}
	for i, p := range planned {
		// A blocked URI's task is terminal before it exists; its manifest
		// can never resolve, so it cannot be the selection target.
		unknown := !p.blocked && p.manifest == nil &&
			(p.normalized.Kind == uri.KindMagnet || p.normalized.Kind == uri.KindTorrent || p.normalized.Kind == uri.KindMetalink)
		if unknown {
			return i, p.engine, -1, true
		}
	}

	return -1, "", 0, false
}

// planURIs normalises and routes every URI, collecting a rejection for each
// refused one. It returns an error only for the whole-request failures: an
// explicit engine that is not registered, or a routed engine that is not.
func (h *TaskHandlers) planURIs(
	ctx context.Context,
	body CreateTasksBody,
	destination string,
	seen map[string]bool,
) ([]plannedTask, []RejectedURI, error) {
	planned := make([]plannedTask, 0, len(body.URIs))
	rejected := []RejectedURI{}

	// The tasks table forbids a live duplicate of an infohash (partial unique
	// indexes), so a repeated torrent would otherwise fail the INSERT. Both
	// an existing row and a repeat within this submission become a per-URI
	// conflict rejection instead; the seen map is shared with the blob
	// planning, so an uploaded .torrent and its magnet are one duplicate too.

	for _, raw := range body.URIs {
		n, err := normaliseSubmission(raw)
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

		// The SSRF preflight runs on the normalised URI — userinfo stripped,
		// obfuscated inner URI already decoded — before the task is planned.
		// A blocked URI still gets its row, created already terminal in
		// error: the refusal stays inspectable and resubmission is
		// idempotent, and because the row never sits in queued the admission
		// pass can never hand it to an engine. The blocked row is not a
		// duplicate holder either, so markPlanned stays skipped.
		if err := h.preflight(ctx, n.URI); err != nil {
			rejected = append(rejected, ssrfRejection(raw))
			planned = append(planned, plannedTask{
				normalized:  n,
				engine:      engineName,
				destination: destination,
				blocked:     true,
			})

			continue
		}

		duplicate, err := h.duplicateRejection(ctx, n, seen, raw)
		if err != nil {
			return nil, nil, err
		}
		if duplicate != nil {
			rejected = append(rejected, *duplicate)

			continue
		}
		markPlanned(seen, n)

		planned = append(planned, plannedTask{normalized: n, engine: engineName, destination: destination})
	}

	return planned, rejected, nil
}

// planSearchResults resolves each res_ id to its stored row — while its
// search job is live — and feeds the preferred acquisition source
// (magnet_uri first, else download_url) through the same normalise → route →
// duplicate pipeline the uris family uses. The resolved URI is server-only:
// it lands in source_uri, while source_display_uri records the opaque
// search-result:<res_id> reference the API renders, and rejected[] entries
// carry search_result_id, never the resolved source. The bool reports
// whether at least one id resolved, which decides the all-fail 404.
func (h *TaskHandlers) planSearchResults(
	ctx context.Context,
	ids []string,
	engineOverride, destination string,
	seen map[string]bool,
) ([]plannedTask, []RejectedURI, bool, error) {
	planned := make([]plannedTask, 0, len(ids))
	rejected := []RejectedURI{}
	resolvedAny := false
	seenIDs := map[string]bool{}

	for _, id := range ids {
		// Duplicate detection runs on the raw ids before any resolution: a
		// repeated id is /problems/conflict even when its first occurrence
		// is /problems/not-found.
		if seenIDs[id] {
			rejected = append(rejected, RejectedURI{
				SearchResultID: id,
				Type:           SlugConflict,
				Detail:         duplicateResultDetail,
			})

			continue
		}
		seenIDs[id] = true

		row, err := store.GetSearchResult(ctx, h.db, id)
		if errors.Is(err, store.ErrNotFound) {
			rejected = append(rejected, RejectedURI{
				SearchResultID: id,
				Type:           SlugNotFound,
				Detail:         searchResultGoneDetail,
			})

			continue
		}
		if err != nil {
			return nil, nil, false, internalFailure(ctx, "resolve search result", err)
		}
		resolvedAny = true

		// The magnet is the preferred acquisition source (doc 05 section
		// 9.2); a result without one falls back to its download URL.
		raw := ""
		if row.MagnetURI != nil && *row.MagnetURI != "" {
			raw = *row.MagnetURI
		} else if row.DownloadURL != nil {
			raw = *row.DownloadURL
		}

		n, err := normaliseSubmission(raw)
		if err != nil {
			rejected = append(rejected, rejectSearchResult(id, err))

			continue
		}

		engineName, err := engine.Route(n, nil)
		if err != nil {
			rejected = append(rejected, rejectSearchResult(id, err))

			continue
		}

		// An explicit engine override is honoured exactly as for a URI
		// submission: the chosen engine must be registered and accept the
		// resolved source, else this one result is refused.
		if engineOverride != "" && engineOverride != engineName {
			chosen, ok := h.engines.Get(engineOverride)
			if !ok {
				return nil, nil, false, engineUnavailable(engineOverride)
			}
			if !chosen.Accepts(n.URI) {
				rejected = append(rejected, RejectedURI{
					SearchResultID: id,
					Type:           SlugUnsupportedScheme,
					Detail:         fmt.Sprintf(engineRefusesURIFmt, engineOverride),
				})

				continue
			}
			engineName = engineOverride
		}
		if _, ok := h.engines.Get(engineName); !ok {
			return nil, nil, false, engineUnavailable(engineName)
		}

		duplicate, err := h.duplicateRejection(ctx, n, seen, "")
		if err != nil {
			return nil, nil, false, err
		}
		if duplicate != nil {
			duplicate.SearchResultID = id
			rejected = append(rejected, *duplicate)

			continue
		}
		markPlanned(seen, n)

		display := searchResultDisplayPrefix + id
		planned = append(planned, plannedTask{
			normalized:    n,
			engine:        engineName,
			destination:   destination,
			displaySource: &display,
		})
	}

	return planned, rejected, resolvedAny, nil
}

// insertPlanned persists one planned task, links its tags, records the
// requested-destination echo, seeds its create-time file selection and
// renders the response DTO.
func (h *TaskHandlers) insertPlanned(
	ctx context.Context,
	p plannedTask,
	body *CreateTasksBody,
	categoryID *string,
) (TaskDTO, error) {
	n := p.normalized

	state := string(engine.StateQueued)
	if body.Paused {
		state = string(engine.StatePaused)
	}

	// A URI the SSRF preflight refused lands directly in error with its
	// stamp: creating it queued first would let the 1 Hz admission pass
	// claim it between the two writes, and the engine must never see the
	// URI (task step 5's insert-then-transition sketch predates the
	// admission pass owning Engine.Add).
	var errorCode, errorMessage *string
	if p.blocked {
		state = string(engine.StateError)
		code := errorCodeSSRFBlocked
		message := ssrfBlockedMessage
		errorCode = &code
		errorMessage = &message
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

	name := p.name
	if name == "" {
		name = displayName(n)
	}
	if name == "" {
		// A metalink part sent without a filename still gets a stable label.
		name = string(n.Kind)
	}

	// A parsed torrent knows its total already; every other submission
	// leaves it unknown until an engine reports it.
	var totalBytes *int64
	if p.manifest != nil {
		size := p.manifest.TotalSize
		totalBytes = &size
	}

	var selectFiles *string
	if p.selection != nil {
		raw, err := json.Marshal(p.selection)
		if err != nil {
			return TaskDTO{}, internalFailure(ctx, "encode file selection", err)
		}
		text := string(raw)
		selectFiles = &text
	}

	// CreateLogged writes the row and its task.created event in one
	// transaction (FR-150): a task can never persist without the first
	// entry of its event log.
	task, err := h.tasks.CreateLogged(ctx, store.Task{
		Engine:           p.engine,
		SourceKind:       string(n.Kind),
		SourceURI:        stringOrNil(source),
		SourceDisplayURI: p.displaySource,
		Name:             name,
		InfohashV1:       stringOrNil(n.InfohashV1),
		InfohashV2:       stringOrNil(n.InfohashV2),
		State:            state,
		ErrorCode:        errorCode,
		ErrorMessage:     errorMessage,
		Destination:      p.destination,
		CategoryID:       categoryID,
		Sequential:       boolToInt(body.Sequential),
		TotalBytes:       totalBytes,
		SelectFiles:      selectFiles,
		// extract_password and create_subfolder have no store.Task field yet:
		// their columns are owned by the auto-extract and upload tasks, which
		// extend the store with them.
	})
	if err != nil {
		return TaskDTO{}, internalFailure(ctx, "create task", err)
	}

	if err := h.linkTags(ctx, task.ID, body.Tags); err != nil {
		return TaskDTO{}, internalFailure(ctx, "link tags", err)
	}

	// The canonical object echoes what the client asked for whenever the
	// server resolved it to something else (doc 05 section 3): a subfoldered
	// destination, or an alias of the configured root. The row carries the
	// same echo in requested_destination (FR-044).
	var requested *string
	if body.Destination != "" && filepath.Clean(body.Destination) != task.Destination {
		echo := body.Destination
		requested = &echo
		if err := h.recordRequestedDestination(ctx, task.ID, echo); err != nil {
			return TaskDTO{}, internalFailure(ctx, "record requested destination", err)
		}
	}

	var category *string
	if body.Category != "" {
		category = &body.Category
	}

	dto := newTaskDTO(task, "", category, body.Tags)
	// The response source is the shared rule every task-emitting path uses:
	// a search-result task renders its opaque search-result:<res_id>, a
	// metalink part — stored without any URI — renders null, and every
	// other source is the stored URI with credentials and query stripped.
	dto.SourceURI = isync.DisplaySourceURI(task)
	dto.RequestedDestination = requested

	return dto, nil
}

// recordRequestedDestination writes the requested_destination echo of a
// task whose effective destination differs from what the client asked for
// (FR-044, doc 04 section 3.3).
func (h *TaskHandlers) recordRequestedDestination(ctx context.Context, taskID, requested string) error {
	if _, err := h.db.ExecContext(ctx, querySetRequestedDestination, requested, taskID); err != nil {
		return fmt.Errorf("record requested destination of %q: %w", taskID, err)
	}

	return nil
}

// resolveCategory maps a category name to its row. The category must
// already exist (doc 05 section 5.2); an unknown name is a validation
// failure before any row is written. The row — not just the id — comes
// back because its save_path is a destination-resolution candidate.
func (h *TaskHandlers) resolveCategory(ctx context.Context, name string) (*store.Category, error) {
	if name == "" {
		return nil, nil
	}

	category, err := h.settings.CategoryByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, Problem(
			SlugValidationFailed,
			http.StatusUnprocessableEntity,
			fmt.Sprintf(unknownCategoryFormat, name),
		)
	}
	if err != nil {
		return nil, internalFailure(ctx, "resolve category", err)
	}

	return &category, nil
}

// resolveDestination applies the destination resolution table of doc 05
// section 5.2: an explicit request destination wins; otherwise the
// category's save_path — a category whose save_path is empty falls
// through; otherwise the default_destination settings row read through
// SettingsStore.DefaultDestination — which the migration seeds no row
// for, so an unset or empty value leaves the empty string that
// ResolveDestination answers with the first root. Every candidate goes
// through the one fsx.ResolveDestination call, so an out-of-roots answer
// is 403 /problems/path-rejected wherever the value came from.
func (h *TaskHandlers) resolveDestination(ctx context.Context, requested string, category *store.Category) (string, error) {
	candidate := requested
	if candidate == "" && category != nil {
		candidate = category.SavePath
	}
	if candidate == "" {
		stored, err := h.settings.DefaultDestination(ctx)
		if err != nil {
			return "", internalFailure(ctx, "read default destination", err)
		}
		candidate = stored
	}

	destination, err := fsx.ResolveDestination(h.roots, candidate)
	if err != nil {
		return "", destinationRejected(candidate)
	}

	return destination, nil
}

// ensureTags creates the tag rows of a submission (doc 05 section 5.2,
// "created on demand") before any task insert. The tag store arrives with
// its own task; until then this statement is the only tag access the create
// path needs.
func (h *TaskHandlers) ensureTags(ctx context.Context, names []string) error {
	now := time.Now().UnixMilli()

	for _, name := range names {
		if _, err := h.db.ExecContext(ctx, queryInsertTag, store.NewID(store.PrefixTag), name, now, now); err != nil {
			return fmt.Errorf("create tag %q: %w", name, err)
		}
	}

	return nil
}

// linkTags links a task to its tag rows. The rows exist by the time this
// runs: ensureTags ran before the task inserts.
func (h *TaskHandlers) linkTags(ctx context.Context, taskID string, names []string) error {
	for _, name := range names {
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

// duplicateRejection reports whether a live task already carries n's
// infohash or the same submission already planned it, as the rejected[]
// entry the response carries: /problems/conflict — the slug registry of
// doc 05 section 1.3 is closed — with a detail naming the existing task
// id, or naming the submission itself when the duplicate is its own
// repeat and no row exists yet to name. The lookup is the store's
// FindByInfohash: both columns together, never one at a time, never
// engine_ref.
func (h *TaskHandlers) duplicateRejection(ctx context.Context, n uri.Normalized, seen map[string]bool, display string) (*RejectedURI, error) {
	if n.InfohashV1 == "" && n.InfohashV2 == "" {
		return nil, nil
	}
	if plannedAlready(seen, n) {
		entry := RejectedURI{URI: display, Type: SlugConflict, Detail: duplicateRepeatDetail}

		return &entry, nil
	}

	existing, err := h.tasks.FindByInfohash(ctx, n.InfohashV1, n.InfohashV2)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, internalFailure(ctx, "check duplicate torrent", err)
	}

	entry := RejectedURI{
		URI:    display,
		Type:   SlugConflict,
		Detail: fmt.Sprintf(duplicateDetailFormat, duplicateDetail, existing.ID),
	}

	return &entry, nil
}

// markPlanned records a planned torrent's identity in the
// within-submission duplicate set, one key per hash: a hybrid torrent
// planned in full form (both hashes) and then submitted by its bare v1 or
// v2 hash alone — or the reverse — must collide on the hash they share,
// not slip past on the one they do not.
func markPlanned(seen map[string]bool, n uri.Normalized) {
	if n.InfohashV1 != "" {
		seen[seenKeyV1+n.InfohashV1] = true
	}
	if n.InfohashV2 != "" {
		seen[seenKeyV2+n.InfohashV2] = true
	}
}

// plannedAlready reports whether the duplicate set already holds either
// of n's hashes.
func plannedAlready(seen map[string]bool, n uri.Normalized) bool {
	if n.InfohashV1 != "" && seen[seenKeyV1+n.InfohashV1] {
		return true
	}

	return n.InfohashV2 != "" && seen[seenKeyV2+n.InfohashV2]
}

// The two within-submission duplicate-set key prefixes, one per column.
const (
	seenKeyV1 = "v1|"
	seenKeyV2 = "v2|"
)

// normaliseSubmission classifies one submitted URI, resolving its
// BitTorrent identity before any routing decision: a magnet through
// uri.ParseMagnet (inside uri.Normalize), a bare infohash through
// store.NormaliseInfohash — the fourth shape of the routing table's row 2
// (docs/06-download-engines.md section 2), which carries no scheme for
// uri.Normalize to classify, so it becomes the magnet of its own hash and
// enters the same path a magnet submission takes.
func normaliseSubmission(raw string) (uri.Normalized, error) {
	if hash, err := store.NormaliseInfohash(raw); err == nil && hash != "" {
		return magnetOfBareInfohash(hash), nil
	}

	return uri.Normalize(raw)
}

// magnetOfBareInfohash rebuilds the submit URI of a bare infohash from its
// normalised form: the v1 hash rides urn:btih, the v2 hash the
// urn:btmh multihash whose digest it is. The column width tells the forms
// apart — 40 hex is a v1 identity, 64 a v2.
func magnetOfBareInfohash(hash string) uri.Normalized {
	if len(hash) == 40 {
		return uri.Normalized{
			Kind:       uri.KindMagnet,
			URI:        "magnet:?xt=urn:btih:" + hash,
			InfohashV1: hash,
		}
	}

	return uri.Normalized{
		Kind:       uri.KindMagnet,
		URI:        "magnet:?xt=urn:btmh:1220" + hash,
		InfohashV2: hash,
	}
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

// rejectSearchResult renders one rejected[] entry for a search result whose
// resolved source normalising or routing refused. The entry names the res_
// id, never the provider source — the resolved URI is server-only.
func rejectSearchResult(id string, err error) RejectedURI {
	detail := uriRejectedDetail
	if reason, ok := strings.CutPrefix(err.Error(), uri.ErrUnsupportedScheme.Error()+": "); ok {
		detail = reason
	}

	return RejectedURI{SearchResultID: id, Type: SlugUnsupportedScheme, Detail: detail}
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

// ListTasks serves GET /tasks: one cursor-paginated page of canonical
// Task objects, filtered, sorted and counted by the store (doc 05 5.1).
func (h *TaskHandlers) ListTasks(ctx context.Context, in *ListTasksInput) (*ListTasksOutput, error) {
	filter := store.TaskFilter{
		State:    in.State,
		Category: in.Category,
		Tag:      in.Tag,
		Query:    in.Q,
		Sort:     in.Sort,
		Limit:    in.Limit,
		Cursor:   in.Cursor,
	}
	// An empty ?category= / ?tag= is the uncategorised / untagged filter;
	// Huma cannot tell it from an absent parameter, so read presence from
	// the raw query the operation middleware stashed. ParseQuery returns
	// the pairs it could decode even when another pair is malformed
	// (e.g. ?q=100% with a lone percent sign); keep the partial result so
	// ?category= and ?tag= still count as present.
	if values, ok := rawQueryValues(ctx); ok {
		_, filter.HasCategory = values["category"]
		_, filter.HasTag = values["tag"]
	}

	rows, nextCursor, total, err := h.tasks.ListTasks(ctx, filter)
	if err != nil {
		return nil, listTasksProblem(ctx, err, filter)
	}

	items, err := h.renderTasks(ctx, rows)
	if err != nil {
		return nil, err
	}

	output := &ListTasksOutput{}
	output.Body.Items = items
	output.Body.Total = total
	if nextCursor != "" {
		output.Body.NextCursor = &nextCursor
	}

	return output, nil
}

// GetTask serves GET /tasks/{id}: one canonical Task object, 404 for an
// unknown id (doc 05 5.4).
func (h *TaskHandlers) GetTask(ctx context.Context, in *GetTaskInput) (*GetTaskOutput, error) {
	task, err := h.tasks.Get(ctx, in.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, Problem(SlugNotFound, http.StatusNotFound, "the addressed task does not exist")
	}
	if err != nil {
		return nil, internalFailure(ctx, "get task", err)
	}

	items, err := h.renderTasks(ctx, []store.Task{task})
	if err != nil {
		return nil, err
	}

	return &GetTaskOutput{Body: items[0]}, nil
}

// listTasksProblem maps the store's list errors onto the registered
// problem slugs: an unknown sort key, state or cursor is 422 with the
// offending field located in errors[]; anything else is internal.
func listTasksProblem(ctx context.Context, err error, filter store.TaskFilter) error {
	var field, detail string
	switch {
	case errors.Is(err, store.ErrInvalidSort):
		field = "query.sort"
		detail = "the sort key is not a sortable column"
	case errors.Is(err, store.ErrUnknownFilterState):
		field = "query.state"
		detail = "state is neither a canonical state nor a sidebar filter"
	case errors.Is(err, store.ErrStaleCursor):
		field = "query.cursor"
		detail = "the cursor was issued for a different filter or sort"
	default:
		return internalFailure(ctx, "list tasks", err)
	}

	return &huma.ErrorModel{
		Type:   SlugValidationFailed,
		Title:  http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity,
		Detail: detail,
		Errors: []*huma.ErrorDetail{{Message: detail, Location: field}},
	}
}

// renderTasks turns stored rows into canonical Task objects: category and
// tag names resolved for the page, the API-safe source URI and the derived
// progress (doc 05 section 3).
func (h *TaskHandlers) renderTasks(ctx context.Context, rows []store.Task) ([]TaskDTO, error) {
	items := make([]TaskDTO, 0, len(rows))
	if len(rows) == 0 {
		return items, nil
	}

	categories, err := h.categoryNames(ctx, rows)
	if err != nil {
		return nil, err
	}
	tags, err := h.tagNamesByTask(ctx, rows)
	if err != nil {
		return nil, err
	}

	for _, row := range rows {
		var category *string
		if row.CategoryID != nil {
			if name, ok := categories[*row.CategoryID]; ok {
				category = &name
			}
		}

		dto := newTaskDTO(row, "", category, tags[row.ID])
		dto.SourceURI = isync.DisplaySourceURI(row)
		dto.Progress = taskProgress(row)
		items = append(items, dto)
	}

	return items, nil
}

// categoryNames maps the page's category ids to their names. The foreign
// key sets the column NULL on category deletion, so a missing id cannot
// happen; the lookup stays defensive anyway.
func (h *TaskHandlers) categoryNames(ctx context.Context, rows []store.Task) (map[string]string, error) {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.CategoryID != nil {
			ids = append(ids, *row.CategoryID)
		}
	}
	if len(ids) == 0 {
		return map[string]string{}, nil
	}

	query, args, err := sqlx.In(queryCategoryNamesByIDs, ids)
	if err != nil {
		return nil, internalFailure(ctx, "list task categories", err)
	}

	var pairs []struct {
		ID   string `db:"id"`
		Name string `db:"name"`
	}
	if err := h.db.SelectContext(ctx, &pairs, query, args...); err != nil {
		return nil, internalFailure(ctx, "list task categories", err)
	}

	names := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		names[pair.ID] = pair.Name
	}

	return names, nil
}

// tagNamesByTask maps the page's task ids to their tag names, in name
// order.
func (h *TaskHandlers) tagNamesByTask(ctx context.Context, rows []store.Task) (map[string][]string, error) {
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}

	query, args, err := sqlx.In(queryTagNamesByTaskIDs, ids)
	if err != nil {
		return nil, internalFailure(ctx, "list task tags", err)
	}

	var pairs []struct {
		TaskID string `db:"task_id"`
		Name   string `db:"name"`
	}
	if err := h.db.SelectContext(ctx, &pairs, query, args...); err != nil {
		return nil, internalFailure(ctx, "list task tags", err)
	}

	tags := make(map[string][]string, len(pairs))
	for _, pair := range pairs {
		tags[pair.TaskID] = append(tags[pair.TaskID], pair.Name)
	}

	return tags, nil
}

// taskProgress derives progress as completed_bytes / total_bytes, 0.0
// while total_bytes is null or zero (doc 05 section 3).
func taskProgress(t store.Task) float64 {
	if t.TotalBytes == nil || *t.TotalBytes == 0 {
		return 0
	}

	return min(float64(t.CompletedBytes)/float64(*t.TotalBytes), 1.0)
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
