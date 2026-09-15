package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/L-K-M/dl-tool/internal/store"
)

// mustJSON renders one value the way a settings row's value_json stores
// it; marshalling these seeds cannot fail.
func mustJSON(t *testing.T, value any) string {
	t.Helper()

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %v: %v", value, err)
	}

	return string(encoded)
}

// getCategories calls GET /categories with the test bearer credential.
func (e *tasksTestEnv) getCategories(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Get("/categories", "Authorization: Bearer "+e.bearer)
}

// createCategory posts one category with the test bearer credential.
func (e *tasksTestEnv) createCategory(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Post("/categories", body, "Authorization: Bearer "+e.bearer)
}

// patchCategory patches one category by name with the test bearer
// credential; the name is percent-encoded the way doc 05 section 8.1
// addresses it.
func (e *tasksTestEnv) patchCategory(t *testing.T, name string, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Patch("/categories/"+url.PathEscape(name), body, "Authorization: Bearer "+e.bearer)
}

// deleteCategory deletes one category by name with the test bearer
// credential.
func (e *tasksTestEnv) deleteCategory(t *testing.T, name string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Delete("/categories/"+url.PathEscape(name), "Authorization: Bearer "+e.bearer)
}

// getTags calls GET /tags with the test bearer credential.
func (e *tasksTestEnv) getTags(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Get("/tags", "Authorization: Bearer "+e.bearer)
}

// decodeCategoryList decodes the GET /categories envelope.
func decodeCategoryList(t *testing.T, recorder *httptest.ResponseRecorder) []CategoryDTO {
	t.Helper()

	var body struct {
		Categories []CategoryDTO `json:"categories"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body.Categories
}

// decodeCategoryBody decodes the flat category object POST and PATCH
// return.
func decodeCategoryBody(t *testing.T, recorder *httptest.ResponseRecorder) CategoryDTO {
	t.Helper()

	var body CategoryDTO
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body
}

// decodeTagList decodes the GET /tags envelope.
func decodeTagList(t *testing.T, recorder *httptest.ResponseRecorder) []TagDTO {
	t.Helper()

	var body struct {
		Tags []TagDTO `json:"tags"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body.Tags
}

// seedTag writes one tag row straight through the store, so the test can
// pin a tag no task carries.
func (e *tasksTestEnv) seedTag(t *testing.T, name string) {
	t.Helper()

	if _, err := e.db.ExecContext(
		t.Context(),
		`INSERT INTO tags (id, name, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		store.NewID(store.PrefixTag), name, time.Now().UnixMilli(), time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("seed tag %q: %v", name, err)
	}
}

// resolvedPath is the expected destination of a path the fsx resolver
// accepted: its symlinks unfolded, matching what ResolveDestination
// stores.
func resolvedPath(t *testing.T, path string) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %q: %v", path, err)
	}

	return resolved
}

// TestCategoryCrud pins doc 05 section 8.1: create returns 201 with the
// category object, the list is name-sorted with task counts, patch merges
// the provided fields, delete is 204 — and an empty list encodes [],
// never null.
func TestCategoryCrud(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.getCategories(t)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"categories":[]`) {
		t.Errorf("empty category list = %s, want \"categories\":[]", response.Body.String())
	}

	linuxPath := filepath.Join(env.dataRoot, "iso")
	if err := os.MkdirAll(linuxPath, 0o755); err != nil {
		t.Fatalf("make save path: %v", err)
	}
	wantLinuxPath := resolvedPath(t, linuxPath)

	response = env.createCategory(t, map[string]any{"name": "linux", "save_path": linuxPath})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	created := decodeCategoryBody(t, response)
	if created.Name != "linux" || created.SavePath != wantLinuxPath || created.TaskCount != 0 {
		t.Errorf("created = %+v, want {linux %s 0}", created, wantLinuxPath)
	}

	response = env.createCategory(t, map[string]any{"name": "debian", "save_path": env.dataRoot})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}

	categories := decodeCategoryList(t, env.getCategories(t))
	if len(categories) != 2 || categories[0].Name != "debian" || categories[1].Name != "linux" {
		t.Fatalf("categories = %+v, want [debian linux] sorted by name", categories)
	}
	if categories[1].TaskCount != 0 {
		t.Errorf("linux task_count = %d, want 0", categories[1].TaskCount)
	}

	// The rename merges: save_path survives untouched.
	response = env.patchCategory(t, "linux", map[string]any{"new_name": "linux-iso"})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	renamed := decodeCategoryBody(t, response)
	if renamed.Name != "linux-iso" || renamed.SavePath != wantLinuxPath {
		t.Errorf("renamed = %+v, want {linux-iso %s 0}", renamed, wantLinuxPath)
	}

	// A save_path-only patch merges the other way: the name survives.
	newPath := filepath.Join(env.dataRoot, "isos")
	if err := os.MkdirAll(newPath, 0o755); err != nil {
		t.Fatalf("make second save path: %v", err)
	}
	wantNewPath := resolvedPath(t, newPath)

	response = env.patchCategory(t, "linux-iso", map[string]any{"save_path": newPath})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	updated := decodeCategoryBody(t, response)
	if updated.Name != "linux-iso" || updated.SavePath != wantNewPath {
		t.Errorf("updated = %+v, want {linux-iso %s 0}", updated, wantNewPath)
	}

	response = env.deleteCategory(t, "linux-iso")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusNoContent, response.Body.String())
	}
	response = env.deleteCategory(t, "debian")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusNoContent, response.Body.String())
	}

	response = env.getCategories(t)
	if !strings.Contains(response.Body.String(), `"categories":[]`) {
		t.Errorf("emptied category list = %s, want \"categories\":[]", response.Body.String())
	}
}

// TestDuplicateCategoryConflicts pins the 409 of doc 05 section 8.1: a
// name already taken conflicts on create, and renaming onto an existing
// name conflicts on patch — never a silent merge.
func TestDuplicateCategoryConflicts(t *testing.T) {
	env := newTasksTestEnv(t)

	body := map[string]any{"name": "linux", "save_path": env.dataRoot}
	if response := env.createCategory(t, body); response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	if response := env.createCategory(t, map[string]any{"name": "debian", "save_path": env.dataRoot}); response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}

	response := env.createCategory(t, body)
	assertProblem(t, response, http.StatusConflict, SlugConflict)

	response = env.patchCategory(t, "debian", map[string]any{"new_name": "linux"})
	assertProblem(t, response, http.StatusConflict, SlugConflict)

	// Both rows survive the refused writes.
	categories := decodeCategoryList(t, env.getCategories(t))
	if len(categories) != 2 {
		t.Errorf("categories = %+v, want both rows intact", categories)
	}
}

// TestCategoryValidation pins the doc 05 section 8.1 name rule: an empty
// name or a name carrying / is 422 on create and on the patch rename.
func TestCategoryValidation(t *testing.T) {
	env := newTasksTestEnv(t)

	if response := env.createCategory(t, map[string]any{"name": "linux", "save_path": env.dataRoot}); response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}

	for _, name := range []string{"a/b", ""} {
		response := env.createCategory(t, map[string]any{"name": name, "save_path": env.dataRoot})
		assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	}

	response := env.patchCategory(t, "linux", map[string]any{"new_name": ""})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	response = env.patchCategory(t, "linux", map[string]any{"new_name": "x/y"})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	// An explicit empty save_path is as invalid as an explicit empty
	// name — on create (schema) and on patch (handler check).
	response = env.createCategory(t, map[string]any{"name": "empty", "save_path": ""})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	response = env.patchCategory(t, "linux", map[string]any{"save_path": ""})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	categories := decodeCategoryList(t, env.getCategories(t))
	if len(categories) != 1 || categories[0].Name != "linux" {
		t.Errorf("categories = %+v, want only the untouched linux row", categories)
	}
}

// TestCategorySavePathRejected pins the root jail of doc 05 section 8.1:
// a save_path outside the configured roots is 403 /problems/path-rejected
// on create and on patch, and writes no row.
func TestCategorySavePathRejected(t *testing.T) {
	env := newTasksTestEnv(t)

	if response := env.createCategory(t, map[string]any{"name": "linux", "save_path": env.dataRoot}); response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}

	response := env.createCategory(t, map[string]any{"name": "evil", "save_path": "/etc"})
	assertProblem(t, response, http.StatusForbidden, SlugPathRejected)

	response = env.patchCategory(t, "linux", map[string]any{"save_path": "/etc"})
	assertProblem(t, response, http.StatusForbidden, SlugPathRejected)

	categories := decodeCategoryList(t, env.getCategories(t))
	if len(categories) != 1 || categories[0].Name != "linux" {
		t.Errorf("categories = %+v, want only the linux row with its path intact", categories)
	}
	wantLinuxPath := resolvedPath(t, env.dataRoot)
	if categories[0].SavePath != wantLinuxPath {
		t.Errorf("linux save_path = %q, want %q unchanged", categories[0].SavePath, wantLinuxPath)
	}
}

// TestCategoryNotFound pins the 404 of doc 05 section 8.1: patch and
// delete of a name no row carries.
func TestCategoryNotFound(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.patchCategory(t, "ghost", map[string]any{"new_name": "spectre"})
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)

	response = env.deleteCategory(t, "ghost")
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
}

// TestCategorySavePathResolvesDestination pins the resolution table's
// second row: a task created in a category with no explicit destination
// lands in the category's save_path, with requested_destination null —
// nothing was requested. An explicit destination still wins.
func TestCategorySavePathResolvesDestination(t *testing.T) {
	env := newTasksTestEnv(t)

	linuxPath := filepath.Join(env.dataRoot, "linux")
	if err := os.MkdirAll(linuxPath, 0o755); err != nil {
		t.Fatalf("make save path: %v", err)
	}
	wantLinuxPath := resolvedPath(t, linuxPath)

	if response := env.createCategory(t, map[string]any{"name": "linux", "save_path": linuxPath}); response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}

	response := env.createTasks(t, map[string]any{
		"uris":     []string{mixedHTTPS},
		"category": "linux",
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	body := decodeCreateBody(t, response)
	if len(body.Created) != 1 {
		t.Fatalf("created %d tasks, want 1", len(body.Created))
	}
	created := body.Created[0]
	if created.Destination != wantLinuxPath {
		t.Errorf("destination = %q, want the category save path %q", created.Destination, wantLinuxPath)
	}
	if created.RequestedDestination != nil {
		t.Errorf("requested_destination = %q, want null — nothing was requested", *created.RequestedDestination)
	}
	if created.Category == nil || *created.Category != "linux" {
		t.Errorf("category = %v, want linux", created.Category)
	}

	// The first row of the resolution table: an explicit destination wins
	// over the category save_path.
	explicitPath := filepath.Join(env.dataRoot, "explicit")
	if err := os.MkdirAll(explicitPath, 0o755); err != nil {
		t.Fatalf("make explicit path: %v", err)
	}
	wantExplicitPath := resolvedPath(t, explicitPath)

	response = env.createTasks(t, map[string]any{
		"uris":        []string{mixedFTP},
		"category":    "linux",
		"destination": explicitPath,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	body = decodeCreateBody(t, response)
	if len(body.Created) != 1 || body.Created[0].Destination != wantExplicitPath {
		t.Fatalf("created = %+v, want the explicit destination %q", body.Created, wantExplicitPath)
	}
}

// TestDefaultDestinationResolves pins the resolution table's third row:
// with neither a destination nor a category the default_destination
// settings row is the candidate — still through the roots check, so a
// stale stored value outside the roots is 403, and an unset row falls
// back to the first root.
func TestDefaultDestinationResolves(t *testing.T) {
	env := newTasksTestEnv(t)

	// Unset: the migration seeds no row, so the first root applies.
	response := env.createTasks(t, map[string]any{"uris": []string{mixedHTTPS}})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	body := decodeCreateBody(t, response)
	wantFirstRoot := resolvedPath(t, env.dataRoot)
	if len(body.Created) != 1 || body.Created[0].Destination != wantFirstRoot {
		t.Fatalf("created = %+v, want the first root %q", body.Created, wantFirstRoot)
	}
	if body.Created[0].RequestedDestination != nil {
		t.Errorf("requested_destination = %q, want null", *body.Created[0].RequestedDestination)
	}

	configured := filepath.Join(env.dataRoot, "configured")
	if err := os.MkdirAll(configured, 0o755); err != nil {
		t.Fatalf("make configured path: %v", err)
	}
	wantConfigured := resolvedPath(t, configured)

	if _, err := env.db.ExecContext(
		t.Context(),
		`INSERT INTO settings (id, key, value_json, created_at, updated_at) VALUES (?, 'default_destination', ?, 0, 0)`,
		store.NewID(store.PrefixSetting), mustJSON(t, configured),
	); err != nil {
		t.Fatalf("seed default_destination: %v", err)
	}

	response = env.createTasks(t, map[string]any{"uris": []string{mixedFTP}})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	body = decodeCreateBody(t, response)
	if len(body.Created) != 1 || body.Created[0].Destination != wantConfigured {
		t.Fatalf("created = %+v, want the configured default %q", body.Created, wantConfigured)
	}

	// A stored value outside the roots is never validated at write time
	// (PATCH /settings is T092's); the create-time check is the only
	// guard and it answers 403.
	if _, err := env.db.ExecContext(
		t.Context(),
		`UPDATE settings SET value_json = ? WHERE key = 'default_destination'`,
		`"/etc"`,
	); err != nil {
		t.Fatalf("poison default_destination: %v", err)
	}

	response = env.createTasks(t, map[string]any{"uris": []string{mixedMagnet}})
	assertProblem(t, response, http.StatusForbidden, SlugPathRejected)
}

// TestDeleteCategoryKeepsTasks pins the doc 05 section 8.1 delete
// semantics: the category row goes, its tasks survive uncategorised and
// no file is touched.
func TestDeleteCategoryKeepsTasks(t *testing.T) {
	env := newTasksTestEnv(t)

	if response := env.createCategory(t, map[string]any{"name": "linux", "save_path": env.dataRoot}); response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}

	response := env.createTasks(t, map[string]any{
		"uris":     []string{mixedHTTPS},
		"category": "linux",
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	created := decodeCreateBody(t, response).Created[0]

	response = env.deleteCategory(t, "linux")
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusNoContent, response.Body.String())
	}

	var categoryID *string
	if err := env.db.GetContext(
		t.Context(), &categoryID, `SELECT category_id FROM tasks WHERE id = ?`, created.ID,
	); err != nil {
		t.Fatalf("read task category: %v", err)
	}
	if categoryID != nil {
		t.Errorf("task category_id = %q, want NULL after the delete", *categoryID)
	}

	response = env.getTask(t, created.ID)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	var task TaskDTO
	if err := json.Unmarshal(response.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode task body %q: %v", response.Body.String(), err)
	}
	if task.Category != nil {
		t.Errorf("task category = %q, want null", *task.Category)
	}
	if env.countTasks(t) != 1 {
		t.Errorf("%d tasks survived, want exactly the created one", env.countTasks(t))
	}
}

// TestListTagsIncludesZeroCount pins doc 05 section 8.2: every tags row
// lists sorted by name with the count of its non-removed tasks — a tag no
// task carries lists with 0, a tag carried only by removed tasks counts
// 0 but still lists, and the empty list encodes [], never null.
func TestListTagsIncludesZeroCount(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.getTags(t)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"tags":[]`) {
		t.Errorf("empty tag list = %s, want \"tags\":[]", response.Body.String())
	}

	response = env.createTasks(t, map[string]any{
		"uris": []string{mixedHTTPS},
		"tags": []string{"weekly", "iso"},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	env.seedTag(t, "middle")

	tags := decodeTagList(t, env.getTags(t))
	if len(tags) != 3 {
		t.Fatalf("tags = %+v, want [iso middle weekly]", tags)
	}
	want := []TagDTO{{Name: "iso", TaskCount: 1}, {Name: "middle", TaskCount: 0}, {Name: "weekly", TaskCount: 1}}
	for i, tag := range tags {
		if tag != want[i] {
			t.Errorf("tags[%d] = %+v, want %+v — order must be name ascending", i, tag, want[i])
		}
	}

	// Removal is a tombstone: the task_tags rows survive, but the task no
	// longer counts.
	if _, err := env.db.ExecContext(t.Context(), `UPDATE tasks SET state = 'removed'`); err != nil {
		t.Fatalf("remove tasks: %v", err)
	}

	tags = decodeTagList(t, env.getTags(t))
	if len(tags) != 3 {
		t.Fatalf("tags = %+v, want all three rows still listed", tags)
	}
	for _, tag := range tags {
		if tag.TaskCount != 0 {
			t.Errorf("tag %q task_count = %d, want 0 — removed tasks never count", tag.Name, tag.TaskCount)
		}
	}
}

// TestCategoriesAndTagsRequireAuth pins the section 8 routes behind the
// auth middleware: every operation declared credentialRequired, so a
// bearer-less request is 401 /problems/unauthenticated, never public.
func TestCategoriesAndTagsRequireAuth(t *testing.T) {
	env := newTasksTestEnv(t)

	for _, path := range []string{"/categories", "/tags"} {
		assertProblem(t, env.api.Get(path), http.StatusUnauthorized, SlugUnauthenticated)
	}

	response := env.api.Post("/categories", map[string]any{"name": "x", "save_path": env.dataRoot})
	assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)
	response = env.api.Patch("/categories/x", map[string]any{"new_name": "y"})
	assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)
	response = env.api.Delete("/categories/x")
	assertProblem(t, response, http.StatusUnauthorized, SlugUnauthenticated)
}
