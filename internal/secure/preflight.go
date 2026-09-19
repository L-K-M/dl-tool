package secure

import (
	"context"
	"net/netip"
	"net/url"
	"strings"
)

// Resolver is satisfied by *net.Resolver. A test substitutes a static map so
// no DNS query is made.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// PreflightURI validates a user-submitted URI before any engine is asked to
// fetch it, because the engines dial for themselves and the dialer guard of
// ssrf.go never sees their connections.
//
// It returns nil for a scheme the guard does not govern: magnet, a bare
// infohash and the obfuscated thunder, flashget and qqdl forms are parsed in
// process and never fetched, and an unsupported scheme is already rejected by
// the router. Callers pass the normalised URI, so a decoded obfuscated inner
// URI still reaches the http/ftp checks here.
//
// For http and https an explicit port other than 80 or 443 is blocked,
// matching rule 5 of 12-security-and-threat-model.md §2.2. For ftp, ftps and
// sftp the port is not constrained; the address check is what protects the
// LAN there.
//
// A literal-IP host is checked without a lookup. Otherwise EVERY address the
// host resolves to must pass g.AllowAddr: one blocked answer blocks the URI,
// and an empty or failing lookup blocks it too.
//
// Residual risk, because this check is preflight-only: the engines re-resolve
// the host when they dial and follow HTTP redirects on their own, so DNS
// rebinding or a 30x from an allowed host to a blocked address can still
// reach the LAN. Closing that window is a plan-level decision (pin the
// resolved address through admission, or have the adapters fetch through
// the guarded dialer); it is recorded under T122's ## Blocked.
func PreflightURI(ctx context.Context, g *Guard, r Resolver, rawURI string) error {
	// The scheme gate is textual, not parse-dependent: the governed set is
	// small and closed, so a URI without a "scheme:" prefix (a bare
	// infohash) or with an ungoverned scheme (magnet, ed2k, the obfuscated
	// forms) is passed through without ever asking url.Parse — which is
	// stricter than the engines' parsers on bodies such as ed2k's pipes.
	scheme, _, found := strings.Cut(rawURI, ":")
	if !found {
		return nil
	}
	switch strings.ToLower(scheme) {
	case "http", "https", "ftp", "ftps", "sftp":
	default:
		return nil
	}

	u, err := url.Parse(rawURI)
	if err != nil {
		// A governed scheme whose body will not parse cannot be proven
		// safe — fail closed, as the "guard" branch does for a wiring gap.
		blocked := &BlockedError{Reason: "parse", URL: RedactURL(rawURI)}
		if g.usable() {
			return g.block(blocked, netip.Addr{}, "")
		}

		return blocked
	}

	if !g.usable() || r == nil {
		// A missing guard or resolver is a wiring bug, not a policy answer;
		// like AllowAddr it fails closed with the distinct "guard" reason.
		return &BlockedError{Reason: "guard", URL: RedactURL(rawURI)}
	}

	if scheme == "http" || scheme == "https" {
		if port := u.Port(); port != "" && port != "80" && port != "443" {
			return g.block(&BlockedError{Reason: "port", URL: RedactURL(rawURI)}, netip.Addr{}, "")
		}
	}

	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		return uriBlockedError(g.AllowAddr(ip), rawURI)
	}

	addrs, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return g.block(&BlockedError{Reason: "resolve", URL: RedactURL(rawURI)}, netip.Addr{}, "")
	}
	for _, ip := range addrs {
		if err := g.AllowAddr(ip); err != nil {
			return uriBlockedError(err, rawURI)
		}
	}

	return nil
}

// uriBlockedError attaches the redacted submission URL to a denial AllowAddr
// produced without it, so a caller that logs or inspects the error sees which
// URI was refused. The guard's own warn record stands as emitted — its field
// set is T123's, and the URL it could not know stays absent there.
func uriBlockedError(err error, rawURI string) error {
	if err == nil {
		return nil
	}
	if be, ok := err.(*BlockedError); ok && be.URL == "" {
		be.URL = RedactURL(rawURI)
	}
	return err
}
