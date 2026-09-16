// Package search loads, validates and runs dlsearch engine definitions.
// This file is the dlsearch/v1 loader: it turns one YAML document into a
// validated *Definition or into a *DefinitionError naming the offending key.
// It only parses — it never fetches and never executes anything
// (ADR-0010, docs/12-security-and-threat-model.md section 5).
package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Definition is one dlsearch/v1 document, 07-search-and-indexers.md section 3.1 field
// for field. Every struct field carries a yaml tag; the decoder runs with
// KnownFields(true), so an unknown key is an error.
type Definition struct {
	DLSearch            int       `yaml:"dlsearch"` // must equal 1
	ID                  string    `yaml:"id"`       // ^[a-z0-9][a-z0-9-]{1,63}$
	Name                string    `yaml:"name"`
	Description         string    `yaml:"description"`
	Homepage            string    `yaml:"homepage"`
	Kind                string    `yaml:"kind"` // torznab | rss | json | html | static
	Version             string    `yaml:"version"`
	LegalTier           string    `yaml:"legal_tier"` // legitimate | user-supplied
	Maintainer          string    `yaml:"maintainer"`
	LicenseNote         string    `yaml:"license_note"`
	AllowPrivateNetwork bool      `yaml:"allow_private_network"`
	Caps                DefCaps   `yaml:"caps"`
	Settings            []Setting `yaml:"settings"`
	Request             *Request  `yaml:"request"`
	Response            *Response `yaml:"response"`
	Entries             []Entry   `yaml:"entries"`
	RefreshNote         string    `yaml:"refresh_note"`
}

// DefCaps is the definition's own caps block. It is distinct from the Torznab Caps
// document parsed in torznab.go.
type DefCaps struct {
	Modes          map[string][]string `yaml:"modes"`      // "search" is mandatory
	Categories     map[string]int      `yaml:"categories"` // site value -> newznab id
	SeedersUnknown bool                `yaml:"seeders_unknown"`
}

type Setting struct {
	Name    string            `yaml:"name"`
	Type    string            `yaml:"type"` // info|text|password|checkbox|select
	Label   string            `yaml:"label"`
	Default string            `yaml:"default"`
	Options map[string]string `yaml:"options"`
}

type Request struct {
	BaseURL            string            `yaml:"base_url"`
	Path               string            `yaml:"path"`   // appended to base_url; a literal path, not a template (doc 07 section 3.1)
	Method             string            `yaml:"method"` // GET only in v1
	Query              map[string]string `yaml:"query"`
	Headers            map[string]string `yaml:"headers"`               // Authorization and Cookie are rejected
	RateLimitPerMinute int               `yaml:"rate_limit_per_minute"` // default 30, max 120
	TimeoutSeconds     int               `yaml:"timeout_seconds"`       // default 15, max 15
}

type Response struct {
	Rows       string                   `yaml:"rows"`
	Total      string                   `yaml:"total"`
	Fields     map[string]Field         `yaml:"fields"`
	Transforms map[string][]TransformOp `yaml:"transforms"`
}

// Field is exactly one of path, template or const, plus optional modifiers.
type Field struct {
	Path     string `yaml:"path"`
	Attr     string `yaml:"attr"`
	Template string `yaml:"template"`
	Const    string `yaml:"const"`
	Type     string `yaml:"type"`   // string|int|float|bytes|datetime
	Format   string `yaml:"format"` // iso8601|rfc1123|unix|relative|strptime:<fmt>
	Optional bool   `yaml:"optional"`
	Default  string `yaml:"default"` // requires optional: true
}

type TransformOp struct {
	Op   string   `yaml:"op"`
	Args []string `yaml:"args"`
}

// Entry is one curated record of a kind: static definition.
type Entry struct {
	Title     string `yaml:"title"`
	Download  string `yaml:"download"`
	Magnet    string `yaml:"magnet"`
	Infohash  string `yaml:"infohash"`
	Size      int64  `yaml:"size"`
	Category  string `yaml:"category"`
	Details   string `yaml:"details"`
	Published string `yaml:"published"`
}

// Limits enforced by LoadDefinition, from doc 07 section 3.5 and doc 12 section 5.1.
const (
	MaxDefinitionBytes = 512 << 10
	MaxNodeCount       = 50_000
	MaxNestingDepth    = 32
	MaxAliasExpansions = 1_000
	MaxParseDuration   = 2 * time.Second
	MaxPatternBytes    = 512
	MaxStaticEntries   = 500
	MaxRateLimit       = 120
	MaxTimeoutSeconds  = 30
)

// requestTimeoutMaxV1 is the v1 ceiling for request.timeout_seconds: it sits
// inside the 15 s per-engine deadline of doc 07 section 4 and can only lower it.
// The exported MaxTimeoutSeconds stays the looser outer bound the schema advertises.
const requestTimeoutMaxV1 = 15

const (
	defaultRateLimitPerMinute = 30
	defaultTimeoutSeconds     = 15
)

// Placeholders and TransformOps are the two closed sets. A token outside them is a
// load-time error; there is no fallthrough to text/template.
var Placeholders = []string{
	"Keywords", "Page", "Limit", "Offset", "Categories", "Query", "Config", "Result", "Today",
}
var TransformOps = []string{
	"trim", "lower", "upper", "html_decode", "url_decode", "prepend", "append",
	"replace", "regex_capture", "split", "query_param", "strip_html",
}

// DefinitionError names the limit or key that was violated, with the YAML line when the
// decoder supplied one.
type DefinitionError struct {
	Line int
	Path string // e.g. "response.fields.title.type"
	Msg  string
}

func (e *DefinitionError) Error() string {
	switch {
	case e.Line > 0 && e.Path != "":
		return fmt.Sprintf("dlsearch: line %d, %s: %s", e.Line, e.Path, e.Msg)
	case e.Line > 0:
		return fmt.Sprintf("dlsearch: line %d: %s", e.Line, e.Msg)
	case e.Path != "":
		return "dlsearch: " + e.Path + ": " + e.Msg
	default:
		return "dlsearch: " + e.Msg
	}
}

// LoadDefinition validates size, then decodes into Definition, then runs the field,
// kind, placeholder, op and limit rules. err is always a *DefinitionError.
func LoadDefinition(data []byte) (*Definition, error) {
	if len(data) > MaxDefinitionBytes {
		return nil, &DefinitionError{Msg: fmt.Sprintf("document is %d bytes, over the %d-byte limit", len(data), MaxDefinitionBytes)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), MaxParseDuration)
	defer cancel()

	type outcome struct {
		def *Definition
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		def, err := decodeAndValidate(data)
		done <- outcome{def, err}
	}()

	select {
	case <-ctx.Done():
		return nil, &DefinitionError{Msg: fmt.Sprintf("parse exceeded the %s wall-clock limit", MaxParseDuration)}
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		return r.def, nil
	}
}

// decodeAndValidate decodes the document twice: once into a yaml.Node tree for the
// tag, depth, node-count and alias caps (doc 12 section 5.1), then into the concrete
// struct under KnownFields. The node pass runs first so a hostile tag can never reach
// the typed decode.
func decodeAndValidate(data []byte) (*Definition, error) {
	var doc yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&doc); err != nil {
		return nil, yamlError(err)
	}
	if len(doc.Content) == 0 {
		return nil, &DefinitionError{Msg: "empty document"}
	}
	if err := checkNodes(&doc); err != nil {
		return nil, err
	}

	def := &Definition{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(def); err != nil {
		return nil, yamlError(err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, yamlError(err)
		}
		return nil, &DefinitionError{Line: extra.Line, Msg: "more than one YAML document"}
	}

	if err := validateDefinition(def, &doc); err != nil {
		return nil, err
	}
	return def, nil
}

var yamlLineRe = regexp.MustCompile(`line (\d+)`)

// yamlError wraps a decoder error; the "line N:" prefix the decoder reports is
// lifted into Line so callers get a uniform *DefinitionError.
func yamlError(err error) *DefinitionError {
	de := &DefinitionError{Msg: err.Error()}
	if m := yamlLineRe.FindStringSubmatch(err.Error()); m != nil {
		if n, convErr := strconv.Atoi(m[1]); convErr == nil {
			de.Line = n
		}
	}
	return de
}

// allowedNodeTags is the doc 12 section 5.1 tag allowlist: anything else —
// !!timestamp, !!binary, !!merge, a !local tag or a language-specific tag such as
// !!python/object/new — is rejected before the typed decode runs.
var allowedNodeTags = map[string]bool{
	"!!str": true, "!!int": true, "!!bool": true,
	"!!float": true, "!!map": true, "!!seq": true, "!!null": true,
}

type nodeBudget struct {
	nodes   int
	aliases int
}

// checkNodes walks the whole tree, counting nodes, nesting depth and alias
// expansions against their caps and rejecting any tag outside allowedNodeTags.
// Alias targets are re-walked so the counts reflect the expanded document the
// typed decode would produce — that is what makes billion-laughs payloads hit
// MaxAliasExpansions or MaxNodeCount before they hit memory.
func checkNodes(root *yaml.Node) error {
	b := &nodeBudget{}
	return b.walk(root, 0)
}

func (b *nodeBudget) walk(n *yaml.Node, depth int) error {
	b.nodes++
	if b.nodes > MaxNodeCount {
		return &DefinitionError{Line: n.Line, Msg: fmt.Sprintf("document has more than %d nodes", MaxNodeCount)}
	}
	if depth > MaxNestingDepth {
		return &DefinitionError{Line: n.Line, Msg: fmt.Sprintf("nesting deeper than %d levels", MaxNestingDepth)}
	}
	if n.Kind == yaml.AliasNode {
		b.aliases++
		if b.aliases > MaxAliasExpansions {
			return &DefinitionError{Line: n.Line, Msg: fmt.Sprintf("more than %d alias expansions", MaxAliasExpansions)}
		}
		if n.Alias == nil {
			return &DefinitionError{Line: n.Line, Msg: "alias without a target"}
		}
		return b.walk(n.Alias, depth+1)
	}
	if n.Kind != yaml.DocumentNode && !allowedNodeTags[n.Tag] {
		return &DefinitionError{Line: n.Line, Msg: fmt.Sprintf("unsupported YAML tag %q", n.Tag)}
	}
	for _, c := range n.Content {
		if err := b.walk(c, depth+1); err != nil {
			return err
		}
	}
	return nil
}

var (
	defIDRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,63}$`)
	infohashRe    = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	settingNameRe = regexp.MustCompile(`^[\pL_][\pL\pN_]*$`)
	strptimePfx   = "strptime:"
	defKinds      = map[string]bool{"torznab": true, "rss": true, "json": true, "html": true, "static": true}
	defLegalTier  = map[string]bool{"legitimate": true, "user-supplied": true}
	defCapModes   = map[string]bool{"search": true, "tv-search": true, "movie-search": true, "music-search": true, "book-search": true}
	settingTypes  = map[string]bool{"info": true, "text": true, "password": true, "checkbox": true, "select": true}
	fieldTypes    = map[string]bool{"string": true, "int": true, "float": true, "bytes": true, "datetime": true}
	queryFields   = map[string]bool{
		"Q": true, "Season": true, "Ep": true, "Year": true, "Genre": true,
		"IMDBID": true, "IMDBIDShort": true, "TMDBID": true, "TVDBID": true, "TVMazeID": true,
		"Artist": true, "Album": true, "Label": true, "Track": true,
		"Author": true, "Title": true, "Publisher": true,
	}
	templateFuncs  = map[string]bool{"join": true, "eq": true, "ne": true, "and": true, "or": true, "not": true}
	placeholderSet = func() map[string]bool {
		m := make(map[string]bool, len(Placeholders))
		for _, p := range Placeholders {
			m[p] = true
		}
		return m
	}()
	transformOpSet = func() map[string]bool {
		m := make(map[string]bool, len(TransformOps))
		for _, op := range TransformOps {
			m[op] = true
		}
		return m
	}()
)

// checkCtx carries the declared settings and field names so .Config.<setting> and
// .Result.<field> references can be resolved against the definition itself.
type checkCtx struct {
	root       *yaml.Node
	settings   map[string]bool
	fields     map[string]bool
	categories map[string]int
}

func validateDefinition(d *Definition, root *yaml.Node) error {
	cx := &checkCtx{root: root}

	if d.DLSearch != 1 {
		return cx.fail("dlsearch", "must equal 1, got %d", d.DLSearch)
	}
	if !defIDRe.MatchString(d.ID) {
		return cx.fail("id", "must match ^[a-z0-9][a-z0-9-]{1,63}$, got %q", d.ID)
	}
	for _, req := range []struct {
		path, val string
	}{
		{"name", d.Name},
		{"description", d.Description},
		{"homepage", d.Homepage},
		{"version", d.Version},
		{"legal_tier", d.LegalTier},
		{"kind", d.Kind},
	} {
		if req.val == "" {
			return cx.fail(req.path, "is required")
		}
	}
	if err := cx.checkURL(d.Homepage, "homepage", "http", "https"); err != nil {
		return err
	}
	if !defKinds[d.Kind] {
		return cx.fail("kind", "must be one of torznab, rss, json, html, static, got %q", d.Kind)
	}
	if !defLegalTier[d.LegalTier] {
		return cx.fail("legal_tier", "must be legitimate or user-supplied, got %q", d.LegalTier)
	}

	if err := cx.checkCaps(&d.Caps); err != nil {
		return err
	}
	cx.settings = make(map[string]bool, len(d.Settings))
	for i, s := range d.Settings {
		path := fmt.Sprintf("settings.%d", i)
		if s.Name == "" {
			return cx.fail(path+".name", "is required")
		}
		// The name must be referenceable as a single .Config.<name> member, so
		// it has to be an identifier text/template accepts after a dot.
		if !settingNameRe.MatchString(s.Name) {
			return cx.fail(path+".name", "%q is not a usable .Config member name", s.Name)
		}
		if cx.settings[s.Name] {
			return cx.fail(path+".name", "duplicate setting name %q", s.Name)
		}
		cx.settings[s.Name] = true
		if !settingTypes[s.Type] {
			return cx.fail(path+".type", "must be one of info, text, password, checkbox, select, got %q", s.Type)
		}
		if s.Type == "select" && len(s.Options) == 0 {
			return cx.fail(path+".options", "is required when type is select")
		}
	}
	if d.Response != nil {
		cx.fields = make(map[string]bool, len(d.Response.Fields))
		for name := range d.Response.Fields {
			cx.fields[name] = true
		}
	}

	if err := cx.checkKind(d); err != nil {
		return err
	}
	if d.Request != nil {
		if err := cx.checkRequest(d.Request); err != nil {
			return err
		}
	}
	if d.Response != nil {
		if err := cx.checkFields(d.Response.Fields); err != nil {
			return err
		}
		if err := cx.checkTransforms(d.Response.Transforms); err != nil {
			return err
		}
	}
	if len(d.Entries) > 0 {
		if err := cx.checkEntries(d); err != nil {
			return err
		}
	}
	return nil
}

// checkCaps applies the ubiquitous caps rules of doc 07 section 3.1: modes comes
// from a closed key set with "search" mandatory, and every categories value is a
// positive newznab id.
func (cx *checkCtx) checkCaps(c *DefCaps) error {
	if len(c.Modes) == 0 {
		return cx.fail("caps.modes", "is required and must contain \"search\"")
	}
	for _, mode := range sortedKeys(c.Modes) {
		if !defCapModes[mode] {
			return cx.fail("caps.modes."+mode, "unknown mode %q", mode)
		}
	}
	if _, ok := c.Modes["search"]; !ok {
		return cx.fail("caps.modes", "must contain \"search\"")
	}
	if len(c.Categories) == 0 {
		return cx.fail("caps.categories", "is required")
	}
	cx.categories = c.Categories
	for _, name := range sortedKeys(c.Categories) {
		if c.Categories[name] <= 0 {
			return cx.fail("caps.categories."+name, "newznab id must be a positive integer, got %d", c.Categories[name])
		}
	}
	return nil
}

// checkKind applies the per-kind block rules of doc 07 section 3.2.
func (cx *checkCtx) checkKind(d *Definition) error {
	switch d.Kind {
	case "torznab", "static":
		if d.Request != nil {
			return cx.fail("request", "is forbidden for kind %q", d.Kind)
		}
		if d.Response != nil {
			return cx.fail("response", "is forbidden for kind %q", d.Kind)
		}
	case "rss", "json", "html":
		if d.Request == nil {
			return cx.fail("request", "is required for kind %q", d.Kind)
		}
		if d.Request.BaseURL == "" {
			return cx.fail("request.base_url", "is required for kind %q", d.Kind)
		}
		if d.Response == nil {
			return cx.fail("response", "is required for kind %q", d.Kind)
		}
		if d.Response.Rows == "" {
			return cx.fail("response.rows", "is required for kind %q", d.Kind)
		}
		if len(d.Response.Fields) == 0 {
			return cx.fail("response.fields", "is required for kind %q", d.Kind)
		}
	}
	if d.Kind == "static" {
		if len(d.Entries) == 0 {
			return cx.fail("entries", "is required for kind \"static\"")
		}
		if d.RefreshNote == "" {
			return cx.fail("refresh_note", "is required for kind \"static\"")
		}
	} else if len(d.Entries) > 0 {
		return cx.fail("entries", "is forbidden for kind %q; only \"static\" carries entries", d.Kind)
	}
	return nil
}

// checkRequest applies the step-9 request rules: GET only, the rate-limit and
// timeout ceilings, the credential-header ban, and template validation of every
// query and header value. Absent limits take their documented defaults.
func (cx *checkCtx) checkRequest(r *Request) error {
	if r.Method == "" {
		r.Method = "GET"
	}
	if r.Method != "GET" {
		return cx.fail("request.method", "must be GET in dlsearch/v1, got %q", r.Method)
	}
	if r.BaseURL != "" {
		if err := cx.checkURL(r.BaseURL, "request.base_url", "http", "https"); err != nil {
			return err
		}
	}
	if r.RateLimitPerMinute < 0 {
		return cx.fail("request.rate_limit_per_minute", "must not be negative, got %d", r.RateLimitPerMinute)
	}
	if r.RateLimitPerMinute == 0 {
		r.RateLimitPerMinute = defaultRateLimitPerMinute
	}
	if r.RateLimitPerMinute > MaxRateLimit {
		return cx.fail("request.rate_limit_per_minute", "must be at most %d, got %d", MaxRateLimit, r.RateLimitPerMinute)
	}
	if r.TimeoutSeconds < 0 {
		return cx.fail("request.timeout_seconds", "must not be negative, got %d", r.TimeoutSeconds)
	}
	if r.TimeoutSeconds == 0 {
		r.TimeoutSeconds = defaultTimeoutSeconds
	}
	if r.TimeoutSeconds > requestTimeoutMaxV1 {
		return cx.fail("request.timeout_seconds", "must be at most %d, got %d", requestTimeoutMaxV1, r.TimeoutSeconds)
	}
	for _, name := range sortedKeys(r.Headers) {
		if strings.EqualFold(name, "authorization") || strings.EqualFold(name, "cookie") {
			return cx.fail("request.headers."+name, "the %s header is not allowed in a definition", name)
		}
		if strings.IndexFunc(name, func(c rune) bool { return c <= 0x20 || c == 0x7f }) >= 0 {
			return cx.fail("request.headers."+name, "must not contain whitespace or control characters")
		}
		if strings.ContainsAny(r.Headers[name], "\r\n\x00") {
			return cx.fail("request.headers."+name, "must not contain control characters")
		}
	}
	for _, key := range sortedKeys(r.Query) {
		if err := cx.checkTemplate(r.Query[key], "request.query."+key); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(r.Headers) {
		if err := cx.checkTemplate(r.Headers[name], "request.headers."+name); err != nil {
			return err
		}
	}
	return nil
}

// checkFields applies the response.fields rules of doc 07 section 3.1: exactly
// one source per field, the required-field set, the optional/default coupling and
// the datetime format list.
func (cx *checkCtx) checkFields(fields map[string]Field) error {
	for _, name := range sortedKeys(fields) {
		f := fields[name]
		path := "response.fields." + name
		sources := 0
		for _, s := range []string{f.Path, f.Template, f.Const} {
			if s != "" {
				sources++
			}
		}
		if sources != 1 {
			return cx.fail(path, "must set exactly one of path, template or const")
		}
		if f.Attr != "" && f.Path == "" {
			return cx.fail(path+".attr", "requires path")
		}
		if f.Type != "" && !fieldTypes[f.Type] {
			return cx.fail(path+".type", "must be one of string, int, float, bytes, datetime, got %q", f.Type)
		}
		if f.Type == "datetime" {
			if !validDatetimeFormat(f.Format) {
				return cx.fail(path+".format", "datetime needs one of iso8601, rfc1123, unix, relative, strptime:<fmt>, got %q", f.Format)
			}
		} else if f.Format != "" {
			return cx.fail(path+".format", "is only valid with type datetime")
		}
		if f.Default != "" && !f.Optional {
			return cx.fail(path+".default", "requires optional: true")
		}
	}
	for _, req := range []string{"title", "size"} {
		if _, ok := fields[req]; !ok {
			return cx.fail("response.fields", "missing required field %q", req)
		}
	}
	if _, ok := fields["download"]; !ok {
		if _, ok := fields["magnet"]; !ok {
			if _, ok := fields["infohash"]; !ok {
				return cx.fail("response.fields", "needs at least one of download, magnet, infohash")
			}
		}
	}
	if _, ok := fields["category"]; !ok && len(cx.categories) != 1 {
		return cx.fail("response.fields", "missing required field \"category\" (caps.categories does not pin a single id)")
	}
	for _, name := range sortedKeys(fields) {
		f := fields[name]
		path := "response.fields." + name
		if f.Template != "" {
			if err := cx.checkTemplate(f.Template, path+".template"); err != nil {
				return err
			}
		}
		if f.Default != "" {
			if err := cx.checkTemplate(f.Default, path+".default"); err != nil {
				return err
			}
		}
	}
	return nil
}

func validDatetimeFormat(f string) bool {
	switch f {
	case "iso8601", "rfc1123", "unix", "relative":
		return true
	}
	return strings.HasPrefix(f, strptimePfx) && len(f) > len(strptimePfx)
}

// checkTransforms applies the step-8 op rules: the op set is closed, arities are
// fixed per doc 07 section 3.4, and every regex_capture pattern must compile under
// RE2 within MaxPatternBytes.
func (cx *checkCtx) checkTransforms(transforms map[string][]TransformOp) error {
	for _, field := range sortedKeys(transforms) {
		if !cx.fields[field] {
			return cx.fail("response.transforms."+field, "targets a field not declared in response.fields")
		}
		for i, op := range transforms[field] {
			path := fmt.Sprintf("response.transforms.%s.%d", field, i)
			if !transformOpSet[op.Op] {
				return cx.fail(path+".op", "unknown transform op %q", op.Op)
			}
			if err := cx.checkOpArgs(op, path); err != nil {
				return err
			}
		}
	}
	return nil
}

func (cx *checkCtx) checkOpArgs(op TransformOp, path string) error {
	need := func(n int) error {
		if len(op.Args) != n {
			return cx.fail(path+".args", "op %q takes exactly %d args, got %d", op.Op, n, len(op.Args))
		}
		return nil
	}
	switch op.Op {
	case "lower", "upper", "html_decode", "url_decode", "strip_html":
		return need(0)
	case "trim":
		if len(op.Args) > 1 {
			return cx.fail(path+".args", "op \"trim\" takes at most one arg, got %d", len(op.Args))
		}
	case "prepend", "append":
		if err := need(1); err != nil {
			return err
		}
		// prepend/append args are template-expanded at run time (doc 07 section
		// 3.4), so they are template strings and must stay inside the closed set.
		return cx.checkTemplate(op.Args[0], path+".args.0")
	case "query_param":
		return need(1)
	case "replace":
		return need(2)
	case "split":
		if err := need(2); err != nil {
			return err
		}
		if _, err := strconv.Atoi(op.Args[1]); err != nil {
			return cx.fail(path+".args", "op \"split\" needs an integer index, got %q", op.Args[1])
		}
	case "regex_capture":
		if err := need(1); err != nil {
			return err
		}
		if len(op.Args[0]) > MaxPatternBytes {
			return cx.fail(path+".args", "regex_capture pattern is %d bytes, over the %d-byte limit", len(op.Args[0]), MaxPatternBytes)
		}
		if _, err := regexp.Compile(op.Args[0]); err != nil {
			return cx.fail(path+".args", "regex_capture pattern does not compile: %v", err)
		}
	}
	return nil
}

// checkEntries applies the step-10 curated-record rules of doc 07 section 3.8.
func (cx *checkCtx) checkEntries(d *Definition) error {
	if len(d.Entries) > MaxStaticEntries {
		return cx.fail("entries", "has %d records, over the %d-entry limit", len(d.Entries), MaxStaticEntries)
	}
	for i, e := range d.Entries {
		path := fmt.Sprintf("entries.%d", i)
		if e.Title == "" {
			return cx.fail(path+".title", "is required")
		}
		if e.Category == "" {
			return cx.fail(path+".category", "is required")
		}
		if _, ok := d.Caps.Categories[e.Category]; !ok {
			return cx.fail(path+".category", "%q is not a declared caps.categories key", e.Category)
		}
		if e.Download == "" && e.Magnet == "" && e.Infohash == "" {
			return cx.fail(path, "needs at least one of download, magnet, infohash")
		}
		if e.Download != "" {
			if err := cx.checkURL(e.Download, path+".download", "https"); err != nil {
				return err
			}
		}
		if e.Details != "" {
			if err := cx.checkURL(e.Details, path+".details", "https"); err != nil {
				return err
			}
		}
		if e.Magnet != "" && !strings.HasPrefix(strings.ToLower(e.Magnet), "magnet:") {
			return cx.fail(path+".magnet", "must use the magnet: scheme")
		}
		if e.Infohash != "" && !infohashRe.MatchString(e.Infohash) {
			return cx.fail(path+".infohash", "must be 40 or 64 lowercase hex characters")
		}
	}
	return nil
}

func (cx *checkCtx) checkURL(raw, path string, schemes ...string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return cx.fail(path, "is not a valid URL: %v", err)
	}
	if u.User != nil {
		return cx.fail(path, "must not embed credentials (userinfo) in a URL; secrets belong in a password setting")
	}
	for _, s := range schemes {
		if u.Scheme == s {
			if u.Host == "" {
				return cx.fail(path, "has scheme %q but no host", u.Scheme)
			}
			return nil
		}
	}
	return cx.fail(path, "scheme must be %s, got %q", strings.Join(schemes, " or "), u.Scheme)
}

// templateBlock is one open {{ if }} or {{ range }} while scanning a template.
type templateBlock struct {
	kind    string // "if" | "range"
	sawElse bool
}

// checkTemplate validates one template string against the closed sets of doc 07
// section 3.3: {{ ... }} actions are tokenised, every action head must be a
// control word, a function or a placeholder, {{ if }} requires an {{ else }},
// and {{ range }} only ever walks .Categories.
func (cx *checkCtx) checkTemplate(tpl, path string) error {
	var stack []templateBlock
	rest := tpl
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			break
		}
		end := actionEnd(rest[i+2:])
		if end < 0 {
			return cx.fail(path, "unclosed \"{{\"")
		}
		action := rest[i+2 : i+2+end]
		rest = rest[i+2+end+2:]

		tokens, err := lexTemplate(action)
		if err != nil {
			return cx.fail(path, "%v", err)
		}
		if len(tokens) == 0 {
			return cx.fail(path, "empty {{ }} action")
		}
		inRange := false
		for _, b := range stack {
			if b.kind == "range" {
				inRange = true
			}
		}
		switch tokens[0] {
		case "if":
			if len(tokens) < 2 {
				return cx.fail(path, "{{ if }} needs a condition")
			}
			if err := cx.checkExpr(tokens[1:], path, inRange); err != nil {
				return err
			}
			stack = append(stack, templateBlock{kind: "if"})
		case "else":
			if len(tokens) != 1 {
				return cx.fail(path, "{{ else }} takes no arguments")
			}
			if len(stack) == 0 || stack[len(stack)-1].kind != "if" {
				return cx.fail(path, "{{ else }} outside an {{ if }} block")
			}
			if stack[len(stack)-1].sawElse {
				return cx.fail(path, "duplicate {{ else }}")
			}
			stack[len(stack)-1].sawElse = true
		case "end":
			if len(tokens) != 1 {
				return cx.fail(path, "{{ end }} takes no arguments")
			}
			if len(stack) == 0 {
				return cx.fail(path, "{{ end }} without a block")
			}
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if top.kind == "if" && !top.sawElse {
				return cx.fail(path, "{{ if }} without the mandatory {{ else }}")
			}
		case "range":
			if len(tokens) != 2 || tokens[1] != ".Categories" {
				return cx.fail(path, "{{ range }} may only walk .Categories")
			}
			if inRange {
				return cx.fail(path, "{{ range }} may not nest inside another {{ range }}")
			}
			stack = append(stack, templateBlock{kind: "range"})
		default:
			if err := cx.checkExpr(tokens, path, inRange); err != nil {
				return err
			}
		}
	}
	if len(stack) > 0 {
		return cx.fail(path, "unclosed {{ %s }} block", stack[len(stack)-1].kind)
	}
	return nil
}

// actionEnd returns the offset of the "}}" that closes the action starting at
// s, skipping over double-quoted literals and their backslash escapes and
// backquoted raw literals so a literal containing "}}" cannot terminate the
// action early. -1 when the action never closes — including an unterminated
// literal, which can never close one.
func actionEnd(s string) int {
	for i := 0; i+1 < len(s); i++ {
		switch s[i] {
		case '`':
			for i++; i < len(s) && s[i] != '`'; i++ {
			}
			if i >= len(s) {
				return -1
			}
		case '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' {
					i++
				}
			}
			if i >= len(s) {
				return -1
			}
		case '}':
			if s[i+1] == '}' {
				return i
			}
		}
	}
	return -1
}

// lexTemplate splits one action body into tokens, keeping double-quoted and
// backquoted literals whole. "-" trim markers are stripped only where
// text/template accepts them:
// adjacent to the delimiter and separated from the expression by whitespace, so
// `{{- x -}}` is trimmed but `{{ x-}}` or `{{ x - }}` surface as bad tokens.
func lexTemplate(action string) ([]string, error) {
	s := action
	if len(s) > 0 && s[0] == '-' && (len(s) == 1 || s[1] == ' ' || s[1] == '\t') {
		s = s[1:]
	}
	if n := len(s); n > 0 && s[n-1] == '-' && (n == 1 || s[n-2] == ' ' || s[n-2] == '\t') {
		s = s[:n-1]
	}
	s = strings.TrimSpace(s)
	var tokens []string
	for s != "" {
		if s[0] == '`' {
			k := strings.IndexByte(s[1:], '`')
			if k < 0 {
				return nil, errors.New("unterminated raw string literal")
			}
			tokens = append(tokens, s[:k+2])
			s = strings.TrimLeft(s[k+2:], " \t")
			continue
		}
		if s[0] == '"' {
			k := 1
			for k < len(s) && s[k] != '"' {
				if s[k] == '\\' {
					k++
				}
				k++
			}
			if k >= len(s) {
				return nil, errors.New("unterminated string literal")
			}
			tokens = append(tokens, s[:k+1])
			s = strings.TrimLeft(s[k+1:], " \t")
			continue
		}
		k := strings.IndexAny(s, " \t")
		if k < 0 {
			tokens = append(tokens, s)
			break
		}
		tokens = append(tokens, s[:k])
		s = strings.TrimLeft(s[k:], " \t")
	}
	return tokens, nil
}

// checkExpr validates the tokens of one expression action: each must be a quoted
// literal, a number, a boolean, a closed-set function, or a placeholder whose head
// and subfield are inside the closed set.
func (cx *checkCtx) checkExpr(tokens []string, path string, inRange bool) error {
	for _, tok := range tokens {
		switch {
		case strings.HasPrefix(tok, "\"") && strings.HasSuffix(tok, "\"") && len(tok) >= 2:
			// quoted literal
		case strings.HasPrefix(tok, "`") && strings.HasSuffix(tok, "`") && len(tok) >= 2:
			// raw string literal
		case tok == "true" || tok == "false":
		case templateFuncs[tok]:
		case tok == ".":
			if !inRange {
				return cx.fail(path, "{{ . }} is only valid inside {{ range .Categories }}")
			}
		case strings.HasPrefix(tok, "."):
			if err := cx.checkPlaceholder(tok, path); err != nil {
				return err
			}
		default:
			if _, err := strconv.ParseFloat(tok, 64); err == nil {
				continue
			}
			return cx.fail(path, "unknown template token %q", tok)
		}
	}
	return nil
}

// checkPlaceholder validates one ".Head[.Sub]" token: the head must be in
// Placeholders and the subfield rules of doc 07 section 3.3 apply per head.
func (cx *checkCtx) checkPlaceholder(tok, path string) error {
	parts := strings.Split(strings.TrimPrefix(tok, "."), ".")
	head, subs := parts[0], parts[1:]
	if !placeholderSet[head] {
		return cx.fail(path, "unknown placeholder %q", tok)
	}
	member := func(set map[string]bool, what string) error {
		if len(subs) != 1 || !set[subs[0]] {
			return cx.fail(path, "%s needs exactly one %s member, got %q", tok, what, strings.Join(subs, "."))
		}
		return nil
	}
	switch head {
	case "Keywords", "Page", "Limit", "Offset", "Categories":
		if len(subs) != 0 {
			return cx.fail(path, "%s takes no member", tok)
		}
	case "Query":
		return member(queryFields, "query field")
	case "Config":
		if len(subs) != 1 || !cx.settings[subs[0]] {
			return cx.fail(path, "%s does not name a declared settings[].name", tok)
		}
	case "Result":
		if len(subs) != 1 || !cx.fields[subs[0]] {
			return cx.fail(path, "%s does not name a declared response field", tok)
		}
	case "Today":
		if len(subs) != 1 || subs[0] != "Year" {
			return cx.fail(path, ".Today has only the .Year member, got %q", tok)
		}
	}
	return nil
}

func (cx *checkCtx) fail(path, format string, args ...any) *DefinitionError {
	return &DefinitionError{Line: lineOf(cx.root, path), Path: path, Msg: fmt.Sprintf(format, args...)}
}

// lineOf resolves a dotted path like "response.fields.title.type" to the line of
// the matching key in the decoded document. When a segment does not resolve it
// returns the line of the closest enclosing node; 0 only when the document is
// empty. Numeric segments index into sequences.
func lineOf(root *yaml.Node, path string) int {
	if root == nil || len(root.Content) == 0 {
		return 0
	}
	n := root.Content[0]
	for _, seg := range strings.Split(path, ".") {
		switch n.Kind {
		case yaml.MappingNode:
			var next *yaml.Node
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == seg {
					next = n.Content[i+1]
					break
				}
			}
			if next == nil {
				return n.Line
			}
			n = next
		case yaml.SequenceNode:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(n.Content) {
				return n.Line
			}
			n = n.Content[i]
		default:
			return n.Line
		}
	}
	return n.Line
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
