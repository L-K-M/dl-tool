package rss

import (
	"bytes"
	"crypto/sha1"
	"encoding/base32"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/L-K-M/dl-tool/internal/store"
	"github.com/L-K-M/dl-tool/internal/uri"
)

// Tier names the branch of the extraction ladder that produced a URI, for
// tests and the dry run (docs/08-rss-automation.md section 3.1).
type Tier string

const (
	TierEnclosureOrMagnet Tier = "A" // x-bittorrent enclosure and magnet <link>: document order, LAST wins
	TierUntypedEnclosure  Tier = "B" // enclosure with an empty or absent type attribute
	TierSynthesised       Tier = "C" // torznab magneturl/infohash, <infohash>, BEP 36, media:*, atom enclosure
	TierLink              Tier = "D" // <link>, atom alternate href, then <guid> when isPermaLink != "false"
)

// enclosureTypeTorrent is the tier-A prefix test. qBittorrent compares the
// enclosure type with exact string equality, which drops
// "application/x-bittorrent;x-scheme-handler/magnet" items entirely;
// dl-tool deliberately does not copy that bug (doc 08 section 3.1).
const enclosureTypeTorrent = "application/x-bittorrent"

// downloadHashPattern recognises a v1 (40) or v2 (64) info-hash embedded in a
// URL — the doc 08 section 9 rule that turns a details-page link such as
// linuxtracker's ?id=<40-hex> into a download-looking link for tier D.
var downloadHashPattern = regexp.MustCompile(`(?i)\b[0-9a-f]{40}\b|\b[0-9a-f]{64}\b`)

// permissiveDateLayout is the non-RFC date Linuxtracker emits
// ("31/08/2026 18:47:19"), day-first with no zone; read as UTC.
const permissiveDateLayout = "02/01/2006 15:04:05"

// itemDateLayouts are tried before the permissive layout, in order.
var itemDateLayouts = []string{
	time.RFC1123Z,
	time.RFC1123,
	time.RFC822Z,
	time.RFC822,
	time.RFC3339,
}

// Parser decodes feed bodies with github.com/mmcdole/gofeed. The bytes are
// fetched by poll.go, never by this file: the timeout, the 16 MiB cap and the
// SSRF guard stay with the caller.
type Parser struct {
	now func() time.Time
}

// NewParser builds the feed parser; now substitutes for missing or
// unparsable item dates.
func NewParser(now func() time.Time) *Parser {
	if now == nil {
		now = time.Now
	}
	return &Parser{now: now}
}

// ParseFeed satisfies ItemParser. Items that resolve no download URI are
// dropped, not returned.
func (p *Parser) ParseFeed(feedID, baseURL string, body []byte) (FeedMeta, []store.FeedItem, error) {
	feed, err := gofeed.NewParser().Parse(bytes.NewReader(body))
	if err != nil {
		return FeedMeta{}, nil, fmt.Errorf("rss: decode feed body: %w", err)
	}

	// gofeed's universal Item loses the two things the ladder needs that
	// live outside its model: the document order between <link> and
	// <enclosure> inside an item (tier A's last-wins) and guid@isPermaLink
	// (tier D's guard). One raw pass over the same bytes recovers both per
	// item, aligned by index — gofeed aborts the whole parse on a malformed
	// item, so a successful Parse always yields the same item count.
	raw := scanFeedBody(body)

	seen := make(map[string]struct{}, len(feed.Items))
	items := make([]store.FeedItem, 0, len(feed.Items))
	for i, it := range feed.Items {
		probe := it
		if i < len(raw.items) {
			probe = adjustItem(it, raw.items[i])
		}

		downloadURI, infoHash, _, ok := ExtractDownloadURI(probe, baseURL)
		if !ok {
			continue
		}
		identity := Identity(feedID, it, downloadURI, infoHash)
		if identity == "" {
			continue
		}
		if _, dup := seen[identity]; dup {
			continue
		}
		seen[identity] = struct{}{}

		items = append(items, p.feedItem(feedID, it, probe, downloadURI, infoHash))
	}

	return feedMeta(feed, raw.channel), items, nil
}

// feedItem projects one resolved item into the feed_items row shape. The
// probe carries the tier-A/isPermaLink adjustments; the original item keeps
// guid and title for identity and display.
func (p *Parser) feedItem(feedID string, it, probe *gofeed.Item, downloadURI, infoHash string) store.FeedItem {
	title := strings.TrimSpace(it.Title)
	row := store.FeedItem{
		FeedID:      feedID,
		Identity:    Identity(feedID, it, downloadURI, infoHash),
		Title:       title,
		TitleNorm:   NormaliseTitle(title),
		DownloadURL: strPtr(downloadURI),
	}
	if g := strings.TrimSpace(it.GUID); g != "" {
		row.GUID = &g
	}
	if l := strings.TrimSpace(it.Link); l != "" {
		row.Link = &l
	}
	if infoHash != "" {
		row.InfoHash = &infoHash
	}
	if size, ok := itemSize(it, probe); ok {
		row.SizeBytes = &size
	}
	row.PublishedAt = ptrInt64(p.itemDate(feedID, it))

	return row
}

// itemDate resolves published_at: RFC dates first, the permissive
// Linuxtracker layout second, now on a second failure (doc 08 section 3.4).
func (p *Parser) itemDate(feedID string, it *gofeed.Item) int64 {
	raw := strings.TrimSpace(it.Published)
	if raw == "" {
		raw = strings.TrimSpace(it.Updated)
	}
	if ms, ok := ParseItemDate(raw); ok {
		return ms
	}
	slog.Debug("rss: item date unparsable, substituting now",
		"feed_id", feedID, "title", it.Title, "raw_date", raw)
	return p.now().UnixMilli()
}

// ExtractDownloadURI applies tiers A to D in order and returns the winning
// URI, the lowercase-hex info hash when one is known (40 chars for v1, 64 for
// v2), and the tier that produced it. ok is false when the item yields
// nothing and must be discarded.
//
// Inside tier A gofeed presents enclosures and links as two ordered lists
// with the interleaving lost; this function lets every magnet link write the
// slot after every x-bittorrent enclosure. ParseFeed restores exact document
// order by removing the losing magnet links before the call — standalone
// callers that build an Item by hand get the same net effect whenever the
// magnet link is meant to win.
func ExtractDownloadURI(it *gofeed.Item, baseURL string) (uriStr, infoHash string, tier Tier, ok bool) {
	if u, h, hit := tierA(it); hit {
		return u, h, TierEnclosureOrMagnet, true
	}
	if u := tierB(it); u != "" {
		return u, "", TierUntypedEnclosure, true
	}
	if u, h, hit := tierC(it); hit {
		return u, h, TierSynthesised, true
	}
	if u := tierD(it, baseURL); u != "" {
		return u, "", TierLink, true
	}
	return "", "", "", false
}

// tierA copies qBittorrent's parseRssArticle: an x-bittorrent enclosure and a
// magnet: <link> write the same slot, so the last document-order write wins.
// Enclosures are already ordered; every magnet link then writes after them.
func tierA(it *gofeed.Item) (uriStr, infoHash string, ok bool) {
	slot := ""
	for _, enc := range it.Enclosures {
		if enc == nil || strings.TrimSpace(enc.URL) == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(enc.Type)), enclosureTypeTorrent) {
			slot = strings.TrimSpace(enc.URL)
		}
	}
	for _, l := range itemLinks(it) {
		if isMagnetLink(l) {
			slot = strings.TrimSpace(l)
		}
	}
	if slot == "" {
		return "", "", false
	}
	return slot, hashFromMagnet(slot), true
}

// tierB takes the first enclosure whose type attribute is empty or absent.
func tierB(it *gofeed.Item) string {
	for _, enc := range it.Enclosures {
		if enc == nil || strings.TrimSpace(enc.URL) == "" {
			continue
		}
		if strings.TrimSpace(enc.Type) == "" {
			return strings.TrimSpace(enc.URL)
		}
	}
	return ""
}

// tierC runs the six synthesised sources of doc 08 section 3.1 in order;
// the first hit wins.
func tierC(it *gofeed.Item) (uriStr, infoHash string, ok bool) {
	// 1. torznab:attr[@name="magneturl"]/@value.
	if v := torznabAttr(it, "magneturl"); v != "" {
		return v, hashFromMagnet(v), true
	}
	// 2. torznab:attr[@name="infohash"]/@value.
	if h := NormaliseHash(torznabAttr(it, "infohash")); h != "" {
		return synthesiseMagnet(h, it.Title), h, true
	}
	// 3. An unprefixed <infohash> child (Academic Torrents): gofeed routes
	// unnamespaced unknown elements to Item.Custom; a prefixed variant would
	// surface as an extension element instead — check both by local name.
	if h := NormaliseHash(firstNonEmpty(customValue(it, "infohash"), extValue(it, "infohash"))); h != "" {
		return synthesiseMagnet(h, it.Title), h, true
	}
	// 4. BEP 36 <torrent>/<magneturi> or <torrent>/<infohash>. A prefixed
	// spelling lands in the extension map by local name; an unprefixed
	// <torrent> wrapper lands in Custom as raw inner text.
	if v := firstNonEmpty(extValue(it, "magneturi"), torrentChild(it, "magneturi")); v != "" {
		return v, hashFromMagnet(v), true
	}
	if h := NormaliseHash(torrentChild(it, "infohash")); h != "" {
		return synthesiseMagnet(h, it.Title), h, true
	}
	if v := strings.TrimSpace(customValue(it, "torrent")); v != "" {
		if isMagnetLink(v) {
			return v, hashFromMagnet(v), true
		}
		if h := NormaliseHash(v); h != "" {
			return synthesiseMagnet(h, it.Title), h, true
		}
	}
	// 5. media:content/@url ending in .torrent, or media:hash[@algo="sha1"].
	if u, h, hit := mediaContent(it); hit {
		return u, h, true
	}
	// 6. An Atom link[@rel="enclosure"]/@href whose type qualified it for
	// neither tier A nor tier B. gofeed folds atom enclosure links into
	// Item.Enclosures exactly like RSS enclosures, but unlike RSS it also
	// lists every entry link in Item.Links — membership there is what marks
	// an enclosure as atom-derived.
	for _, enc := range it.Enclosures {
		if enc == nil || strings.TrimSpace(enc.URL) == "" {
			continue
		}
		t := strings.TrimSpace(enc.Type)
		if t == "" || strings.HasPrefix(strings.ToLower(t), enclosureTypeTorrent) {
			continue
		}
		if slices.Contains(itemLinks(it), strings.TrimSpace(enc.URL)) {
			return strings.TrimSpace(enc.URL), "", true
		}
	}
	return "", "", false
}

// tierD is the last resort: <link> (RSS) / the atom alternate or first link,
// then <guid>. A candidate is only a download URI when it looks like one —
// a .torrent/.metalink path, a magnet, or a URL carrying an info-hash — so a
// plain details page (the doc 08 section 9 Gutenberg/LibriVox shape) yields
// nothing and the item is discarded (FR-072).
func tierD(it *gofeed.Item, baseURL string) string {
	for _, l := range itemLinks(it) {
		if resolved := resolveAgainst(strings.TrimSpace(l), baseURL); resolved != "" && looksLikeDownload(resolved) {
			return resolved
		}
	}
	if g := strings.TrimSpace(it.GUID); g != "" {
		if resolved := resolveAgainst(g, baseURL); resolved != "" && looksLikeDownload(resolved) {
			return resolved
		}
	}
	return ""
}

// itemLinks is every <link>/atom-link value of the item in document order,
// with Item.Link appended when the list missed it.
func itemLinks(it *gofeed.Item) []string {
	links := it.Links
	if it.Link != "" && !slices.Contains(links, it.Link) {
		links = append(slices.Clone(links), it.Link)
	}
	return links
}

// isMagnetLink reports whether s is a magnet: URI (case-insensitive scheme,
// leading space tolerated).
func isMagnetLink(s string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "magnet:")
}

// hashFromMagnet returns the info hash of a magnet URI: the v1 btih when
// present, else the v2 btmh — either width fits feed_items.info_hash.
func hashFromMagnet(raw string) string {
	if !isMagnetLink(raw) {
		return ""
	}
	m, err := uri.ParseMagnet(raw)
	if err != nil {
		return ""
	}
	if m.InfohashV1 != "" {
		return m.InfohashV1
	}
	return m.InfohashV2
}

// NormaliseHash renders a recovered hash as lowercase hex: 40 hex chars for
// v1, 64 for v2, and a 32-char base32 v1 value decoded to 20 bytes first
// (doc 06 section 3.5). A v2 hash is never truncated. "" means unrecoverable.
func NormaliseHash(s string) string {
	s = strings.TrimSpace(s)
	switch len(s) {
	case 40, 64:
		if _, err := hex.DecodeString(s); err == nil {
			return strings.ToLower(s)
		}
	case 32:
		if raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(s)); err == nil {
			return hex.EncodeToString(raw)
		}
	}
	return ""
}

// synthesiseMagnet builds the doc 08 section 3.1 magnet for a recovered
// hash: magnet:?xt=urn:btih:<hash>&dn=<url-encoded title>.
func synthesiseMagnet(hash, title string) string {
	return "magnet:?xt=urn:btih:" + hash + "&dn=" + url.QueryEscape(strings.TrimSpace(title))
}

// looksLikeDownload is the tier-D gate: a magnet, a .torrent/.metalink path,
// or a URL carrying a 40/64-hex info-hash.
func looksLikeDownload(raw string) bool {
	if isMagnetLink(raw) {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	path := strings.ToLower(u.Path)
	if strings.HasSuffix(path, ".torrent") || strings.HasSuffix(path, ".metalink") || strings.HasSuffix(path, ".meta4") {
		return true
	}
	return downloadHashPattern.MatchString(raw)
}

// resolveAgainst turns a feed-relative candidate into an absolute URI,
// resolved against baseURL; "" when the candidate is not a usable URI.
func resolveAgainst(raw, baseURL string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.IsAbs() {
		return u.String()
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return base.ResolveReference(u).String()
}

// torznabAttr finds the first torznab/newznab <attr name="…">/@value. gofeed
// keys extension elements by prefix and local name; scanning every prefix
// keeps the lookup on local name, the same rule torznab.go applies.
func torznabAttr(it *gofeed.Item, name string) string {
	for _, byName := range it.Extensions {
		for _, e := range byName["attr"] {
			if e.Attrs["name"] == name {
				return strings.TrimSpace(e.Attrs["value"])
			}
		}
	}
	return ""
}

// customValue reads an unprefixed unknown item child — gofeed stores those
// in Item.Custom keyed by the original-case element name, keeping the last
// occurrence.
func customValue(it *gofeed.Item, name string) string {
	for k, v := range it.Custom {
		if strings.EqualFold(k, name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// extValue returns the first extension element with the given local name,
// across every prefix.
func extValue(it *gofeed.Item, name string) string {
	for _, byName := range it.Extensions {
		for _, e := range byName[name] {
			if strings.TrimSpace(e.Value) != "" {
				return strings.TrimSpace(e.Value)
			}
		}
	}
	return ""
}

// torrentChild reads <torrent>/<child> whether the feed emits it as a
// prefixed extension element (torznab-style) or as a wrapper element whose
// children gofeed records in the extension's Children map.
func torrentChild(it *gofeed.Item, name string) string {
	for _, byName := range it.Extensions {
		for _, e := range byName["torrent"] {
			for _, child := range e.Children[name] {
				if strings.TrimSpace(child.Value) != "" {
					return strings.TrimSpace(child.Value)
				}
			}
		}
	}
	return ""
}

// mediaContent is tier C source 5: a media:content/@url ending in .torrent
// wins outright, else a media:hash[@algo="sha1"] child (or standalone
// element) synthesises a magnet.
func mediaContent(it *gofeed.Item) (uriStr, infoHash string, ok bool) {
	for _, byName := range it.Extensions {
		for _, e := range byName["content"] {
			if u := strings.TrimSpace(e.Attrs["url"]); strings.HasSuffix(strings.ToLower(u), ".torrent") {
				return u, "", true
			}
			for _, child := range e.Children["hash"] {
				if strings.EqualFold(strings.TrimSpace(child.Attrs["algo"]), "sha1") {
					if h := NormaliseHash(child.Value); h != "" {
						return synthesiseMagnet(h, it.Title), h, true
					}
				}
			}
		}
		for _, e := range byName["hash"] {
			if strings.EqualFold(strings.TrimSpace(e.Attrs["algo"]), "sha1") {
				if h := NormaliseHash(e.Value); h != "" {
					return synthesiseMagnet(h, it.Title), h, true
				}
			}
		}
	}
	return "", "", false
}

// itemSize resolves size_bytes: torznab attr size, then unprefixed <size>,
// then enclosure/@length — the last a hint only (doc 08 section 3.3).
func itemSize(it, probe *gofeed.Item) (int64, bool) {
	for _, raw := range []string{torznabAttr(it, "size"), customValue(it, "size"), extValue(it, "size")} {
		if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && n > 0 {
			return n, true
		}
	}
	for _, enc := range probe.Enclosures {
		if enc == nil {
			continue
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(enc.Length), 10, 64); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// Identity resolves feed_items.identity: <guid>/<id>, else the info hash,
// else the download URI, else hex(sha1(title + "\x00" + feedID)).
func Identity(feedID string, it *gofeed.Item, downloadURI, infoHash string) string {
	if g := strings.TrimSpace(it.GUID); g != "" {
		return g
	}
	if infoHash != "" {
		return infoHash
	}
	if downloadURI != "" {
		return downloadURI
	}
	sum := sha1.Sum([]byte(it.Title + "\x00" + feedID))
	return hex.EncodeToString(sum[:])
}

// NormaliseTitle lowercases, replaces '.' and '_' with a space, collapses
// whitespace runs and trims — the feed_items.title_norm rule of doc 08
// section 3.4.
func NormaliseTitle(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer(".", " ", "_", " ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// ParseItemDate tries RFC 1123/822 first, then the permissive form
// "02/01/2006 15:04:05" that Linuxtracker emits. ok is false when both fail
// and the caller substitutes now.
func ParseItemDate(raw string) (unixMS int64, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	for _, layout := range itemDateLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UnixMilli(), true
		}
	}
	if t, err := time.ParseInLocation(permissiveDateLayout, raw, time.UTC); err == nil {
		return t.UnixMilli(), true
	}
	return 0, false
}

// --- raw XML pass -------------------------------------------------------
//
// gofeed flattens each item into Link/Links and Enclosures lists, erasing
// the interleaving tier A needs and the guid@isPermaLink attribute tier D
// needs. scanFeedBody replays the same bytes through encoding/xml and
// records, per item (document order, aligned with gofeed's item order):
// which tier-A element wrote last, and whether the guid is a permalink.
// Channel-level publisher hints (ttl, sy:*, skipHours, skipDays) go to
// FeedMeta the same way — local-name matching throughout, so prefixed and
// unprefixed spellings land in the same place.

const (
	tierANone      = 0
	tierAEnclosure = 'e'
	tierAMagnet    = 'l'
)

type rawItemHints struct {
	lastTierA        byte
	guidNotPermaLink bool
}

type rawChannelHints struct {
	ttl       string
	skipHours []string
	skipDays  []string
	syPeriod  string
	syFreq    string
}

type rawScan struct {
	items   []rawItemHints
	channel rawChannelHints
}

// scanFeedBody walks the document once. Element names match by local name
// — an xmlns prefix makes no difference to the hints collected here.
func scanFeedBody(body []byte) rawScan {
	var res rawScan
	dec := xml.NewDecoder(bytes.NewReader(body))
	var stack []string
	var inItem bool
	var textEl string
	var text strings.Builder

	for {
		tok, err := dec.Token()
		if err != nil {
			// io.EOF ends a healthy walk; a malformed tail ends it early —
			// gofeed already judged the document, hints just stop there.
			return res
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := t.Name.Local
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			switch {
			case name == "item" || name == "entry":
				res.items = append(res.items, rawItemHints{})
				inItem = true
			case inItem && (parent == "item" || parent == "entry"):
				cur := &res.items[len(res.items)-1]
				switch name {
				case "enclosure":
					if strings.HasPrefix(strings.ToLower(strings.TrimSpace(attr(t, "type"))), enclosureTypeTorrent) {
						cur.lastTierA = tierAEnclosure
					}
				case "link":
					if parent == "entry" {
						// Atom links carry the URI in href; rel defaults to
						// "alternate" per RFC 4287.
						if strings.EqualFold(attr(t, "rel"), "enclosure") {
							if strings.HasPrefix(strings.ToLower(strings.TrimSpace(attr(t, "type"))), enclosureTypeTorrent) {
								cur.lastTierA = tierAEnclosure
							}
						} else if isMagnetLink(attr(t, "href")) {
							cur.lastTierA = tierAMagnet
						}
					} else {
						textEl, text = "link", strings.Builder{}
					}
				case "guid":
					if strings.EqualFold(strings.TrimSpace(attr(t, "isPermaLink")), "false") {
						cur.guidNotPermaLink = true
					}
				}
			case !inItem && parent == "channel":
				switch name {
				case "ttl", "updatePeriod", "updateFrequency":
					textEl, text = name, strings.Builder{}
				}
			case !inItem && parent == "skipHours" && name == "hour":
				textEl, text = "hour", strings.Builder{}
			case !inItem && parent == "skipDays" && name == "day":
				textEl, text = "day", strings.Builder{}
			}
			stack = append(stack, name)
		case xml.CharData:
			if textEl != "" {
				text.Write(t)
			}
		case xml.EndElement:
			if textEl == t.Name.Local {
				p := stackParent(stack)
				switch textEl {
				case "link":
					if inItem && isMagnetLink(text.String()) && len(res.items) > 0 {
						res.items[len(res.items)-1].lastTierA = tierAMagnet
					}
				case "ttl":
					res.channel.ttl = strings.TrimSpace(text.String())
				case "updatePeriod":
					res.channel.syPeriod = strings.TrimSpace(text.String())
				case "updateFrequency":
					res.channel.syFreq = strings.TrimSpace(text.String())
				case "hour":
					if p == "skipHours" {
						res.channel.skipHours = append(res.channel.skipHours, strings.TrimSpace(text.String()))
					}
				case "day":
					if p == "skipDays" {
						res.channel.skipDays = append(res.channel.skipDays, strings.TrimSpace(text.String()))
					}
				}
				textEl = ""
			}
			if t.Name.Local == "item" || t.Name.Local == "entry" {
				inItem = false
			}
			if n := len(stack); n > 0 {
				stack = stack[:n-1]
			}
		}
	}
}

// stackParent is the element enclosing the one the text accumulator just
// closed — stack still holds it because the pop happens after.
func stackParent(stack []string) string {
	if len(stack) < 2 {
		return ""
	}
	return stack[len(stack)-2]
}

func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// adjustItem rewrites the gofeed item into the shape ExtractDownloadURI
// needs so the pinned signature reproduces exact document order and the
// isPermaLink guard:
//   - when the last tier-A write is an x-bittorrent enclosure, every magnet
//     <link> lost the race and is removed from the probe;
//   - when guid isPermaLink="false", the guid cannot serve as tier D's link.
//
// The original item is untouched: Identity still reads its guid.
func adjustItem(it *gofeed.Item, h rawItemHints) *gofeed.Item {
	if h.lastTierA != tierAEnclosure && !h.guidNotPermaLink {
		return it
	}
	cp := *it
	if h.lastTierA == tierAEnclosure {
		links := make([]string, 0, len(it.Links))
		for _, l := range it.Links {
			if !isMagnetLink(l) {
				links = append(links, l)
			}
		}
		cp.Links = links
		if isMagnetLink(cp.Link) {
			cp.Link = ""
			if len(links) > 0 {
				cp.Link = links[len(links)-1]
			}
		}
	}
	if h.guidNotPermaLink {
		cp.GUID = ""
	}
	return &cp
}

// feedMeta fills the channel-level publisher hints of doc 08 section 2.2
// from the raw pass: <ttl> clamped to [5,1440] minutes, sy:updatePeriod /
// sy:updateFrequency as an implied interval, and the skip windows.
func feedMeta(feed *gofeed.Feed, ch rawChannelHints) FeedMeta {
	m := FeedMeta{Title: strings.TrimSpace(feed.Title)}

	if n, err := strconv.Atoi(ch.ttl); err == nil && n > 0 {
		m.TTLMinutes = min(max(n, 5), 1440)
	}

	if ch.syPeriod != "" || ch.syFreq != "" {
		seconds := map[string]int{
			"hourly": 3600, "daily": 86400, "weekly": 604800,
			"monthly": 2592000, "yearly": 31536000,
		}[strings.ToLower(ch.syPeriod)]
		if seconds == 0 {
			seconds = 86400 // omitted or unknown period defaults to daily
		}
		freq, err := strconv.Atoi(ch.syFreq)
		if err != nil || freq <= 0 {
			freq = 1
		}
		m.ImpliedIntervalS = seconds / freq
	}

	for _, h := range ch.skipHours {
		if n, err := strconv.Atoi(h); err == nil && n >= 0 && n <= 23 {
			m.SkipHours = append(m.SkipHours, n)
		}
	}
	weekdays := map[string]time.Weekday{
		"monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday,
		"thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday,
		"sunday": time.Sunday,
	}
	for _, d := range ch.skipDays {
		if w, ok := weekdays[strings.ToLower(d)]; ok {
			m.SkipDays = append(m.SkipDays, w)
		}
	}
	return m
}

// firstNonEmpty returns the first non-empty argument.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ptrInt64 boxes an int64 for the nullable published_at column.
func ptrInt64(v int64) *int64 { return &v }

// strPtr boxes a non-empty string for a nullable column.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

var _ ItemParser = (*Parser)(nil)
