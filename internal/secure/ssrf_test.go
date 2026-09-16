package secure

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testGuard(allowPrivate bool) *Guard {
	return NewGuard(slog.New(slog.NewTextHandler(io.Discard, nil)), allowPrivate)
}

// blockedReason unwraps err into a *BlockedError so a test can pin which rule fired.
func blockedReason(t *testing.T, err error) string {
	t.Helper()

	var blocked *BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("err = %v, want a *BlockedError", err)
	}
	return blocked.Reason
}

func TestCheckBlocksLoopback(t *testing.T) {
	g := testGuard(false)
	if err := g.Check(context.Background(), "tcp4", "127.0.0.1:80"); !errors.Is(err, ErrSSRFBlocked) {
		t.Errorf("Check(127.0.0.1:80) = %v, want ErrSSRFBlocked", err)
	}
}

// TestCheckBlocksLinkLocalMapped feeds a 4-in-6 form of the cloud metadata
// address: Is4In6 plus Unmap must route it into the IPv4 rules.
func TestCheckBlocksLinkLocalMapped(t *testing.T) {
	g := testGuard(false)
	err := g.Check(context.Background(), "tcp6", "[::ffff:169.254.169.254]:443")
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("Check(::ffff:169.254.169.254:443) = %v, want ErrSSRFBlocked", err)
	}
	if reason := blockedReason(t, err); reason != "address" {
		t.Errorf("reason = %q, want %q", reason, "address")
	}
}

func TestCheckBlocksNonStandardPort(t *testing.T) {
	g := testGuard(false)
	err := g.Check(context.Background(), "tcp4", "93.184.216.34:8080")
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("Check(93.184.216.34:8080) = %v, want ErrSSRFBlocked", err)
	}
	if reason := blockedReason(t, err); reason != "port" {
		t.Errorf("reason = %q, want %q", reason, "port")
	}
}

func TestCheckAllowsPublicAddress(t *testing.T) {
	g := testGuard(false)
	if err := g.Check(context.Background(), "tcp4", "93.184.216.34:443"); err != nil {
		t.Errorf("Check(93.184.216.34:443) = %v, want nil", err)
	}
}

func TestAllowPrivateLiftsRFC1918(t *testing.T) {
	g := testGuard(true)
	if err := g.Check(context.Background(), "tcp4", "10.1.2.3:80"); err != nil {
		t.Errorf("Check(10.1.2.3:80) with allowPrivate = %v, want nil", err)
	}
}

// TestAllowPrivateKeepsLinkLocalDenied pins section 2.3: no switch ever lifts
// 169.254.0.0/16, because that is where the metadata endpoints live.
func TestAllowPrivateKeepsLinkLocalDenied(t *testing.T) {
	g := testGuard(true)
	if err := g.Check(context.Background(), "tcp4", "169.254.169.254:80"); !errors.Is(err, ErrSSRFBlocked) {
		t.Errorf("Check(169.254.169.254:80) with allowPrivate = %v, want ErrSSRFBlocked", err)
	}
}

func TestCheckRedirectCapsAtFiveHops(t *testing.T) {
	g := testGuard(false)
	via := make([]*http.Request, redirectHopCap)
	for i := range via {
		via[i] = &http.Request{URL: &url.URL{Scheme: "https", Host: "x.example"}}
	}
	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "x.example"}}

	err := g.CheckRedirect(req, via)
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("CheckRedirect at hop %d = %v, want ErrSSRFBlocked", len(via), err)
	}
	if reason := blockedReason(t, err); reason != "redirect_cap" {
		t.Errorf("reason = %q, want %q", reason, "redirect_cap")
	}

	if err := g.CheckRedirect(req, via[:redirectHopCap-1]); err != nil {
		t.Errorf("CheckRedirect at hop %d = %v, want nil", redirectHopCap-1, err)
	}
}

// TestCheckRedirectRejectsFileScheme is the 301-to-file: case of rule 4: the
// scheme check runs on every hop, not only the first.
func TestCheckRedirectRejectsFileScheme(t *testing.T) {
	g := testGuard(false)
	via := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "x.example"}}}
	req := &http.Request{URL: &url.URL{Scheme: "file", Path: "/etc/passwd"}}

	err := g.CheckRedirect(req, via)
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("CheckRedirect to file: = %v, want ErrSSRFBlocked", err)
	}
	if reason := blockedReason(t, err); reason != "scheme" {
		t.Errorf("reason = %q, want %q", reason, "scheme")
	}
}

// TestClientBlocksRedirectToMetadata runs the full client against a local
// server — reachable only through allowPrivate plus the ForOrigin port lift —
// that 302s to the cloud metadata endpoint. The dialer's ControlContext must
// refuse the hop before a connection is made.
func TestClientBlocksRedirectToMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer server.Close()

	origin, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	client := NewClient(testGuard(true).ForOrigin(origin))

	resp, err := client.Get(server.URL)
	if resp != nil {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Errorf("close response body: %v", cerr)
		}
	}
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("client.Get redirect to metadata = %v, want ErrSSRFBlocked", err)
	}
}

// trackingReader records whether Read ran so a test can prove the declared
// length is rejected before a single byte is consumed.
type trackingReader struct {
	read bool
}

func (r *trackingReader) Read([]byte) (int, error) {
	r.read = true
	return 0, io.EOF
}

func (r *trackingReader) Close() error { return nil }

func TestReadCappedRejectsDeclaredLength(t *testing.T) {
	body := &trackingReader{}
	resp := &http.Response{ContentLength: 9 << 20, Body: body}

	if _, err := ReadCapped(resp, MetadataFetchCap); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("ReadCapped with 9 MiB Content-Length = %v, want ErrBodyTooLarge", err)
	}
	if body.read {
		t.Error("ReadCapped read from a body whose declared length was already over cap")
	}
}

// TestReadCappedRejectsLyingLength pins rule 7: a small declared length must
// not let an over-cap body through the streaming cap.
func TestReadCappedRejectsLyingLength(t *testing.T) {
	resp := &http.Response{
		ContentLength: 10,
		Body:          io.NopCloser(strings.NewReader(strings.Repeat("a", 9<<20))),
	}

	if _, err := ReadCapped(resp, MetadataFetchCap); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("ReadCapped with lying Content-Length = %v, want ErrBodyTooLarge", err)
	}
}

func TestRedactURLDropsUserinfoAndQuery(t *testing.T) {
	got := RedactURL("https://u:p@x.example/a?apikey=k")
	if got != "https://x.example/a" {
		t.Errorf("RedactURL = %q, want %q", got, "https://x.example/a")
	}
	if got := RedactURL("http://[::1"); got != "[unparseable url]" {
		t.Errorf("RedactURL of unparseable input = %q, want %q", got, "[unparseable url]")
	}
}
