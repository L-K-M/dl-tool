package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/humatest"

	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/store"
)

// The multi-file fixture torrent of the subfolder and selection cases: a
// two-file manifest whose name is a traversal attempt, so the create path
// must sanitise it before it becomes a directory. The single-file fixture
// closes the outer dictionary inspectTorrentFixture leaves open (that one
// is scanned by uri.InspectTorrent's info-dict reader, which never decodes
// the outer dictionary; the upload classifier decodes the whole document).
const uploadTorrentFixture = "d8:announce35:http://tracker.example.com/announce" +
	"4:infod5:filesld6:lengthi5e4:pathl5:a.txteed6:lengthi7e4:pathl5:b.txteee" +
	"4:name7:../evil12:piece lengthi16384e6:pieces20:" +
	"\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00ee"

// uploadSingleFileTorrent is tasks_inspect_test.go's fixture with its outer
// dictionary closed.
const uploadSingleFileTorrent = inspectTorrentFixture + "e"

// The metalink fixture of the aria2 upload lane.
const uploadMetalinkFixture = `<?xml version="1.0" encoding="UTF-8"?>` +
	`<metalink xmlns="urn:ietf:params:xml:ns:metalink">` +
	`<file name="ubuntu.iso"><url>https://mirror.example.com/ubuntu.iso</url></file></metalink>`

// createdBody decodes the 201 body of POST /tasks for the upload cases.
type createdBody struct {
	Created []struct {
		ID                   string  `json:"id"`
		Engine               string  `json:"engine"`
		SourceKind           string  `json:"source_kind"`
		SourceURI            *string `json:"source_uri"`
		InfohashV1           *string `json:"infohash_v1"`
		Name                 string  `json:"name"`
		Destination          string  `json:"destination"`
		RequestedDestination *string `json:"requested_destination"`
		TotalBytes           *int64  `json:"total_bytes"`
	} `json:"created"`
	Rejected []RejectedURI `json:"rejected"`
}

// multipartForm builds one multipart/form-data body: an optional payload
// part (nil to omit it) and the given file parts, in order.
func multipartForm(t *testing.T, payload []byte, files ...UploadedFile) (*bytes.Buffer, string) {
	t.Helper()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if payload != nil {
		part, err := writer.CreateFormField(payloadPart)
		if err != nil {
			t.Fatalf("create payload part: %v", err)
		}
		if _, err := part.Write(payload); err != nil {
			t.Fatalf("write payload part: %v", err)
		}
	}
	for _, f := range files {
		part, err := writer.CreateFormFile(filePart, f.Name)
		if err != nil {
			t.Fatalf("create file part %q: %v", f.Name, err)
		}
		if _, err := part.Write(f.Bytes); err != nil {
			t.Fatalf("write file part %q: %v", f.Name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	return &buf, writer.FormDataContentType()
}

// postForm posts one multipart submission to /tasks with the test bearer.
func (e *tasksTestEnv) postForm(t *testing.T, body io.Reader, contentType string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Do(http.MethodPost, "/tasks", body,
		"Content-Type: "+contentType, "Authorization: Bearer "+e.bearer)
}

// decodeCreated decodes a 201 body, failing the test on anything else.
func decodeCreated(t *testing.T, recorder *httptest.ResponseRecorder) createdBody {
	t.Helper()

	var body createdBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode create body: %v (%s)", err, recorder.Body.String())
	}

	return body
}

// TestUploadTorrentPart pins the torrent lane of the form: one .torrent
// part becomes exactly one task on qbittorrent, without any engine contact
// — the admission pass owns Engine.Add.
func TestUploadTorrentPart(t *testing.T) {
	env := newTasksTestEnv(t)

	body, contentType := multipartForm(t, nil, UploadedFile{Name: "hello.torrent", Bytes: []byte(uploadSingleFileTorrent)})
	response := env.postForm(t, body, contentType)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	created := decodeCreated(t, response)
	if len(created.Created) != 1 || len(created.Rejected) != 0 {
		t.Fatalf("created = %+v, want exactly one task and no rejection", created)
	}

	task := created.Created[0]
	if task.Engine != engine.NameQBittorrent || task.SourceKind != "torrent" {
		t.Errorf("engine = %q, source_kind = %q; want qbittorrent/torrent", task.Engine, task.SourceKind)
	}
	if task.InfohashV1 == nil || len(*task.InfohashV1) != 40 {
		t.Errorf("infohash_v1 = %v, want the fixture's 40-char hash", task.InfohashV1)
	}
	if task.Name != "hello.txt" {
		t.Errorf("name = %q, want the manifest name hello.txt", task.Name)
	}
	if task.TotalBytes == nil || *task.TotalBytes != 11 {
		t.Errorf("total_bytes = %v, want 11 from the manifest", task.TotalBytes)
	}

	// The stored engine source is the rebuilt magnet: the admission pass
	// resubmits it once the uploaded bytes are gone.
	var sourceURI string
	if err := env.db.GetContext(t.Context(), &sourceURI, `SELECT source_uri FROM tasks`); err != nil {
		t.Fatalf("read source_uri: %v", err)
	}
	if !strings.HasPrefix(sourceURI, "magnet:?xt=urn:btih:") {
		t.Errorf("source_uri = %q, want the rebuilt magnet", sourceURI)
	}

	if calls := append(env.aria2.recorded(), env.qbittorrent.recorded()...); len(calls) != 0 {
		t.Errorf("engine calls = %v, want none at create time", calls)
	}
}

// TestUploadMetalinkPart pins the metalink lane: one .metalink part becomes
// one task on aria2.
func TestUploadMetalinkPart(t *testing.T) {
	env := newTasksTestEnv(t)

	body, contentType := multipartForm(t, nil, UploadedFile{Name: "ubuntu.metalink", Bytes: []byte(uploadMetalinkFixture)})
	response := env.postForm(t, body, contentType)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	created := decodeCreated(t, response)
	if len(created.Created) != 1 || len(created.Rejected) != 0 {
		t.Fatalf("created = %+v, want exactly one task and no rejection", created)
	}
	if created.Created[0].Engine != engine.NameAria2 || created.Created[0].SourceKind != "metalink" {
		t.Errorf("engine = %q, source_kind = %q; want aria2/metalink",
			created.Created[0].Engine, created.Created[0].SourceKind)
	}
	if created.Created[0].Name != "ubuntu.metalink" {
		t.Errorf("name = %q, want the part's filename", created.Created[0].Name)
	}
}

// TestUploadTextListExpands pins the .txt lane: every accepted line becomes
// one URI submission routed by scheme; comment and blank lines are dropped.
func TestUploadTextListExpands(t *testing.T) {
	env := newTasksTestEnv(t)

	list := "# a comment\n" + mixedHTTPS + "\n\n   # an indented comment\n" + mixedMagnet + "\n"
	body, contentType := multipartForm(t, nil, UploadedFile{Name: "list.txt", Bytes: []byte(list)})
	response := env.postForm(t, body, contentType)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	created := decodeCreated(t, response)
	if len(created.Created) != 2 || len(created.Rejected) != 0 {
		t.Fatalf("created = %+v, want two tasks and no rejection", created)
	}
	if created.Created[0].Engine != engine.NameAria2 || created.Created[1].Engine != engine.NameQBittorrent {
		t.Errorf("engines = %q, %q; want aria2 then qbittorrent (routed by scheme)",
			created.Created[0].Engine, created.Created[1].Engine)
	}
	if env.countTasks(t) != 2 {
		t.Errorf("tasks = %d, want 2", env.countTasks(t))
	}
}

// TestMultipartMergesPayloadAndParts pins task step 5: the payload's uris
// and the file parts build one submission list — a payload URI, a .txt line
// and a .torrent part create side by side.
func TestMultipartMergesPayloadAndParts(t *testing.T) {
	env := newTasksTestEnv(t)

	payload := []byte(fmt.Sprintf(`{"uris":[%q]}`, mixedHTTPS))
	body, contentType := multipartForm(t, payload,
		UploadedFile{Name: "hello.torrent", Bytes: []byte(uploadSingleFileTorrent)},
		UploadedFile{Name: "list.txt", Bytes: []byte(mixedMagnet + "\n")},
	)
	response := env.postForm(t, body, contentType)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	created := decodeCreated(t, response)
	if len(created.Created) != 3 {
		t.Fatalf("created = %+v, want three tasks", created)
	}
	// Payload uris first (with the .txt line among them), then the blobs.
	wantEngines := []string{engine.NameAria2, engine.NameQBittorrent, engine.NameQBittorrent}
	for i, want := range wantEngines {
		if created.Created[i].Engine != want {
			t.Errorf("created[%d].engine = %q, want %q", i, created.Created[i].Engine, want)
		}
	}
}

// TestUploadRejectsUnrecognisedPart pins the part table's last row: bytes
// that are none of the three kinds land in rejected[] with
// /problems/unsupported-media-type, beside the accepted submissions.
func TestUploadRejectsUnrecognisedPart(t *testing.T) {
	env := newTasksTestEnv(t)

	jpg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46}
	body, contentType := multipartForm(t, nil,
		UploadedFile{Name: "photo.jpg", Bytes: jpg},
		UploadedFile{Name: "list.txt", Bytes: []byte(mixedHTTPS + "\n")},
	)
	response := env.postForm(t, body, contentType)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	created := decodeCreated(t, response)
	if len(created.Created) != 1 || len(created.Rejected) != 1 {
		t.Fatalf("created = %+v, want one task and one rejection", created)
	}
	if created.Rejected[0].Type != SlugUnsupportedMediaType || created.Rejected[0].URI != "photo.jpg" {
		t.Errorf("rejection = %+v, want the unsupported-media-type entry for photo.jpg", created.Rejected[0])
	}
}

// TestTooManyExpandedLines pins the merged 50-source cap of doc 05 section
// 5.2: the lines of a .txt pool with the payload's uris under MaxURIs.
func TestTooManyExpandedLines(t *testing.T) {
	env := newTasksTestEnv(t)

	var list strings.Builder
	for i := 0; i < MaxURIs+1; i++ {
		fmt.Fprintf(&list, "https://mirror.example.com/file-%d.iso\n", i)
	}
	body, contentType := multipartForm(t, nil, UploadedFile{Name: "many.txt", Bytes: []byte(list.String())})
	response := env.postForm(t, body, contentType)

	problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	wantDetail := fmt.Sprintf(tooManyURIsFormat, MaxURIs+1, MaxURIs)
	if problem.Detail != wantDetail {
		t.Errorf("detail = %q, want %q", problem.Detail, wantDetail)
	}
	if env.countTasks(t) != 0 {
		t.Errorf("over-long submission created tasks, want none")
	}
}

// TestRequestTooLarge pins the 32 MiB request cap at the endpoint: an
// oversized form is a 413 and creates nothing.
func TestRequestTooLarge(t *testing.T) {
	env := newTasksTestEnv(t)

	body, contentType := multipartForm(t, nil,
		UploadedFile{Name: "big.txt", Bytes: bytes.Repeat([]byte("x"), 33<<20)})
	response := env.postForm(t, body, contentType)

	assertProblem(t, response, http.StatusRequestEntityTooLarge, SlugPayloadTooLarge)
	if env.countTasks(t) != 0 {
		t.Errorf("oversized request created tasks, want none")
	}
}

// TestParseSubmissionCutsAtTheCap pins the no-full-buffering half of the
// cap: parseSubmission refuses a body above MaxRequestBytes after reading
// barely past the limit, and a part above MaxBlobBytes after barely past
// its own limit. humatest cannot observe this — its request dumper reads
// the whole body first — so the property is pinned at the parser itself.
func TestParseSubmissionCutsAtTheCap(t *testing.T) {
	boundary := "testboundary"

	newCappedRequest := func(partName string, size int64) (*http.Request, *countingReader) {
		// Mirror multipartForm's part shapes: the payload part is a plain
		// form field (no filename); only file parts carry one.
		disposition := `form-data; name="` + partName + `"`
		if partName == filePart {
			disposition += `; filename="big"`
		}
		body := &countingReader{r: io.MultiReader(
			strings.NewReader("--"+boundary+"\r\nContent-Disposition: "+disposition+"\r\n\r\n"),
			&zeroReader{remaining: size},
			strings.NewReader("\r\n--"+boundary+"--\r\n"),
		)}
		req := httptest.NewRequest(http.MethodPost, "/tasks", body)
		req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)

		return req, body
	}

	// The request cap: a 40 MiB payload part is cut at 32 MiB.
	req, body := newCappedRequest(payloadPart, 40<<20)
	if _, _, err := parseSubmission(req); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("parseSubmission err = %v, want ErrPayloadTooLarge", err)
	}
	if body.n > MaxRequestBytes+(1<<20) {
		t.Errorf("read %d bytes of a 40 MiB body, want barely past the %d-byte cap", body.n, MaxRequestBytes)
	}

	// The per-part cap: a 40 MiB file part is cut at 10 MiB.
	req, body = newCappedRequest(filePart, 40<<20)
	if _, _, err := parseSubmission(req); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("parseSubmission err = %v, want ErrPayloadTooLarge", err)
	}
	if body.n > MaxBlobBytes+(1<<20) {
		t.Errorf("read %d bytes of a 40 MiB part, want barely past the %d-byte blob cap", body.n, MaxBlobBytes)
	}
}

// countingReader proves how much of a request body the server consumed.
type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)

	return n, err
}

// zeroReader streams zero bytes without materialising them.
type zeroReader struct{ remaining int64 }

func (r *zeroReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), r.remaining)
	clear(p[:n])
	r.remaining -= n

	return int(n), nil
}

// TestBlobPartTooLarge pins the per-part 10 MiB cap: one oversized file
// part is a 413 for the whole request even though the request itself fits
// the 32 MiB cap.
func TestBlobPartTooLarge(t *testing.T) {
	env := newTasksTestEnv(t)

	oversized := bytes.Repeat([]byte("d"), MaxBlobBytes+1)
	body, contentType := multipartForm(t, nil, UploadedFile{Name: "big.torrent", Bytes: oversized})
	response := env.postForm(t, body, contentType)

	assertProblem(t, response, http.StatusRequestEntityTooLarge, SlugPayloadTooLarge)
	if env.countTasks(t) != 0 {
		t.Errorf("oversized part created tasks, want none")
	}
}

// TestCreateSubfolderSanitisesName pins FR-008: a multi-file manifest moves
// the destination to <destination>/<manifest name>/, sanitised per doc 12
// section 3.2 and re-resolved inside the configured root, and the original
// request is recorded in requested_destination.
func TestCreateSubfolderSanitisesName(t *testing.T) {
	env := newTasksTestEnv(t)

	payload := []byte(fmt.Sprintf(`{"destination":%q,"create_subfolder":true}`, env.dataRoot))
	body, contentType := multipartForm(t, payload, UploadedFile{Name: "evil.torrent", Bytes: []byte(uploadTorrentFixture)})
	response := env.postForm(t, body, contentType)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	created := decodeCreated(t, response)
	if len(created.Created) != 1 {
		t.Fatalf("created = %+v, want one task", created)
	}

	// The traversal name "../evil" sanitises to ".._evil" beneath the root.
	resolvedRoot, err := filepath.EvalSymlinks(env.dataRoot)
	if err != nil {
		t.Fatalf("resolve data root: %v", err)
	}
	want := filepath.Join(resolvedRoot, ".._evil")

	task := created.Created[0]
	if task.Destination != want {
		t.Errorf("destination = %q, want %q (manifest name sanitised inside the root)", task.Destination, want)
	}
	if task.RequestedDestination == nil || *task.RequestedDestination != env.dataRoot {
		t.Errorf("requested_destination = %v, want the original %q", task.RequestedDestination, env.dataRoot)
	}

	var storedDestination string
	var storedRequested *string
	if err := env.db.GetContext(t.Context(), &storedDestination, `SELECT destination FROM tasks`); err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if storedDestination != want {
		t.Errorf("stored destination = %q, want %q", storedDestination, want)
	}
	if err := env.db.GetContext(t.Context(), &storedRequested, `SELECT requested_destination FROM tasks`); err != nil {
		t.Fatalf("read requested_destination: %v", err)
	}
	if storedRequested == nil || *storedRequested != env.dataRoot {
		t.Errorf("stored requested_destination = %v, want %q", storedRequested, env.dataRoot)
	}
}

// TestSelectFilesRejectedOnIncapableEngine pins the capability gate of doc
// 05 section 5.2: select_files to an engine without per_file_select is 422,
// and no task is created.
func TestSelectFilesRejectedOnIncapableEngine(t *testing.T) {
	// The env's recording stand-ins declare no capabilities at all.
	env := newTasksTestEnv(t)

	response := env.createTasks(t, map[string]any{
		"uris":         []string{mixedMagnet},
		"select_files": []map[string]any{{"index": 0, "selected": false}},
	})

	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	if env.countTasks(t) != 0 {
		t.Errorf("incapable selection created tasks, want none")
	}
}

// capableEngine is a recordingEngine that declares capabilities; the
// tasks_test.go stand-in declares none, and the registry takes one engine
// per name, so a capable qBittorrent needs its own server.
type capableEngine struct {
	*recordingEngine
	caps []engine.Capability
}

func (e *capableEngine) Capabilities() []engine.Capability { return e.caps }

// newCapableQBTEnv builds the tasks test env with a qBittorrent stand-in
// declaring caps, for the selection acceptance cases.
func newCapableQBTEnv(t *testing.T, caps []engine.Capability) *tasksTestEnv {
	t.Helper()

	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	if err := os.Mkdir(dataRoot, 0o755); err != nil {
		t.Fatalf("make data root: %v", err)
	}

	configDir := filepath.Join(root, "config")
	db, err := store.Open(t.Context(), filepath.Join(configDir, "dl-tool.db"), filepath.Join(root, "backups"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	server, err := NewServer(
		&config.Config{ConfigDir: configDir, SessionTTL: time.Hour, DataRoots: []string{dataRoot}},
		db,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	env := &tasksTestEnv{
		api:         humatest.Wrap(t, server.API),
		db:          db,
		logs:        &strings.Builder{},
		aria2:       newRecordingEngine(engine.NameAria2, acceptsAria2Lanes),
		qbittorrent: newRecordingEngine(engine.NameQBittorrent, acceptsBitTorrent),
		dataRoot:    dataRoot,
	}
	server.Engines.Register(env.aria2)
	server.Engines.Register(&capableEngine{recordingEngine: env.qbittorrent, caps: caps})

	user := seedUser(t, db)
	env.bearer = seedLiveAPIToken(t, db, user.ID)

	return env
}

// TestSelectFilesAcceptedOnCapableEngine pins the acceptance path: a
// well-formed selection on a per_file_select engine creates the task.
func TestSelectFilesAcceptedOnCapableEngine(t *testing.T) {
	env := newCapableQBTEnv(t, []engine.Capability{engine.CapPerFileSelect, engine.CapPerFilePriority})

	payload := []byte(`{"select_files":[{"index":0,"selected":true,"priority":"high"},{"index":1,"selected":false}]}`)
	body, contentType := multipartForm(t, payload, UploadedFile{Name: "evil.torrent", Bytes: []byte(uploadTorrentFixture)})
	response := env.postForm(t, body, contentType)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	if env.countTasks(t) != 1 {
		t.Errorf("tasks = %d, want 1", env.countTasks(t))
	}
}

// TestSelectFilesPriorityNeedsPerFilePriority pins task step 7: a high or
// maximum priority to an engine that only selects is 422, not a silently
// dropped priority.
func TestSelectFilesPriorityNeedsPerFilePriority(t *testing.T) {
	env := newCapableQBTEnv(t, []engine.Capability{engine.CapPerFileSelect})

	payload := []byte(`{"select_files":[{"index":0,"priority":"high"}]}`)
	body, contentType := multipartForm(t, payload, UploadedFile{Name: "evil.torrent", Bytes: []byte(uploadTorrentFixture)})
	response := env.postForm(t, body, contentType)

	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	if env.countTasks(t) != 0 {
		t.Errorf("priority on a select-only engine created tasks, want none")
	}
}

// TestInspectAcceptsMultipartForm pins the shared form of doc 05 section
// 5.3: the inspect endpoint parses the identical parts and still creates no
// task.
func TestInspectAcceptsMultipartForm(t *testing.T) {
	env := newTasksTestEnv(t)

	payload := []byte(fmt.Sprintf(`{"uris":[%q]}`, mixedHTTPS))
	list := "# a comment\n" + mixedMagnet + "\n"
	body, contentType := multipartForm(t, payload,
		UploadedFile{Name: "hello.torrent", Bytes: []byte(uploadSingleFileTorrent)},
		UploadedFile{Name: "list.txt", Bytes: []byte(list)},
	)
	response := env.api.Do(http.MethodPost, "/tasks/inspect", body,
		"Content-Type: "+contentType, "Authorization: Bearer "+env.bearer)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	var inspected struct {
		Manifests []ManifestDTO `json:"manifests"`
		Rejected  []RejectedURI `json:"rejected"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &inspected); err != nil {
		t.Fatalf("decode inspect body: %v", err)
	}
	if len(inspected.Manifests) != 3 || len(inspected.Rejected) != 0 {
		t.Fatalf("manifests = %+v, rejected = %+v; want three manifests and no rejection",
			inspected.Manifests, inspected.Rejected)
	}

	// The torrent part first, then the uris in merged order.
	wantKinds := []string{"torrent", "http", "magnet"}
	for i, want := range wantKinds {
		if inspected.Manifests[i].Kind != want {
			t.Errorf("manifests[%d].kind = %q, want %q", i, inspected.Manifests[i].Kind, want)
		}
	}
	if inspected.Manifests[0].SourceURI != "hello.torrent" {
		t.Errorf("torrent manifest source = %q, want the part's filename", inspected.Manifests[0].SourceURI)
	}
	if env.countTasks(t) != 0 {
		t.Errorf("inspect created tasks, want none")
	}
}

// TestExpandTextList pins the line grammar of the .txt expander.
func TestExpandTextList(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"comment and blank dropped", "# c\n\nhttp://a.example/x\n", []string{"http://a.example/x"}},
		{"indented comment dropped", "   # c\nhttp://a.example/x\n", []string{"http://a.example/x"}},
		{"crlf tolerated", "http://a.example/x\r\nhttp://a.example/y\r\n", []string{"http://a.example/x", "http://a.example/y"}},
		{"order and duplicates kept", "http://a.example/x\nhttp://a.example/x\nhttp://a.example/y", []string{"http://a.example/x", "http://a.example/x", "http://a.example/y"}},
		{"hash inside a uri is not a comment", "http://a.example/x#frag\n", []string{"http://a.example/x#frag"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expandTextList([]byte(tc.in))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("expandTextList(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestClassifyUpload pins the sniff order and the .txt tie-break: the bytes
// decide, the extension only settles a text file whose content looks like
// one of the binary kinds.
func TestClassifyUpload(t *testing.T) {
	// Valid bencode and valid UTF-8 at once: the tie the extension breaks.
	bencodeText := []byte("d3:foo3:bare")

	cases := []struct {
		name     string
		file     UploadedFile
		wantKind string
		wantErr  bool
	}{
		{"torrent by bytes", UploadedFile{Name: "x.torrent", Bytes: []byte(uploadSingleFileTorrent)}, uploadKindTorrent, false},
		{"metalink by bytes", UploadedFile{Name: "x.meta4", Bytes: []byte(uploadMetalinkFixture)}, uploadKindMetalink, false},
		{"text list", UploadedFile{Name: "list.txt", Bytes: []byte("http://a.example/x\n")}, uploadKindText, false},
		{"tie broken to text", UploadedFile{Name: "list.txt", Bytes: bencodeText}, uploadKindText, false},
		{"tie kept as torrent", UploadedFile{Name: "x.torrent", Bytes: bencodeText}, uploadKindTorrent, false},
		{"jpeg rejected", UploadedFile{Name: "photo.jpg", Bytes: []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}}, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, err := classifyUpload(tc.file)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("classifyUpload(%q) = %q, want an error", tc.file.Name, kind)
				}
				if kind != "" {
					t.Errorf("classifyUpload(%q) returned kind %q alongside error, want empty", tc.file.Name, kind)
				}

				return
			}
			if err != nil {
				t.Fatalf("classifyUpload(%q): %v", tc.file.Name, err)
			}
			if kind != tc.wantKind {
				t.Errorf("classifyUpload(%q) = %q, want %q", tc.file.Name, kind, tc.wantKind)
			}
		})
	}
}

// TestSanitiseSegmentReservedNames pins doc 12 section 3.2 step 10 for the
// short names the extension window must not swallow: a reserved stem stays
// reserved with an extension attached (example table rows 9 and 10).
func TestSanitiseSegmentReservedNames(t *testing.T) {
	cases := []struct{ in, want string }{
		{"normal.mkv", "normal.mkv"},
		{"CON", "_CON"},
		{"nul.txt", "_nul.txt"},
		{"CON.txt", "_CON.txt"},
		{"com1.bin", "_com1.bin"},
	}

	for _, tc := range cases {
		if got := sanitiseSegment(tc.in); got != tc.want {
			t.Errorf("sanitiseSegment(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
