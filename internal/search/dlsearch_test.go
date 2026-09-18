package search

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/definitions"
)

// testScope is a fully populated scope for the Expand tests.
func testScope() Scope {
	return Scope{
		Keywords:   "ubuntu lts",
		Page:       2,
		Limit:      50,
		Offset:     50,
		Categories: []string{"linux", "iso"},
		Query:      map[string]string{"Q": "ubuntu lts", "Season": "S01"},
		Config:     map[string]string{"region": "eu"},
		Result:     map[string]string{"_id": "abc123"},
		TodayYear:  "2026",
	}
}

// TestExpandClosedSet exercises every placeholder head, both control
// constructs and all six functions of doc 07 section 3.3.
func TestExpandClosedSet(t *testing.T) {
	s := testScope()
	cases := []struct {
		tmpl string
		want string
	}{
		{"{{ .Keywords }}", "ubuntu lts"},
		{"p{{ .Page }}l{{ .Limit }}o{{ .Offset }}", "p2l50o50"},
		{"{{ .Query.Season }}/{{ .Query.Q }}", "S01/ubuntu lts"},
		{"{{ .Config.region }}", "eu"},
		{"{{ .Result._id }}", "abc123"},
		{"{{ .Today.Year }}", "2026"},
		{"{{ .Categories }}", "linux,iso"},
		{`{{ join .Categories "+" }}`, "linux+iso"},
		{`{{ join "+" .Categories }}`, "linux+iso"},
		{`{{ if .Keywords }}yes{{ else }}no{{ end }}`, "yes"},
		{`{{ if .Query.Ep }}yes{{ else }}no{{ end }}`, "no"},
		{`{{ if eq .Query.Season "S01" }}yes{{ else }}no{{ end }}`, "yes"},
		{`{{ if ne .Query.Season "S02" }}yes{{ else }}no{{ end }}`, "yes"},
		{`{{ if and .Keywords .Query.Season }}y{{ else }}n{{ end }}`, "y"},
		{`{{ if or .Query.Ep .Query.Season }}y{{ else }}n{{ end }}`, "y"},
		{`{{ if not .Query.Ep }}y{{ else }}n{{ end }}`, "y"},
		{`{{ range .Categories }}{{ . }};{{ end }}`, "linux;iso;"},
		{`{{ range .Categories }}c={{ . }} {{ end }}`, "c=linux c=iso "},
		{`{{ if .Keywords }}{{ if eq .Config.region "eu" }}match{{ else }}x{{ end }}{{ else }}none{{ end }}`, "match"},
		{`plain {{ "lit" }} and {{ ` + "`raw`" + ` }}`, "plain lit and raw"},
	}
	for _, tc := range cases {
		got, err := Expand(tc.tmpl, s)
		require.NoError(t, err, "template %q", tc.tmpl)
		assert.Equal(t, tc.want, got, "template %q", tc.tmpl)
	}
}

// TestExpandRejectsUnknownToken asserts that a token outside the closed set
// is an error naming the token — never a silent empty string.
func TestExpandRejectsUnknownToken(t *testing.T) {
	for _, tc := range []struct{ tmpl, token string }{
		{"{{ .Env.HOME }}", ".Env.HOME"},
		{`{{ printf "x" }}`, "printf"},
		{"{{ .Keywords.Foo }}", ".Keywords.Foo"},
		{"{{ .Query.Bogus }}", ".Query.Bogus"},
		{"{{ .Config }}", ".Config"},
		{`{{ if .Keywords }}yes{{ end }}`, "else"},
		{`{{ range .Query }}{{ end }}`, "range"},
		{"{{ . }}", "."},
	} {
		_, err := Expand(tc.tmpl, testScope())
		require.Error(t, err, "template %q must fail", tc.tmpl)
		assert.Contains(t, err.Error(), tc.token, "template %q: error must name the token", tc.tmpl)
	}
}

// TestExpandOutputCap covers the expansion limits of doc 07 section 3.5:
// 64 KiB of output, 1000 loop iterations and nesting depth 8.
func TestExpandOutputCap(t *testing.T) {
	// 1000 iterations of a 100-byte category exceed the output cap before the
	// loop cap.
	big := make([]string, 1000)
	for i := range big {
		big[i] = strings.Repeat("x", 100)
	}
	_, err := Expand("{{ range .Categories }}{{ . }}{{ end }}", Scope{Categories: big})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "65536")

	// 1001 iterations of one byte each stay under the output cap but trip the
	// loop cap.
	many := make([]string, 1001)
	for i := range many {
		many[i] = "x"
	}
	_, err = Expand("{{ range .Categories }}{{ . }}{{ end }}", Scope{Categories: many})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1000")

	// Nine nested ifs exceed the depth-8 cap.
	deep := strings.Repeat("{{ if .Keywords }}", 9) +
		strings.Repeat("{{ else }}{{ end }}", 9)
	_, err = Expand(deep, Scope{Keywords: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "8")
}

// TestTransformOpsTable covers all twelve ops of doc 07 section 3.4, and
// asserts an unknown op is an error rather than the input unchanged.
func TestTransformOpsTable(t *testing.T) {
	s := Scope{Config: map[string]string{"host": "https://x.test"}}
	cases := []struct {
		name string
		ops  []TransformOp
		in   string
		want string
	}{
		{"trim default", []TransformOp{{Op: "trim"}}, "  x\t ", "x"},
		{"trim cutset", []TransformOp{{Op: "trim", Args: []string{"-"}}}, "--x--", "x"},
		{"lower", []TransformOp{{Op: "lower"}}, "AbC", "abc"},
		{"upper", []TransformOp{{Op: "upper"}}, "aBc", "ABC"},
		{"html_decode", []TransformOp{{Op: "html_decode"}}, "a &amp; b &lt;ok&gt;", "a & b <ok>"},
		{"url_decode", []TransformOp{{Op: "url_decode"}}, "a%20b%2Fc", "a b/c"},
		{"prepend", []TransformOp{{Op: "prepend", Args: []string{"pre-"}}}, "v", "pre-v"},
		{"prepend template", []TransformOp{{Op: "prepend", Args: []string{"{{ .Config.host }}/"}}}, "v", "https://x.test/v"},
		{"append", []TransformOp{{Op: "append", Args: []string{"-post"}}}, "v", "v-post"},
		{"replace", []TransformOp{{Op: "replace", Args: []string{"a", "b"}}}, "banana", "bbnbnb"},
		{"regex_capture", []TransformOp{{Op: "regex_capture", Args: []string{`^t-(\d+)$`}}}, "t-42", "42"},
		{"regex_capture nomatch", []TransformOp{{Op: "regex_capture", Args: []string{`^t-(\d+)$`}}}, "nope", ""},
		{"split", []TransformOp{{Op: "split", Args: []string{",", "1"}}}, "a,b,c", "b"},
		{"split negative", []TransformOp{{Op: "split", Args: []string{",", "-1"}}}, "a,b,c", "c"},
		{"query_param", []TransformOp{{Op: "query_param", Args: []string{"id"}}}, "https://x.test/p?id=7&z=1", "7"},
		{"strip_html", []TransformOp{{Op: "strip_html"}}, "<p>hi</p>", "hi"},
		{"chained", []TransformOp{{Op: "trim"}, {Op: "lower"}}, "  AB ", "ab"},
	}
	for _, tc := range cases {
		got, err := ApplyTransforms(tc.in, tc.ops, s)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.want, got, tc.name)
	}

	_, err := ApplyTransforms("unchanged", []TransformOp{{Op: "bogus"}}, s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bogus")

	_, err = ApplyTransforms("a,b", []TransformOp{{Op: "split", Args: []string{",", "9"}}}, s)
	require.Error(t, err)
}

// loadTestDef parses a definition document through the real loader so tests
// exercise validation and field-order capture too.
func loadTestDef(t *testing.T, doc string) *Definition {
	t.Helper()
	def, err := LoadDefinition([]byte(doc))
	require.NoError(t, err)
	return def
}

// rssDef is the arch-linux fixture definition with its base_url pointed at
// the test server.
func rssDef(t *testing.T, base string) *Definition {
	t.Helper()
	def, err := LoadDefinition(readFixture(t, "def_valid_rss.yaml"))
	require.NoError(t, err)
	def.Request.BaseURL = base
	return def
}

// jsonDef mirrors the doc 07 section 3.7 internet-archive example with its
// base_url pointed at the test server.
func jsonDef(t *testing.T, base string) *Definition {
	t.Helper()
	def := loadTestDef(t, `
dlsearch: 1
id: internet-archive
name: Internet Archive
description: "test"
homepage: https://x.test/
version: "1.0.0"
legal_tier: legitimate
kind: json
caps:
  modes: {search: [q]}
  categories: {texts: 7000}
  seeders_unknown: true
settings:
  - name: title_only
    type: checkbox
    label: Search titles only
    default: true
request:
  base_url: `+base+`
  path: advancedsearch.php
  method: GET
  query:
    q: '{{ if .Config.title_only }}title:({{ .Keywords }}){{ else }}{{ .Keywords }}{{ end }} AND format:("Archive BitTorrent")'
    rows: "{{ .Limit }}"
    page: "{{ .Page }}"
    output: json
response:
  rows: "$.response.docs"
  total: "$.response.numFound"
  fields:
    _id:       {path: "identifier"}
    title:     {path: "title", optional: true, default: "Untitled {{ .Result._id }}"}
    size:      {path: "item_size", type: bytes}
    published: {path: "publicdate", type: datetime, format: iso8601}
    infohash:  {path: "btih", optional: true}
    details:   {template: "https://archive.org/details/{{ .Result._id }}"}
    download:  {template: "https://archive.org/download/{{ .Result._id }}/{{ .Result._id }}_archive.torrent"}
  transforms:
    title:
      - {op: trim}
      - {op: html_decode}
`)
	return def
}

// newTestRunner builds a Runner on the test server's own client — the SSRF
// guard is exercised by its own package's tests; here the injected client is
// the contract under test.
func newTestRunner(srv *httptest.Server) *Runner {
	return NewRunner(srv.Client(), nil, "dl-tool/test")
}

// TestRSSRowExtraction runs the arch-linux definition against the real feed
// fixture and checks the mapped row end to end: path and attr extraction,
// bytes and rfc1123 coercion, the const category mapping and
// seeders_unknown.
func TestRSSRowExtraction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write(readFixture(t, "archlinux_releases.xml"))
	}))
	defer srv.Close()

	def := rssDef(t, srv.URL)
	res, err := newTestRunner(srv).Search(context.Background(), def, nil, Query{Limit: 10})
	require.NoError(t, err)
	require.Len(t, res, 3)

	r := res[0]
	assert.Equal(t, "2026.09.01", r.Title)
	assert.Equal(t, "https://archlinux.org//releng/releases/2026.09.01/torrent/", r.DownloadURL)
	assert.Equal(t, "https://archlinux.org/releng/releases/2026.09.01/", r.DetailsURL)
	assert.Equal(t, int64(1608286208), r.SizeBytes)
	require.NotNil(t, r.PublishedAt)
	assert.Equal(t, "2026-09-01T00:00:00Z", *r.PublishedAt)
	assert.Equal(t, []int{4020}, r.CategoryIDs)
	assert.Nil(t, r.Seeders, "seeders_unknown must keep Seeders nil")
	assert.Nil(t, r.Leechers)
	assert.Equal(t, "arch-linux", r.EngineID)
	assert.Equal(t, 1.0, r.DownloadVolumeFactor)
}

// TestJSONRowExtraction runs the internet-archive definition against the
// real advancedsearch fixture: JSONPath rows, nested field extraction, the
// {{ .Result.<field> }} declaration-order dependency in the download
// template, and bytes/iso8601 coercion.
func TestJSONRowExtraction(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(readFixture(t, "archive_advancedsearch.json"))
	}))
	defer srv.Close()

	def := jsonDef(t, srv.URL)
	res, err := newTestRunner(srv).Search(context.Background(), def, nil, Query{Q: "euler", Limit: 2})
	require.NoError(t, err)
	require.Len(t, res, 2)

	// The request carried the expanded query: title:(euler) because the
	// title_only checkbox defaults true, plus the page math.
	assert.Equal(t, `title:(euler) AND format:("Archive BitTorrent")`, gotQuery.Get("q"))
	assert.Equal(t, "2", gotQuery.Get("rows"))
	assert.Equal(t, "1", gotQuery.Get("page"))

	r := res[1]
	assert.Equal(t, "Euler bio", r.Title)
	assert.Equal(t, "b8016106d0da596dbda06bbb325c4fdb4ac788b0", r.Infohash)
	assert.Equal(t, int64(33977), r.SizeBytes)
	assert.Equal(t, "https://archive.org/download/Euler_201701/Euler_201701_archive.torrent", r.DownloadURL)
	assert.Equal(t, "https://archive.org/details/Euler_201701", r.DetailsURL)
	require.NotNil(t, r.PublishedAt)
	assert.Equal(t, "2017-01-26T01:08:56Z", *r.PublishedAt)
	assert.Nil(t, r.Seeders)

	// The first row's non-ASCII title is pinned end to end through the
	// trim + html_decode chain.
	assert.Equal(t, "ผู้แทนเรื่อง", res[0].Title,
		"non-ASCII titles must survive the transform chain")
}

// TestBrowseEngineFiltersByKeyword: an engine whose request.query never
// references {{ .Keywords }} returns its whole feed and the keyword match
// runs in process, case-insensitively (doc 07 section 3.6).
func TestBrowseEngineFiltersByKeyword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(readFixture(t, "academic_torrents_rss.xml"))
	}))
	defer srv.Close()

	def := loadTestDef(t, `
dlsearch: 1
id: academic-torrents
name: Academic Torrents
description: "test"
homepage: https://x.test/
version: "1.0.0"
legal_tier: legitimate
kind: rss
caps:
  modes: {search: [q]}
  categories: {Dataset: 8000}
  seeders_unknown: true
request:
  base_url: `+srv.URL+`
  path: rss.xml
  method: GET
response:
  rows: "rss > channel > item"
  fields:
    title:    {path: "title"}
    _hash:    {path: "infohash"}
    infohash: {path: "infohash"}
    size:     {path: "size", type: bytes}
    download: {template: "https://academictorrents.com/download/{{ .Result._hash }}.torrent"}
`)

	res, err := newTestRunner(srv).Search(context.Background(), def, nil, Query{Q: "ALGORITHMIC"})
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, "Algorithmic Lower Bounds: Fun with Hardness Proofs (MIT 6.890) - Video lectures 2014", res[0].Title)

	// No query at all returns every row — the engine is a browse feed.
	res, err = newTestRunner(srv).Search(context.Background(), def, nil, Query{})
	require.NoError(t, err)
	assert.Len(t, res, 3)
}

// TestBodyCapEnforcedWhileStreaming asserts a response that declares 1 MiB
// but streams 9 MiB is refused: ReadCapped's LimitReader is the enforcement,
// not the declared length (doc 07 section 3.5).
func TestBodyCapEnforcedWhileStreaming(t *testing.T) {
	// A declared Content-Length alone would let the client stop at 1 MiB, so
	// the stub lies in the other direction: it declares 1 MiB but frames the
	// body as chunked, which overrides Content-Length per RFC 7230 and lets
	// all 9 MiB reach ReadCapped's LimitReader.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		body := make([]byte, 9<<20)
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1048576\r\nTransfer-Encoding: chunked\r\n\r\n")
		_, _ = fmt.Fprintf(rw, "%x\r\n", len(body))
		_, _ = rw.Write(body)
		_, _ = rw.WriteString("\r\n0\r\n\r\n")
		_ = rw.Flush()
	}))
	defer srv.Close()

	def := rssDef(t, srv.URL)
	_, err := newTestRunner(srv).Search(context.Background(), def, nil, Query{Limit: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

// TestProbeReportsUpstreamErrorAsData: a reachable engine answering 503 is a
// ProbeResult{Ok: false, Error: ...503...}, not a Go error — the Go error
// return is only for a probe that could not be attempted.
func TestProbeReportsUpstreamErrorAsData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("upstream maintenance"))
	}))
	defer srv.Close()

	def := rssDef(t, srv.URL)
	res, err := newTestRunner(srv).Probe(context.Background(), def, nil)
	require.NoError(t, err)
	assert.False(t, res.Ok)
	assert.Contains(t, res.Error, "503")
	assert.Contains(t, res.Error, "upstream maintenance")

	// A healthy endpoint probes ok and reports categories and Server header.
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "fixture/1.0")
		_, _ = w.Write(readFixture(t, "archlinux_releases.xml"))
	}))
	defer good.Close()

	res, err = newTestRunner(good).Probe(context.Background(), rssDef(t, good.URL), nil)
	require.NoError(t, err)
	assert.True(t, res.Ok)
	assert.Equal(t, 1, res.CategoriesFound)
	assert.Equal(t, "fixture/1.0", res.Server)
	assert.Empty(t, res.Error)
}

// TestRateLimitPerEngine asserts the per-engine token bucket rejects a
// second immediate request rather than sleeping past the deadline, and that
// a different engine id has its own bucket.
func TestRateLimitPerEngine(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write(readFixture(t, "archlinux_releases.xml"))
	}))
	defer srv.Close()

	def := rssDef(t, srv.URL)
	def.Request.RateLimitPerMinute = 30
	r := newTestRunner(srv)

	_, err := r.Search(context.Background(), def, nil, Query{Limit: 1})
	require.NoError(t, err)
	_, err = r.Search(context.Background(), def, nil, Query{Limit: 1})
	require.ErrorIs(t, err, ErrRateLimited)
	assert.Equal(t, 1, calls, "the refused call must not reach the server")

	// A second engine id has its own bucket. The fixture sets
	// rate_limit_per_minute, but pin it explicitly so the isolation check
	// stays meaningful if the fixture ever changes.
	other := rssDef(t, srv.URL)
	other.ID = "other-engine"
	other.Request.RateLimitPerMinute = 30
	_, err = r.Search(context.Background(), other, nil, Query{Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "the other engine's request must reach the server")
}

// TestAdmitClampsNonpositiveRate: a definition that somehow reaches the
// limiter with a zero or negative per-minute rate must take the default
// bucket, never divide by zero.
func TestAdmitClampsNonpositiveRate(t *testing.T) {
	r := NewRunner(nil, nil, "dl-tool/test")
	for _, rate := range []int{0, -5} {
		require.NoError(t, r.admit("e", rate))
		require.ErrorIs(t, r.admit("e", rate), ErrRateLimited,
			"rate %d: a bucket was created with the default interval", rate)
		r.mu.Lock()
		delete(r.limiter, "e")
		r.mu.Unlock()
	}
}

// TestDeadlineCoversParsing asserts the 15 s ceiling applies to the whole
// call — a hung server trips the context deadline even though the request
// timeout allows it.
func TestDeadlineCoversParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stall mid-body: the deadline must fire while the client is still
		// reading, not while the request is in flight — otherwise a read or
		// parse path that ignored ctx would pass this test.
		feed := readFixture(t, "archlinux_releases.xml")
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write(feed[:len(feed)/2])
		w.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write(feed[len(feed)/2:])
	}))
	defer srv.Close()

	def := rssDef(t, srv.URL)
	def.Request.TimeoutSeconds = 15
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := newTestRunner(srv).Search(ctx, def, nil, Query{Limit: 1})
	require.Error(t, err)
}

// TestTransformFieldOrder asserts fields resolve in declaration order: the
// download template reads .Result._hash, which is declared before it, and
// _-prefixed temporaries never reach the result.
func TestTransformFieldOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(readFixture(t, "academic_torrents_rss.xml"))
	}))
	defer srv.Close()

	def := loadTestDef(t, `
dlsearch: 1
id: at
name: AT
description: "test"
homepage: https://x.test/
version: "1.0.0"
legal_tier: legitimate
kind: rss
caps:
  modes: {search: [q]}
  categories: {Dataset: 8000}
  seeders_unknown: true
request:
  base_url: `+srv.URL+`
  path: rss.xml
response:
  rows: "rss > channel > item"
  fields:
    title:    {path: "title"}
    _hash:    {path: "infohash"}
    infohash: {path: "infohash"}
    size:     {path: "size", type: bytes}
    download: {template: "https://x.test/{{ .Result._hash }}.torrent"}
`)
	res, err := newTestRunner(srv).Search(context.Background(), def, nil, Query{})
	require.NoError(t, err)
	require.NotEmpty(t, res)
	assert.Equal(t, "https://x.test/f447784b18231897c892dbe45d943924583eab64.torrent", res[0].DownloadURL)
	assert.Equal(t, "f447784b18231897c892dbe45d943924583eab64", res[0].Infohash)
}

// TestExpandBadTemplatePins: an unclosed {{ or an empty action is an error
// at expansion, not a hang or a silent literal.
func TestExpandBadTemplatePins(t *testing.T) {
	_, err := Expand("{{ .Keywords", testScope())
	require.Error(t, err)
	_, err = Expand("{{ }}", testScope())
	require.Error(t, err)
}

// TestTransformOpsArgCounts: ApplyTransforms is exported and reachable
// without LoadDefinition's arity checks, so a short Args list is an error,
// never an index-out-of-range panic.
func TestTransformOpsArgCounts(t *testing.T) {
	for _, tc := range []struct {
		op   string
		args []string
	}{
		{"prepend", nil},
		{"append", nil},
		{"regex_capture", nil},
		{"query_param", nil},
		{"replace", nil},
		{"replace", []string{"a"}},
		{"split", nil},
		{"split", []string{","}},
	} {
		_, err := ApplyTransforms("v", []TransformOp{{Op: tc.op, Args: tc.args}}, Scope{})
		require.Error(t, err, "op %q with %d args must error, not panic", tc.op, len(tc.args))
	}
}

// TestRegexCaptureNeedsGroup: a groupless pattern is a load error, and the
// exported ApplyTransforms returns an error for one rather than panicking
// on m[1].
func TestRegexCaptureNeedsGroup(t *testing.T) {
	_, err := ApplyTransforms("abc123", []TransformOp{{Op: "regex_capture", Args: []string{`[0-9]+`}}}, Scope{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no capture group")

	_, err = LoadDefinition([]byte(`
dlsearch: 1
id: groupless
name: Groupless
description: "test"
homepage: https://x.test/
version: "1.0.0"
legal_tier: legitimate
kind: rss
caps:
  modes: {search: [q]}
  categories: {A: 1}
request:
  base_url: https://x.test/
response:
  rows: "rss > channel > item"
  fields:
    title:    {path: "title"}
    size:     {path: "size", type: bytes}
    download: {path: "download"}
    category: {const: "A"}
  transforms:
    title:
      - {op: regex_capture, args: ["[0-9]+"]}
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no capture group")

	// An oversized pattern is refused before it can fill the cache — the
	// exported path enforces the same cap the loader does.
	_, err = ApplyTransforms("v", []TransformOp{{Op: "regex_capture", Args: []string{strings.Repeat("a", MaxPatternBytes+1)}}}, Scope{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "512")
}

// TestURLDecodeKeepsPlus: url_decode is data-string percent decoding — a
// literal '+' survives and %41 decodes.
func TestURLDecodeKeepsPlus(t *testing.T) {
	got, err := ApplyTransforms("a+b%41", []TransformOp{{Op: "url_decode"}}, Scope{})
	require.NoError(t, err)
	assert.Equal(t, "a+bA", got)
}

// TestQueryParamBareQuery: a bare query string — no "?", so not a URL —
// resolves through ParseQuery instead of coming back silently empty.
func TestQueryParamBareQuery(t *testing.T) {
	got, err := ApplyTransforms("id=42&x=1", []TransformOp{{Op: "query_param", Args: []string{"id"}}}, Scope{})
	require.NoError(t, err)
	assert.Equal(t, "42", got)
}

// TestOrderedFieldsFallback: the recorded declaration order is trusted only
// while it covers every decoded field — a partial or foreign order falls
// back to sorted keys.
func TestOrderedFieldsFallback(t *testing.T) {
	f := Field{Path: "p"}
	decl := &Response{Fields: map[string]Field{"b": f, "a": f}, fieldOrder: []string{"b", "a"}}
	assert.Equal(t, []string{"b", "a"}, decl.OrderedFields())

	partial := &Response{Fields: map[string]Field{"b": f, "a": f}, fieldOrder: []string{"<<", "a"}}
	assert.Equal(t, []string{"a", "b"}, partial.OrderedFields())

	empty := &Response{Fields: map[string]Field{"a": f}, fieldOrder: []string{}}
	assert.Equal(t, []string{"a"}, empty.OrderedFields())
}

// TestXMLTreeKeepsFirstRoot: a trailing element after the real feed root —
// appended by a proxy or a concatenated reply — must not replace it.
func TestXMLTreeKeepsFirstRoot(t *testing.T) {
	root, err := parseXMLTree([]byte(`<rss><channel><item/></channel></rss><extra/>`))
	require.NoError(t, err)
	assert.Equal(t, "rss", root.name)
}

// TestUpstreamErrorOnLargeErrorBody: a non-2xx reply is an *UpstreamError
// even when its error page crosses the parsed-document cap, and a Retry-After
// on it still holds the engine's bucket.
func TestUpstreamErrorOnLargeErrorBody(t *testing.T) {
	big := make([]byte, 3<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	r := newTestRunner(srv)
	def := rssDef(t, srv.URL)
	_, err := r.Search(context.Background(), def, nil, Query{Limit: 1})
	var ue *UpstreamError
	require.ErrorAs(t, err, &ue)
	assert.Equal(t, http.StatusServiceUnavailable, ue.Status)

	// The Retry-After hold landed: next sits ~30 s out, well past the
	// fixture's 6-per-minute interval, and the next call is refused before
	// the wire.
	r.mu.Lock()
	next := r.limiter[def.ID].next
	r.mu.Unlock()
	assert.Greater(t, time.Until(next), 20*time.Second, "Retry-After must push the bucket past its interval")
	_, err = r.Search(context.Background(), def, nil, Query{Limit: 1})
	require.ErrorIs(t, err, ErrRateLimited)
}

// TestSearchKindHTML: Probe fetches an html definition but Search has no
// row extraction for it in v1 — the error says so explicitly.
func TestSearchKindHTML(t *testing.T) {
	def := rssDef(t, "https://x.test")
	def.Kind = "html"
	_, err := NewRunner(nil, nil, "dl-tool/test").Search(context.Background(), def, nil, Query{Limit: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not implemented")
}

// TestJSONPathStarLastSegment: [*] must close the path — a segment after it
// is a descriptive error, not silently empty fields.
func TestJSONPathStarLastSegment(t *testing.T) {
	doc := map[string]any{"items": []any{map[string]any{"name": "a"}}}
	_, err := jsonPath(doc, "$.items[*].name")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "last segment")

	v, err := jsonPath(doc, "$.items[*]")
	require.NoError(t, err)
	assert.NotNil(t, v)
}

// TestExtractJSONBadFieldPath: a syntactically valid field path that errors
// on a row — a data-shape mismatch — blanks only that field, so the row
// survives on its optional-field default and the search is not lost. A
// syntactically invalid path is a definition bug and fails the call.
func TestExtractJSONBadFieldPath(t *testing.T) {
	def := jsonDef(t, "https://x.test")
	def.Response.Fields["title"] = Field{Path: "response.oops", Optional: true, Default: "Untitled {{ .Result._id }}"}

	rows, skipped, err := extractJSON(context.Background(),
		[]byte(`{"response": {"docs": [{"identifier": "a", "item_size": 1, "publicdate": "2026-01-01"}]}}`),
		def, Scope{})
	require.NoError(t, err)
	assert.Equal(t, 0, skipped)
	require.Len(t, rows, 1)
	assert.Equal(t, "Untitled a", rows[0]["title"],
		"a traversal mismatch blanks the field; the default fills in")

	def.Response.Fields["title"] = Field{Path: "a[*].b"}
	_, _, err = extractJSON(context.Background(),
		[]byte(`{"response": {"docs": [{"identifier": "a"}]}}`),
		def, Scope{})
	require.Error(t, err, "a path-syntax error is a definition bug")
	assert.Contains(t, err.Error(), "field title")
}

// TestMapResultMagnetCase: the magnet: scheme check is case-insensitive per
// RFC 3986 — an uppercase scheme lands on MagnetURI, not DownloadURL.
func TestMapResultMagnetCase(t *testing.T) {
	def := &Definition{ID: "e"}
	r := mapResult(def, map[string]string{"download": "MAGNET:?xt=urn:btih:abc"})
	assert.Equal(t, "MAGNET:?xt=urn:btih:abc", r.MagnetURI)
	assert.Empty(t, r.DownloadURL)
}

// deadDialer fails every RoundTrip — a client that cannot dial at all.
type deadDialer struct{}

func (deadDialer) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial refused")
}

// staticDef parses the doc 07 section 3.8 worked-example fixture.
func staticDef(t *testing.T) *Definition {
	t.Helper()
	return loadTestDef(t, string(readFixture(t, "def_static.yaml")))
}

// TestStaticSearchMakesNoRequest: a kind: static search answers from
// entries[] in process — a client that cannot dial still returns every row
// (doc 07 section 3.8).
func TestStaticSearchMakesNoRequest(t *testing.T) {
	r := NewRunner(&http.Client{Transport: deadDialer{}}, nil, "dl-tool/test")
	res, err := r.Search(context.Background(), staticDef(t), nil, Query{})
	require.NoError(t, err)
	require.Len(t, res, 3)

	row := res[0]
	assert.Equal(t, "Ubuntu 24.04.4 LTS Desktop (amd64)", row.Title)
	assert.Equal(t, "https://releases.ubuntu.com/24.04/ubuntu-24.04.4-desktop-amd64.iso.torrent", row.DownloadURL)
	assert.Equal(t, "https://releases.ubuntu.com/24.04/", row.DetailsURL)
	assert.Equal(t, []int{4020}, row.CategoryIDs)
	assert.Equal(t, "iso", row.CategoryDesc)
	assert.Equal(t, int64(0), row.SizeBytes)
	assert.Equal(t, 1.0, row.DownloadVolumeFactor)
	assert.Equal(t, 1.0, row.UploadVolumeFactor)
	assert.Equal(t, "linux-distributions", row.EngineID)
}

// TestStaticKeywordFilterIsCaseInsensitive: the browse-style filter matches
// a case-insensitive substring of the entry title, and an empty query
// returns the whole list.
func TestStaticKeywordFilterIsCaseInsensitive(t *testing.T) {
	r := NewRunner(&http.Client{Transport: deadDialer{}}, nil, "dl-tool/test")
	def := staticDef(t)

	res, err := r.Search(context.Background(), def, nil, Query{Q: "DEBIAN"})
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, "Debian 13.6.0 netinst (amd64)", res[0].Title)

	res, err = r.Search(context.Background(), def, nil, Query{Q: "lts"})
	require.NoError(t, err)
	assert.Len(t, res, 2)

	res, err = r.Search(context.Background(), def, nil, Query{Q: "no-such-release"})
	require.NoError(t, err)
	assert.Empty(t, res)
}

// TestStaticSeedersAlwaysNull: a static entry carries no swarm counts, so
// Seeders and Leechers are nil for every row — unknown stays null, never a
// fabricated number (doc 07 section 5 rule 4).
func TestStaticSeedersAlwaysNull(t *testing.T) {
	r := NewRunner(&http.Client{Transport: deadDialer{}}, nil, "dl-tool/test")
	res, err := r.Search(context.Background(), staticDef(t), nil, Query{})
	require.NoError(t, err)
	require.NotEmpty(t, res)
	for _, row := range res {
		assert.Nil(t, row.Seeders, "row %q must have null seeders", row.Title)
		assert.Nil(t, row.Leechers, "row %q must have null leechers", row.Title)
		assert.Nil(t, row.Grabs)
	}
}

// TestStaticProbeReportsDeadURL: the static probe issues one HEAD per
// distinct download URL and reports each non-2xx answer with its status in
// Error — ok:false is data, not a Go error.
func TestStaticProbeReportsDeadURL(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		assert.Equal(t, http.MethodHead, r.Method)
		if r.URL.Path == "/gone.torrent" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	def := staticDef(t)
	dead := srv.URL + "/gone.torrent"
	live := srv.URL + "/dup.torrent"
	// Entries may share a URL; the probe de-duplicates before issuing HEADs.
	// The control-character URL cannot even form a request — it is reported
	// per-URL like a dial failure, not as a probe-level error.
	unbuildable := "https://exa\x7fmple.test/bad.torrent"
	def.Entries = []Entry{
		{Title: "dead", Download: dead, Category: "iso"},
		{Title: "dup a", Download: live, Category: "iso"},
		{Title: "dup b", Download: live, Category: "iso"},
		{Title: "unbuildable", Download: unbuildable, Category: "iso"},
	}

	res, err := newTestRunner(srv).Probe(context.Background(), def, nil)
	require.NoError(t, err)
	assert.False(t, res.Ok)
	assert.Contains(t, res.Error, dead)
	assert.Contains(t, res.Error, "404")
	assert.Contains(t, res.Error, unbuildable)
	assert.Equal(t, 2, hits, "the duplicated URL must be probed once")
}

// TestLinuxDistributionsDefinitionIsWellFormed is the refresh gate of doc 07
// section 3.9: every entry parses, every download URL is unique, https and
// ends in .torrent, and every category is a declared caps.categories key.
func TestLinuxDistributionsDefinitionIsWellFormed(t *testing.T) {
	data, err := definitions.FS.ReadFile("engines/linux-distributions.yaml")
	require.NoError(t, err)
	def, err := LoadDefinition(data)
	require.NoError(t, err)

	assert.Equal(t, "static", def.Kind)
	// Sscanf stops at the first non-numeric byte, so pin the exact x.y.z
	// shape first — "1.1.0-rc1" must not satisfy the bump guard.
	require.Regexp(t, `^\d+\.\d+\.\d+$`, def.Version)
	var vmajor, vminor, vpatch int
	_, err = fmt.Sscanf(def.Version, "%d.%d.%d", &vmajor, &vminor, &vpatch)
	require.NoError(t, err, "version %q must be x.y.z", def.Version)
	assert.True(t, vmajor > 1 || (vmajor == 1 && (vminor > 0 || vpatch > 0)),
		"a refresh must bump the version above the 1.0.0 T057 shipped, got %s", def.Version)
	assert.NotEmpty(t, def.RefreshNote)
	assert.Nil(t, def.Request, "a static definition carries no request block")
	assert.Nil(t, def.Response, "a static definition carries no response block")
	require.NotEmpty(t, def.Entries)

	seen := map[string]bool{}
	for i, e := range def.Entries {
		require.NotEmpty(t, e.Download, "entry %d needs a download URL", i)
		u, err := url.Parse(e.Download)
		require.NoError(t, err, "entry %d", i)
		assert.Equal(t, "https", u.Scheme, "entry %d", i)
		assert.True(t, strings.HasSuffix(u.Path, ".torrent"),
			"entry %d download %q must end in .torrent", i, e.Download)
		assert.False(t, seen[e.Download], "entry %d duplicates a download URL", i)
		seen[e.Download] = true
		_, ok := def.Caps.Categories[e.Category]
		assert.True(t, ok, "entry %d category %q is not a declared caps.categories key", i, e.Category)
	}
}
