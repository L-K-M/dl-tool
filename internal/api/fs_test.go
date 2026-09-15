//go:build linux

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// browse calls GET /fs/browse with the test bearer credential.
func (e *tasksTestEnv) browse(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Get("/fs/browse"+query, "Authorization: Bearer "+e.bearer)
}

// decodeBrowseBody decodes the GET /fs/browse response.
func decodeBrowseBody(t *testing.T, recorder *httptest.ResponseRecorder) struct {
	Path        string  `json:"path"`
	Parent      *string `json:"parent"`
	Separator   string  `json:"separator"`
	Writable    bool    `json:"writable"`
	FreeBytes   int64   `json:"free_bytes"`
	TotalBytes  int64   `json:"total_bytes"`
	Directories []struct {
		Name     string `json:"name"`
		Path     string `json:"path"`
		Writable bool   `json:"writable"`
	} `json:"directories"`
} {
	t.Helper()

	var body struct {
		Path        string  `json:"path"`
		Parent      *string `json:"parent"`
		Separator   string  `json:"separator"`
		Writable    bool    `json:"writable"`
		FreeBytes   int64   `json:"free_bytes"`
		TotalBytes  int64   `json:"total_bytes"`
		Directories []struct {
			Name     string `json:"name"`
			Path     string `json:"path"`
			Writable bool   `json:"writable"`
		} `json:"directories"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body
}

func browseQuery(path string, showHidden bool) string {
	query := "?path=" + url.QueryEscape(path)
	if showHidden {
		query += "&show_hidden=true"
	}

	return query
}

// TestListFSRoots serves the configured roots with space and writability.
func TestListFSRoots(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.api.Get("/fs/roots", "Authorization: Bearer "+env.bearer)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	var body struct {
		Roots []struct {
			Path       string `json:"path"`
			Writable   bool   `json:"writable"`
			FreeBytes  int64  `json:"free_bytes"`
			TotalBytes int64  `json:"total_bytes"`
		} `json:"roots"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Roots) != 1 || body.Roots[0].Path != env.dataRoot {
		t.Fatalf("roots = %+v, want exactly %q", body.Roots, env.dataRoot)
	}
	if !body.Roots[0].Writable {
		t.Error("root reports not writable on a fresh temp dir")
	}
	if body.Roots[0].FreeBytes <= 0 || body.Roots[0].TotalBytes <= 0 {
		t.Errorf("space = %d/%d, want positive free and total", body.Roots[0].FreeBytes, body.Roots[0].TotalBytes)
	}
}

// TestBrowseListsRoot lists the directories of a root, sorted by name with
// a case-insensitive comparison, and proves files never appear.
func TestBrowseListsRoot(t *testing.T) {
	env := newTasksTestEnv(t)
	for _, name := range []string{"iso", "Incoming", "archive"} {
		if err := os.Mkdir(filepath.Join(env.dataRoot, name), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(env.dataRoot, "film.iso"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	response := env.browse(t, browseQuery(env.dataRoot, false))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	body := decodeBrowseBody(t, response)
	if body.Path != env.dataRoot {
		t.Errorf("path = %q, want %q", body.Path, env.dataRoot)
	}
	if body.Parent != nil {
		t.Errorf("parent = %q at a root, want null", *body.Parent)
	}
	if body.Separator != "/" {
		t.Errorf("separator = %q, want /", body.Separator)
	}
	if !body.Writable {
		t.Error("writable = false on a fresh temp dir")
	}
	if body.FreeBytes <= 0 || body.TotalBytes <= 0 {
		t.Errorf("space = %d/%d, want positive free and total", body.FreeBytes, body.TotalBytes)
	}

	names := make([]string, 0, len(body.Directories))
	for _, dir := range body.Directories {
		names = append(names, dir.Name)
		if dir.Path != filepath.Join(env.dataRoot, dir.Name) {
			t.Errorf("entry path = %q, want child of %q", dir.Path, env.dataRoot)
		}
	}
	want := []string{"archive", "Incoming", "iso"}
	if len(names) != len(want) {
		t.Fatalf("directories = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("directories = %v, want case-insensitive order %v", names, want)
		}
	}
}

// TestBrowseRejectsOutsideRoots covers the three escape spellings of the
// acceptance criteria: an absolute foreign path and the two ".." forms.
func TestBrowseRejectsOutsideRoots(t *testing.T) {
	env := newTasksTestEnv(t)

	if err := os.Mkdir(filepath.Join(env.dataRoot, "ok"), 0o755); err != nil {
		t.Fatalf("mkdir ok: %v", err)
	}
	for _, path := range []string{
		"/etc",
		env.dataRoot + "/../etc",
		env.dataRoot + "/ok/../../etc",
	} {
		response := env.browse(t, browseQuery(path, false))
		assertProblem(t, response, http.StatusForbidden, SlugPathRejected)
	}
}

// TestBrowseDoesNotTraverseSymlink creates a symlink inside the root
// pointing at /etc and proves it is neither listed nor followed.
func TestBrowseDoesNotTraverseSymlink(t *testing.T) {
	env := newTasksTestEnv(t)
	if err := os.Mkdir(filepath.Join(env.dataRoot, "real"), 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	if err := os.Symlink("/etc", filepath.Join(env.dataRoot, "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	response := env.browse(t, browseQuery(filepath.Join(env.dataRoot, "escape"), false))
	assertProblem(t, response, http.StatusForbidden, SlugPathRejected)

	response = env.browse(t, browseQuery(env.dataRoot, false))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	for _, dir := range decodeBrowseBody(t, response).Directories {
		if dir.Name == "escape" {
			t.Error("the /etc symlink listed as a browsable directory")
		}
	}
}

// TestCallerCannotWalkAboveRoot asserts the root jail on the way up: a
// path above a root is 403 and a root's parent is null.
func TestCallerCannotWalkAboveRoot(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.browse(t, browseQuery(filepath.Dir(env.dataRoot), false))
	assertProblem(t, response, http.StatusForbidden, SlugPathRejected)

	response = env.browse(t, browseQuery(env.dataRoot, false))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if body := decodeBrowseBody(t, response); body.Parent != nil {
		t.Errorf("parent = %q at a root, want null", *body.Parent)
	}
}

// TestBrowseSubdirectory sets parent to the resolved path's dirname.
func TestBrowseSubdirectory(t *testing.T) {
	env := newTasksTestEnv(t)
	sub := filepath.Join(env.dataRoot, "iso")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	response := env.browse(t, browseQuery(sub, false))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	body := decodeBrowseBody(t, response)
	if body.Parent == nil || *body.Parent != env.dataRoot {
		t.Errorf("parent = %v, want %q", body.Parent, env.dataRoot)
	}
}

// TestBrowseNotFound answers 404 for a path inside a root that does not
// exist or is a file, never a filtered listing.
func TestBrowseNotFound(t *testing.T) {
	env := newTasksTestEnv(t)
	file := filepath.Join(env.dataRoot, "film.iso")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	for _, path := range []string{
		filepath.Join(env.dataRoot, "missing"),
		file,
		filepath.Join(file, "child"),
	} {
		response := env.browse(t, browseQuery(path, false))
		assertProblem(t, response, http.StatusNotFound, SlugNotFound)
	}
}

// TestBrowseHiddenDirectories keeps dot-directories out unless the caller
// asks for them.
func TestBrowseHiddenDirectories(t *testing.T) {
	env := newTasksTestEnv(t)
	if err := os.Mkdir(filepath.Join(env.dataRoot, ".cache"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	response := env.browse(t, browseQuery(env.dataRoot, false))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", response.Code, response.Body.String())
	}
	for _, dir := range decodeBrowseBody(t, response).Directories {
		if dir.Name == ".cache" {
			t.Error("dot-directory listed without show_hidden")
		}
	}

	response = env.browse(t, browseQuery(env.dataRoot, true))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body %s", response.Code, response.Body.String())
	}
	found := false
	for _, dir := range decodeBrowseBody(t, response).Directories {
		if dir.Name == ".cache" {
			found = true
		}
	}
	if !found {
		t.Error("dot-directory missing with show_hidden=true")
	}
}

// TestBrowseRejectsNUL pins doc 12 section 3.2 step 5: a NUL in a path the
// user typed rejects the request rather than truncating inside open().
func TestBrowseRejectsNUL(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.api.Get("/fs/browse?path="+url.QueryEscape(env.dataRoot+"/bad\x00dir"),
		"Authorization: Bearer "+env.bearer)
	assertProblem(t, response, http.StatusForbidden, SlugPathRejected)
}

// TestBrowseMissingPath answers 422 /problems/validation-failed when the
// required path parameter is absent.
func TestBrowseMissingPath(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.api.Get("/fs/browse", "Authorization: Bearer "+env.bearer)
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
}

// TestFSRejectUnknownQuery pins RejectUnknownQueryParameters on both /fs
// operations: a mistyped query key is 422, never silently ignored.
func TestFSRejectUnknownQuery(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.browse(t, browseQuery(env.dataRoot, false)+"&wat=1")
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	response = env.api.Get("/fs/roots?wat=1", "Authorization: Bearer "+env.bearer)
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
}

// mkdir calls POST /fs/mkdir with the test bearer credential.
func (e *tasksTestEnv) mkdir(t *testing.T, path, name string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Post("/fs/mkdir",
		map[string]string{"path": path, "name": name},
		"Authorization: Bearer "+e.bearer)
}

// TestMkdirCreatesWithUmask creates one directory inside the root and
// proves the mode is the process umask's answer — 0o777 minus the mask —
// never a mode mkdir set itself. The umask is process-wide, so it is
// restored before the next test runs.
func TestMkdirCreatesWithUmask(t *testing.T) {
	env := newTasksTestEnv(t)

	old := syscall.Umask(0o027)
	response := env.mkdir(t, env.dataRoot, "fresh")
	syscall.Umask(old)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}

	created := filepath.Join(env.dataRoot, "fresh")
	info, err := os.Stat(created)
	if err != nil {
		t.Fatalf("stat %s: %v", created, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", created)
	}
	if mode := info.Mode().Perm(); mode != 0o750 {
		t.Errorf("mode = %#o, want %#o (0o777 &^ umask 0o027)", mode, 0o750)
	}

	var body struct {
		Path     string `json:"path"`
		Writable bool   `json:"writable"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Path != created {
		t.Errorf("path = %q, want %q", body.Path, created)
	}
	if !body.Writable {
		t.Error("writable = false on a directory the process can write")
	}
}

// TestMkdirConflict covers both spellings of an existing name: a directory
// and a regular file.
func TestMkdirConflict(t *testing.T) {
	env := newTasksTestEnv(t)
	if err := os.Mkdir(filepath.Join(env.dataRoot, "taken"), 0o755); err != nil {
		t.Fatalf("mkdir taken: %v", err)
	}
	if err := os.WriteFile(filepath.Join(env.dataRoot, "film.iso"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	assertProblem(t, env.mkdir(t, env.dataRoot, "taken"), http.StatusConflict, SlugConflict)
	assertProblem(t, env.mkdir(t, env.dataRoot, "film.iso"), http.StatusConflict, SlugConflict)
}

// TestMkdirRejectsSeparatorInName answers 422 for every spelling of a
// name that is not a single path component.
func TestMkdirRejectsSeparatorInName(t *testing.T) {
	env := newTasksTestEnv(t)

	for _, name := range []string{"a/b", "..", ".", ""} {
		response := env.mkdir(t, env.dataRoot, name)
		assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	}
	if entries, err := os.ReadDir(env.dataRoot); err != nil || len(entries) != 0 {
		t.Fatalf("data root changed by a rejected name: entries %v, err %v", entries, err)
	}
}

// TestMkdirOutsideRoots answers 403 when the parent resolves outside the
// configured roots — before the name is even looked at on disk.
func TestMkdirOutsideRoots(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.mkdir(t, "/etc", "escape")
	assertProblem(t, response, http.StatusForbidden, SlugPathRejected)
}

// TestMkdirMissingParent answers 404 for a parent inside a root that does
// not exist.
func TestMkdirMissingParent(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.mkdir(t, filepath.Join(env.dataRoot, "gone"), "fresh")
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
}

// TestMkdirMissingFields answers 422 when a required body member is
// absent.
func TestMkdirMissingFields(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.api.Post("/fs/mkdir",
		map[string]string{"path": env.dataRoot},
		"Authorization: Bearer "+env.bearer)
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
}

// TestFreeSpaceReturnsIntegerBytes proves the statfs answer crosses the
// API as plain integers: the JSON literals carry no fractional part and
// no exponent.
func TestFreeSpaceReturnsIntegerBytes(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.api.Get("/fs/free-space?path="+url.QueryEscape(env.dataRoot),
		"Authorization: Bearer "+env.bearer)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}

	decoder := json.NewDecoder(response.Body)
	decoder.UseNumber()
	var body struct {
		Path       string      `json:"path"`
		FreeBytes  json.Number `json:"free_bytes"`
		TotalBytes json.Number `json:"total_bytes"`
	}
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Path != env.dataRoot {
		t.Errorf("path = %q, want %q", body.Path, env.dataRoot)
	}
	for field, number := range map[string]json.Number{"free_bytes": body.FreeBytes, "total_bytes": body.TotalBytes} {
		if _, err := number.Int64(); err != nil {
			t.Errorf("%s = %q is not an integer JSON literal: %v", field, number.String(), err)
		}
		value, _ := number.Int64()
		if value <= 0 {
			t.Errorf("%s = %d, want positive", field, value)
		}
	}
}

// TestFreeSpaceOutsideRoots applies the same jail as browse: a path
// outside the configured roots is 403, never a statfs answer for it.
func TestFreeSpaceOutsideRoots(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.api.Get("/fs/free-space?path="+url.QueryEscape("/etc"),
		"Authorization: Bearer "+env.bearer)
	assertProblem(t, response, http.StatusForbidden, SlugPathRejected)
}
