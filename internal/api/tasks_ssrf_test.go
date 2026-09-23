package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/L-K-M/dl-tool/internal/secure"
)

// staticResolver is the test Resolver of task step 9: a fixed map, no DNS.
// An unlisted host answers NXDOMAIN so a fixture typo fails closed instead
// of passing on a real lookup.
type staticResolver map[string][]netip.Addr

func (r staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := r[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}

	return addrs, nil
}

// permissiveResolver answers every host with one public address, so the
// shared submission envs' fixture hostnames (releases.example.com and
// friends) pass the preflight exactly as they did when no resolution ran at
// all. Literal-IP submissions never consult it — they still face the guard's
// tables.
type permissiveResolver struct{}

func (permissiveResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

// ssrfResolver is the pinned map of task step 9: blocked.example resolves to
// the link-local metadata address — denied under every allow-private switch —
// and public.example to a public one. multi.example answers a public and a
// private address together, for the one-blocked-answer-blocks-all rule.
var ssrfResolver = staticResolver{
	"blocked.example": {netip.MustParseAddr("169.254.169.254")},
	"public.example":  {netip.MustParseAddr("93.184.216.34")},
	"multi.example":   {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.0.0.7")},
}

// newSSRFEnv is the shared tasks env with the pinned resolver substituted for
// the permissive default, so the fixture hosts resolve to their test answers.
func newSSRFEnv(t *testing.T) *tasksTestEnv {
	t.Helper()

	env := newTasksTestEnv(t)
	env.server.tasks.resolver = ssrfResolver

	return env
}

// assertNoEngineAdd proves no engine recorded an Add call.
func assertNoEngineAdd(t *testing.T, env *tasksTestEnv) {
	t.Helper()

	for _, call := range slices.Concat(env.aria2.recorded(), env.qbittorrent.recorded()) {
		if call == "Add" {
			t.Fatalf("engine Add was called for a blocked URI")
		}
	}
}

// TestCreateTasksBlocksLoopbackURI pins acceptance criterion 1: a loopback
// submission is the 403 problem, and the engine is never asked to fetch it.
// The default-port variant exercises the address rule itself — the :8080
// form the criterion names is refused by the port rule before resolution.
func TestCreateTasksBlocksLoopbackURI(t *testing.T) {
	env := newSSRFEnv(t)

	for _, raw := range []string{"http://127.0.0.1:8080/x", "http://127.0.0.1/x", "http://[::1]/x"} {
		t.Run(raw, func(t *testing.T) {
			resp := env.createTasks(t, map[string]any{"uris": []string{raw}})
			assertProblem(t, resp, http.StatusForbidden, SlugSSRFBlocked)
		})
	}
	assertNoEngineAdd(t, env)
}

// TestCreateTasksMarksBlockedTaskError pins acceptance criterion 2: the row
// a blocked URI leaves behind lands in error with its error_code stamped.
func TestCreateTasksMarksBlockedTaskError(t *testing.T) {
	env := newSSRFEnv(t)

	resp := env.createTasks(t, map[string]any{"uris": []string{"http://blocked.example/f.iso"}})
	assertProblem(t, resp, http.StatusForbidden, SlugSSRFBlocked)

	var row struct {
		State     string  `db:"state"`
		ErrorCode *string `db:"error_code"`
	}
	if err := env.db.GetContext(t.Context(), &row, `SELECT state, error_code FROM tasks`); err != nil {
		t.Fatalf("read blocked task: %v", err)
	}
	if row.State != "error" {
		t.Errorf("state = %q, want error", row.State)
	}
	if row.ErrorCode == nil || *row.ErrorCode != "ssrf_blocked" {
		t.Errorf("error_code = %v, want ssrf_blocked", row.ErrorCode)
	}
	assertNoEngineAdd(t, env)
}

// TestBlockedTaskCannotResume pins the terminal shape of a blocked row:
// error -> queued is a legal transition, so without the actions gate a
// resume would requeue the row and the admission pass would hand the
// refused uri to an engine — the hole the preflight exists to close.
// Every lifecycle action but remove is refused on an ssrf_blocked row.
func TestBlockedTaskCannotResume(t *testing.T) {
	env := newSSRFEnv(t)

	resp := env.createTasks(t, map[string]any{"uris": []string{"http://blocked.example/f.iso"}})
	assertProblem(t, resp, http.StatusForbidden, SlugSSRFBlocked)

	var id string
	if err := env.db.GetContext(t.Context(), &id, `SELECT id FROM tasks`); err != nil {
		t.Fatalf("read blocked task id: %v", err)
	}

	for _, action := range []string{actionResume, actionPause, actionRecheck, actionForceComplete} {
		response := env.api.Post("/tasks/actions",
			map[string]any{"action": action, "ids": []string{id}},
			"Authorization: Bearer "+env.bearer)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want %d; body %s", action, response.Code, http.StatusOK, response.Body.String())
		}
		want := ActionResult{ID: id, Ok: false, Type: SlugSSRFBlocked, Detail: detailSSRFBlockedAction}
		if result := decodeActionsBody(t, response).Results[0]; result != want {
			t.Errorf("%s: result = %+v, want %+v", action, result, want)
		}
	}

	var state string
	if err := env.db.GetContext(t.Context(), &state, `SELECT state FROM tasks WHERE id = ?`, id); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state != "error" {
		t.Errorf("state = %q, want error", state)
	}
	assertNoEngineAdd(t, env)

	// remove is the one action a blocked row still accepts — cleanup,
	// never a requeue.
	response := env.api.Post("/tasks/actions",
		map[string]any{"action": actionRemove, "ids": []string{id}},
		"Authorization: Bearer "+env.bearer)
	if response.Code != http.StatusOK {
		t.Fatalf("remove: status = %d, body %s", response.Code, response.Body.String())
	}
	if result := decodeActionsBody(t, response).Results[0]; !result.Ok {
		t.Errorf("remove: result = %+v, want ok", result)
	}
}

// TestCreateTasksMixedSubmission pins acceptance criterion 3: one blocked and
// one public URI create exactly one task and one typed rejection.
func TestCreateTasksMixedSubmission(t *testing.T) {
	env := newSSRFEnv(t)

	resp := env.createTasks(t, map[string]any{
		"uris": []string{"http://blocked.example/a.iso", "http://public.example/b.iso"},
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", resp.Code, http.StatusCreated, resp.Body.String())
	}
	body := decodeCreateBody(t, resp)
	if len(body.Created) != 1 {
		t.Fatalf("created = %+v, want exactly one task", body.Created)
	}
	if len(body.Rejected) != 1 {
		t.Fatalf("rejected = %+v, want exactly one entry", body.Rejected)
	}
	if body.Rejected[0].Type != SlugSSRFBlocked {
		t.Errorf("rejected type = %q, want %q", body.Rejected[0].Type, SlugSSRFBlocked)
	}
}

// TestCreateTasksBlockedAndJunk pins the boundary of the all-blocked 403:
// a blocked URI beside a URI refused for another reason creates nothing
// but is not "every URI blocked", so the answer stays the all-rejected
// 422 of doc 05 section 5.2 with the first rejection's detail.
func TestCreateTasksBlockedAndJunk(t *testing.T) {
	env := newSSRFEnv(t)

	resp := env.createTasks(t, map[string]any{
		"uris": []string{"http://blocked.example/f.iso", "ed2k://|file|x|1|AA|/"},
	})
	assertProblem(t, resp, http.StatusUnprocessableEntity, SlugUnsupportedScheme)
	assertNoEngineAdd(t, env)
}

// TestInspectBlocksBlockedHost pins acceptance criterion 4: a blocked host
// is the 403 problem and inspect writes nothing.
func TestInspectBlocksBlockedHost(t *testing.T) {
	env := newInspectTestEnv(t)
	env.server.tasks.resolver = ssrfResolver

	resp := env.inspect(t, map[string]any{"uris": []string{"http://blocked.example/x.iso"}})
	assertProblem(t, resp, http.StatusForbidden, SlugSSRFBlocked)
	env.assertNoTask(t)
}

// TestTorrentURLCreatesIdentitylessTask keeps endpoint-level coverage of
// the contract the late-resolution test used to prove through POST /tasks
// before its 127.0.0.1 stub URL became unsubmitable: a .torrent URI creates
// the task with no infohash — identity arrives with the metadata, not the
// submission.
func TestTorrentURLCreatesIdentitylessTask(t *testing.T) {
	env := newSSRFEnv(t)

	resp := env.createTasks(t, map[string]any{"uris": []string{"http://public.example/fixture.torrent"}})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", resp.Code, http.StatusCreated, resp.Body.String())
	}
	created := decodeCreateBody(t, resp).Created
	if len(created) != 1 {
		t.Fatalf("created = %+v, want exactly one task", created)
	}
	if created[0].InfohashV1 != nil || created[0].InfohashV2 != nil {
		t.Errorf("infohashes = %v/%v, want both nil at create", created[0].InfohashV1, created[0].InfohashV2)
	}
}

// TestInspectBlockedAndJunk mirrors TestCreateTasksBlockedAndJunk on the
// inspect endpoint: the all-blocked 403 is for submissions whose every URI
// the guard refused, and a blocked URI beside another refusal keeps the
// all-rejected 422 — with nothing written either way.
func TestInspectBlockedAndJunk(t *testing.T) {
	env := newInspectTestEnv(t)
	env.server.tasks.resolver = ssrfResolver

	resp := env.inspect(t, map[string]any{
		"uris": []string{"http://blocked.example/x.iso", "ed2k://|file|x|1|AA|/"},
	})
	assertProblem(t, resp, http.StatusUnprocessableEntity, SlugUnsupportedScheme)
	env.assertNoTask(t)
}

// TestPreflightBlocksUnparseable pins the fail-closed parse branch: input
// the normaliser never produced — engines parse URIs more permissively than
// url.Parse — is refused, not allowed.
func TestPreflightBlocksUnparseable(t *testing.T) {
	guard := newSSRFGuard(slog.New(slog.NewJSONHandler(io.Discard, nil)), false)

	err := secure.PreflightURI(t.Context(), guard, ssrfResolver, "http://127.0.0.1:8080/%zz")
	if !errors.Is(err, secure.ErrSSRFBlocked) {
		t.Fatalf("PreflightURI = %v, want ErrSSRFBlocked", err)
	}
	var blocked *secure.BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("PreflightURI error = %T, want *secure.BlockedError", err)
	}
	if blocked.Reason != "parse" {
		t.Errorf("reason = %q, want parse", blocked.Reason)
	}
}

// TestPreflightIgnoresMagnet pins acceptance criterion 5: schemes the guard
// does not govern pass through untouched.
func TestPreflightIgnoresMagnet(t *testing.T) {
	guard := newSSRFGuard(slog.New(slog.NewJSONHandler(io.Discard, nil)), false)

	for _, raw := range []string{
		"magnet:?xt=urn:btih:" + btihV1Hex,
		btihV1Hex,
		"thunder://QUJodHRwOi8vZXhhbXBsZS5jb20vZg==",
		"ed2k://|file|x|1|0123456789abcdef0123456789abcdef|/",
	} {
		if err := secure.PreflightURI(t.Context(), guard, ssrfResolver, raw); err != nil {
			t.Errorf("PreflightURI(%q) = %v, want nil", raw, err)
		}
	}
}

// TestPreflightBlocksNonStandardHTTPPort pins step 2: an explicit http port
// outside 80 and 443 is refused before any lookup.
func TestPreflightBlocksNonStandardHTTPPort(t *testing.T) {
	guard := newSSRFGuard(slog.New(slog.NewJSONHandler(io.Discard, nil)), false)

	err := secure.PreflightURI(t.Context(), guard, ssrfResolver, "http://public.example:8080/x")
	if !errors.Is(err, secure.ErrSSRFBlocked) {
		t.Fatalf("PreflightURI = %v, want ErrSSRFBlocked", err)
	}
	var blocked *secure.BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("PreflightURI error = %T, want *secure.BlockedError", err)
	}
	if blocked.Reason != "port" {
		t.Errorf("reason = %q, want port", blocked.Reason)
	}
}

// TestPreflightBlocksNonStandardHTTPPortMixedCase pins the scheme-case
// regression: schemes are case-insensitive and the submission's raw
// spelling reaches preflight, so "HTTP://…:8080" must hit the same port
// rule "http://…:8080" does — an uppercase scheme is no bypass.
func TestPreflightBlocksNonStandardHTTPPortMixedCase(t *testing.T) {
	guard := newSSRFGuard(slog.New(slog.NewJSONHandler(io.Discard, nil)), false)

	for _, raw := range []string{
		"HTTP://public.example:8080/x",
		"Https://public.example:8443/x",
		"HtTp://127.0.0.1:8080/x",
	} {
		err := secure.PreflightURI(t.Context(), guard, ssrfResolver, raw)
		if !errors.Is(err, secure.ErrSSRFBlocked) {
			t.Errorf("PreflightURI(%q) = %v, want ErrSSRFBlocked", raw, err)
			continue
		}
		var blocked *secure.BlockedError
		if !errors.As(err, &blocked) {
			t.Fatalf("PreflightURI(%q) error = %T, want *secure.BlockedError", raw, err)
		}
		if blocked.Reason != "port" {
			t.Errorf("PreflightURI(%q) reason = %q, want port", raw, blocked.Reason)
		}
	}
}

// TestCreateTasksBlocksMixedCaseSchemeURI pins the end-to-end reachability
// of the scheme-case hole: uri.Normalize keeps the submitted scheme's case
// in the normalised URI, so an uppercase HTTP URI must face the same port
// rule as the lowercase form and never reach an engine.
func TestCreateTasksBlocksMixedCaseSchemeURI(t *testing.T) {
	env := newSSRFEnv(t)

	resp := env.createTasks(t, map[string]any{"uris": []string{"HTTP://public.example:8080/x.iso"}})
	assertProblem(t, resp, http.StatusForbidden, SlugSSRFBlocked)
	assertNoEngineAdd(t, env)
}

// TestPreflightAllowsSFTPOnPort2222 pins step 2's other half: the
// file-transfer schemes carry no port constraint; the address check alone
// decides.
func TestPreflightAllowsSFTPOnPort2222(t *testing.T) {
	guard := newSSRFGuard(slog.New(slog.NewJSONHandler(io.Discard, nil)), false)

	if err := secure.PreflightURI(t.Context(), guard, ssrfResolver, "sftp://public.example:2222/x"); err != nil {
		t.Errorf("PreflightURI = %v, want nil", err)
	}
}

// TestPreflightBlocksWhenOneAnswerIsPrivate pins step 3: one blocked answer
// among many blocks the URI.
func TestPreflightBlocksWhenOneAnswerIsPrivate(t *testing.T) {
	guard := newSSRFGuard(slog.New(slog.NewJSONHandler(io.Discard, nil)), false)

	err := secure.PreflightURI(t.Context(), guard, ssrfResolver, "http://multi.example/x")
	if !errors.Is(err, secure.ErrSSRFBlocked) {
		t.Fatalf("PreflightURI = %v, want ErrSSRFBlocked", err)
	}
	var blocked *secure.BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("PreflightURI error = %T, want *secure.BlockedError", err)
	}
	if blocked.Reason != "address" {
		t.Errorf("reason = %q, want address", blocked.Reason)
	}
}

// TestPreflightRedactsUserinfo pins acceptance criterion 6: the rejection for
// a credential-carrying blocked URI exposes neither the password nor the
// query string, and nothing in the body leaks the resolved address either.
func TestPreflightRedactsUserinfo(t *testing.T) {
	env := newSSRFEnv(t)

	resp := env.createTasks(t, map[string]any{
		"uris": []string{
			"ftp://u:Sup3rSecret@blocked.example/f?passkey=k3y",
			"http://public.example/b.iso",
		},
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", resp.Code, http.StatusCreated, resp.Body.String())
	}
	body := decodeCreateBody(t, resp)
	if len(body.Rejected) != 1 || body.Rejected[0].Type != SlugSSRFBlocked {
		t.Fatalf("rejected = %+v, want one ssrf entry", body.Rejected)
	}
	if body.Rejected[0].URI != "ftp://blocked.example/f" {
		t.Errorf("rejected uri = %q, want ftp://blocked.example/f", body.Rejected[0].URI)
	}
	for _, leaked := range []string{"Sup3rSecret", "k3y", "169.254.169.254", "169.254.0.0/16"} {
		if strings.Contains(resp.Body.String(), leaked) {
			t.Errorf("response body leaks %q: %s", leaked, resp.Body.String())
		}
	}
}
