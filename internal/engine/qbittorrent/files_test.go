package qbittorrent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/engine"
)

// filesRequest is one captured call of the files endpoints.
type filesRequest struct {
	Method string
	Path   string
	Form   url.Values
}

// filesFake is a WebAPI stand-in for the two endpoints of T032: it serves
// one canned torrents/files listing and answers torrents/filePrio with a
// configurable status, recording every call. The login and session rules
// mirror fakeServer's (docs/06 section 5.2).
type filesFake struct {
	t   *testing.T
	srv *httptest.Server

	mu           sync.Mutex
	calls        []filesRequest
	files        []fileJSON // served by torrents/files
	filePrioCode int        // answered to torrents/filePrio; 0 means 200
	cookie       string
}

func newFilesFake(t *testing.T, tune func(*filesFake)) *filesFake {
	t.Helper()

	f := &filesFake{t: t}
	if tune != nil {
		tune(f)
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)

	return f
}

func (f *filesFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("parse form: %v", err)
	}
	rec := filesRequest{Method: r.Method, Path: r.URL.Path, Form: r.PostForm}
	if r.Method == http.MethodGet {
		rec.Form = r.URL.Query()
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// record keeps one call: only a request the session accepted is
	// observable behaviour, and the lazy re-login's first 401 attempt is
	// not part of what a test pins.
	record := func() { f.calls = append(f.calls, rec) }

	switch r.URL.Path {
	case "/api/v2/auth/login":
		if rec.Form.Get("username") != testUsername || rec.Form.Get("password") != testPassword {
			http.Error(w, "Fails.", http.StatusUnauthorized)

			return
		}
		f.cookie = "sid-files"
		http.SetCookie(w, &http.Cookie{Name: testCookie, Value: f.cookie, Path: "/", HttpOnly: true})
		w.WriteHeader(http.StatusNoContent)

	case "/api/v2/app/version":
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)

			return
		}
		record()
		_, _ = w.Write([]byte(testVersion))

	case "/api/v2/torrents/files":
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)

			return
		}
		record()
		body, err := json.Marshal(f.files)
		if err != nil {
			f.t.Errorf("marshal listing: %v", err)

			return
		}
		_, _ = w.Write(body)

	case "/api/v2/torrents/filePrio":
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)

			return
		}
		record()
		status := f.filePrioCode
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)

	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

// sessionOK enforces the cookie of the latest login.
func (f *filesFake) sessionOK(r *http.Request) bool {
	cookie, err := r.Cookie(testCookie)

	return err == nil && f.cookie != "" && cookie.Value == f.cookie
}

// prioCalls returns every torrents/filePrio call in order.
func (f *filesFake) prioCalls() []filesRequest {
	f.t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	var calls []filesRequest
	for _, call := range f.calls {
		if call.Path == "/api/v2/"+pathTorrentsFilePrio {
			calls = append(calls, call)
		}
	}
	require.NotEmpty(f.t, calls, "no torrents/filePrio call was made")

	return calls
}

// prioCallCount returns how many torrents/filePrio calls were made.
func (f *filesFake) prioCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0
	for _, call := range f.calls {
		if call.Path == "/api/v2/"+pathTorrentsFilePrio {
			n++
		}
	}

	return n
}

// newFilesClient returns a client over one fake. Connect is never called:
// the files endpoints reach the daemon through the lazy re-login of
// authenticated, and skipping Connect keeps the sync/maindata poll out of
// the fixture.
func newFilesClient(t *testing.T, f *filesFake) *Client {
	t.Helper()

	c, err := New(Config{BaseURL: f.srv.URL, Username: testUsername, Password: testPassword}, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, c.Close())
	})

	return c
}

// fourFileListing is a listing with one file per priority shape,
// including the -1 (Mixed) aggregate row of a folder.
func fourFileListing() []fileJSON {
	return []fileJSON{
		{Index: 0, Name: "ubuntu.iso", Size: 1000, Progress: 0.5, Priority: 1},
		{Index: 1, Name: "extras/SHA256SUMS", Size: 100, Progress: 1, Priority: 7},
		{Index: 2, Name: "extras/sample.mkv", Size: 200, Progress: 0, Priority: 0},
		{Index: 3, Name: "extras", Size: 10, Progress: 0.25, Priority: -1},
	}
}

func intPtr(i int) *int { return &i }

func TestFilesMapsListing(t *testing.T) {
	f := newFilesFake(t, func(fake *filesFake) { fake.files = fourFileListing() })
	c := newFilesClient(t, f)

	entries, err := c.Files(context.Background(), engine.NameQBittorrent+":"+testHash)
	require.NoError(t, err)
	require.Len(t, entries, 4)

	// The identity mapping of 06 section 1.1, with the -1 (Mixed)
	// aggregate degrading to normal instead of erroring.
	require.Equal(t, []engine.FileEntry{
		{Index: 0, Path: "ubuntu.iso", Size: 1000, Completed: 500, Selected: true, Priority: intPtr(1)},
		{Index: 1, Path: "extras/SHA256SUMS", Size: 100, Completed: 100, Selected: true, Priority: intPtr(7)},
		{Index: 2, Path: "extras/sample.mkv", Size: 200, Completed: 0, Selected: false, Priority: intPtr(0)},
		{Index: 3, Path: "extras", Size: 10, Completed: 2, Selected: true, Priority: intPtr(1)},
	}, entries)

	// The listing named the torrent by hash.
	f.mu.Lock()
	last := f.calls[len(f.calls)-1]
	f.mu.Unlock()
	require.Equal(t, "/api/v2/"+pathTorrentsFiles, last.Path)
	require.Equal(t, testHash, last.Form.Get("hash"))
}

func TestSetFilesGroupsByPriority(t *testing.T) {
	f := newFilesFake(t, nil)
	c := newFilesClient(t, f)

	err := c.SetFiles(context.Background(), engine.NameQBittorrent+":"+testHash, nil,
		map[int]int{0: filePriorityHigh, 1: filePriorityMaximum, 2: filePrioritySkip, 3: filePriorityHigh})
	require.NoError(t, err)

	// One POST per priority, ids pipe-separated, ascending priority and
	// index order; the vocabulary is sent as-is and never as 4.
	calls := f.prioCalls()
	require.Len(t, calls, 3)
	want := []url.Values{
		{"hash": {testHash}, "id": {"2"}, "priority": {"0"}},
		{"hash": {testHash}, "id": {"0|3"}, "priority": {"6"}},
		{"hash": {testHash}, "id": {"1"}, "priority": {"7"}},
	}
	for i, call := range calls {
		require.Equal(t, http.MethodPost, call.Method)
		require.Equal(t, want[i], call.Form)
	}
}

func TestRejectsPriority4(t *testing.T) {
	// 4 is libtorrent's internal scale and -1 the read-only Mixed
	// aggregate; every other value outside {0,1,6,7} is equally wrong.
	for index, value := range map[int]int{
		0: 4, 1: -1, 2: 2, 3: 5, 4: 8,
	} {
		f := newFilesFake(t, nil)
		c := newFilesClient(t, f)

		err := c.SetFiles(context.Background(), engine.NameQBittorrent+":"+testHash, nil,
			map[int]int{index: value})
		require.ErrorContains(t, err, "outside the WebAPI vocabulary")
		require.ErrorContains(t, err, strconv.Itoa(value))

		// Rejected before any request: the daemon was never contacted.
		require.Zero(t, f.prioCallCount(), "a rejected priority reached the daemon")
	}
}

func TestSetFilesSelectionNormalisation(t *testing.T) {
	f := newFilesFake(t, func(fake *filesFake) { fake.files = fourFileListing() })
	c := newFilesClient(t, f)

	// selected names the complete desired selection: 0 and 2 keep their
	// place as normal, 3 is promoted explicitly, and the reported 1 — in
	// neither argument — is deselected.
	err := c.SetFiles(context.Background(), engine.NameQBittorrent+":"+testHash,
		[]int{0, 2}, map[int]int{3: filePriorityHigh})
	require.NoError(t, err)

	calls := f.prioCalls()
	require.Len(t, calls, 3)
	want := []url.Values{
		{"hash": {testHash}, "id": {"1"}, "priority": {"0"}},
		{"hash": {testHash}, "id": {"0|2"}, "priority": {"1"}},
		{"hash": {testHash}, "id": {"3"}, "priority": {"6"}},
	}
	for i, call := range calls {
		require.Equal(t, want[i], call.Form)
	}
}

func TestSetFilesFilePrioConflictNamesTheIndex(t *testing.T) {
	f := newFilesFake(t, func(fake *filesFake) { fake.filePrioCode = http.StatusConflict })
	c := newFilesClient(t, f)

	err := c.SetFiles(context.Background(), engine.NameQBittorrent+":"+testHash, nil,
		map[int]int{5: filePriorityNormal})
	require.Error(t, err)
	require.ErrorContains(t, err, "out of range")
	// The failing group's ids are named so a stale listing is tellable
	// from a transport fault.
	require.ErrorContains(t, err, "5")
}

func TestSetFilesSelectionKeepsExplicitValues(t *testing.T) {
	f := newFilesFake(t, func(fake *filesFake) { fake.files = fourFileListing() })
	c := newFilesClient(t, f)

	// An index that appears in both arguments keeps its explicit priority:
	// 1 is selected and maximum, not demoted to normal by the selection.
	// The reported indices outside the selection — 2 and the Mixed folder
	// row 3 — are deselected, exactly as the normalisation promises.
	err := c.SetFiles(context.Background(), engine.NameQBittorrent+":"+testHash,
		[]int{0, 1}, map[int]int{1: filePriorityMaximum})
	require.NoError(t, err)

	calls := f.prioCalls()
	require.Len(t, calls, 3)
	joined := make([]string, 0, len(calls))
	for _, call := range calls {
		joined = append(joined, call.Form.Get("id")+"@"+call.Form.Get("priority"))
	}
	require.Equal(t, []string{"2|3@0", "0@1", "1@7"}, joined)
}

func TestGroupByPriorityOrder(t *testing.T) {
	groups := groupByPriority(map[int]int{9: 0, 1: 6, 4: 0, 2: 7, 0: 6})
	require.Equal(t, []priorityGroup{
		{priority: 0, indices: []int{4, 9}},
		{priority: 6, indices: []int{0, 1}},
		{priority: 7, indices: []int{2}},
	}, groups)
}
