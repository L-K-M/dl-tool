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
func PreflightURI(ctx context.Context, g *Guard, r Resolver, rawURI string) error {
	u, err := url.Parse(rawURI)
	if err != nil {
		// Unparseable input carries no scheme to govern; the normaliser and
		// router reject it before any engine is involved.
		return nil
	}

	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "ftp", "ftps", "sftp":
	default:
		return nil
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
