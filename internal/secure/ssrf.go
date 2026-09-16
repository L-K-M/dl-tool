package secure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"syscall"
	"time"
)

// ErrSSRFBlocked is returned when a resolved peer address, port, scheme or hop count is not
// permitted. internal/api maps it to /problems/ssrf-blocked with status 403.
var ErrSSRFBlocked = errors.New("secure: ssrf blocked")

// ErrBodyTooLarge is returned by ReadCapped for an over-cap Content-Length or body.
var ErrBodyTooLarge = errors.New("secure: response body over cap")

// MetadataFetchCap is the body cap for a .torrent, feed or indexer fetch: 8 MiB.
const MetadataFetchCap int64 = 8 << 20

// redirectHopCap is the rule-6 limit of docs/12-security-and-threat-model.md
// section 2.2: at most five redirects per fetch.
const redirectHopCap = 5

// BlockedError names the rule that fired so a support request ends in one round trip.
// Reason is one of "network", "port", "scheme", "address", "resolve", "redirect_cap" or
// "guard" — the last means the Guard itself was never built by NewGuard, a wiring bug,
// so it is kept distinct from the policy-denial reasons rather than masquerading as one.
type BlockedError struct {
	Reason string
	IP     netip.Addr // zero when Reason is not "address"
	Prefix string     // the matched prefix, "" when Reason is not "address"
	Hop    int        // 0 for the original request
	URL    string     // already passed through RedactURL
}

func (e *BlockedError) Error() string { return "secure: ssrf blocked: " + e.Reason }
func (e *BlockedError) Unwrap() error { return ErrSSRFBlocked }

// denied4 is the 18-prefix IPv4 block list of docs/12-security-and-threat-model.md
// section 2.1, verbatim and in the order the document lists them.
var denied4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"),
	netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// allowed6 is the section 2.1 IPv6 allow-list: global unicast only. Every IPv6
// address outside it is blocked, which is what denies ::1, fc00::/7, fe80::/10,
// ff00::/8, 64:ff9b::/96 and IPv4-mapped ::ffff:0:0/96 without listing them.
var allowed6 = []netip.Prefix{
	netip.MustParsePrefix("2000::/3"),
}

// denied6 holds the five prefixes inside allowed6 that section 2.1 keeps blocked.
var denied6 = []netip.Prefix{
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2620:4f:8000::/48"),
	netip.MustParsePrefix("3fff::/20"),
}

// privateLift is the set an allow-private switch unblocks, per section 2.3:
// RFC 1918, loopback and unique-local. 169.254.0.0/16 and fe80::/10 are
// deliberately absent — link-local is where the cloud metadata services live
// and it stays denied under every switch.
var privateLift = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("::1/128"),
}

// Guard holds the prefix tables and the allow-private switch. It is safe for concurrent use.
type Guard struct {
	denied4      []netip.Prefix
	denied6      []netip.Prefix
	allowed6     []netip.Prefix
	allowPrivate bool
	log          *slog.Logger

	// originPort and originAddrs are populated only by ForOrigin: the one port
	// beyond 80 and 443 that the configured origin of a fetch may use, and the
	// resolved addresses that exemption applies to — never a whole port for
	// every destination.
	originPort  string
	originAddrs []netip.Addr
}

// NewGuard builds the guard from 12-security-and-threat-model.md §2.1. allowPrivate is
// config.Config.SSRFAllowPrivate for the global guard, or an indexer's own allow_private_network flag.
// It lifts 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 127.0.0.0/8, fc00::/7 and ::1/128 only;
// 169.254.0.0/16 and fe80::/10 stay denied under every switch.
func NewGuard(log *slog.Logger, allowPrivate bool) *Guard {
	if log == nil {
		log = slog.Default()
	}
	g := &Guard{
		denied4:      denied4,
		denied6:      denied6,
		allowed6:     allowed6,
		allowPrivate: allowPrivate,
		log:          log,
	}
	if !allowPrivate {
		return g
	}

	// Lift by rebuilding the tables rather than special-casing at match time:
	// the IPv4 members of privateLift leave denied4, and the IPv6 members —
	// which sit outside 2000::/3 and so are unreachable through the allow-list —
	// join allowed6.
	g.denied4 = make([]netip.Prefix, 0, len(denied4))
	for _, p := range denied4 {
		if !slices.Contains(privateLift, p) {
			g.denied4 = append(g.denied4, p)
		}
	}
	g.allowed6 = slices.Clone(allowed6)
	for _, p := range privateLift {
		if p.Addr().Is6() {
			g.allowed6 = append(g.allowed6, p)
		}
	}
	return g
}

// AllowAddr applies the §2.1 tables to one already-resolved address. It calls Is4In6 and Unmap
// before the IPv4 rules, and logs one warn record per denial. The record carries the fields
// section 2.4 names — url_redacted, resolved_ip, matched_prefix and hop — and a field this
// layer cannot know, such as the URL inside the dialer, stays zero rather than fabricated.
func (g *Guard) AllowAddr(ip netip.Addr) error {
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	if !g.usable() {
		return &BlockedError{Reason: "guard", IP: ip}
	}
	if ip.Is4() {
		for _, p := range g.denied4 {
			if p.Contains(ip) {
				return g.deny(ip, p)
			}
		}
		return nil
	}

	allowed := false
	for _, p := range g.allowed6 {
		if p.Contains(ip) {
			allowed = true
			break
		}
	}
	if !allowed {
		// Denied by the allow-list, so no prefix matched: Prefix stays "".
		return g.deny(ip, netip.Prefix{})
	}
	for _, p := range g.denied6 {
		if p.Contains(ip) {
			return g.deny(ip, p)
		}
	}
	return nil
}

// Check is wired into net.Dialer.ControlContext, never Control. network is "tcp4" or "tcp6";
// addr is always "ip:port", never a hostname. Ports 80 and 443 are the only ones permitted,
// except on a guard returned by ForOrigin, which also permits that one origin's own port.
func (g *Guard) Check(_ context.Context, network, addr string) error {
	if !g.usable() {
		return &BlockedError{Reason: "guard"}
	}
	if network != "tcp4" && network != "tcp6" {
		return g.block(&BlockedError{Reason: "network"}, netip.Addr{}, "")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return g.block(&BlockedError{Reason: "port"}, netip.Addr{}, "")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		// ControlContext runs after resolution, so addr should always carry an
		// IP literal; a non-IP here means resolution produced nothing usable.
		return g.block(&BlockedError{Reason: "resolve"}, netip.Addr{}, "")
	}
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	if !g.portAllowed(port, ip) {
		return g.block(&BlockedError{Reason: "port"}, ip, "")
	}
	return g.AllowAddr(ip)
}

// ForOrigin returns a copy of g that additionally permits u's port, for u's own resolved address
// and nothing else, when allowPrivate is set. It is how a Prowlarr on :9696 or a Jackett on :9117
// is reachable while every redirect hop to another host stays limited to 80 and 443
// (12-security-and-threat-model.md sections 2.2 rule 5 and 2.3).
func (g *Guard) ForOrigin(u *url.URL) *Guard {
	if g == nil || !g.allowPrivate || u == nil || u.Port() == "" {
		return g
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if err != nil || len(ips) == 0 {
		// Fail closed: an unresolvable origin keeps the stock 80/443 set rather
		// than permitting its port for every address.
		g.log.Warn("ssrf guard could not resolve permitted origin",
			"url_redacted", RedactURL(u.String()), "error", err)
		return g
	}
	out := *g
	out.originPort = u.Port()
	out.originAddrs = make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		out.originAddrs = append(out.originAddrs, ip.Unmap())
	}
	return &out
}

// portAllowed reports whether a dialed ip:port may proceed: 80 and 443 for any
// destination, or the ForOrigin port only when the dialed IP is one the origin
// resolved to — compared post-Unmap so a 4-in-6 dial still matches its origin.
func (g *Guard) portAllowed(port string, ip netip.Addr) bool {
	if port == "80" || port == "443" {
		return true
	}
	if g.originPort == "" || port != g.originPort {
		return false
	}
	return slices.Contains(g.originAddrs, ip)
}

// CheckRedirect caps hops at 5 and requires the scheme to stay http or https on every hop.
func (g *Guard) CheckRedirect(req *http.Request, via []*http.Request) error {
	if !g.usable() {
		return &BlockedError{Reason: "guard", Hop: len(via)}
	}
	target := ""
	if req.URL != nil {
		target = RedactURL(req.URL.String())
	}
	if len(via) >= redirectHopCap {
		return g.block(&BlockedError{Reason: "redirect_cap", Hop: len(via), URL: target}, netip.Addr{}, "")
	}
	if req.URL == nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
		return g.block(&BlockedError{Reason: "scheme", Hop: len(via), URL: target}, netip.Addr{}, "")
	}
	return nil
}

// NewClient returns the one outbound client: dial timeout 10s, total timeout 120s,
// ForceAttemptHTTP2, ControlContext wired to Check and CheckRedirect wired to the guard.
// No other file in the repository may construct an http.Client or call http.Get or http.Post.
func NewClient(g *Guard) *http.Client {
	d := &net.Dialer{
		Timeout: 10 * time.Second,
		// ControlContext, never Control — Control is ignored whenever
		// ControlContext is set (section 2.2 rule 1). Check keeps its
		// contract signature for direct calls; this adapts it to the
		// RawConn-carrying dialer hook.
		ControlContext: func(ctx context.Context, network, address string, _ syscall.RawConn) error {
			return g.Check(ctx, network, address)
		},
	}
	return &http.Client{
		Transport:     &http.Transport{DialContext: d.DialContext, ForceAttemptHTTP2: true},
		CheckRedirect: g.CheckRedirect,
		Timeout:       120 * time.Second,
	}
}

// ReadCapped rejects an over-cap declared Content-Length before reading a byte, then reads
// through an io.LimitReader of limit+1 bytes so a lying Content-Length cannot bypass the cap.
func ReadCapped(resp *http.Response, limit int64) ([]byte, error) {
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("%w: declared content length %d exceeds %d", ErrBodyTooLarge, resp.ContentLength, limit)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("secure: read response body: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: streamed body exceeds %d", ErrBodyTooLarge, limit)
	}
	return body, nil
}

// RedactURL returns scheme://host/path with userinfo and the whole query string removed, for
// log records and problem details. An unparseable input returns "[unparseable url]".
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparseable url]"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// RedactError rewrites the URL inside a *url.Error produced by the guarded client.
// http.Client fills it with the password-stripped but query-intact request or redirect
// target, so logging the raw error would leak API keys carried in query strings — callers
// that log or return fetch errors route them through this first. Errors without a
// *url.Error pass through unchanged, and the wrapped cause is untouched.
func RedactError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		scrubbed := *urlErr
		scrubbed.URL = RedactURL(urlErr.URL)
		return &scrubbed
	}
	return err
}

// usable reports whether g came from NewGuard. A zero-value Guard must fail
// closed rather than allow every IPv4 address through nil prefix tables or
// panic on a nil logger at the first denial.
func (g *Guard) usable() bool {
	return g != nil && g.log != nil && g.denied4 != nil && g.allowed6 != nil
}

// deny builds the "address"-reason BlockedError for a prefix match (or an
// allow-list miss, where prefix is the zero Prefix) and logs the block.
func (g *Guard) deny(ip netip.Addr, prefix netip.Prefix) error {
	matched := ""
	if prefix != (netip.Prefix{}) {
		matched = prefix.String()
	}
	return g.block(&BlockedError{Reason: "address", IP: ip, Prefix: matched}, ip, matched)
}

// block logs the one warn record per denial that section 2.4 requires and
// returns the error. The record carries url_redacted, resolved_ip,
// matched_prefix and hop; a field the blocking layer cannot know — the request
// URL inside the dialer — stays zero rather than fabricated.
func (g *Guard) block(err *BlockedError, ip netip.Addr, prefix string) *BlockedError {
	attrs := []any{
		"reason", err.Reason,
		"url_redacted", err.URL,
		"resolved_ip", ip,
		"matched_prefix", prefix,
		"hop", err.Hop,
	}
	g.log.Warn("ssrf blocked", attrs...)
	return err
}
