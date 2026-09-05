package aria2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/engine"
)

// testSecret is the RPC secret every request must carry as params[0]
// ("token:s3cret"), per docs/06-download-engines.md §4.2.
const testSecret = "s3cret"

// testGID is a recorded aria2 1.37.0 GID shape.
const testGID = "2089b05ecca3d829"

// recordedCall is one JSON-RPC method invocation captured by fakeServer.
type recordedCall struct {
	Method string
	Params []any
}

// wsUpgrader answers the notification dial. The fake is not a browser
// origin, so every origin is accepted.
var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

// fakeServer is an httptest JSON-RPC endpoint. It asserts that the first
// positional parameter of every request — single or batch element — is
// token:<secret>, records every call, and answers from the injected
// responder. A nil result with a nil fault answers "OK". A WebSocket
// upgrade is answered with 101 and held open, so tests can push the six
// notifications of docs/06-download-engines.md §4.5 through notify.
type fakeServer struct {
	t *testing.T

	mu      sync.Mutex
	calls   []recordedCall
	respond func(method string) (any, *rpcFault)
	isBatch bool // whether the last request body was a batch array
	srv     *httptest.Server

	wsMu   sync.Mutex
	wsConn *websocket.Conn // the current notification connection, if any
}

// newTestClient starts a fakeServer and returns a client pointed at it.
func newTestClient(t *testing.T, respond func(method string) (any, *rpcFault)) (*Client, *fakeServer) {
	t.Helper()

	f := &fakeServer{t: t, respond: respond}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)

	c, err := New(Config{URL: f.srv.URL, Secret: testSecret, Timeout: 2 * time.Second}, f.srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, f
}

// okResponder answers every uncalled-for method with the string result "OK".
func okResponder(results map[string]any) func(string) (any, *rpcFault) {
	return func(method string) (any, *rpcFault) {
		if res, ok := results[method]; ok {
			return res, nil
		}
		return "OK", nil
	}
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.t.Helper()

	// A WebSocket dial is the notification transport (§4.5): an Upgrade
	// GET that carries no JSON-RPC call — no token, no body — so it branches
	// before the POST-only assertions below.
	if websocket.IsWebSocketUpgrade(r) {
		f.serveNotifications(w, r)
		return
	}

	if r.Method != http.MethodPost {
		f.t.Errorf("rpc request method = %q, want POST", r.Method)
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		f.t.Errorf("rpc request content-type = %q, want application/json", ct)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read rpc body: %v", err)
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		f.t.Errorf("rpc request body is empty")
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	// A body is either a single call object or a batch array (§4.2); both
	// shapes must carry the token.
	var requests []rpcRequest
	if body[0] == '[' {
		f.setBatch(true)
		if err := json.Unmarshal(body, &requests); err != nil {
			f.t.Errorf("decode rpc batch: %v", err)
			http.Error(w, "decode error", http.StatusBadRequest)
			return
		}
	} else {
		f.setBatch(false)
		var single rpcRequest
		if err := json.Unmarshal(body, &single); err != nil {
			f.t.Errorf("decode rpc request: %v", err)
			http.Error(w, "decode error", http.StatusBadRequest)
			return
		}
		requests = []rpcRequest{single}
	}

	for _, req := range requests {
		f.assertToken(req)
		f.mu.Lock()
		f.calls = append(f.calls, recordedCall{Method: req.Method, Params: req.Params})
		f.mu.Unlock()
	}

	if body[0] == '[' {
		var replies []wireReply
		for _, req := range requests {
			replies = append(replies, replyFor(req, f.respond))
		}
		writeJSON(f.t, w, replies)
		return
	}
	writeJSON(f.t, w, replyFor(requests[0], f.respond))
}

// serveNotifications answers the dial with 101 and holds the connection
// open, draining until the client drops it. Notifications are
// unidirectional (§4.5), so the only legal client frame is the close the
// read loop exits on — that exit, not an error, is the normal end.
func (f *fakeServer) serveNotifications(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		f.t.Errorf("upgrade notification websocket: %v", err)
		return
	}
	defer func() {
		if err := conn.Close(); err != nil {
			f.t.Logf("close notification websocket: %v", err)
		}
	}()

	f.wsMu.Lock()
	f.wsConn = conn
	f.wsMu.Unlock()

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// notify pushes one aria2 notification frame over the upgraded connection,
// waiting for the dial if the client has not connected yet.
func (f *fakeServer) notify(method, gid string) {
	f.t.Helper()

	var conn *websocket.Conn
	require.Eventually(f.t, func() bool {
		f.wsMu.Lock()
		defer f.wsMu.Unlock()
		conn = f.wsConn
		return conn != nil
	}, 2*time.Second, time.Millisecond)

	frame := rpcNotification{
		Method: method,
		Params: []struct {
			GID string `json:"gid"`
		}{{GID: gid}},
	}
	if err := conn.WriteJSON(frame); err != nil {
		f.t.Errorf("write notification %s: %v", method, err)
	}
}

// wireReply is one JSON-RPC reply, result or fault.
type wireReply struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      string    `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcFault `json:"error,omitempty"`
}

func replyFor(req rpcRequest, respond func(string) (any, *rpcFault)) wireReply {
	result, fault := respond(req.Method)
	return wireReply{JSONRPC: "2.0", ID: req.ID, Result: result, Error: fault}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode rpc reply: %v", err)
	}
}

func (f *fakeServer) assertToken(req rpcRequest) {
	f.t.Helper()
	if len(req.Params) == 0 {
		f.t.Errorf("request %s carries no params, want the token", req.Method)
		return
	}
	token, ok := req.Params[0].(string)
	if !ok || token != "token:"+testSecret {
		f.t.Errorf("request %s params[0] = %#v, want %q", req.Method, req.Params[0], "token:"+testSecret)
	}
}

func (f *fakeServer) setBatch(b bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isBatch = b
}

func (f *fakeServer) recorded() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedCall(nil), f.calls...)
}

func (f *fakeServer) wasBatch() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.isBatch
}

// TestCallSendsToken covers the token convention of §4.2: getVersion carries
// token:<secret> as its only positional parameter, and the server asserts the
// token on every request of every test that shares it.
func TestCallSendsToken(t *testing.T) {
	c, f := newTestClient(t, okResponder(map[string]any{
		methodGetVersion: map[string]any{"version": "1.37.0"},
	}))

	version, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if version != "1.37.0" {
		t.Errorf("version = %q, want 1.37.0", version)
	}

	calls := f.recorded()
	if len(calls) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(calls))
	}
	if calls[0].Method != methodGetVersion {
		t.Errorf("method = %q, want %q", calls[0].Method, methodGetVersion)
	}
	if diff := cmp.Diff([]any{"token:" + testSecret}, calls[0].Params); diff != "" {
		t.Errorf("getVersion params mismatch (-want +got):\n%s", diff)
	}
}

func TestAddURI(t *testing.T) {
	c, f := newTestClient(t, okResponder(map[string]any{
		methodAddURI: testGID,
	}))

	id, err := c.Add(context.Background(), engine.AddRequest{
		URIs:        []string{"https://example.org/a.iso", "https://mirror.example.org/a.iso"},
		SaveDir:     "/data/iso",
		Filename:    "a.iso",
		StartPaused: true,
		SelectFiles: []int{0, 2},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if want := engine.NameAria2 + ":" + testGID; id != want {
		t.Errorf("Add id = %q, want %q", id, want)
	}

	calls := f.recorded()
	if len(calls) != 1 {
		t.Fatalf("recorded %d calls, want 1 addUri", len(calls))
	}
	if calls[0].Method != methodAddURI {
		t.Errorf("method = %q, want %q", calls[0].Method, methodAddURI)
	}
	// params: [token, uris, options] — every option value a string (§4.2).
	wantURIs := []any{"https://example.org/a.iso", "https://mirror.example.org/a.iso"}
	if !reflect.DeepEqual(calls[0].Params[1], wantURIs) {
		t.Errorf("addUri uris = %#v, want %#v", calls[0].Params[1], wantURIs)
	}
	wantOpts := map[string]any{"dir": "/data/iso", "out": "a.iso", "pause": "true", "select-file": "1,3"}
	if diff := cmp.Diff(wantOpts, calls[0].Params[2]); diff != "" {
		t.Errorf("addUri options mismatch (-want +got):\n%s", diff)
	}
}

func TestAddMetalinkRecordsFirstGID(t *testing.T) {
	c, f := newTestClient(t, okResponder(map[string]any{
		methodAddMetalink: []any{"1111111111111111", "2222222222222222"},
	}))

	id, err := c.Add(context.Background(), engine.AddRequest{
		Blob:     []byte("<metalink/>"),
		BlobKind: blobKindMetalink,
		SaveDir:  "/data",
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if want := engine.NameAria2 + ":1111111111111111"; id != want {
		t.Errorf("Add id = %q, want %q (first gid of the array)", id, want)
	}

	calls := f.recorded()
	if len(calls) != 1 || calls[0].Method != methodAddMetalink {
		t.Fatalf("recorded %v, want one addMetalink", calls)
	}
	blob, _ := calls[0].Params[1].(string)
	if want := "PG1ldGFsaW5rLz4="; blob != want { // base64("<metalink/>")
		t.Errorf("addMetalink blob = %q, want %q", blob, want)
	}
}

func TestAddTorrentBlobRejected(t *testing.T) {
	c, f := newTestClient(t, okResponder(nil))

	_, err := c.Add(context.Background(), engine.AddRequest{Blob: []byte("d4:info"), BlobKind: "torrent"})
	if !errors.Is(err, engine.ErrNotSupported) {
		t.Errorf("Add torrent blob err = %v, want ErrNotSupported", err)
	}
	if calls := f.recorded(); len(calls) != 0 {
		t.Errorf("Add sent %d requests, want none", len(calls))
	}
}

func TestListSendsOneBatch(t *testing.T) {
	c, f := newTestClient(t, okResponder(map[string]any{
		methodTellActive:  []any{map[string]any{"gid": testGID, "status": "active"}},
		methodTellWaiting: []any{},
		methodTellStopped: []any{map[string]any{"gid": "0000000000000001", "status": "complete"}},
	}))

	infos, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// One POST, carrying all three tell* calls (§4.3), merging every GID the
	// daemon reports — the foreign-transfer filter belongs to T026.
	calls := f.recorded()
	if len(calls) != 3 {
		t.Fatalf("recorded %d calls, want 3", len(calls))
	}
	if !f.wasBatch() {
		t.Error("List did not send its three calls as one batch array")
	}
	for i, method := range []string{methodTellActive, methodTellWaiting, methodTellStopped} {
		if calls[i].Method != method {
			t.Errorf("batch[%d].method = %q, want %q", i, calls[i].Method, method)
		}
	}

	wantIDs := []string{
		engine.NameAria2 + ":" + testGID,
		engine.NameAria2 + ":0000000000000001",
	}
	gotIDs := make([]string, 0, len(infos))
	for _, info := range infos {
		gotIDs = append(gotIDs, info.ID)
	}
	if diff := cmp.Diff(wantIDs, gotIDs); diff != "" {
		t.Errorf("List ids mismatch (-want +got):\n%s", diff)
	}
}

func TestListIncludesQueuesBeyondOneThousand(t *testing.T) {
	const transfersPerQueue int64 = 1001
	c := newListTestClient(t, func(requests []rpcRequest) []wireReply {
		replies := make([]wireReply, len(requests))
		for queue, request := range requests {
			count := int64(1)
			if request.Method != methodTellActive {
				var err error
				count, err = request.Params[2].(json.Number).Int64()
				if err != nil {
					t.Errorf("decode count: %v", err)
					return nil
				}
				count = min(count, transfersPerQueue)
			}
			entries := make([]statusResult, count)
			for index := range entries {
				entries[index] = statusResult{
					GID: fmt.Sprintf("%016x", int64(queue)*transfersPerQueue+int64(index)+1), Status: statusWaiting,
				}
			}
			replies[queue] = wireReply{JSONRPC: request.JSONRPC, ID: request.ID, Result: entries}
		}
		return replies
	})

	infos, err := c.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	const want = 1 + 2*transfersPerQueue
	if int64(len(infos)) != want {
		t.Fatalf("listed %d transfers, want %d", len(infos), want)
	}
	ids := make(map[string]struct{}, len(infos))
	for _, info := range infos {
		ids[info.ID] = struct{}{}
	}
	if len(ids) != len(infos) {
		t.Fatal("list duplicated transfers")
	}
}

func TestListMatchesReplyIDs(t *testing.T) {
	const repliesPerList = 3
	for _, test := range []struct {
		name      string
		transform func([]wireReply) []wireReply
		wantError bool
	}{
		{"reversed", func(replies []wireReply) []wireReply { slices.Reverse(replies); return replies }, false},
		{"duplicate", func(replies []wireReply) []wireReply { replies[0].ID = replies[1].ID; return replies }, true},
		{"unknown", func(replies []wireReply) []wireReply { replies[0].ID = "unknown"; return replies }, true},
		{"missing", func(replies []wireReply) []wireReply { return replies[:len(replies)-1] }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := newListTestClient(t, func(requests []rpcRequest) []wireReply {
				replies := make([]wireReply, len(requests))
				for index, request := range requests {
					replies[index] = wireReply{JSONRPC: request.JSONRPC, ID: request.ID,
						Result: []statusResult{{GID: fmt.Sprintf("%016x", index+1), Status: statusWaiting}},
					}
				}
				return test.transform(replies)
			})
			infos, err := c.List(t.Context())
			if test.wantError {
				if err == nil {
					t.Fatal("malformed reply IDs were accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(infos) != repliesPerList {
				t.Fatalf("got %d results, want %d", len(infos), repliesPerList)
			}
			for index, info := range infos {
				if want := fmt.Sprintf("%s:%016x", engine.NameAria2, index+1); info.ID != want {
					t.Errorf("result %d ID = %s, want %s", index, info.ID, want)
				}
			}
		})
	}
}

// newListTestClient enforces a single authenticated batch and preserves int64 counts.
func newListTestClient(t *testing.T, respond func([]rpcRequest) []wireReply) *Client {
	t.Helper()
	seen := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case seen <- struct{}{}:
		default:
			t.Error("List sent more than one HTTP request")
		}
		var requests []rpcRequest
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&requests); err != nil {
			t.Errorf("decode batch: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		methods := []string{methodTellActive, methodTellWaiting, methodTellStopped}
		if len(requests) != len(methods) {
			t.Errorf("batch has %d requests, want %d", len(requests), len(methods))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for index, request := range requests {
			if request.Method != methods[index] || len(request.Params) == 0 || request.Params[0] != "token:"+testSecret {
				t.Errorf("unexpected batch request: %+v", request)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		writeJSON(t, w, respond(requests))
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, Secret: testSecret}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestGet(t *testing.T) {
	c, _ := newTestClient(t, okResponder(map[string]any{
		methodTellStatus: map[string]any{
			"gid":             testGID,
			"status":          "active",
			"totalLength":     "34896138",
			"completedLength": "1024",
			"downloadSpeed":   "2048",
			"dir":             "/data",
		},
	}))

	info, err := c.Get(context.Background(), engine.NameAria2+":"+testGID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.ID != engine.NameAria2+":"+testGID || info.Engine != engine.NameAria2 {
		t.Errorf("Get id/engine = %q/%q", info.ID, info.Engine)
	}
	if info.State != engine.StateDownloading {
		t.Errorf("Get state = %q, want downloading", info.State)
	}
	if info.CompletedBytes != 1024 || info.DownloadRate != 2048 || info.SaveDir != "/data" {
		t.Errorf("Get mapped fields wrong: %+v", info)
	}
	if info.TotalBytes == nil || *info.TotalBytes != 34896138 {
		t.Errorf("Get TotalBytes = %v, want 34896138", info.TotalBytes)
	}
}

func TestFilesPathsAreRelativeToSaveDir(t *testing.T) {
	directory := t.TempDir()
	for _, test := range []struct {
		name, path, want string
		wantError        bool
	}{
		{"nested", filepath.Join(directory, "collection", "payload.iso"), "collection/payload.iso", false},
		{"root", filepath.Join(directory, "payload.iso"), "payload.iso", false},
		{"unknown", "", "", false},
		{"parent", filepath.Join(directory, "..", "payload.iso"), "", true},
		{"sibling", directory + "-sibling/payload.iso", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, f := newTestClient(t, okResponder(map[string]any{
				methodGetFiles:   []fileEntry{{Index: "1", Path: test.path, Selected: aria2True}},
				methodTellStatus: statusResult{Dir: directory},
			}))
			entries, err := c.Files(t.Context(), engine.NameAria2+":"+testGID)
			if test.wantError {
				if err == nil {
					t.Fatal("accepted a path outside SaveDir")
				}
				return
			}
			if err != nil || len(entries) != 1 {
				t.Fatalf("Files = %+v, error %v", entries, err)
			}
			if entries[0].Path != test.want || entries[0].Index != 0 || !entries[0].Selected || entries[0].Priority != nil {
				t.Fatalf("file = %+v, want relative path %q, zero index and no priority", entries[0], test.want)
			}
			for _, call := range f.recorded() {
				if call.Method != methodTellStatus {
					continue
				}
				want := []any{"token:" + testSecret, testGID, []any{optDir}}
				if diff := cmp.Diff(want, call.Params); diff != "" {
					t.Errorf("directory lookup parameters (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func TestFilesPropagatesDirectoryLookupFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload.iso")
	c, _ := newTestClient(t, func(method string) (any, *rpcFault) {
		if method == methodGetFiles {
			return []fileEntry{{Index: "1", Path: path}}, nil
		}
		return nil, &rpcFault{Code: 1, Message: "GID " + testGID + " is not found"}
	})
	if _, err := c.Files(t.Context(), engine.NameAria2+":"+testGID); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("Files error = %v, want ErrNotFound", err)
	}
}

func TestFilesEmptyNeedsNoDirectoryLookup(t *testing.T) {
	c, f := newTestClient(t, okResponder(map[string]any{methodGetFiles: []fileEntry{}}))
	entries, err := c.Files(t.Context(), engine.NameAria2+":"+testGID)
	if err != nil || len(entries) != 0 || len(f.recorded()) != 1 {
		t.Fatalf("empty Files = %+v, error %v, calls %+v", entries, err, f.recorded())
	}
}

func TestGetUnknownGIDIsNotFound(t *testing.T) {
	// Both real aria2 shapes: a gid the session never issued fails in
	// str2Gid, a purged result fails in tellStatus itself
	// (release-1.37.0 src/RpcMethodImpl.cc).
	for _, msg := range []string{
		"GID ffffffffffffffff is not found",
		"No such download for GID#ffffffffffffffff",
	} {
		c, f := newTestClient(t, func(string) (any, *rpcFault) {
			return nil, &rpcFault{Code: 1, Message: msg}
		})

		_, err := c.Get(context.Background(), engine.NameAria2+":ffffffffffffffff")
		if !errors.Is(err, engine.ErrNotFound) {
			t.Errorf("Get with fault %q: err = %v, want ErrNotFound", msg, err)
		}
		if calls := f.recorded(); len(calls) != 1 {
			t.Errorf("Get sent %d requests, want 1", len(calls))
		}
	}
}

// Faults that fire for downloads that exist but are in the wrong state
// must stay generic errors, not ErrNotFound (§4.7).
func TestAmbiguousFaultsStayGeneric(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		call func(*Client) error
	}{
		{
			"pause of a pausing download",
			"GID#2089b05ecca3d829 cannot be paused now",
			func(c *Client) error { return c.Pause(context.Background(), engine.NameAria2+":"+testGID) },
		},
		{
			"changeOption on a stopped download",
			"Cannot change option for GID#2089b05ecca3d829",
			func(c *Client) error {
				return c.SetFiles(context.Background(), engine.NameAria2+":"+testGID, []int{0}, nil)
			},
		},
		{
			"remove of a dependency-unresolved download",
			"GID#2089b05ecca3d829 cannot be removed now",
			func(c *Client) error { return c.Remove(context.Background(), engine.NameAria2+":"+testGID) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(string) (any, *rpcFault) {
				return nil, &rpcFault{Code: 1, Message: tt.msg}
			})
			err := tt.call(c)
			if err == nil {
				t.Fatal("err = nil, want a generic error")
			}
			if errors.Is(err, engine.ErrNotFound) {
				t.Errorf("err = %v, want a generic error not ErrNotFound", err)
			}
		})
	}
}

func TestPauseSendsBareGID(t *testing.T) {
	c, f := newTestClient(t, okResponder(nil))

	if err := c.Pause(context.Background(), engine.NameAria2+":"+testGID); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	calls := f.recorded()
	if len(calls) != 1 || calls[0].Method != methodPause {
		t.Fatalf("recorded %v, want one pause", calls)
	}
	if diff := cmp.Diff([]any{"token:" + testSecret, testGID}, calls[0].Params); diff != "" {
		t.Errorf("pause params mismatch (-want +got):\n%s", diff)
	}
}

func TestRemove(t *testing.T) {
	stopped := func(method string) (any, *rpcFault) {
		if method == methodRemove {
			// aria2.remove faults on an already-stopped download, which is
			// the normal case for a completed task (§4.3).
			return nil, &rpcFault{Code: 1, Message: "Active Download not found for GID#" + testGID}
		}
		return "OK", nil
	}
	c, f := newTestClient(t, stopped)

	if err := c.Remove(context.Background(), engine.NameAria2+":"+testGID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	calls := f.recorded()
	want := []string{methodRemove, methodRemoveDownloadResult}
	got := make([]string, 0, len(calls))
	for _, call := range calls {
		got = append(got, call.Method)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Remove calls mismatch (-want +got):\n%s", diff)
	}
}

func TestRemoveUnknownGIDIsNotFound(t *testing.T) {
	// remove faults with "Active Download not found" for a GID with no
	// active/waiting group; removeDownloadResult then proves the GID is
	// gone entirely with "Could not remove download result".
	c, _ := newTestClient(t, func(method string) (any, *rpcFault) {
		switch method {
		case methodRemove:
			return nil, &rpcFault{Code: 1, Message: "Active Download not found for GID#ffffffffffffffff"}
		default:
			return nil, &rpcFault{Code: 1, Message: "Could not remove download result of GID#ffffffffffffffff"}
		}
	})

	err := c.Remove(context.Background(), engine.NameAria2+":ffffffffffffffff")
	if !errors.Is(err, engine.ErrNotFound) {
		t.Errorf("Remove err = %v, want ErrNotFound", err)
	}
}

func TestSetFiles(t *testing.T) {
	c, f := newTestClient(t, okResponder(nil))

	if err := c.SetFiles(context.Background(), engine.NameAria2+":"+testGID, []int{0, 2, 4}, nil); err != nil {
		t.Fatalf("SetFiles: %v", err)
	}

	calls := f.recorded()
	if len(calls) != 1 || calls[0].Method != methodChangeOption {
		t.Fatalf("recorded %v, want one changeOption", calls)
	}
	wantOpts := map[string]any{optSelectFile: "1,3,5"}
	if diff := cmp.Diff(wantOpts, calls[0].Params[2]); diff != "" {
		t.Errorf("select-file options mismatch (-want +got):\n%s", diff)
	}

	// "Select nothing" is inexpressible in aria2's select-file: an
	// unsupported capability, not a silent no-op.
	empty, f2 := newTestClient(t, okResponder(nil))
	err := empty.SetFiles(context.Background(), engine.NameAria2+":"+testGID, nil, nil)
	if !errors.Is(err, engine.ErrNotSupported) {
		t.Errorf("SetFiles empty selection err = %v, want ErrNotSupported", err)
	}
	if calls := f2.recorded(); len(calls) != 0 {
		t.Errorf("SetFiles empty selection sent %d requests, want none", len(calls))
	}
}

// aria2 has selection only, no numeric per-file priority, so a non-nil
// priorities map is ErrNotSupported and nothing is sent (§1.1).
func TestSetFilesRejectsPriorities(t *testing.T) {
	c, f := newTestClient(t, okResponder(nil))

	err := c.SetFiles(context.Background(), engine.NameAria2+":"+testGID, []int{0}, map[int]int{0: 6})
	if !errors.Is(err, engine.ErrNotSupported) {
		t.Errorf("SetFiles err = %v, want ErrNotSupported", err)
	}
	if calls := f.recorded(); len(calls) != 0 {
		t.Errorf("SetFiles sent %d requests, want none", len(calls))
	}
}

func TestOptionalMethodsUnsupported(t *testing.T) {
	c, f := newTestClient(t, okResponder(nil))
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
	}{
		{"Rename", func() error { return c.Rename(ctx, "aria2:x", "n") }},
		{"SetCategory", func() error { return c.SetCategory(ctx, "aria2:x", "c") }},
		{"SetShareLimits", func() error { return c.SetShareLimits(ctx, "aria2:x", nil, nil) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, engine.ErrNotSupported) {
				t.Errorf("err = %v, want ErrNotSupported", err)
			}
		})
	}
	if calls := f.recorded(); len(calls) != 0 {
		t.Errorf("unsupported methods sent %d requests, want none", len(calls))
	}
}

func TestSetRateLimits(t *testing.T) {
	c, f := newTestClient(t, okResponder(nil))
	ctx := context.Background()

	down := int64(1048576)
	up := int64(0)
	if err := c.SetRateLimits(ctx, engine.NameAria2+":"+testGID, &down, &up); err != nil {
		t.Fatalf("per-task SetRateLimits: %v", err)
	}
	if err := c.SetRateLimits(ctx, "", &down, nil); err != nil {
		t.Fatalf("global SetRateLimits: %v", err)
	}

	calls := f.recorded()
	want := []struct {
		method string
		opts   map[string]any
	}{
		{methodChangeOption, map[string]any{
			"max-download-limit": "1048576", "max-upload-limit": "0"}},
		{methodChangeGlobalOption, map[string]any{"max-overall-download-limit": "1048576"}},
	}
	if len(calls) != len(want) {
		t.Fatalf("recorded %d calls, want %d", len(calls), len(want))
	}
	for i, w := range want {
		if calls[i].Method != w.method {
			t.Errorf("call[%d].method = %q, want %q", i, calls[i].Method, w.method)
		}
		if diff := cmp.Diff(w.opts, calls[i].Params[len(calls[i].Params)-1]); diff != "" {
			t.Errorf("call[%d] options mismatch (-want +got):\n%s", i, diff)
		}
	}
}

// Capabilities is the declared set, exactly, sorted and stable, without
// rename (the interface contract of task T019).
func TestCapabilities(t *testing.T) {
	c, _ := newTestClient(t, okResponder(nil))

	want := []engine.Capability{
		engine.CapFTP, engine.CapHTTP, engine.CapMetalink, engine.CapPerFileSelect,
		engine.CapPushEvents, engine.CapSetLocation, engine.CapSFTP,
	}
	got := c.Capabilities()
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Capabilities mismatch (-want +got):\n%s", diff)
	}
	for _, cap := range got {
		if cap == engine.CapRename {
			t.Errorf("Capabilities includes rename, which is not declared")
		}
	}
}

func TestAccepts(t *testing.T) {
	c, _ := newTestClient(t, okResponder(nil))

	for _, uri := range []string{"http://e/f", "https://e/f", "ftp://e/f", "sftp://e/f"} {
		if !c.Accepts(uri) {
			t.Errorf("Accepts(%q) = false, want true", uri)
		}
	}
	for _, uri := range []string{"magnet:?xt=urn:btih:x", "ed2k://|file|", "ftps://e/f", "not a uri"} {
		if c.Accepts(uri) {
			t.Errorf("Accepts(%q) = true, want false", uri)
		}
	}
}

// A non-200 status is a transport failure, mapped to ErrUnavailable.
func TestUnavailableOnHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c, err := New(Config{URL: srv.URL, Secret: testSecret, Timeout: time.Second}, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Health(context.Background())
	if !errors.Is(err, engine.ErrUnavailable) {
		t.Errorf("Health err = %v, want ErrUnavailable", err)
	}
}

// TestRegistry covers Register, Get and Names against the real client.
func TestRegistry(t *testing.T) {
	c, _ := newTestClient(t, okResponder(nil))

	r := engine.NewRegistry()
	r.Register(c)

	got, ok := r.Get(engine.NameAria2)
	if !ok || got != c {
		t.Errorf("Get(%q) = %v, %v; want the registered client", engine.NameAria2, got, ok)
	}
	if _, ok := r.Get("qbittorrent"); ok {
		t.Error(`Get("qbittorrent") found an engine, want none`)
	}
	if diff := cmp.Diff([]string{engine.NameAria2}, r.Names()); diff != "" {
		t.Errorf("Names mismatch (-want +got):\n%s", diff)
	}

	// A duplicate name is a composition bug and panics rather than shadowing.
	defer func() {
		if recover() == nil {
			t.Error("duplicate Register did not panic")
		}
	}()
	r.Register(c)
}

// Events emits one progress event per active transfer from its 1 Hz poll.
// The WebSocket transport is T026's; this pins the polling shape.
func TestEventsEmitsProgress(t *testing.T) {
	c, _ := newTestClient(t, okResponder(map[string]any{
		methodTellActive: []any{map[string]any{"gid": testGID, "status": "active", "dir": "/data"}},
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	events, err := c.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	select {
	case ev := <-events:
		if ev.Kind != engine.EventProgress {
			t.Errorf("event kind = %q, want progress", ev.Kind)
		}
		if ev.Info == nil || ev.TaskID != engine.NameAria2+":"+testGID {
			t.Fatalf("event = %+v, want an info-carrying event for %s", ev, testGID)
		}
		if ev.Info.State != engine.StateDownloading {
			t.Errorf("event state = %q, want downloading", ev.Info.State)
		}
	case <-ctx.Done():
		t.Fatal("no event before the poll interval elapsed")
	}

	// Cancelling the context closes the channel; a tick racing the
	// cancellation may have buffered one more event first, so drain until
	// close.
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, open := <-events:
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("channel not closed after cancellation")
		}
	}
}

// The six aria2 WebSocket notifications map onto their event kinds with a
// namespaced task id (§4.5). okResponder answers tellActive with a string,
// which cannot decode as a status list, so the poll stays silent and every
// event on the channel is a pushed notification.
func TestEventsMapsWebSocketNotifications(t *testing.T) {
	c, f := newTestClient(t, okResponder(nil))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	events, err := c.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	for _, want := range []struct {
		method string
		kind   engine.EventKind
	}{
		{notifyDownloadStart, engine.EventStarted},
		{notifyDownloadPause, engine.EventPaused},
		{notifyDownloadStop, engine.EventRemoved},
		{notifyDownloadComplete, engine.EventCompleted},
		{notifyDownloadError, engine.EventError},
		{notifyBtDownloadComplete, engine.EventProgress},
	} {
		f.notify(want.method, testGID)
		select {
		case ev := <-events:
			if ev.TaskID != engine.NameAria2+":"+testGID {
				t.Errorf("notification %s: task id = %q, want %q", want.method, ev.TaskID, engine.NameAria2+":"+testGID)
			}
			if ev.Kind != want.kind {
				t.Errorf("notification %s: kind = %q, want %q", want.method, ev.Kind, want.kind)
			}
		case <-ctx.Done():
			t.Fatalf("no event for notification %s", want.method)
		}
	}
}
