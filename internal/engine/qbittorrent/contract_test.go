//go:build integration

// The qBittorrent call site of the shared engine contract suite
// (docs/06-download-engines.md section 11), plus T038's own coverage: the
// live probe of torrents/fetchMetadata and torrents/parseMetadata whose
// observed shapes the Evidence section records, InspectMagnet's
// leaves-no-handle obligation against the real daemon and against a fake
// for the fallback paths a 5.2.3 daemon never takes, and the
// composition-root registration this task wires.
//
// The file is the package's external test package: the registration test
// builds internal/api's Server, which imports the adapter, so an internal
// test file would close an import cycle.
package qbittorrent_test

import (
	"bytes"
	"context"
	"crypto/pbkdf2"
	cryptorand "crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/L-K-M/dl-tool/internal/api"
	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/engine/enginetest"
	"github.com/L-K-M/dl-tool/internal/engine/qbittorrent"
	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/uri"
)

const (
	// qbtImage is the pinned daemon of docs/13-testing-and-verification.md
	// section 4.
	qbtImage = "lscr.io/linuxserver/qbittorrent:5.2.3"

	// qbtWebUIPort is the daemon's WebUI port the adapter connects to.
	qbtWebUIPort = "8080/tcp"

	// qbtAdminUser and qbtAdminPass seed the WebUI credentials the adapter
	// config uses. Test-local values; they never leave the throwaway
	// container.
	qbtAdminUser = "admin"
	qbtAdminPass = "dl-tool-enginetest"

	// qbtConfPath is where the image's init script looks for a pre-seeded
	// configuration and leaves it untouched when one exists.
	qbtConfPath = "/config/qBittorrent/qBittorrent.conf"

	// The PBKDF2 shape of release-5.2.3 src/base/utils/password.cpp:
	// HMAC-SHA512, 100000 iterations, 16-byte salt, 64-byte key,
	// serialised as "<base64(salt)>:<base64(key)>".
	pbkdf2Iterations = 100000
	pbkdf2SaltBytes  = 16
	pbkdf2KeyBytes   = 64

	// containerTimeout bounds container start (pull, s6 init, readiness
	// wait), connect and terminate.
	containerTimeout = 180 * time.Second

	// addVisibilityTimeout bounds the wait for the engine's own Get to
	// observe an add: the sync/maindata poll runs at 1 Hz.
	addVisibilityTimeout = 15 * time.Second

	// wrongThreeQuarters is the wrong daemon value the readback test
	// injects: three quarters of the request, small enough to stay inside
	// the suite's timing window — the exact setting only the readback can
	// expose.
	wrongThreeQuarters = 3 * (1 << 20) / 4

	// fixtureBytes and fixturePieceBytes shape the generated torrent: the
	// shared 8 MiB fixture body in regular 256 KiB pieces, so a transfer's
	// progress and rate are observable piece by piece and a 1048576 B/s
	// cap keeps it in flight for several seconds.
	fixtureBytes      = 8 << 20
	fixturePieceBytes = 256 << 10

	// qbtListenPort is the daemon's BitTorrent listen port; the seeded
	// configuration pins Connection\PortRangeMin to it.
	qbtListenPort = "6881"

	// magnetProbeTimeout bounds the live timeout subtest: short enough to
	// keep the suite quick, long enough for several 500 ms polls.
	magnetProbeTimeout = 5 * time.Second
)

// seededConfPath writes the pre-seeded qBittorrent.conf: the image defaults
// plus the admin credentials (as the PBKDF2 blob qBittorrent verifies on
// login) and an auth-subnet whitelist that admits the unauthenticated
// readiness probe the container wait strategy sends. The whitelist does not
// weaken the credential check — auth/login validates the username and
// password even when the client address is whitelisted
// (release-5.2.3 authcontroller.cpp).
func seededConfPath(t *testing.T) string {
	t.Helper()

	salt := make([]byte, pbkdf2SaltBytes)
	_, err := cryptorand.Read(salt)
	require.NoError(t, err, "draw the password salt")
	key, err := pbkdf2.Key(sha512.New, qbtAdminPass, salt, pbkdf2Iterations, pbkdf2KeyBytes)
	require.NoError(t, err, "derive the password key")
	secret := base64.StdEncoding.EncodeToString(salt) + ":" + base64.StdEncoding.EncodeToString(key)

	conf := strings.Join([]string{
		"[AutoRun]",
		"enabled=false",
		"program=",
		"",
		"[LegalNotice]",
		"Accepted=true",
		"",
		"[Preferences]",
		"Connection\\UPnP=false",
		"Connection\\PortRangeMin=6881",
		"Downloads\\SavePath=/downloads/",
		"Downloads\\TempPath=/downloads/incomplete/",
		"WebUI\\Address=*",
		"WebUI\\ServerDomains=*",
		// The tests dial the daemon through Docker's mapped port, so the
		// Host header carries that port, not 8080 — and qBittorrent
		// rejects a Host whose port differs from its listening port
		// outright (validateHostHeader, release-5.2.3 webapplication.cpp),
		// the trap docs/06 section 5.2 names. This WebUI is dl-tool's own
		// test fixture, so the validation is off.
		"WebUI\\HostHeaderValidation=false",
		"WebUI\\AuthSubnetWhitelistEnabled=true",
		"WebUI\\AuthSubnetWhitelist=0.0.0.0/0, ::/0",
		"WebUI\\Username=" + qbtAdminUser,
		"WebUI\\Password_PBKDF2=\"@ByteArray(" + secret + ")\"",
		"",
	}, "\n")

	path := filepath.Join(t.TempDir(), "qBittorrent.conf")
	require.NoError(t, os.WriteFile(path, []byte(conf), 0o600))
	return path
}

// startedDaemon is one booted daemon: its mapped base URL and the
// container handle the seeder wiring needs the IP of.
type startedDaemon struct {
	baseURL   string
	container testcontainers.Container
}

// startDaemon boots one throwaway qBittorrent 5.2.3 and returns its
// mapped base URL, optionally joined to caller-owned networks (the suite
// runs a second container beside it). The fixture server must already
// exist so its port can be tunneled in for the web-seeded fetches the
// seeder container runs.
func startDaemon(t *testing.T, networks ...string) startedDaemon {
	t.Helper()

	// enginetest.Fixture is per-t; calling it here also makes the suite's
	// own later call serve from this same server.
	fixtureURL, _ := enginetest.Fixture(t)
	fixturePort, err := fixturePort(fixtureURL)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), containerTimeout)
	defer cancel()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: qbtImage,
			// Without a /downloads volume the image's download directory
			// stays root-owned while qbittorrent-nox runs as the abc user,
			// so the very first piece write fails with a file error. The
			// image's documented switch for self-managed permissions runs
			// the daemon as root instead; a throwaway CI container with no
			// data worth protecting is exactly that case.
			Env:          map[string]string{"LSIO_NON_ROOT_USER": "1"},
			ExposedPorts: []string{qbtWebUIPort},
			Networks:     networks,
			// The readiness probe is the unauthenticated app/version GET
			// the seeded subnet whitelist admits with a 200.
			WaitingFor: wait.ForHTTP("/api/v2/app/version").WithPort(qbtWebUIPort),
			// Tunnels the fixture port so host.testcontainers.internal
			// resolves and reaches it from inside the daemon container.
			HostAccessPorts: []int{fixturePort},
			Files: []testcontainers.ContainerFile{{
				HostFilePath:      seededConfPath(t),
				ContainerFilePath: qbtConfPath,
				FileMode:          0o644,
			}},
		},
		Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		terminateCtx, cancel := context.WithTimeout(context.Background(), containerTimeout)
		defer cancel()
		// The daemon's own log is the ground truth a failed subtest
		// needs; passing subtests stay quiet.
		if t.Failed() {
			if logs, logErr := container.Logs(terminateCtx); logErr == nil {
				if data, readErr := io.ReadAll(logs); readErr == nil {
					t.Logf("daemon log:\n%s", data)
				}
			}
		}
		require.NoError(t, container.Terminate(terminateCtx))
	})

	host, err := container.Host(ctx)
	require.NoError(t, err)
	mapped, err := container.MappedPort(ctx, qbtWebUIPort)
	require.NoError(t, err)
	return startedDaemon{
		baseURL:   "http://" + net.JoinHostPort(host, mapped.Port()),
		container: container,
	}
}

// daemonClient returns a connected adapter for one started daemon.
func daemonClient(t *testing.T, baseURL string) *qbittorrent.Client {
	t.Helper()

	client, err := qbittorrent.New(qbittorrent.Config{
		BaseURL:  baseURL,
		Username: qbtAdminUser,
		Password: qbtAdminPass,
	}, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), containerTimeout)
	defer cancel()
	require.NoError(t, client.Connect(ctx), "connect to the throwaway qbittorrent daemon")
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return client
}

// fixturePort extracts the numeric port of a fixture URL.
func fixturePort(fixtureURL string) (int, error) {
	served, err := url.Parse(fixtureURL)
	if err != nil {
		return 0, fmt.Errorf("parse fixture url %q: %w", fixtureURL, err)
	}
	_, port, err := net.SplitHostPort(served.Host)
	if err != nil {
		return 0, fmt.Errorf("split fixture host %q: %w", served.Host, err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		return 0, fmt.Errorf("fixture port %q: %w", port, err)
	}
	return number, nil
}

// daemonSession is an independent WebAPI client the tests read daemon truth
// through: the contract suite's download-limit readback, the live endpoint
// probe and the torrent counts. It shares nothing with the adapter — its
// own login, its own cookie jar — so an adapter that echoes its last
// request instead of querying the daemon cannot satisfy an equality check
// by accident.
type daemonSession struct {
	t    *testing.T
	base string
	hc   *http.Client
}

// newDaemonSession logs in with the seeded credentials and proves the jar
// holds a session cookie.
func newDaemonSession(t *testing.T, baseURL string) *daemonSession {
	t.Helper()

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	s := &daemonSession{t: t, base: baseURL, hc: &http.Client{Jar: jar, Timeout: 30 * time.Second}}

	status, body := s.do(http.MethodPost, "auth/login",
		url.Values{"username": {qbtAdminUser}, "password": {qbtAdminPass}}, "")
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, status,
		"login with the seeded credentials: %s", body)

	parsed, err := url.Parse(baseURL)
	require.NoError(t, err)
	require.NotEmpty(t, jar.Cookies(parsed), "login set no session cookie")
	return s
}

// do performs one request: a POST's form is the urlencoded body, a GET's
// form is the query, and the Referer qBittorrent's CSRF check compares is
// sent on every call.
func (s *daemonSession) do(method, apiPath string, form url.Values, contentType string) (int, []byte) {
	s.t.Helper()

	target := s.base + "/api/v2/" + apiPath
	var body io.Reader
	if form != nil {
		if method == http.MethodGet {
			target += "?" + form.Encode()
		} else if contentType == "" {
			body = strings.NewReader(form.Encode())
			contentType = "application/x-www-form-urlencoded"
		} else {
			body = strings.NewReader(form.Encode())
		}
	}

	req, err := http.NewRequest(method, target, body)
	require.NoError(s.t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Referer", s.base)

	resp, err := s.hc.Do(req)
	require.NoError(s.t, err)
	defer func() { require.NoError(s.t, resp.Body.Close()) }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	require.NoError(s.t, err)
	return resp.StatusCode, data
}

// doMultipart posts one file part, the shape torrents/parseMetadata takes.
func (s *daemonSession) doMultipart(apiPath, field, filename string, data []byte) (int, []byte) {
	s.t.Helper()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile(field, filename)
	require.NoError(s.t, err)
	_, err = part.Write(data)
	require.NoError(s.t, err)
	require.NoError(s.t, w.Close())

	req, err := http.NewRequest(http.MethodPost, s.base+"/api/v2/"+apiPath, &buf)
	require.NoError(s.t, err)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Referer", s.base)

	resp, err := s.hc.Do(req)
	require.NoError(s.t, err)
	defer func() { require.NoError(s.t, resp.Body.Close()) }()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	require.NoError(s.t, err)
	return resp.StatusCode, reply
}

// torrentCount answers how many torrents torrents/info reports — the count
// the leaves-no-handle obligation is measured against.
func (s *daemonSession) torrentCount() int {
	s.t.Helper()

	status, body := s.do(http.MethodGet, "torrents/info", nil, "")
	require.Equal(s.t, http.StatusOK, status, "read torrents/info: %s", body)
	var rows []json.RawMessage
	require.NoError(s.t, json.Unmarshal(body, &rows))
	return len(rows)
}

// downloadLimit reads the daemon's configured download limit: one hash's
// torrents/downloadLimit answer, or the global transfer/info one. The value
// is bytes per second, -1 meaning unlimited.
func (s *daemonSession) downloadLimit(id string) int64 {
	s.t.Helper()

	if id == "" {
		status, body := s.do(http.MethodGet, "transfer/info", nil, "")
		require.Equal(s.t, http.StatusOK, status, "read transfer/info: %s", body)
		var info struct {
			DlRateLimit int64 `json:"dl_rate_limit"`
		}
		require.NoError(s.t, json.Unmarshal(body, &info))
		return info.DlRateLimit
	}

	hash := strings.TrimPrefix(id, engine.NameQBittorrent+":")
	status, body := s.do(http.MethodGet, "torrents/downloadLimit", url.Values{"hashes": {hash}}, "")
	require.Equal(s.t, http.StatusOK, status, "read torrents/downloadLimit: %s", body)
	limits := map[string]int64{}
	require.NoError(s.t, json.Unmarshal(body, &limits))
	require.Contains(s.t, limits, hash, "the daemon named the requested hash")
	return limits[hash]
}

// injectDownloadLimit overwrites a limit directly in the daemon, bypassing
// the adapter, so the readback's honesty can be pinned.
func (s *daemonSession) injectDownloadLimit(id string, limit int64) {
	s.t.Helper()

	if id == "" {
		status, body := s.do(http.MethodPost, "transfer/setDownloadLimit",
			url.Values{"limit": {strconv.FormatInt(limit, 10)}}, "")
		require.Equal(s.t, http.StatusOK, status, "inject the global limit: %s", body)
		return
	}

	hash := strings.TrimPrefix(id, engine.NameQBittorrent+":")
	status, body := s.do(http.MethodPost, "torrents/setDownloadLimit",
		url.Values{"hashes": {hash}, "limit": {strconv.FormatInt(limit, 10)}}, "")
	require.Equal(s.t, http.StatusOK, status, "inject the per-task limit: %s", body)
}

// daemonRemove deletes a torrent straight through the daemon, for test
// cleanup on hashes the adapter's ownership gate deliberately refuses.
func (s *daemonSession) daemonRemove(hash string, deleteFiles bool) {
	s.t.Helper()

	status, body := s.do(http.MethodPost, "torrents/delete", url.Values{
		"hashes":      {hash},
		"deleteFiles": {strconv.FormatBool(deleteFiles)},
	}, "")
	require.Equal(s.t, http.StatusOK, status, "remove the torrent: %s", body)
}

// fixtureTorrent is a locally generated single-file torrent whose bytes
// come only from the shared fixture server — no tracker, no public host,
// nothing outside the test.
type fixtureTorrent struct {
	// blob is the plain spelling: no sources inside, bytes arrive from
	// real BitTorrent peers alone.
	blob []byte
	// seedBlob is the same info dict with the fixture URL as a web seed,
	// the spelling the seeder container fetches the body through.
	seedBlob []byte
	magnet   string
	hash     string
	name     string
	size     int64
}

// fetchFixtureBody downloads the fixture body over loopback — the fixture
// URL names host.testcontainers.internal, which resolves only inside
// containers — and proves it against the digest enginetest.Fixture
// reported, so the torrent built from it cannot hash the wrong bytes.
func fetchFixtureBody(t *testing.T, fixtureURL, wantSHA256 string) []byte {
	t.Helper()

	parsed, err := url.Parse(fixtureURL)
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(parsed.Host)
	require.NoError(t, err)

	resp, err := http.Get("http://127.0.0.1:" + port + "/")
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	sum := sha256.Sum256(body)
	require.Equal(t, wantSHA256, hex.EncodeToString(sum[:]),
		"the fixture body does not match the digest enginetest.Fixture reported")
	return body
}

// buildFixtureTorrent bencodes one single-file v1 torrent of the body
// with regular fixturePieceBytes pieces, so a transfer's progress and
// rate are observable piece by piece. Two spellings share one infohash:
// seedBlob carries the fixture URL as a url-list web seed — the seeder
// container pulls the body through it — and plainBlob carries none, so
// the downloader under test can only take the bytes from real BitTorrent
// peers. The local parser cross-checks the infohash and size, so the
// magnet derived from the same bytes cannot disagree with what the
// daemon will add.
func buildFixtureTorrent(t *testing.T, body []byte, name, webSeed string) fixtureTorrent {
	t.Helper()

	require.Len(t, body, fixtureBytes, "the fixture body must be whole pieces")

	var pieces []byte
	for offset := 0; offset < len(body); offset += fixturePieceBytes {
		piece := sha1.Sum(body[offset : offset+fixturePieceBytes])
		pieces = append(pieces, piece[:]...)
	}
	info := map[string]any{
		"length":       int64(len(body)),
		"name":         name,
		"piece length": int64(fixturePieceBytes),
		"pieces":       pieces,
	}
	infoBytes, err := bencode.Marshal(info)
	require.NoError(t, err)
	hashSum := sha1.Sum(infoBytes)
	hash := hex.EncodeToString(hashSum[:])

	plain, err := bencode.Marshal(map[string]any{"info": info})
	require.NoError(t, err)
	seeded, err := bencode.Marshal(map[string]any{"info": info, "url-list": []string{webSeed}})
	require.NoError(t, err)

	manifest, err := uri.InspectTorrent(seeded)
	require.NoError(t, err, "the local parser must accept the generated torrent")
	require.Equal(t, hash, manifest.InfohashV1)
	require.EqualValues(t, len(body), manifest.TotalSize)

	return fixtureTorrent{
		blob:     plain,
		seedBlob: seeded,
		magnet:   "magnet:?xt=urn:btih:" + hash + "&dn=" + url.QueryEscape(name),
		hash:     hash,
		name:     name,
		size:     int64(len(body)),
	}
}

// suiteEngine lifts the adapter onto the surface the contract suite
// needs. The suite's fixture-URL adds become locally generated torrents —
// torrents/add cannot fetch a plain HTTP URL — whose bytes reach the
// daemon under test over real BitTorrent from a second, seeder container
// on a private network: the seeder pulls the fixture body through the
// torrent's web seed, and the downloader's own torrent spelling carries
// no web seed at all, so the transfer under assertion is genuine peer
// traffic. This is not the web-seeded download T038's step 9 sketched:
// the pinned image excludes web-seed payload from dlspeed, completed and
// progress until a whole piece lands (observed on CI, see the task's
// Evidence), so a web-seeded transfer can never satisfy the suite's
// growth and rate assertions. Two local containers keep the promise that
// actually matters: no public tracker, no distribution mirror, nothing
// outside the test.
//
// Every add also waits for the engine's own Get to observe it (the suite
// reads state straight after Add), the ownership filter the composition
// root's reconciler installs in production is supplied from the ids added
// here, and the suite's DownloadLimitReadback rides an independent
// session.
type suiteEngine struct {
	*qbittorrent.Client

	t *testing.T

	baseURL    string
	fixtureURL string
	fixtureSHA string
	body       []byte
	// session is the independent readback, logged in once per daemon.
	session *daemonSession
	// seederSession drives the seeder container's own daemon, and
	// seederAddr is its listen address on the shared network, handed to
	// the downloader as a static peer so no tracker or DHT is involved.
	seederSession *daemonSession
	seederAddr    string
	mu            sync.Mutex
	adds          int
	// owned holds bare hashes: the ownership snapshot is keyed on
	// engine_refs, not namespaced ids.
	owned map[string]struct{}
}

// newSuiteEngine starts the seeder and the daemon under test on one
// private network and returns the wired engine; it is the suite's
// newEngine, so every subtest gets its own pair.
func newSuiteEngine(t *testing.T) *suiteEngine {
	t.Helper()

	ctx := context.Background()
	swarm, err := network.New(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, swarm.Remove(ctx)) })

	seeder := startDaemon(t, swarm.Name)
	seederSession := newDaemonSession(t, seeder.baseURL)
	seederIP, err := seeder.container.ContainerIP(ctx)
	require.NoError(t, err)

	underTest := startDaemon(t, swarm.Name)
	client := daemonClient(t, underTest.baseURL)
	fixtureURL, fixtureSHA := enginetest.Fixture(t)

	s := &suiteEngine{
		Client:        client,
		t:             t,
		baseURL:       underTest.baseURL,
		fixtureURL:    fixtureURL,
		fixtureSHA:    fixtureSHA,
		body:          fetchFixtureBody(t, fixtureURL, fixtureSHA),
		owned:         map[string]struct{}{},
		session:       newDaemonSession(t, underTest.baseURL),
		seederSession: seederSession,
		seederAddr:    net.JoinHostPort(seederIP, qbtListenPort),
	}
	client.SetOwnershipFilter(s.snapshot)
	return s
}

// snapshot is the ownership source: exactly the hashes added through this
// engine, the store's engine_refs in production.
func (s *suiteEngine) snapshot() map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]struct{}, len(s.owned))
	for hash := range s.owned {
		out[hash] = struct{}{}
	}
	return out
}

// Add translates the suite's fixture-URL submission into the locally
// generated torrent pair: the seeder gets the web-seeded spelling and
// fetches the fixture body through it, and the daemon under test gets the
// plain spelling plus the seeder as a static peer, so the transfer the
// suite observes is real peer traffic. The add then applies the two
// wirings the suite's assertions need: a zero share ratio, so the daemon
// stops a finished download and the suite's completed-state poll can ever
// return (a seeding torrent would stay StateSeeding forever), and the
// ownership + visibility wait, so List and Get observe the add before Add
// returns.
func (s *suiteEngine) Add(ctx context.Context, req engine.AddRequest) (string, error) {
	if len(req.Blob) == 0 && len(req.URIs) == 1 && req.URIs[0] == s.fixtureURL {
		s.mu.Lock()
		s.adds++
		name := fmt.Sprintf("enginetest-fixture-%02d.bin", s.adds)
		s.mu.Unlock()

		torrent := buildFixtureTorrent(s.t, s.body, name, s.fixtureURL)
		s.seedThroughSeeder(torrent)

		req.URIs = nil
		req.Blob = torrent.blob
		req.BlobKind = "torrent"
	}

	id, err := s.Client.Add(ctx, req)
	if err != nil {
		return "", err
	}

	// The seeder as a static peer: no tracker, no DHT, one deterministic
	// hop on the private network.
	hash := strings.TrimPrefix(id, engine.NameQBittorrent+":")
	status, body := s.session.do(http.MethodPost, "torrents/addPeers",
		url.Values{"hashes": {hash}, "peers": {s.seederAddr}}, "")
	require.Equal(s.t, http.StatusOK, status, "add the seeder as a static peer: %s", body)

	zeroRatio := 0.0
	if err := s.SetShareLimits(ctx, id, &zeroRatio, nil); err != nil {
		return "", err
	}

	s.mu.Lock()
	s.owned[hash] = struct{}{}
	s.mu.Unlock()

	require.Eventually(s.t, func() bool {
		_, err := s.Get(ctx, id)
		return err == nil
	}, addVisibilityTimeout, 100*time.Millisecond, "the engine's own Get never observed task %s", id)
	return id, nil
}

// seedThroughSeeder makes the seeder fetch and hold one torrent's body:
// the web-seeded spelling is uploaded there directly, and this returns
// only when the seeder reports the torrent complete, so the downloader
// never waits on an empty swarm.
func (s *suiteEngine) seedThroughSeeder(torrent fixtureTorrent) {
	s.t.Helper()

	status, body := s.seederSession.doMultipart("torrents/add", "torrents", torrent.hash+".torrent", torrent.seedBlob)
	require.Equal(s.t, http.StatusOK, status, "the seeder must accept the torrent: %s", body)

	require.Eventually(s.t, func() bool {
		status, body := s.seederSession.do(http.MethodGet, "torrents/info",
			url.Values{"hashes": {torrent.hash}}, "")
		if status != http.StatusOK {
			return false
		}
		var rows []struct {
			Progress float64 `json:"progress"`
		}
		return json.Unmarshal(body, &rows) == nil && len(rows) == 1 && rows[0].Progress >= 1
	}, containerTimeout, 250*time.Millisecond, "the seeder never completed the fixture body")
}

// independent session — never the adapter's own bookkeeping.
func (s *suiteEngine) DaemonDownloadLimit(ctx context.Context, id string) (int64, error) {
	return s.session.downloadLimit(id), nil
}

// TestQBittorrentContract runs the shared engine conformance suite against
// a real qBittorrent 5.2.3 container.
func TestQBittorrentContract(t *testing.T) {
	enginetest.RunContract(t, func(t *testing.T) engine.Engine { return newSuiteEngine(t) })
}

// TestQBittorrentDaemonLimitReadback pins the readback the contract suite
// relies on to daemon truth. A limit set through the adapter must read
// back exactly; overwriting the daemon's option directly — bypassing the
// adapter entirely — must read back as the injected value, so an adapter
// that echoes its last request instead of querying the daemon cannot
// satisfy the suite's equality check by accident.
func TestQBittorrentDaemonLimitReadback(t *testing.T) {
	s := newSuiteEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), containerTimeout)
	defer cancel()

	zeroRatio := 0.0
	torrent := buildFixtureTorrent(t, s.body, "enginetest-readback.bin", s.fixtureURL)
	id, err := s.Client.Add(ctx, engine.AddRequest{Blob: torrent.blob, BlobKind: "torrent", StartPaused: true})
	require.NoError(t, err)
	require.NoError(t, s.SetShareLimits(ctx, id, &zeroRatio, nil))

	// Per-task: adapter round trip, then a wrong value injected straight
	// into the daemon at three quarters of the request.
	requested := enginetest.RateLimitBytesPerSecond
	require.NoError(t, s.SetRateLimits(ctx, id, &requested, nil))
	require.Equal(t, requested, s.session.downloadLimit(id),
		"the daemon must report the limit the adapter set")
	s.session.injectDownloadLimit(id, wrongThreeQuarters)
	require.Equal(t, int64(wrongThreeQuarters), s.session.downloadLimit(id),
		"the readback must query the daemon, not echo the adapter's request")

	// Global: the same two steps through the daemon's global options.
	require.NoError(t, s.SetRateLimits(ctx, "", &requested, nil))
	require.Equal(t, requested, s.session.downloadLimit(""))
	s.session.injectDownloadLimit("", wrongThreeQuarters)
	require.Equal(t, int64(wrongThreeQuarters), s.session.downloadLimit(""))
}

// TestQBittorrentMetadataEndpointsProbe is the live probe T038's step 1
// demands: it drives torrents/fetchMetadata and torrents/parseMetadata
// against a real 5.2.3 daemon and logs every request and response verbatim
// — the shapes recorded in the task's Evidence section come from this
// test's output. The assertions pin the observed contract the adapter's
// primary path is built on.
func TestQBittorrentMetadataEndpointsProbe(t *testing.T) {
	daemon := startDaemon(t)
	client := daemonClient(t, daemon.baseURL)
	session := newDaemonSession(t, daemon.baseURL)
	ctx, cancel := context.WithTimeout(context.Background(), containerTimeout)
	defer cancel()

	torrent := buildFixtureTorrent(t,
		fetchFixtureBody(t, mustFixtureURL(t), mustFixtureSHA(t)), "enginetest-probe.bin", mustFixtureURL(t))

	// The torrent must be present for the resolved half of the probe: the
	// daemon answers fetchMetadata from its transfer list once the torrent
	// is there with metadata.
	_, err := client.Add(ctx, engine.AddRequest{Blob: torrent.blob, BlobKind: "torrent", StartPaused: true})
	require.NoError(t, err)

	// A magnet the daemon does not know yet: fetchMetadata starts the
	// hidden metadata download and answers 202 with the identity trio.
	// The hidden download has no swarm to draw from and stays hidden until
	// the container is torn down; it never appears in torrents/info.
	unknown := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=unknown-probe"
	status, body := session.do(http.MethodPost, "torrents/fetchMetadata",
		url.Values{"source": {unknown}}, "")
	t.Logf("probe: POST torrents/fetchMetadata source=<unknown magnet> -> %d %s", status, body)
	require.Equal(t, http.StatusAccepted, status, "a fresh magnet must answer 202")
	var pending struct {
		InfohashV1 string `json:"infohash_v1"`
		InfohashV2 string `json:"infohash_v2"`
		Hash       string `json:"hash"`
	}
	require.NoError(t, json.Unmarshal(body, &pending))
	require.Equal(t, "0123456789abcdef0123456789abcdef01234567", pending.InfohashV1)
	require.Equal(t, pending.InfohashV1, pending.Hash, "a v1 magnet's TorrentID is its v1 hash")

	// The known magnet: the transfer-list branch answers the full info
	// object on the first call.
	status, body = session.do(http.MethodPost, "torrents/fetchMetadata",
		url.Values{"source": {torrent.magnet}}, "")
	t.Logf("probe: POST torrents/fetchMetadata source=<known magnet> -> %d %s", status, body)
	require.Equal(t, http.StatusOK, status, "a magnet the daemon knows must answer 200")

	var resolved struct {
		InfohashV1 string `json:"infohash_v1"`
		InfohashV2 string `json:"infohash_v2"`
		Hash       string `json:"hash"`
		Info       struct {
			Name    string `json:"name"`
			Length  int64  `json:"length"`
			Private bool   `json:"private"`
			Files   []struct {
				Path   string `json:"path"`
				Length int64  `json:"length"`
			} `json:"files"`
		} `json:"info"`
	}
	require.NoError(t, json.Unmarshal(body, &resolved))
	require.Equal(t, torrent.hash, resolved.InfohashV1)
	require.Equal(t, torrent.name, resolved.Info.Name)
	require.Equal(t, torrent.size, resolved.Info.Length)
	require.Len(t, resolved.Info.Files, 1)
	require.Equal(t, torrent.name, resolved.Info.Files[0].Path)
	require.Equal(t, torrent.size, resolved.Info.Files[0].Length)

	// parseMetadata takes uploaded .torrent file parts and answers the
	// same serialised shape in an array. It is not part of the magnet
	// flow — dl-tool parses blobs locally — but the probe records it
	// because the task named both endpoints.
	status, body = session.doMultipart("torrents/parseMetadata", "torrents", "probe.torrent", torrent.blob)
	t.Logf("probe: POST torrents/parseMetadata <torrent file part> -> %d %s", status, body)
	require.Equal(t, http.StatusOK, status)
	var parsed []json.RawMessage
	require.NoError(t, json.Unmarshal(body, &parsed))
	require.Len(t, parsed, 1)

	// An invalid uploaded part is the BadData refusal: 415 with the
	// daemon's own message — the observed shape, recorded verbatim.
	status, body = session.doMultipart("torrents/parseMetadata", "nothing", "empty", nil)
	t.Logf("probe: POST torrents/parseMetadata <invalid part> -> %d %s", status, body)
	require.Equal(t, http.StatusUnsupportedMediaType, status)
}

// mustFixtureURL returns just the fixture URL.
func mustFixtureURL(t *testing.T) string {
	t.Helper()
	url, _ := enginetest.Fixture(t)
	return url
}

// mustFixtureSHA returns just the fixture digest.
func mustFixtureSHA(t *testing.T) string {
	t.Helper()
	_, sha := enginetest.Fixture(t)
	return sha
}

// TestInspectMagnetLeavesNoHandle pins T038's central acceptance
// criterion: an inspection never leaves a torrent behind. The two daemon
// subtests prove it against a real 5.2.3 through the paths it actually
// takes there — the primary fetchMetadata path, whose hidden metadata
// download never enters torrents/info, and its timeout. The fake-driven
// subtests cover the fallback a 5.2.3 daemon never takes (fetchMetadata
// answering 404), its timeout, a cancelled caller, and the primary path's
// no-handle property without a container.
func TestInspectMagnetLeavesNoHandle(t *testing.T) {
	t.Run("daemon primary path", func(t *testing.T) {
		daemon := startDaemon(t)
		client := daemonClient(t, daemon.baseURL)
		session := newDaemonSession(t, daemon.baseURL)
		ctx, cancel := context.WithTimeout(context.Background(), containerTimeout)
		defer cancel()

		torrent := buildFixtureTorrent(t,
			fetchFixtureBody(t, mustFixtureURL(t), mustFixtureSHA(t)), "enginetest-inspect.bin", mustFixtureURL(t))
		id, err := client.Add(ctx, engine.AddRequest{Blob: torrent.blob, BlobKind: "torrent", StartPaused: true})
		require.NoError(t, err)

		before := session.torrentCount()
		manifest, err := client.InspectMagnet(ctx, torrent.magnet)
		require.NoError(t, err, "the known magnet must resolve through fetchMetadata")

		// The manifest's field sources, exactly the task's table.
		require.Equal(t, torrent.name, manifest.Name)
		require.Equal(t, torrent.size, manifest.TotalSize)
		require.Len(t, manifest.Files, 1)
		require.Equal(t, 0, manifest.Files[0].Index)
		require.Equal(t, torrent.name, manifest.Files[0].Path)
		require.Equal(t, torrent.size, manifest.Files[0].Size)
		require.Equal(t, torrent.hash, manifest.InfohashV1)
		require.Empty(t, manifest.InfohashV2)
		require.NotNil(t, manifest.Private)
		require.False(t, *manifest.Private)

		require.Equal(t, before, session.torrentCount(),
			"InspectMagnet must leave torrents/info exactly as it found it")

		session.daemonRemove(strings.TrimPrefix(id, engine.NameQBittorrent+":"), false)
		require.Eventually(t, func() bool { return session.torrentCount() == before-1 },
			addVisibilityTimeout, 250*time.Millisecond, "the seeded torrent was never removed")
	})

	t.Run("daemon timeout", func(t *testing.T) {
		daemon := startDaemon(t)
		client := daemonClient(t, daemon.baseURL)
		session := newDaemonSession(t, daemon.baseURL)

		// A magnet no swarm can resolve: metadata never arrives.
		unknown := "magnet:?xt=urn:btih:89abcdef0123456789abcdef0123456789abcdef&dn=never-resolves"
		ctx, cancel := context.WithTimeout(context.Background(), magnetProbeTimeout)
		defer cancel()

		before := session.torrentCount()
		_, err := client.InspectMagnet(ctx, unknown)
		require.ErrorIs(t, err, qbittorrent.ErrMetadataTimeout,
			"an unresolved magnet inside the deadline must answer ErrMetadataTimeout")
		require.Equal(t, before, session.torrentCount(),
			"a timed-out inspection must leave torrents/info untouched")
	})

	t.Run("primary path adds and deletes nothing", func(t *testing.T) {
		f := newInspectFake(t, nil)
		c := f.connectClient(t)

		manifest, err := c.InspectMagnet(context.Background(), f.magnet)
		require.NoError(t, err)
		require.Equal(t, "resolved name", manifest.Name)
		require.Equal(t, int64(2048), manifest.TotalSize)
		require.Equal(t, "0123456789abcdef0123456789abcdef01234567", manifest.InfohashV1)
		require.Empty(t, manifest.InfohashV2)
		require.Len(t, manifest.Files, 2)
		require.Equal(t, "dir/file one.bin", manifest.Files[0].Path)
		require.Equal(t, "top.bin", manifest.Files[1].Path)
		require.NotNil(t, manifest.Private)
		require.True(t, *manifest.Private)

		require.Zero(t, f.count(inspectPathAdd), "the primary path must not add a handle")
		require.Zero(t, f.count(inspectPathDelete), "the primary path must not delete anything")
	})

	t.Run("fallback success", func(t *testing.T) {
		f := newInspectFake(t, func(f *inspectFake) {
			f.fetchStatus = http.StatusNotFound
			f.filesEmptyPolls = 2
		})
		c := f.connectClient(t)

		manifest, err := c.InspectMagnet(context.Background(), f.magnet)
		require.NoError(t, err)
		require.Equal(t, "fallback name", manifest.Name)
		require.Equal(t, int64(3072), manifest.TotalSize)
		// The infohashes come from torrents/info's infohash_v1/infohash_v2
		// keys, never from hash, which here deliberately differs.
		require.Equal(t, "0123456789abcdef0123456789abcdef01234567", manifest.InfohashV1)
		require.Equal(t, "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedc", manifest.InfohashV2)
		require.Len(t, manifest.Files, 2)
		require.Equal(t, 0, manifest.Files[0].Index)
		require.Equal(t, 1, manifest.Files[1].Index)
		require.Nil(t, manifest.Private)

		// The add form carried the metadata-only shape exactly.
		add := f.lastCall(inspectPathAdd)
		require.Equal(t, []string{f.magnet}, add.Form["urls"])
		require.Equal(t, []string{"true"}, add.Form["stopped"])
		require.Equal(t, []string{"true"}, add.Form["paused"])
		require.Equal(t, []string{"MetadataReceived"}, add.Form["stopCondition"])
		require.Equal(t, []string{"false"}, add.Form["autoTMM"])
		require.NotContains(t, add.Form, "savepath", "no path may be hardcoded")

		// The handle was removed with deleteFiles=true and the fake's
		// torrent count is back where it started.
		del := f.lastCall(inspectPathDelete)
		require.Equal(t, []string{"0123456789abcdef0123456789abcdef01234567"}, del.Form["hashes"])
		require.Equal(t, []string{"true"}, del.Form["deleteFiles"])
		require.Equal(t, 1, f.fetchCount(), "fetchMetadata was probed once before the fallback")
	})

	t.Run("fallback timeout", func(t *testing.T) {
		f := newInspectFake(t, func(f *inspectFake) {
			f.fetchStatus = http.StatusNotFound
			f.filesEmptyPolls = 1 << 30 // metadata never arrives
		})
		c := f.connectClient(t)

		ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
		defer cancel()
		_, err := c.InspectMagnet(ctx, f.magnet)
		require.ErrorIs(t, err, qbittorrent.ErrMetadataTimeout)

		require.Equal(t, 1, f.count(inspectPathDelete),
			"a timed-out fallback must still remove its handle")
		del := f.lastCall(inspectPathDelete)
		require.Equal(t, []string{"true"}, del.Form["deleteFiles"])
	})

	t.Run("cancelled caller still removes the handle", func(t *testing.T) {
		f := newInspectFake(t, func(f *inspectFake) {
			f.fetchStatus = http.StatusNotFound
			f.filesEmptyPolls = 1 << 30
		})
		c := f.connectClient(t)

		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(300*time.Millisecond, cancel)
		_, err := c.InspectMagnet(ctx, f.magnet)
		require.ErrorIs(t, err, qbittorrent.ErrMetadataTimeout)

		require.Equal(t, 1, f.count(inspectPathDelete),
			"a cancelled caller must still trigger the removal")
		del := f.lastCall(inspectPathDelete)
		require.Equal(t, []string{"true"}, del.Form["deleteFiles"])
	})
}

// The paths the inspect fake serves.
const (
	inspectPathAdd     = "torrents/add"
	inspectPathDelete  = "torrents/delete"
	inspectPathFiles   = "torrents/files"
	inspectPathInfo    = "torrents/info"
	inspectPathFetch   = "torrents/fetchMetadata"
	inspectPathMaindat = "sync/maindata"

	// inspectHash is the v1 identity the fake's magnet carries and its
	// add answer echoes.
	inspectHash = "0123456789abcdef0123456789abcdef01234567"
)

// inspectFake is a qBittorrent WebAPI stand-in for the InspectMagnet
// subtests that drive the fallback — the paths a real 5.2.3 never takes
// because its fetchMetadata exists. It enforces the session cookie,
// answers fetchMetadata with a configurable status, and models the
// fallback's torrent lifecycle: add creates the handle, files answers an
// empty list until filesEmptyPolls polls have passed, delete removes it.
type inspectFake struct {
	t   *testing.T
	srv *httptest.Server

	// magnet is the submission every subtest inspects; its btih is
	// inspectHash so the add answer can echo the identity the adapter
	// resolved.
	magnet string

	mu     sync.Mutex
	cookie string
	calls  []recordedCall

	fetchStatus     int
	fetchBody       string
	filesEmptyPolls int
	filesPolls      int
	live            int
}

// recordedCall is one captured API call.
type recordedCall struct {
	Path string
	Form url.Values
}

// newInspectFake starts one fake. The default knobs serve the primary
// path: fetchMetadata answering 200 with the resolved info object.
func newInspectFake(t *testing.T, tune func(*inspectFake)) *inspectFake {
	t.Helper()

	f := &inspectFake{
		t:           t,
		fetchStatus: http.StatusOK,
		fetchBody: `{"infohash_v1":"0123456789abcdef0123456789abcdef01234567",` +
			`"infohash_v2":"","hash":"0123456789abcdef0123456789abcdef01234567",` +
			`"info":{"name":"resolved name","length":2048,"private":true,` +
			`"piece_length":16384,"pieces_num":1,` +
			`"files":[{"path":"dir/file one.bin","length":1536},` +
			`{"path":"./top.bin","length":512}]}}`,
	}
	if tune != nil {
		tune(f)
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)

	f.magnet = "magnet:?xt=urn:btih:" + inspectHash + "&dn=fallback+probe"
	return f
}

func (f *inspectFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := f.record(r)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, rec)

	switch r.URL.Path {
	case "/api/v2/auth/login":
		if rec.Form.Get("username") != qbtAdminUser || rec.Form.Get("password") != qbtAdminPass {
			http.Error(w, "Fails.", http.StatusUnauthorized)
			return
		}
		f.cookie = "sid-" + strconv.Itoa(len(f.calls))
		http.SetCookie(w, &http.Cookie{Name: "QBT_SID_8080", Value: f.cookie, Path: "/", HttpOnly: true})
		w.WriteHeader(http.StatusNoContent)
		return
	case "/api/v2/app/version", "/api/v2/app/webapiVersion":
		if !f.sessionOKLocked(r) {
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("v5.2.3"))
		return
	}

	if !f.sessionOKLocked(r) {
		http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.URL.Path {
	case "/api/v2/torrents/add":
		f.live++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success_count":1,"failure_count":0,"pending_count":0,` +
			`"added_torrent_ids":["` + inspectHash + `"]}`))

	case "/api/v2/torrents/delete":
		if rec.Form.Get("hashes") == inspectHash && f.live > 0 {
			f.live--
		}
		w.WriteHeader(http.StatusOK)

	case "/api/v2/torrents/files":
		f.filesPolls++
		w.Header().Set("Content-Type", "application/json")
		if f.filesPolls <= f.filesEmptyPolls {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`[{"index":0,"name":"dir/file one.bin","size":2048,` +
			`"progress":0,"priority":1,"availability":1,"piece_range":[0,0]},` +
			`{"index":1,"name":"top.bin","size":1024,` +
			`"progress":0,"priority":1,"availability":1,"piece_range":[0,0]}]`))

	case "/api/v2/torrents/info":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"hash":"` + inspectHash + `",` +
			`"infohash_v1":"0123456789abcdef0123456789abcdef01234567",` +
			`"infohash_v2":"fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210fedc",` +
			`"name":"fallback name","state":"metaDL","progress":0,` +
			`"private":null,"total_size":3072,"size":3072}]`))

	case "/api/v2/torrents/fetchMetadata":
		if f.fetchStatus != http.StatusOK {
			http.Error(w, "Not Found", f.fetchStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.fetchBody))

	case "/api/v2/sync/maindata":
		// The poll loop's read: an empty, always-full answer keeps the
		// cache empty without any error noise.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rid":1,"full_update":true,"torrents":{},"torrents_removed":[]}`))

	default:
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

// record captures one request's path and form. Torrents/add is multipart;
// every other call this fake serves is urlencoded or query-formed.
func (f *inspectFake) record(r *http.Request) recordedCall {
	rec := recordedCall{Path: r.URL.Path}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			f.t.Errorf("parse multipart form: %v", err)
			return rec
		}
		rec.Form = url.Values{}
		for name, values := range r.MultipartForm.Value {
			rec.Form[name] = append([]string(nil), values...)
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

// sessionOKLocked reports whether the request carries the live session
// cookie. Caller holds f.mu.
func (f *inspectFake) sessionOKLocked(r *http.Request) bool {
	if f.cookie == "" {
		return false
	}
	for _, cookie := range r.Cookies() {
		if cookie.Value == f.cookie {
			return true
		}
	}
	return false
}

// connectClient returns a client past Connect against this fake: the
// login above sets the session cookie the adapter demands.
func (f *inspectFake) connectClient(t *testing.T) *qbittorrent.Client {
	t.Helper()

	c, err := qbittorrent.New(qbittorrent.Config{
		BaseURL:  f.srv.URL,
		Username: qbtAdminUser,
		Password: qbtAdminPass,
	}, nil)
	require.NoError(t, err)
	require.NoError(t, c.Connect(context.Background()))
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	return c
}

// count returns how many requests reached one path.
func (f *inspectFake) count(path string) int {
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

// fetchCount returns how many times fetchMetadata was probed.
func (f *inspectFake) fetchCount() int { return f.count(inspectPathFetch) }

// lastCall returns the last request to one path.
func (f *inspectFake) lastCall(path string) recordedCall {
	f.t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Path == "/api/v2/"+path {
			return f.calls[i]
		}
	}
	f.t.Fatalf("no request to %s", path)
	return recordedCall{}
}

// TestNewServerRegistersQBittorrent pins the composition-root branch T038
// wires: a configured WebUI endpoint builds the client and registers it in
// the engine registry, an empty URL leaves it absent, and a malformed URL
// fails server construction loudly.
func TestNewServerRegistersQBittorrent(t *testing.T) {
	discard := slog.New(slog.NewJSONHandler(io.Discard, nil))

	valid, err := api.NewServer(&config.Config{
		QBittorrentURL:  "http://qbittorrent.test:8080",
		QBittorrentUser: qbtAdminUser,
		QBittorrentPass: secure.Secret(qbtAdminPass),
	}, nil, discard)
	require.NoError(t, err, "NewServer with a configured qbittorrent endpoint")
	e, ok := valid.Engines.Get(engine.NameQBittorrent)
	require.True(t, ok, "qbittorrent is not registered")
	_, isClient := e.(*qbittorrent.Client)
	require.True(t, isClient, "the registered engine is the qbittorrent client")

	empty, err := api.NewServer(&config.Config{}, nil, discard)
	require.NoError(t, err)
	_, ok = empty.Engines.Get(engine.NameQBittorrent)
	require.False(t, ok, "an empty URL must leave qbittorrent unregistered")

	_, err = api.NewServer(&config.Config{
		QBittorrentURL:  "not-a-url",
		QBittorrentUser: qbtAdminUser,
		QBittorrentPass: secure.Secret(qbtAdminPass),
	}, nil, discard)
	require.Error(t, err, "a malformed URL must fail construction loudly")
}
