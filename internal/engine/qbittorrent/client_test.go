package qbittorrent

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/engine"
)

// Shared fixtures. The fake may name the session cookie — only the client
// is forbidden from doing so (docs/06 section 5.2).
const (
	testUsername  = "admin"
	testPassword  = "password123"
	testCookie    = "QBT_SID_8080"
	testVersion   = "v5.2.3"
	testWebapi    = "2.15.1"
	testHash      = "8c212779b4abde7c6bc608063a0d008b7e40ce32"
	otherHash     = "54eddd830a5b58480a6143d616a97e3a6c23c439"
	testMaxUpload = 1 << 20
)

// uploadedFile is one captured multipart file part.
type uploadedFile struct {
	ContentType string
	Data        []byte
}

// recordedRequest is one captured API call.
type recordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Form   url.Values
	Files  map[string][]uploadedFile
}

// fakeServer is a qBittorrent WebAPI stand-in. It enforces the session
// cookie on every authenticated path and answers from the knobs set before
// the client talks to it.
type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu     sync.Mutex
	calls  []recordedRequest
	logins int
	cookie string

	loginRefuse   bool // answer auth/login with 401
	loginNoCookie bool // answer auth/login 204 without Set-Cookie
	legacyAPI     bool // answer torrents/stop|start with 404
	refuseOnce    map[string]bool
	addStatus     int
	addBody       string
}

func newFakeServer(t *testing.T, tune func(*fakeServer)) *fakeServer {
	t.Helper()

	f := &fakeServer{t: t, refuseOnce: map[string]bool{}, addStatus: http.StatusOK}
	if tune != nil {
		tune(f)
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := f.record(r)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, rec)

	if f.refuseOnce[r.URL.Path] {
		delete(f.refuseOnce, r.URL.Path)
		http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.URL.Path {
	case "/api/v2/auth/login":
		f.logins++
		if f.loginRefuse || rec.Form.Get("username") != testUsername || rec.Form.Get("password") != testPassword {
			http.Error(w, "Fails.", http.StatusUnauthorized)
			return
		}
		if f.loginNoCookie {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		f.cookie = "sid-" + strconv.Itoa(f.logins)
		http.SetCookie(w, &http.Cookie{Name: testCookie, Value: f.cookie, Path: "/", HttpOnly: true})
		w.WriteHeader(http.StatusNoContent)

	case "/api/v2/app/version":
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(testVersion))

	case "/api/v2/app/webapiVersion":
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(testWebapi))

	case "/api/v2/torrents/add":
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(f.addStatus)
		_, _ = w.Write([]byte(f.addBody))

	case "/api/v2/torrents/stop", "/api/v2/torrents/start":
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
			return
		}
		if f.legacyAPI {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)

	case "/api/v2/torrents/pause", "/api/v2/torrents/resume", "/api/v2/torrents/delete":
		if !f.sessionOK(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)

	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

// record captures one request's method, headers, fields and files. It runs
// on the httptest handler goroutine, so it reports with Errorf and answers
// the request; Fatalf would Goexit the handler goroutine, not the test.
func (f *fakeServer) record(r *http.Request) recordedRequest {
	rec := recordedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone()}

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(testMaxUpload); err != nil {
			f.t.Errorf("parse multipart form: %v", err)
			return rec
		}
		rec.Form = url.Values{}
		for name, values := range r.MultipartForm.Value {
			rec.Form[name] = append([]string(nil), values...)
		}
		rec.Files = map[string][]uploadedFile{}
		for name, headers := range r.MultipartForm.File {
			for _, header := range headers {
				file, err := header.Open()
				if err != nil {
					f.t.Errorf("open uploaded file: %v", err)
					continue
				}
				data, err := io.ReadAll(file)
				closeErr := file.Close()
				if err != nil {
					f.t.Errorf("read uploaded file: %v", err)
					continue
				}
				if closeErr != nil {
					f.t.Errorf("close uploaded file: %v", closeErr)
				}
				rec.Files[name] = append(rec.Files[name],
					uploadedFile{ContentType: header.Header.Get("Content-Type"), Data: data})
			}
		}
		return rec
	}

	if err := r.ParseForm(); err != nil {
		f.t.Errorf("parse form: %v", err)
	}
	rec.Form = url.Values{}
	for name, values := range r.Form {
		rec.Form[name] = append([]string(nil), values...)
	}
	return rec
}

// sessionOK enforces the cookie of the latest login.
func (f *fakeServer) sessionOK(r *http.Request) bool {
	cookie, err := r.Cookie(testCookie)
	return err == nil && f.cookie != "" && cookie.Value == f.cookie
}

// call returns the last request to one path.
func (f *fakeServer) call(path string) recordedRequest {
	f.t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Path == "/api/v2/"+path {
			return f.calls[i]
		}
	}
	f.t.Fatalf("no request to %s", path)
	return recordedRequest{}
}

// count returns how many requests reached one path.
func (f *fakeServer) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0
	for _, call := range f.calls {
		if call.Path == "/api/v2/"+path {
			n++
		}
	}
	return n
}

// loginCount returns the number of logins the fake has served.
func (f *fakeServer) loginCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins
}

// newClient returns a connected-or-connectable client for one fake.
func newClient(t *testing.T, f *fakeServer) *Client {
	t.Helper()

	c, err := New(Config{BaseURL: f.srv.URL, Username: testUsername, Password: testPassword}, nil)
	require.NoError(t, err)
	return c
}

// connectedClient returns a client past Connect.
func connectedClient(t *testing.T, f *fakeServer) *Client {
	t.Helper()

	c := newClient(t, f)
	require.NoError(t, c.Connect(context.Background()))
	return c
}

// magnetOf builds a one-hash magnet with a display name.
func magnetOf(hash string) string {
	return "magnet:?xt=urn:btih:" + hash + "&dn=test"
}

// addBody renders the WebAPI 2.14 torrents/add reply.
func addBody(t *testing.T, success, pending, failure int, ids ...string) string {
	t.Helper()

	body, err := json.Marshal(addResult{
		SuccessCount:    success,
		PendingCount:    pending,
		FailureCount:    failure,
		AddedTorrentIDs: ids,
	})
	require.NoError(t, err)
	return string(body)
}

func TestLoginAcceptsNoContent(t *testing.T) {
	f := newFakeServer(t, nil)
	c := connectedClient(t, f)

	version, err := c.Health(context.Background())
	require.NoError(t, err)
	require.Equal(t, testVersion, version)

	// Health served the cache: the two app/version hits are the login probe
	// and Connect's own GET, no more.
	require.Equal(t, 2, f.count("app/version"))
}

func TestLoginRefusedOn401(t *testing.T) {
	f := newFakeServer(t, func(f *fakeServer) { f.loginRefuse = true })
	c := newClient(t, f)

	err := c.Connect(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, engine.ErrUnavailable)
}

func TestLoginRequiresCookie(t *testing.T) {
	f := newFakeServer(t, func(f *fakeServer) { f.loginNoCookie = true })
	c := newClient(t, f)

	err := c.Connect(context.Background())
	require.ErrorContains(t, err, "no session cookie")
}

func TestRetriesOnceOn401(t *testing.T) {
	f := newFakeServer(t, func(f *fakeServer) {
		f.refuseOnce["/api/v2/torrents/stop"] = true
	})
	c := connectedClient(t, f)

	// The first stop is refused with 401; the client must re-login exactly
	// once and retry the stop exactly once, never loop.
	require.NoError(t, c.Pause(context.Background(), engine.NameQBittorrent+":"+testHash))
	require.Equal(t, 2, f.loginCount())
	require.Equal(t, 2, f.count("torrents/stop"))

	// The retried call carried the new session cookie and succeeded.
	stop := f.call("torrents/stop")
	require.Equal(t, []string{testHash}, stop.Form["hashes"])
}

func TestRefererHeaderPresent(t *testing.T) {
	f := newFakeServer(t, func(f *fakeServer) {
		f.addStatus = http.StatusOK
		f.addBody = addBody(t, 1, 0, 0, testHash)
	})
	c := connectedClient(t, f)

	_, err := c.Add(context.Background(), engine.AddRequest{URIs: []string{magnetOf(testHash)}})
	require.NoError(t, err)

	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.calls)
	for _, call := range f.calls {
		require.Equal(t, f.srv.URL, call.Header.Get("Referer"), "request to %s", call.Path)
	}
}

func TestAddSendsBothPausedSpellings(t *testing.T) {
	f := newFakeServer(t, func(f *fakeServer) {
		f.addStatus = http.StatusOK
		f.addBody = addBody(t, 1, 0, 0, testHash)
	})
	c := connectedClient(t, f)

	id, err := c.Add(context.Background(), engine.AddRequest{
		URIs:        []string{magnetOf(testHash)},
		SaveDir:     "/data/media",
		Category:    "media",
		Tags:        []string{"tv", "hd"},
		StartPaused: true,
		Sequential:  true,
	})
	require.NoError(t, err)
	require.Equal(t, engine.NameQBittorrent+":"+testHash, id)

	add := f.call("torrents/add")
	require.Equal(t, []string{"true"}, add.Form["stopped"])
	require.Equal(t, []string{"true"}, add.Form["paused"])
	require.Equal(t, []string{"false"}, add.Form["autoTMM"])
	require.Equal(t, []string{"/data/media"}, add.Form["savepath"])
	require.Equal(t, []string{"media"}, add.Form["category"])
	require.Equal(t, []string{"tv,hd"}, add.Form["tags"])
	require.Equal(t, []string{"true"}, add.Form["sequentialDownload"])
	require.Equal(t, []string{magnetOf(testHash)}, add.Form["urls"])
}

func TestAddResolvesIDAndRejectsBadCounts(t *testing.T) {
	t.Run("id from added_torrent_ids", func(t *testing.T) {
		f := newFakeServer(t, func(f *fakeServer) {
			f.addStatus = http.StatusOK
			f.addBody = addBody(t, 1, 0, 0, testHash)
		})
		c := connectedClient(t, f)

		id, err := c.Add(context.Background(), engine.AddRequest{URIs: []string{magnetOf(testHash)}})
		require.NoError(t, err)
		require.Equal(t, engine.NameQBittorrent+":"+testHash, id)
	})

	t.Run("counts disagree with submission", func(t *testing.T) {
		f := newFakeServer(t, func(f *fakeServer) {
			f.addStatus = http.StatusOK
			f.addBody = addBody(t, 2, 0, 0, testHash)
		})
		c := connectedClient(t, f)

		_, err := c.Add(context.Background(), engine.AddRequest{URIs: []string{magnetOf(testHash)}})
		require.ErrorContains(t, err, "outcomes")
	})

	t.Run("success count disagrees with ids", func(t *testing.T) {
		body, err := json.Marshal(map[string]any{
			"success_count": 2, "pending_count": 0, "failure_count": 0,
			"added_torrent_ids": []string{testHash},
		})
		require.NoError(t, err)

		f := newFakeServer(t, func(f *fakeServer) {
			f.addStatus = http.StatusOK
			f.addBody = string(body)
		})
		c := connectedClient(t, f)

		_, err = c.Add(context.Background(), engine.AddRequest{
			URIs: []string{magnetOf(testHash), magnetOf(otherHash)},
		})
		require.ErrorContains(t, err, "success_count")
	})

	t.Run("immediate add reports failure", func(t *testing.T) {
		// 200 with failure_count=1 and no ids: never infer success from
		// a 2xx status (06 section 5.3, T029 step 9).
		f := newFakeServer(t, func(f *fakeServer) {
			f.addStatus = http.StatusOK
			f.addBody = addBody(t, 0, 0, 1)
		})
		c := connectedClient(t, f)

		_, err := c.Add(context.Background(), engine.AddRequest{URIs: []string{magnetOf(testHash)}})
		require.ErrorContains(t, err, "for a single submission")
	})

	t.Run("unexpected id", func(t *testing.T) {
		f := newFakeServer(t, func(f *fakeServer) {
			f.addStatus = http.StatusOK
			f.addBody = addBody(t, 1, 0, 0, otherHash)
		})
		c := connectedClient(t, f)

		_, err := c.Add(context.Background(), engine.AddRequest{URIs: []string{magnetOf(testHash)}})
		require.ErrorContains(t, err, "want "+testHash)
	})
}

func TestAddPendingRetainsIdentity(t *testing.T) {
	f := newFakeServer(t, func(f *fakeServer) {
		f.addStatus = http.StatusAccepted
		f.addBody = addBody(t, 0, 1, 0)
	})
	c := connectedClient(t, f)

	// A pending magnet add: the identity resolved before the submission is
	// the engine reference T030 reconciles later.
	id, err := c.Add(context.Background(), engine.AddRequest{URIs: []string{magnetOf(testHash)}})
	require.NoError(t, err)
	require.Equal(t, engine.NameQBittorrent+":"+testHash, id)
}

func TestAddPendingURLHasNoIdentity(t *testing.T) {
	f := newFakeServer(t, func(f *fakeServer) {
		f.addStatus = http.StatusAccepted
		f.addBody = addBody(t, 0, 1, 0)
	})
	c := connectedClient(t, f)

	_, err := c.Add(context.Background(), engine.AddRequest{
		URIs: []string{"https://example.org/some.torrent"},
	})
	require.ErrorContains(t, err, "not locally resolvable")
}

// Hand-built bencode fixtures: a v1 dict (length/name/pieces), a v2-only
// dict (file tree + meta version 2) and a hybrid carrying both payloads.
// Keys are bencode-sorted; the file-tree node keeps its properties under
// BEP 52's empty-string key. Expected ids come from the standard library,
// independently of the metainfo parser the client uses.
const (
	v1Info = "d6:lengthi42e4:name4:test12:piece lengthi16384e" +
		"6:pieces20:AAAAAAAAAAAAAAAAAAAAe"
	v1Blob = "d8:announce27:http://example.org/announce4:info" + v1Info + "e"

	v2Info = "d9:file treed4:testd0:d6:lengthi42eeee12:meta versioni2e" +
		"12:piece layersde12:piece lengthi16384ee"
	v2Blob = "d4:info" + v2Info + "e"

	hybridInfo = "d9:file treed4:testd0:d6:lengthi42eeee6:lengthi42e4:name4:test" +
		"12:meta versioni2e12:piece layersde12:piece lengthi16384e" +
		"6:pieces20:AAAAAAAAAAAAAAAAAAAAe"
	hybridBlob = "d4:info" + hybridInfo + "e"
)

func TestAddBlobTorrentID(t *testing.T) {
	sha1Hex := func(b []byte) string {
		sum := sha1.Sum(b)
		return hex.EncodeToString(sum[:])
	}
	v2ShortHex := func(b []byte) string {
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:20])
	}

	t.Run("v1", func(t *testing.T) {
		testAddBlob(t, v1Blob, sha1Hex([]byte(v1Info)))
	})
	t.Run("v2 only", func(t *testing.T) {
		testAddBlob(t, v2Blob, v2ShortHex([]byte(v2Info)))
	})
	t.Run("hybrid keys on truncated v2", func(t *testing.T) {
		// libtorrent's get_best() prefers the truncated v2 hash whenever a
		// v2 hash exists, hybrid included; see expectedTorrentID.
		testAddBlob(t, hybridBlob, v2ShortHex([]byte(hybridInfo)))
	})
}

func testAddBlob(t *testing.T, blob, wantHash string) {
	t.Helper()

	f := newFakeServer(t, func(f *fakeServer) {
		f.addStatus = http.StatusOK
		f.addBody = addBody(t, 1, 0, 0, wantHash)
	})
	c := connectedClient(t, f)

	id, err := c.Add(context.Background(), engine.AddRequest{
		Blob:     []byte(blob),
		BlobKind: blobKindTorrent,
		SaveDir:  "/data/bt",
	})
	require.NoError(t, err)
	require.Equal(t, engine.NameQBittorrent+":"+wantHash, id)

	add := f.call("torrents/add")
	require.Equal(t, "/data/bt", add.Form.Get("savepath"))
	files, ok := add.Files["torrents"]
	require.True(t, ok, "no torrents file part")
	require.Len(t, files, 1)
	require.Equal(t, torrentMIME, files[0].ContentType)
	require.Equal(t, blob, string(files[0].Data))
}

func TestPauseFallsBackTo4x(t *testing.T) {
	f := newFakeServer(t, func(f *fakeServer) { f.legacyAPI = true })
	c := connectedClient(t, f)

	// First call probes torrents/stop, gets 404, retries torrents/pause.
	require.NoError(t, c.Pause(context.Background(), engine.NameQBittorrent+":"+testHash))
	require.Equal(t, 1, f.count("torrents/stop"))
	require.Equal(t, 1, f.count("torrents/pause"))

	// The answered pair is cached: the second Pause goes straight to pause.
	require.NoError(t, c.Pause(context.Background(), engine.NameQBittorrent+":"+testHash))
	require.Equal(t, 1, f.count("torrents/stop"))
	require.Equal(t, 2, f.count("torrents/pause"))

	// The cached pair serves Resume too: straight to torrents/resume,
	// no torrents/start probe.
	require.NoError(t, c.Resume(context.Background(), engine.NameQBittorrent+":"+testHash))
	require.Equal(t, 0, f.count("torrents/start"))
	require.Equal(t, 1, f.count("torrents/resume"))

	// A fresh client still probes the 5.x spelling on its first Resume.
	fresh := connectedClient(t, f)
	require.NoError(t, fresh.Resume(context.Background(), engine.NameQBittorrent+":"+testHash))
	require.Equal(t, 1, f.count("torrents/start"))
	require.Equal(t, 2, f.count("torrents/resume"))
}

func TestRemoveSendsDeleteFiles(t *testing.T) {
	f := newFakeServer(t, nil)
	c := connectedClient(t, f)

	require.NoError(t, c.Remove(context.Background(), engine.NameQBittorrent+":"+testHash, true))
	del := f.call("torrents/delete")
	require.Equal(t, []string{testHash}, del.Form["hashes"])
	require.Equal(t, []string{"true"}, del.Form["deleteFiles"])

	require.NoError(t, c.Remove(context.Background(), testHash, false))
	del = f.call("torrents/delete")
	require.Equal(t, []string{testHash}, del.Form["hashes"])
	require.Equal(t, []string{"false"}, del.Form["deleteFiles"])
}

func TestCapabilities(t *testing.T) {
	c := newClient(t, newFakeServer(t, nil))

	want := []engine.Capability{
		engine.CapBitTorrent,
		engine.CapBTV2,
		engine.CapCategories,
		engine.CapMagnet,
		engine.CapPerFilePriority,
		engine.CapPerFileSelect,
		engine.CapRename,
		engine.CapSequential,
		engine.CapSetLocation,
		engine.CapShareLimits,
		engine.CapTags,
	}
	require.Equal(t, want, c.Capabilities())
	require.Equal(t, want, c.Capabilities(), "capabilities must be stable")
}

func TestAccepts(t *testing.T) {
	c := newClient(t, newFakeServer(t, nil))

	cases := []struct {
		uri  string
		want bool
	}{
		{magnetOf(testHash), true},
		{"MAGNET:?xt=urn:btih:" + testHash, true},
		{"https://example.org/some.torrent", true},
		{"https://example.org/some.TORRENT", true},
		{"https://example.org/torrents/some", false},
		{testHash, true},
		{strings.ToUpper(testHash), true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 39), false},
		{strings.Repeat("a", 41), false},
		{"https://example.org/file.iso", false},
		{"ed2k://|file|x|1|0123456789abcdef0123456789abcdef|/", false},
		{"", false},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, c.Accepts(tc.uri), "uri %q", tc.uri)
	}
}

func TestNormaliseState(t *testing.T) {
	cases := []struct {
		state    string
		progress float64
		want     engine.TaskState
	}{
		// downloading row
		{"downloading", 0, engine.StateDownloading},
		{"metaDL", 0, engine.StateDownloading},
		{"forcedDL", 0, engine.StateDownloading},
		{"forcedMetaDL", 0, engine.StateDownloading},
		{"stalledDL", 0, engine.StateDownloading},
		// seeding row; forcedUP is the serialiser's spelling, not section
		// 5.6's table typo "forcedOP"
		{"uploading", 1, engine.StateSeeding},
		{"forcedUP", 1, engine.StateSeeding},
		{"stalledUP", 1, engine.StateSeeding},
		// queued row
		{"queuedDL", 0, engine.StateQueued},
		{"queuedUP", 0, engine.StateQueued},
		{"allocating", 0, engine.StateQueued},
		// paused row, both spellings
		{"pausedDL", 0, engine.StatePaused},
		{"stoppedDL", 0, engine.StatePaused},
		// completed-when-done row, both spellings
		{"pausedUP", 1, engine.StateCompleted},
		{"pausedUP", 0.5, engine.StatePaused},
		{"stoppedUP", 1, engine.StateCompleted},
		{"stoppedUP", 0.5, engine.StatePaused},
		// checking row
		{"checkingDL", 0, engine.StateChecking},
		{"checkingUP", 1, engine.StateChecking},
		{"checkingResumeData", 0, engine.StateChecking},
		{"moving", 0, engine.StateChecking},
		// error row
		{"error", 0, engine.StateError},
		{"missingFiles", 0, engine.StateError},
		// fallback: unknown plus unrecognised, never an error
		{"unknown", 0, engine.StateQueued},
		{"whateverNewState", 0, engine.StateQueued},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, normaliseState(tc.state, tc.progress), "state %q", tc.state)
	}
}

func TestToTaskInfo(t *testing.T) {
	private := true
	tj := torrentJSON{
		Hash:          testHash,
		InfohashV1:    testHash,
		InfohashV2:    strings.Repeat("b", 64),
		HasMetadata:   true,
		Name:          "test torrent",
		State:         "stalledDL",
		Progress:      0.5,
		Size:          1000,
		TotalSize:     2000,
		Completed:     500,
		Uploaded:      10,
		DlSpeed:       120,
		UpSpeed:       30,
		ETA:           60,
		Ratio:         0.2,
		SavePath:      "/data/bt",
		ContentPath:   "/data/bt/test torrent",
		Category:      "media",
		Tags:          "tv, hd",
		NumSeeds:      3,
		NumLeechs:     4,
		NumComplete:   9,
		NumIncomplete: 8,
		AddedOn:       1700000000,
		CompletionOn:  -1,
		Private:       &private,
	}

	info := toTaskInfo(tj)
	require.Equal(t, engine.NameQBittorrent+":"+testHash, info.ID)
	require.Equal(t, engine.NameQBittorrent, info.Engine)
	require.Equal(t, "test torrent", info.Name)
	require.Equal(t, engine.StateDownloading, info.State)
	require.NotNil(t, info.TotalBytes)
	require.Equal(t, int64(1000), *info.TotalBytes)
	require.Equal(t, int64(500), info.CompletedBytes)
	require.Equal(t, int64(10), info.UploadedBytes)
	require.Equal(t, int64(120), info.DownloadRate)
	require.Equal(t, int64(30), info.UploadRate)
	require.NotNil(t, info.ETASeconds)
	require.Equal(t, int64(60), *info.ETASeconds)
	require.Equal(t, "/data/bt", info.SaveDir)
	require.Equal(t, "/data/bt/test torrent", info.ContentPath)
	require.Equal(t, testHash, info.InfohashV1)
	require.Equal(t, strings.Repeat("b", 64), info.InfohashV2)
	require.NotNil(t, info.NumSeeds)
	require.Equal(t, 3, *info.NumSeeds)
	require.NotNil(t, info.NumPeers)
	require.Equal(t, 4, *info.NumPeers)
	require.NotNil(t, info.Ratio)
	require.InDelta(t, 0.2, *info.Ratio, 0.0001)
	require.NotNil(t, info.CreatedAt)
	require.Equal(t, int64(1700000000), info.CreatedAt.Unix())
	require.Nil(t, info.CompletedAt)
	require.Empty(t, info.ErrorCode)

	// Without metadata the size is unknown; the ETA sentinel is nil.
	noMeta := tj
	noMeta.HasMetadata = false
	noMeta.ETA = etaSentinel
	info = toTaskInfo(noMeta)
	require.Nil(t, info.TotalBytes)
	require.Nil(t, info.ETASeconds)

	// An error state carries the generic error code.
	errored := tj
	errored.State = stateError
	info = toTaskInfo(errored)
	require.Equal(t, engine.StateError, info.State)
	require.Equal(t, errCodeUnknownTorrents, info.ErrorCode)
}

func TestSplitTags(t *testing.T) {
	require.Equal(t, []string{"tv", "hd", "x"}, splitTags("tv, hd,,x"))
	require.Empty(t, splitTags(""))
	require.Empty(t, splitTags(" , "))
}
