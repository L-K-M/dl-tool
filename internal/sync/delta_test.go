// Diff, projection, loop and endpoint tests for the rid-delta machinery of
// T025. The endpoint tests live here because this is the task's one test
// file: they drive the api handlers over humatest, which is why the api
// surface they need (NewSSEHandlers, RegisterOperations) is exported.

package sync_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/google/go-cmp/cmp"

	"github.com/L-K-M/dl-tool/internal/api"
	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/store"
	isync "github.com/L-K-M/dl-tool/internal/sync"
)

// taskFixture is one row the snapshot source hands Project.
func taskFixture(id string) store.Task {
	return store.Task{
		ID:             id,
		Engine:         "aria2",
		SourceKind:     "http",
		SourceURI:      strPtr("https://files.example.org/" + id + ".iso"),
		Name:           id + ".iso",
		State:          "downloading",
		Destination:    "/data",
		TotalBytes:     i64Ptr(2000),
		CompletedBytes: 1000,
		DownloadRate:   1024,
		Sequential:     1,
		AddedAt:        1725000000000,
		UpdatedAt:      1725000001000,
	}
}

func strPtr(v string) *string { return &v }
func i64Ptr(v int64) *int64   { return &v }

// snapState is a concurrency-safe stand-in for the store the loop reads.
type snapState struct {
	mu     stdsync.Mutex
	tasks  map[string]store.Task
	failOn bool
	failed int // snapshot calls that failed, so tests can await real ticks
}

func newSnapState(tasks ...store.Task) *snapState {
	byID := make(map[string]store.Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}

	return &snapState{tasks: byID}
}

func (s *snapState) set(t store.Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[t.ID] = t
}

func (s *snapState) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, id)
}

func (s *snapState) fail(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failOn = fail
}

// snapshot projects the current tasks the way the server's source does.
func (s *snapState) snapshot(context.Context) (isync.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failOn {
		s.failed++
		return nil, errSnapshotUnavailable
	}

	snap := make(isync.Snapshot, len(s.tasks))
	for id, t := range s.tasks {
		snap[id] = isync.Project(t)
	}

	return snap, nil
}

type snapshotError struct{}

func (snapshotError) Error() string { return "snapshot unavailable" }

var errSnapshotUnavailable error = snapshotError{}

// emptySeed is the cheap full side of a miss on a hub with no cached state.
func emptySeed() isync.Delta { return isync.Delta{} }

// currentRID reads the hub's rid counter through a subscribe seed, which
// carries RID = the newest published rid.
func currentRID(t *testing.T, hub *isync.Hub) int64 {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, unsub := hub.Subscribe(ctx, "", emptySeed)
	seed := <-ch
	unsub()

	return seed.RID
}

// waitForRID polls the rid counter until it reaches want, proving the loop
// published.
func waitForRID(t *testing.T, hub *isync.Hub, want int64) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if currentRID(t, hub) >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("rid %d was never published; hub stands at %d", want, currentRID(t, hub))
}

// deltaSince reads the coalesced diff after one rid through a subscribe seed.
func deltaSince(t *testing.T, hub *isync.Hub, lastEventID string) isync.Delta {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, unsub := hub.Subscribe(ctx, lastEventID, emptySeed)
	seed := <-ch
	unsub()

	return seed
}

// failedTicks reports how many snapshot calls have failed, so a test can
// prove the loop really ran its failing branch instead of sleeping past it.
func (s *snapState) failedTicks() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.failed
}

// startLoop runs the hub's diff loop in the background of a test. Cleanup
// cancels the loop and then insists it exited cleanly: a loop that died on
// its own error would otherwise surface only as a publish timeout.
func startLoop(t *testing.T, hub *isync.Hub, snap func(context.Context) (isync.Snapshot, error)) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- hub.Loop(ctx, 2*time.Millisecond, snap)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-loopDone:
			if err != nil {
				t.Errorf("hub loop exited with error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("hub loop did not exit within 2s of cancel")
		}
	})
}

func TestDiffOnlyChangedTasks(t *testing.T) {
	a := taskFixture("tsk_a")
	b := taskFixture("tsk_b")

	prev := isync.Snapshot{a.ID: isync.Project(a), b.ID: isync.Project(b)}

	mutated := a
	mutated.State = "paused"
	mutated.UpdatedAt += 1000
	next := isync.Snapshot{a.ID: isync.Project(mutated), b.ID: isync.Project(b)}

	changed, removed := isync.Diff(prev, next)

	if len(removed) != 0 {
		t.Fatalf("no task was removed, got tasks_removed %v", removed)
	}
	if len(changed) != 1 {
		t.Fatalf("exactly one task changed, got %d: %v", len(changed), changed)
	}

	patch, ok := changed["tsk_a"]
	if !ok {
		t.Fatalf("the changed delta names the mutated task, got %v", changed)
	}

	// Only the two mutated fields appear, and nothing else.
	wantAt := time.UnixMilli(mutated.UpdatedAt).UTC().Format(time.RFC3339)
	want := `{"state":"paused","updated_at":"` + wantAt + `"}`
	if diff := cmp.Diff(want, string(patch)); diff != "" {
		t.Fatalf("the patch carries only the changed fields:\n%s", diff)
	}
}

// A task absent from prev projects its whole wire object, not a patch.
func TestDiffCarriesTheWholeTaskWhenNew(t *testing.T) {
	a := taskFixture("tsk_a")

	changed, removed := isync.Diff(isync.Snapshot{}, isync.Snapshot{a.ID: isync.Project(a)})

	if len(removed) != 0 || len(changed) != 1 {
		t.Fatalf("one new task, got changed %v removed %v", changed, removed)
	}

	var whole map[string]any
	if err := json.Unmarshal(changed["tsk_a"], &whole); err != nil {
		t.Fatalf("unmarshal new task: %v", err)
	}
	if whole["state"] != "downloading" || whole["id"] != "tsk_a" {
		t.Fatalf("the new task carries its fields, got %v", whole)
	}
}

func TestDiffReportsRemovedTasks(t *testing.T) {
	a := taskFixture("tsk_a")
	b := taskFixture("tsk_b")
	c := taskFixture("tsk_c")

	prev := isync.Snapshot{a.ID: isync.Project(a), b.ID: isync.Project(b), c.ID: isync.Project(c)}
	next := isync.Snapshot{a.ID: isync.Project(a), c.ID: isync.Project(c)}

	changed, removed := isync.Diff(prev, next)

	if diff := cmp.Diff([]string{"tsk_b"}, removed); diff != "" {
		t.Fatalf("the removed task appears only in tasks_removed:\n%s", diff)
	}
	if len(changed) != 0 {
		t.Fatalf("no surviving task changed, got %v", changed)
	}
}

func TestIdleTickPublishesNothing(t *testing.T) {
	hub := isync.NewHub()
	state := newSnapState(taskFixture("tsk_a"), taskFixture("tsk_b"))
	startLoop(t, hub, state.snapshot)

	waitForRID(t, hub, 1)

	// ~25 idle ticks: nothing may publish, and the ring stays at rid 1.
	time.Sleep(50 * time.Millisecond)

	if rid := currentRID(t, hub); rid != 1 {
		t.Fatalf("an idle second publishes no rid, got rid %d", rid)
	}

	d := deltaSince(t, hub, "1")
	if len(d.Tasks) != 0 || len(d.TasksRemoved) != 0 {
		t.Fatalf("nothing followed rid 1, got tasks %v removed %v", d.Tasks, d.TasksRemoved)
	}
	if d.RID != 1 {
		t.Fatalf("the reader still stands at rid 1, got %d", d.RID)
	}
}

func TestLoopPublishesOnlyTheChangedTask(t *testing.T) {
	hub := isync.NewHub()
	state := newSnapState(taskFixture("tsk_a"), taskFixture("tsk_b"))
	startLoop(t, hub, state.snapshot)

	waitForRID(t, hub, 1)

	// Mutate one task: the delta names that id and no other.
	mutated := taskFixture("tsk_a")
	mutated.State = "paused"
	state.set(mutated)

	waitForRID(t, hub, 2)

	d := deltaSince(t, hub, "1")
	if len(d.Tasks) != 1 {
		t.Fatalf("one task changed, got %v", d.Tasks)
	}
	patch, ok := d.Tasks["tsk_a"]
	if !ok {
		t.Fatalf("the delta names the mutated task, got %v", d.Tasks)
	}

	var fields map[string]any
	if err := json.Unmarshal(patch, &fields); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	if fields["state"] != "paused" {
		t.Fatalf("the patch carries the new state, got %v", fields)
	}
	if _, carriesName := fields["name"]; carriesName {
		t.Fatalf("an unchanged field never appears, got %v", fields)
	}

	// Remove the other: it appears only in tasks_removed.
	state.remove("tsk_b")

	waitForRID(t, hub, 3)

	d = deltaSince(t, hub, "2")
	if diff := cmp.Diff([]string{"tsk_b"}, d.TasksRemoved); diff != "" {
		t.Fatalf("the removal appears only in tasks_removed:\n%s", diff)
	}
	if len(d.Tasks) != 0 {
		t.Fatalf("no task changed alongside the removal, got %v", d.Tasks)
	}
}

// A failed tick skips without disturbing the previous snapshot, so the next
// good tick folds the whole missed window into one delta.
func TestLoopFoldsTheWindowAroundAFailedTick(t *testing.T) {
	hub := isync.NewHub()
	state := newSnapState(taskFixture("tsk_a"), taskFixture("tsk_b"))
	startLoop(t, hub, state.snapshot)

	waitForRID(t, hub, 1)

	// Fail the source while both a mutation and a removal land. Wait until
	// the loop has really run (and skipped) a few failed ticks — a fixed
	// sleep could race past the branch entirely on a loaded runner.
	state.fail(true)
	mutated := taskFixture("tsk_a")
	mutated.CompletedBytes = 1500
	state.set(mutated)
	state.remove("tsk_b")
	deadline := time.Now().Add(2 * time.Second)
	for state.failedTicks() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if state.failedTicks() < 3 {
		t.Fatalf("the loop never ran a failed tick in 2s")
	}
	if rid := currentRID(t, hub); rid != 1 {
		t.Fatalf("a failed tick publishes nothing, got rid %d", rid)
	}

	// The source recovers; one delta carries both changes.
	state.fail(false)
	waitForRID(t, hub, 2)

	d := deltaSince(t, hub, "1")
	if diff := cmp.Diff([]string{"tsk_b"}, d.TasksRemoved); diff != "" {
		t.Fatalf("the missed removal is folded in:\n%s", diff)
	}
	var fields map[string]any
	if err := json.Unmarshal(d.Tasks["tsk_a"], &fields); err != nil {
		t.Fatalf("unmarshal patch: %v", err)
	}
	if fields["completed_bytes"] != float64(1500) {
		t.Fatalf("the missed mutation is folded in, got %v", fields)
	}
}

func TestProjectRendersWireScalars(t *testing.T) {
	row := taskFixture("tsk_a")
	row.SourceURI = strPtr("ftp://user:secret@mirror.example.org/pub/file.iso")
	row.ETASeconds = i64Ptr(10)
	row.StartedAt = i64Ptr(1725000000500)

	wire := isync.Project(row)

	// Bytes and rates marshal as JSON integers.
	if v, ok := wire["completed_bytes"].(int64); !ok || v != 1000 {
		t.Fatalf("completed_bytes is an integer, got %v (%T)", wire["completed_bytes"], wire["completed_bytes"])
	}
	// Timestamps are RFC 3339 UTC.
	wantAt := time.UnixMilli(1725000000000).UTC().Format(time.RFC3339)
	if wire["added_at"] != wantAt {
		t.Fatalf("added_at renders RFC 3339 UTC, got %v want %v", wire["added_at"], wantAt)
	}
	if started, ok := wire["started_at"].(string); !ok || started != time.UnixMilli(1725000000500).UTC().Format(time.RFC3339) {
		t.Fatalf("started_at renders RFC 3339 UTC, got %v", wire["started_at"])
	}
	// progress derives from the byte counts.
	if wire["progress"] != 0.5 {
		t.Fatalf("progress is completed/total, got %v", wire["progress"])
	}
	// A null column stays null; a boolean stays boolean.
	if wire["error_code"] != nil {
		t.Fatalf("a null column projects to nil, got %v", wire["error_code"])
	}
	if wire["sequential"] != true {
		t.Fatalf("sequential projects as boolean, got %v", wire["sequential"])
	}
	// The stored source may embed credentials; the wire never does.
	if wire["source_uri"] != "ftp://mirror.example.org/pub/file.iso" {
		t.Fatalf("source_uri strips userinfo, got %v", wire["source_uri"])
	}
	if wire["eta_seconds"] != int64(10) {
		t.Fatalf("eta_seconds dereferences its column, got %v", wire["eta_seconds"])
	}

	// Unknown size: progress is 0.0, total_bytes null.
	unknown := taskFixture("tsk_b")
	unknown.TotalBytes = nil
	wire = isync.Project(unknown)
	if wire["progress"] != 0.0 || wire["total_bytes"] != nil {
		t.Fatalf("unknown size yields progress 0.0 and total_bytes null, got %v %v", wire["progress"], wire["total_bytes"])
	}
}

// A source whose credentials cannot be stripped — a scheme-less opaque
// URI, where they ride in u.Opaque and u.User is nil — is never echoed;
// authority and magnet sources still render.
func TestProjectDropsUnsanitizableSources(t *testing.T) {
	cases := []struct {
		stored string
		want   any
	}{
		{"ftp://user:secret@mirror.example.org/pub/file.iso", "ftp://mirror.example.org/pub/file.iso"},
		{"user:secret@mirror.example.org", nil},
		{"magnet:?xt=urn:btih:abcdef", "magnet:?xt=urn:btih:abcdef"},
		{"/data/watch/file.torrent", "/data/watch/file.torrent"},
		{"mailto:admin@example.org", nil}, // opaque without credentials: dropped too
	}

	for _, tc := range cases {
		row := taskFixture("tsk_a")
		row.SourceURI = strPtr(tc.stored)

		if got := isync.Project(row)["source_uri"]; got != tc.want {
			t.Fatalf("stored %q projects to %v, want %v", tc.stored, got, tc.want)
		}
	}
}

// --- endpoint tests -------------------------------------------------------

// sseFrame is one parsed SSE frame of the stream.
type sseFrame struct {
	comment string
	retry   string
	id      string
	event   string
	data    string
}

// parseFrames splits an SSE body into frames and fields.
func parseFrames(body string) []sseFrame {
	var frames []sseFrame
	for _, block := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var f sseFrame
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, ": "):
				f.comment = strings.TrimPrefix(line, ": ")
			case strings.HasPrefix(line, "retry: "):
				f.retry = strings.TrimPrefix(line, "retry: ")
			case strings.HasPrefix(line, "id: "):
				f.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			}
		}
		frames = append(frames, f)
	}

	return frames
}

// eventsTestAPI mounts the SSE handlers on a bare humatest API — no auth
// middleware, no store: the tests drive the hub directly.
func eventsTestAPI(t *testing.T, hub *isync.Hub, opts ...api.SSEHandlerOption) humatest.TestAPI {
	t.Helper()

	h := api.NewSSEHandlers(hub, nil, opts...)
	_, testAPI := humatest.New(t)
	h.RegisterOperations(testAPI)

	return testAPI
}

// getEvents closes the stream shortly after the seed, so the handler returns
// and the recorder holds everything written meanwhile.
func getEvents(t *testing.T, testAPI humatest.TestAPI, header ...string) string {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)

	rec := testAPI.GetCtx(ctx, "/events", anyArgs(header)...)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /events answered %d: %s", rec.Code, rec.Body.String())
	}

	return rec.Body.String()
}

func anyArgs(header []string) []any {
	args := make([]any, len(header))
	for i, h := range header {
		args[i] = h
	}

	return args
}

// publishDelta publishes one delta with a single-task patch, as the loop
// would after a state change.
func publishDelta(hub *isync.Hub, id, state string, stats isync.Stats) {
	hub.Publish(isync.Delta{
		Tasks: map[string]json.RawMessage{
			id: json.RawMessage(`{"state":"` + state + `"}`),
		},
		TasksRemoved: []string{},
		Stats:        stats,
	})
}

func syncEventFrames(t *testing.T, body string) []sseFrame {
	t.Helper()

	var out []sseFrame
	for _, f := range parseFrames(body) {
		if f.event == "sync" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no sync event in stream:\n%s", body)
	}

	return out
}

func TestSSEAndSyncPayloadsAreIdentical(t *testing.T) {
	hub := isync.NewHub()
	publishDelta(hub, "tsk_a", "downloading", isync.Stats{SpeedDown: 1024, Active: 1})
	publishDelta(hub, "tsk_a", "paused", isync.Stats{SpeedDown: 0, Active: 1, Queued: 1})

	testAPI := eventsTestAPI(t, hub)

	// Ring-hit path: the stream resumed at rid 1 and /sync asked for rid 1
	// answer with the same object.
	body := getEvents(t, testAPI, "Last-Event-ID: 1")
	frames := parseFrames(body)
	if frames[0].retry != "3000" {
		t.Fatalf("the first message carries retry 3000, got %q in\n%s", frames[0].retry, body)
	}
	for _, f := range frames[1:] {
		if f.retry != "" {
			t.Fatalf("retry appears only on the first message, got %q again", f.retry)
		}
	}

	synced := syncEventFrames(t, body)
	first := synced[0]
	if first.id != "2" {
		t.Fatalf("the id line is the payload's rid, got id %q in\n%s", first.id, body)
	}

	var payload struct {
		RID int64 `json:"rid"`
	}
	if err := json.Unmarshal([]byte(first.data), &payload); err != nil {
		t.Fatalf("unmarshal data line: %v", err)
	}
	if payload.RID != 2 {
		t.Fatalf("the id line equals the rid inside the payload, got id %s rid %d", first.id, payload.RID)
	}

	rec := testAPI.Get("/sync?rid=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sync?rid=1 answered %d: %s", rec.Code, rec.Body.String())
	}
	if diff := cmp.Diff(first.data, strings.TrimSpace(rec.Body.String())); diff != "" {
		t.Fatalf("the /sync body and the /events data line are not byte-identical:\n%s", diff)
	}

	// Miss path: no Last-Event-ID, and rid=0, both force the same full
	// update through the same seed.
	body = getEvents(t, testAPI)
	first = syncEventFrames(t, body)[0]
	rec = testAPI.Get("/sync?rid=0")
	if diff := cmp.Diff(first.data, strings.TrimSpace(rec.Body.String())); diff != "" {
		t.Fatalf("the forced full update is also byte-identical:\n%s", diff)
	}
}

// A Last-Event-ID the ring has evicted yields full_update and seq_gap.
func TestEvictedLastEventIDForcesAFullUpdate(t *testing.T) {
	hub := isync.NewHub()
	publishDelta(hub, "tsk_a", "downloading", isync.Stats{})
	// Overflow the 300-deep ring so rid 1 is evicted.
	for i := 0; i < isync.RingDepth; i++ {
		publishDelta(hub, "tsk_a", "downloading", isync.Stats{})
	}

	testAPI := eventsTestAPI(t, hub)

	body := getEvents(t, testAPI, "Last-Event-ID: 1")
	first := syncEventFrames(t, body)[0]

	var payload struct {
		RID        int64 `json:"rid"`
		FullUpdate bool  `json:"full_update"`
		SeqGap     bool  `json:"seq_gap"`
	}
	if err := json.Unmarshal([]byte(first.data), &payload); err != nil {
		t.Fatalf("unmarshal data line: %v", err)
	}
	if !payload.FullUpdate || !payload.SeqGap {
		t.Fatalf("an evicted Last-Event-ID forces full_update and seq_gap, got %+v in\n%s", payload, first.data)
	}

	// The polling fallback honours the same miss rule.
	rec := testAPI.Get("/sync?rid=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sync?rid=1 answered %d: %s", rec.Code, rec.Body.String())
	}
	var delta struct {
		FullUpdate bool `json:"full_update"`
		SeqGap     bool `json:"seq_gap"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &delta); err != nil {
		t.Fatalf("unmarshal sync body: %v", err)
	}
	if !delta.FullUpdate || !delta.SeqGap {
		t.Fatalf("the evicted rid also forces the full update over polling, got %+v", delta)
	}
}

func TestSyncRejectsNegativeRID(t *testing.T) {
	// Through the real server constructor, which installs the problem
	// factory a bare humatest API lacks; nil db skips auth without touching
	// the registered operations.
	server, err := api.NewServer(&config.Config{}, nil, slog.Default())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	testAPI := humatest.Wrap(t, server.API)

	rec := testAPI.Get("/sync?rid=-1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a negative rid answers 422, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/problems/validation-failed") {
		t.Fatalf("the problem names validation-failed, got %s", rec.Body.String())
	}
}

// While idle the stream beats: a comment for the proxies and a named hb
// event for the EventSource.
func TestEventsHeartbeatWhileIdle(t *testing.T) {
	hub := isync.NewHub()
	testAPI := eventsTestAPI(t, hub, api.WithHeartbeatEvery(40*time.Millisecond))

	body := getEvents(t, testAPI)

	// The comment heartbeat asserted through parsed frames: a substring
	// check against ": hb" would also match inside "event: hb".
	commentBeats := 0
	for _, f := range parseFrames(body) {
		if f.comment == "hb" {
			commentBeats++
		}
	}
	if commentBeats == 0 {
		t.Fatalf("the idle stream carries the comment heartbeat:\n%s", body)
	}
	if !strings.Contains(body, "event: hb") {
		t.Fatalf("the idle stream carries the named hb event:\n%s", body)
	}

	// Exactly one retry, on the first frame.
	frames := parseFrames(body)
	retries := 0
	for _, f := range frames {
		if f.retry != "" {
			retries++
		}
	}
	if retries != 1 {
		t.Fatalf("retry appears exactly once, got %d in\n%s", retries, body)
	}
}
