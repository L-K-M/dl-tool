package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/uri"
)

const (
	operationInspectTasks = "inspect-tasks"

	// maxInspectBlobBytes is the decoded .torrent cap of doc 05 section 5.3.
	maxInspectBlobBytes = 10 << 20 // 10 MiB
)

// magnetInspector is implemented by an engine that can resolve magnet metadata
// without creating a task. The qBittorrent implementation lands in T038; while
// no registered engine implements it, a magnet submission returns
// metadata_pending true with files null.
type magnetInspector interface {
	InspectMagnet(ctx context.Context, magnet string) (uri.Manifest, error)
}

// InspectTasksBody is the JSON body of POST /tasks/inspect: the same uris,
// blob and filename fields POST /tasks takes; everything else is ignored.
type InspectTasksBody struct {
	URIs     []string `json:"uris"             maxItems:"50" doc:"One entry per submission; http(s), ftp(s), sftp, magnet and the obfuscated schemes"`
	Blob     string   `json:"blob,omitempty"   doc:"A base64-encoded .torrent file, 10 MiB decoded maximum"`
	Filename string   `json:"filename,omitempty" doc:"Display name for a blob submission"`
}

// InspectTasksInput is the operation input carrying InspectTasksBody.
type InspectTasksInput struct {
	Body InspectTasksBody
}

// ManifestFileDTO is one file of a ManifestDTO. Size is null when unknown —
// for a torrent it is always known.
type ManifestFileDTO struct {
	Index int    `json:"index"`
	Path  string `json:"path" doc:"Relative, cleaned; never absolute, never containing \"..\""`
	Size  *int64 `json:"size"`
}

// ManifestDTO is the inspect-before-commit result for one submission. Producing
// it never touches disk and never creates an engine task (doc 05 section 5.3).
type ManifestDTO struct {
	SourceURI       string            `json:"source_uri" doc:"The submission as sent; a search result renders as search-result:<res_id>"`
	Kind            string            `json:"kind"       doc:"The source_kind vocabulary of docs/04-data-model.md section 3.3"`
	Name            string            `json:"name"`
	TotalSize       *int64            `json:"total_size"`
	FileCount       *int              `json:"file_count"`
	MetadataPending bool              `json:"metadata_pending" doc:"True when magnet metadata did not arrive inside the deadline; files is then null and the UI offers add-paused"`
	InfohashV1      *string           `json:"infohash_v1"`
	InfohashV2      *string           `json:"infohash_v2"`
	Files           []ManifestFileDTO `json:"files"`
}

// InspectTasksOutput carries one manifest per accepted submission.
type InspectTasksOutput struct {
	Body struct {
		Manifests []ManifestDTO `json:"manifests"`
		Rejected  []RejectedURI `json:"rejected"`
	}
}

// InspectTasks serves POST /tasks/inspect: one manifest per accepted
// submission, without creating a task, writing to disk or inserting a tasks
// row. The only permitted engine contact is the metadata-only magnet fetch.
func (h *TaskHandlers) InspectTasks(ctx context.Context, in *InspectTasksInput) (*InspectTasksOutput, error) {
	if len(in.Body.URIs) == 0 && in.Body.Blob == "" {
		return nil, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, emptySubmissionDetail)
	}

	output := &InspectTasksOutput{}
	output.Body.Manifests = []ManifestDTO{}
	output.Body.Rejected = []RejectedURI{}

	if in.Body.Blob != "" {
		manifest, err := h.inspectBlob(in.Body)
		if err != nil {
			return nil, err
		}
		output.Body.Manifests = append(output.Body.Manifests, manifest)
	}

	for _, raw := range in.Body.URIs {
		manifest, rejection, err := h.inspectURI(ctx, raw)
		if err != nil {
			return nil, err
		}
		if rejection != nil {
			output.Body.Rejected = append(output.Body.Rejected, *rejection)

			continue
		}
		output.Body.Manifests = append(output.Body.Manifests, *manifest)
	}

	// Every entry refused is the create endpoint's behaviour mirrored: 422
	// with the first rejection's reason (doc 05 section 5.3).
	if len(output.Body.Manifests) == 0 {
		detail := allRejectedDetail
		if len(output.Body.Rejected) > 0 {
			detail = output.Body.Rejected[0].Detail
		}

		return nil, Problem(SlugUnsupportedScheme, http.StatusUnprocessableEntity, detail)
	}

	return output, nil
}

// inspectBlob turns a base64 .torrent blob into its manifest DTO. The size
// cap is checked on the encoded form first: base64 inflation is a fixed
// ratio, so an encoded body under the cap can never decode past it, and the
// oversized case is refused before any decode allocates.
func (h *TaskHandlers) inspectBlob(body InspectTasksBody) (ManifestDTO, error) {
	encodedCap := base64.StdEncoding.EncodedLen(maxInspectBlobBytes)
	if len(body.Blob) > encodedCap {
		return ManifestDTO{}, Problem(
			SlugPayloadTooLarge,
			http.StatusRequestEntityTooLarge,
			fmt.Sprintf("the encoded blob is %d bytes; the decoded cap is %d", len(body.Blob), maxInspectBlobBytes),
		)
	}

	decoded, err := base64Decode(body.Blob)
	if err != nil {
		return ManifestDTO{}, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, "the blob is not valid base64")
	}

	manifest, err := uri.InspectTorrent(decoded)
	if err != nil {
		return ManifestDTO{}, Problem(SlugValidationFailed, http.StatusUnprocessableEntity, err.Error())
	}

	dto := torrentManifestDTO(manifest)
	dto.SourceURI = body.Filename

	return dto, nil
}

// inspectURI routes one submission and builds its manifest DTO. A rejection is
// a per-entry outcome, not a request failure.
func (h *TaskHandlers) inspectURI(ctx context.Context, raw string) (*ManifestDTO, *RejectedURI, error) {
	n, err := uri.Normalize(raw)
	if err != nil {
		rejection := rejectURI(raw, err)

		return nil, &rejection, nil
	}

	// MediaMatcher stays nil until the T088 ADR lands (IMPLEMENTING.md).
	if _, err := engine.Route(n, nil); err != nil {
		rejection := rejectURI(raw, err)
		return nil, &rejection, nil
	}

	if n.Kind == uri.KindMagnet {
		return h.inspectMagnet(ctx, n)
	}

	dto := transportManifestDTO(n)

	return &dto, nil, nil
}

// inspectMagnet resolves magnet metadata through the registered
// magnetInspector under the 60-second deadline of doc 05 section 5.3.
func (h *TaskHandlers) inspectMagnet(ctx context.Context, n uri.Normalized) (*ManifestDTO, *RejectedURI, error) {
	inspector, ok := magnetInspectorOf(h.engines)
	if !ok {
		dto := pendingMagnetDTO(displayName(n), n)

		return &dto, nil, nil
	}

	inspectCtx, cancel := context.WithTimeout(ctx, magnetInspectDeadline)
	defer cancel()

	manifest, err := inspector.InspectMagnet(inspectCtx, n.URI)
	if errors.Is(err, engine.ErrUnavailable) {
		return nil, nil, Problem(
			SlugEngineUnavailable,
			http.StatusServiceUnavailable,
			"magnet metadata needs the qBittorrent engine and it is down",
		)
	}
	if err != nil {
		// Deadline exceeded or metadata unresolvable right now: not an
		// error, the UI offers add-paused instead of a file selection.
		dto := pendingMagnetDTO(displayName(n), n)

		return &dto, nil, nil
	}

	dto := magnetManifestDTO(displayName(n), n, manifest)

	return &dto, nil, nil
}

// magnetInspectDeadline bounds the metadata-only magnet fetch of doc 05
// section 5.3. A var so tests can shorten it without waiting a real minute.
var magnetInspectDeadline = 60 * time.Second

// magnetInspectorOf returns the first registered engine that resolves magnet
// metadata. Today only qBittorrent can (T038); the scan keeps the handler
// honest whichever adapters are registered.
func magnetInspectorOf(registry *engine.Registry) (magnetInspector, bool) {
	for _, name := range registry.Names() {
		e, _ := registry.Get(name)
		if mi, ok := e.(magnetInspector); ok {
			return mi, true
		}
	}

	return nil, false
}

// torrentManifestDTO renders a parsed .torrent manifest.
func torrentManifestDTO(m uri.Manifest) ManifestDTO {
	files := make([]ManifestFileDTO, 0, len(m.Files))
	for _, f := range m.Files {
		size := f.Size
		files = append(files, ManifestFileDTO{Index: f.Index, Path: f.Path, Size: &size})
	}

	fileCount := len(files)

	return ManifestDTO{
		Kind:       string(uri.KindTorrent),
		Name:       m.Name,
		TotalSize:  &m.TotalSize,
		FileCount:  &fileCount,
		InfohashV1: stringOrNil(m.InfohashV1),
		InfohashV2: stringOrNil(m.InfohashV2),
		Files:      files,
	}
}

// magnetManifestDTO renders a magnet whose metadata resolved.
func magnetManifestDTO(name string, n uri.Normalized, m uri.Manifest) ManifestDTO {
	dto := torrentManifestDTO(m)
	dto.SourceURI = n.URI
	dto.Kind = string(uri.KindMagnet)
	if m.Name == "" {
		dto.Name = name
	}

	return dto
}

// pendingMagnetDTO renders the metadata_pending fallback: files null, the
// display name and whatever identity the magnet URI itself carried.
func pendingMagnetDTO(name string, n uri.Normalized) ManifestDTO {
	return ManifestDTO{
		SourceURI:       n.URI,
		Kind:            string(uri.KindMagnet),
		Name:            name,
		MetadataPending: true,
		InfohashV1:      stringOrNil(n.InfohashV1),
		InfohashV2:      stringOrNil(n.InfohashV2),
	}
}

// transportManifestDTO renders an http, ftp, sftp, metalink, torrent-URL or
// media submission: one file entry whose size may be null and both infohashes
// null (doc 05 section 5.3).
func transportManifestDTO(n uri.Normalized) ManifestDTO {
	name := displayName(n)

	return ManifestDTO{
		SourceURI: n.URI,
		Kind:      string(n.Kind),
		Name:      name,
		FileCount: intPtr(1),
		Files:     []ManifestFileDTO{{Path: name}},
	}
}

// base64Decode is base64.StdEncoding.DecodeString, factored out so the
// endpoint's accepted alphabet has one home.
func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// intPtr is stringOrNil for counters.
func intPtr(i int) *int {
	return &i
}
