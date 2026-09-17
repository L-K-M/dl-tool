// This file is the .dlm importer of doc 07 section 4.1 and doc 12 section
// 5.3: a Synology Download Station search module is read entirely in
// memory, validated member by member, and converted to a dlsearch/v1 draft
// by static analysis only. Nothing is written to disk, nothing is
// extracted, and no interpreter is ever started (ADR-0010). Every
// rejection names the rule it hit.
package search

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// ImportResult is what every importer produces. Converted is false for the
// metadata-only path, and Definition is then nil.
type ImportResult struct {
	Definition *Definition
	Name       string // INFO.displayname, or INFO.name
	Kind       string // dlsearch | torznab
	Provenance string // "imported:dlm"
	Origin     string // the uploaded file name, stored in settings_json.origin
	Source     []byte // the original module, stored inert for the "view source" pane
	Converted  bool
	Warnings   []string

	// The nova3 .py path (T060) never produces a Definition; the
	// extracted plugin metadata rides these fields into the indexer row
	// and its settings_json instead.
	Version        string            // the #VERSION: line, "" when absent
	URL            string            // the class's url literal
	SiteCategories map[string]string // supported_categories verbatim: friendly name -> site value
	Categories     []Category        // SiteCategories mapped through NovaCategories: newznab id -> friendly name
}

// Archive limits, doc 07 section 4.1. They are stricter than doc 12 section 5.3, so
// satisfying these satisfies both.
const (
	MaxDLMCompressedBytes   = 1 << 20 // 1 MiB uploaded
	MaxDLMMembers           = 16
	MaxDLMMemberBytes       = 1 << 20 // 1 MiB per member, uncompressed
	MaxDLMUncompressedBytes = 4 << 20 // 4 MiB total, enforced with io.LimitReader
)

// maxDLMMemberNameBytes is the doc 07 section 4.1 member-name cap.
const maxDLMMemberNameBytes = 255

// DLMInfo is the INFO member, doc 07 section 4.1.
type DLMInfo struct {
	Name           string `json:"name"`
	DisplayName    string `json:"displayname"`
	Description    string `json:"description"`
	Version        string `json:"version"`
	Site           string `json:"site"`
	Module         string `json:"module"`
	Type           string `json:"type"` // only "search" is supported
	Class          string `json:"class"`
	AccountSupport bool   `json:"accountsupport"` // observed in third-party modules only
}

// The provenance values an ImportResult carries, echoed onto the indexers
// row by the API layer.
const (
	ProvenanceDLM  = "imported:dlm"
	ProvenanceFile = "imported:file"
)

// unconvertibleWarning is the doc 07 section 4.1 UI message the
// metadata-only path reports verbatim.
const unconvertibleWarning = "This module contains custom PHP that dl-tool does not execute. Re-express it as a dl-tool YAML engine."

// torznabCredentialWarning is the doc 07 section 4.1 explanation of the
// jackett.dlm credential abuse: username carried the host, password the
// API key.
const torznabCredentialWarning = "Download Station stored the Jackett host as the username and the API key as the password; they are now base_url and api_key"

// noHomepageWarning marks a converted draft whose INFO.site was not a
// usable homepage; the draft cannot validate until one is set.
const noHomepageWarning = "the module's INFO.site is not an http or https URL; the generated definition has no homepage and must be given one before it can be enabled"

// ImportDLM validates the archive, reads exactly INFO and the member
// INFO.module, and statically analyses the module. It never writes a file,
// never extracts and never executes. Every rejection names the rule it
// hit.
func ImportDLM(data []byte, filename string) (ImportResult, error) {
	if len(data) > MaxDLMCompressedBytes {
		return ImportResult{}, fmt.Errorf(
			"dlm: upload is %d bytes, over the %d-byte compressed limit", len(data), MaxDLMCompressedBytes)
	}

	decompressed, err := inflateDLM(data)
	if err != nil {
		return ImportResult{}, err
	}

	// First pass: validate every member header against the section 4.1
	// table and read the one INFO member. Every other member's content is
	// skipped unread (doc 12 section 5.3: exactly two files are read).
	tr := tar.NewReader(bytes.NewReader(decompressed))
	var infoBytes []byte
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ImportResult{}, fmt.Errorf("dlm: read tar member: %w", err)
		}
		if len(seen) >= MaxDLMMembers {
			return ImportResult{}, fmt.Errorf("dlm: archive has more than %d members", MaxDLMMembers)
		}
		if err := validateDLMMember(hdr); err != nil {
			return ImportResult{}, err
		}
		if seen[hdr.Name] {
			return ImportResult{}, fmt.Errorf("dlm: the archive contains a duplicate %q member", hdr.Name)
		}
		seen[hdr.Name] = true
		if hdr.Name == "INFO" {
			if infoBytes, err = readDLMMember(tr); err != nil {
				return ImportResult{}, err
			}
		}
	}
	if infoBytes == nil {
		return ImportResult{}, errors.New("dlm: the INFO member is missing")
	}

	info, err := parseDLMInfo(infoBytes)
	if err != nil {
		return ImportResult{}, err
	}

	// Second pass over the in-memory archive: the member named by
	// INFO.module is the only other file read.
	php, err := findDLMMember(decompressed, info.Module)
	if err != nil {
		return ImportResult{}, err
	}

	res := ImportResult{
		Name:       info.DisplayName,
		Kind:       "dlsearch",
		Provenance: ProvenanceDLM,
		Origin:     filename,
		Source:     php,
		Warnings:   []string{},
	}
	if res.Name == "" {
		res.Name = info.Name
	}
	res.Definition, res.Converted, res.Warnings = analyseModule(php, info)
	if res.Converted && res.Definition.Kind == "torznab" {
		res.Kind = "torznab"
	}
	return res, nil
}

// ImportDefinitionFile accepts an uploaded .dlsearch.yaml, validates it
// with LoadDefinition and returns it with Provenance "imported:file".
func ImportDefinitionFile(data []byte, filename string) (ImportResult, error) {
	def, err := LoadDefinition(data)
	if err != nil {
		return ImportResult{}, err
	}
	kind := "dlsearch"
	if def.Kind == "torznab" {
		kind = "torznab"
	}
	return ImportResult{
		Definition: def,
		Name:       def.Name,
		Kind:       kind,
		Provenance: ProvenanceFile,
		Origin:     filename,
		Source:     data,
		Converted:  true,
		Warnings:   []string{},
	}, nil
}

// inflateDLM decompresses the gzip stream with the 4 MiB total cap enforced
// by io.LimitReader, so a bomb is stopped while inflating, not after.
func inflateDLM(data []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("dlm: not a gzip-compressed tar archive: %w", err)
	}
	decompressed, err := io.ReadAll(io.LimitReader(gz, MaxDLMUncompressedBytes+1))
	if err != nil {
		return nil, fmt.Errorf("dlm: decompress: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("dlm: decompress: %w", err)
	}
	if len(decompressed) > MaxDLMUncompressedBytes {
		return nil, fmt.Errorf(
			"dlm: uncompressed archive exceeds the %d-byte limit", MaxDLMUncompressedBytes)
	}
	return decompressed, nil
}

// validateDLMMember applies the member-validation table of doc 07 section
// 4.1 to one header, before any of its content is read.
func validateDLMMember(hdr *tar.Header) error {
	// The reader normalises the legacy '\x00' regular-file flag to
	// TypeReg, so one comparison covers both spellings.
	if hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf(
			"dlm: member %q is a %s; only regular files are accepted", hdr.Name, dlmMemberKind(hdr.Typeflag))
	}
	name := hdr.Name
	switch {
	case name == "":
		return errors.New("dlm: a member name is empty")
	case name[0] == '/':
		return fmt.Errorf("dlm: member name %q is absolute", name)
	case strings.Contains(name, ".."):
		return fmt.Errorf("dlm: member name %q contains a parent-directory reference", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("dlm: member name %q contains a path separator", name)
	case !utf8.ValidString(name):
		return fmt.Errorf("dlm: member name %q is not valid UTF-8", name)
	case len(name) > maxDLMMemberNameBytes:
		return fmt.Errorf(
			"dlm: member name is %d bytes, over the %d-byte limit", len(name), maxDLMMemberNameBytes)
	}
	if hdr.Size < 0 || hdr.Size > MaxDLMMemberBytes {
		return fmt.Errorf(
			"dlm: member %q is %d bytes, over the %d-byte per-member limit", name, hdr.Size, MaxDLMMemberBytes)
	}
	return nil
}

// dlmMemberKind names the entry type a rejection reports.
func dlmMemberKind(t byte) string {
	switch t {
	case tar.TypeLink:
		return "hardlink"
	case tar.TypeSymlink:
		return "symlink"
	case tar.TypeChar:
		return "character device"
	case tar.TypeBlock:
		return "block device"
	case tar.TypeDir:
		return "directory"
	case tar.TypeFifo:
		return "FIFO"
	default:
		return fmt.Sprintf("typeflag %d entry", t)
	}
}

// readDLMMember consumes one member's content. The header was already
// validated, so the read is bounded by MaxDLMMemberBytes plus the
// per-stream bookkeeping a tar.Reader applies.
func readDLMMember(tr *tar.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(tr, MaxDLMMemberBytes+1))
	if err != nil {
		return nil, fmt.Errorf("dlm: read member: %w", err)
	}
	if len(body) > MaxDLMMemberBytes {
		return nil, fmt.Errorf("dlm: member content exceeds the %d-byte per-member limit", MaxDLMMemberBytes)
	}
	return body, nil
}

// findDLMMember re-walks the in-memory archive and returns the content of
// the member named name — the second of the two files the importer reads.
func findDLMMember(decompressed []byte, name string) ([]byte, error) {
	tr := tar.NewReader(bytes.NewReader(decompressed))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("dlm: read tar member: %w", err)
		}
		if hdr.Name == name {
			return readDLMMember(tr)
		}
	}
	return nil, fmt.Errorf("dlm: the member named by INFO.module (%q) is not present", name)
}

// parseDLMInfo decodes INFO as a JSON object and applies the section 4.1
// key rules: name, version, module and class are mandatory and type must
// be "search".
func parseDLMInfo(data []byte) (DLMInfo, error) {
	var probe any
	if err := json.Unmarshal(data, &probe); err != nil {
		return DLMInfo{}, fmt.Errorf("dlm: INFO is not valid JSON: %w", err)
	}
	if _, ok := probe.(map[string]any); !ok {
		return DLMInfo{}, errors.New("dlm: INFO is not a JSON object")
	}
	var info DLMInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return DLMInfo{}, fmt.Errorf("dlm: INFO is not valid JSON: %w", err)
	}
	for _, req := range []struct{ key, val string }{
		{"name", info.Name},
		{"version", info.Version},
		{"module", info.Module},
		{"class", info.Class},
	} {
		if strings.TrimSpace(req.val) == "" {
			return DLMInfo{}, fmt.Errorf("dlm: INFO.%s is required", req.key)
		}
	}
	if info.Type != "search" {
		return DLMInfo{}, fmt.Errorf("dlm: INFO.type is %q; only \"search\" is supported", info.Type)
	}
	return info, nil
}

// analyseModule detects the convertible shapes of doc 07 section 4.1:
//
//	addRSSResults  -> kind: rss,     base_url from the single http string literal
//	torznab proxy  -> kind: torznab, <host> and <apikey> mapped to the two settings
//
// Anything else returns converted=false and the metadata-only result.
func analyseModule(php []byte, info DLMInfo) (*Definition, bool, []string) {
	if def, warnings, ok := convertRSSModule(php, info); ok {
		return def, true, warnings
	}
	if def, warnings, ok := convertTorznabModule(php, info); ok {
		return def, true, warnings
	}
	return nil, false, []string{unconvertibleWarning}
}

// phpStringLiteralRe matches one single- or double-quoted PHP string
// literal. Static analysis only scans the text; escape sequences are left
// verbatim because the detection only asks whether the literal mentions
// "http".
var phpStringLiteralRe = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)'`)

// stripPHPComments removes //, # and /* */ comment text outside string
// literals, so a quoted URL inside a comment cannot be mistaken for the
// module's endpoint. Quote state is tracked because a naive removal of
// "//" would corrupt https:// inside literals.
func stripPHPComments(php []byte) []byte {
	out := make([]byte, 0, len(php))
	for i := 0; i < len(php); {
		switch c := php[i]; {
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(php) && php[j] != c {
				if php[j] == '\\' {
					j++
				}
				j++
			}
			// An escape at end-of-input pushes j past the buffer; clamp
			// before slicing, like the block-comment branch below.
			j = min(j, len(php))
			if j < len(php) {
				j++ // include the closing quote
			}
			out = append(out, php[i:j]...)
			i = j
		case c == '/' && i+1 < len(php) && php[i+1] == '/':
			for i < len(php) && php[i] != '\n' {
				i++
			}
		case c == '#':
			for i < len(php) && php[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(php) && php[i+1] == '*':
			i += 2
			for i+1 < len(php) && (php[i] != '*' || php[i+1] != '/') {
				i++
			}
			i = min(i+2, len(php)) // skip the closing */, if present
		default:
			out = append(out, c)
			i++
		}
	}
	return out
}

// phpStringLiterals returns the contents of every quoted string literal in
// the module source.
func phpStringLiterals(php []byte) []string {
	out := []string{}
	for _, m := range phpStringLiteralRe.FindAllSubmatch(php, -1) {
		if m[1] != nil {
			out = append(out, string(m[1]))
		} else {
			out = append(out, string(m[2]))
		}
	}
	return out
}

// convertRSSModule recognises the guide's RSS shape: parse() calls
// addRSSResults and the class carries exactly one string literal
// containing "http" — the URL prefix prepare() concatenates the query
// onto. The literal splits into request.base_url plus request.path, and
// its last query parameter becomes {{ .Keywords }}.
func convertRSSModule(php []byte, info DLMInfo) (*Definition, []string, bool) {
	if !bytes.Contains(php, []byte("addRSSResults")) {
		return nil, nil, false
	}
	var literal string
	count := 0
	for _, s := range phpStringLiterals(stripPHPComments(php)) {
		if strings.Contains(s, "http") {
			literal = s
			count++
		}
	}
	if count != 1 {
		return nil, nil, false
	}
	u, err := url.Parse(literal)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, nil, false
	}

	def := importedDefinition(info)
	def.Kind = "rss"
	def.Caps.SeedersUnknown = true
	def.Request = &Request{
		BaseURL: u.Scheme + "://" + u.Host,
		Path:    strings.TrimPrefix(u.EscapedPath(), "/"),
		Method:  "GET",
	}
	hasKeywordParam := false
	if u.RawQuery != "" {
		def.Request.Query = map[string]string{}
		pairs := strings.Split(u.RawQuery, "&")
		// The concatenated parameter is the trailing pair: prepare()
		// appends urlencode($query) to the literal. Keys and values are
		// decoded because the runner re-encodes the whole query map.
		for _, pair := range pairs[:len(pairs)-1] {
			key, value, _ := strings.Cut(pair, "=")
			if key = decodeQueryComponent(key); key != "" {
				def.Request.Query[key] = decodeQueryComponent(value)
			}
		}
		key, _, _ := strings.Cut(pairs[len(pairs)-1], "=")
		if key = decodeQueryComponent(key); key != "" {
			def.Request.Query[key] = "{{ .Keywords }}"
			hasKeywordParam = true
		}
	}
	if !hasKeywordParam {
		// No query parameter carries the keywords — a path-appended or
		// otherwise unmodelled shape. Converted would emit an engine that
		// ignores the user's search terms; fall back to metadata-only.
		return nil, nil, false
	}
	def.Response = &Response{
		Rows:   "rss > channel > item",
		Fields: defaultRSSFields(),
	}

	warnings := []string{}
	if def.Homepage == "" {
		// The URL literal is the one address the module provably used.
		def.Homepage = def.Request.BaseURL
	}
	return def, warnings, true
}

// decodeQueryComponent unescapes one query key or value; a malformed
// escape is kept verbatim rather than dropped.
func decodeQueryComponent(s string) string {
	if decoded, err := url.QueryUnescape(s); err == nil {
		return decoded
	}
	return s
}

// defaultRSSFields is the RSS half of the doc 07 section 4.2 addResult
// mapping: the Download Station RSS parser reads the item title, the
// enclosure as the download, pubDate as the timestamp, link/guid as the
// details page and no seed or leech counts — hence seeders_unknown.
func defaultRSSFields() map[string]Field {
	return map[string]Field{
		"title":     {Path: "title"},
		"download":  {Path: "enclosure", Attr: "url", Optional: true},
		"details":   {Path: "link", Optional: true},
		"published": {Path: "pubDate", Type: "datetime", Format: "rfc1123", Optional: true},
		"size":      {Path: "enclosure", Attr: "length", Type: "bytes", Optional: true, Default: "0"},
		"category":  {Const: "all"},
	}
}

// convertTorznabModule recognises the jackett.dlm shape: parse() runs
// simplexml_load_string over a Torznab response and reads torznab:attr
// elements. The emitted kind: torznab draft carries the two settings the
// module's <host> and <apikey> placeholders map to.
func convertTorznabModule(php []byte, info DLMInfo) (*Definition, []string, bool) {
	if !bytes.Contains(php, []byte("simplexml_load_string")) || !bytes.Contains(php, []byte("torznab:attr")) {
		return nil, nil, false
	}
	def := importedDefinition(info)
	def.Kind = "torznab"
	def.Settings = []Setting{
		{Name: "base_url", Type: "text", Label: "Jackett or Prowlarr base URL"},
		{Name: "api_key", Type: "password", Label: "Instance API key"},
	}
	warnings := []string{torznabCredentialWarning}
	if def.Homepage == "" {
		warnings = append(warnings, noHomepageWarning)
	}
	return def, warnings, true
}

// importedDefinition is the metadata half of every converted draft: the
// INFO fields mapped onto dlsearch/v1, always user-supplied, with the
// generic single-category caps.
func importedDefinition(info DLMInfo) *Definition {
	def := &Definition{
		DLSearch:  1,
		ID:        importedDefinitionID(info),
		Name:      info.DisplayName,
		Version:   info.Version,
		LegalTier: "user-supplied",
		Caps: DefCaps{
			Modes:      map[string][]string{"search": {"q"}},
			Categories: map[string]int{"all": 8000},
		},
	}
	if def.Name == "" {
		def.Name = info.Name
	}
	def.Description = info.Description
	if def.Description == "" {
		def.Description = "Imported from a Synology .dlm module."
	}
	if u, err := url.Parse(info.Site); err == nil && u.Host != "" &&
		(u.Scheme == "http" || u.Scheme == "https") {
		def.Homepage = info.Site
	}
	return def
}

// importedDefinitionID derives a dlsearch/v1 id from INFO.name. A name
// that cannot slugify into the id grammar falls back to a digest of the
// module name, so the id is still deterministic.
func importedDefinitionID(info DLMInfo) string {
	var b strings.Builder
	for _, r := range strings.ToLower(info.Name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.', r == ' ':
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 64 {
		slug = strings.Trim(slug[:64], "-")
	}
	if defIDRe.MatchString(slug) {
		return slug
	}
	sum := sha256.Sum256([]byte(info.Name))
	return "dlm-" + hex.EncodeToString(sum[:4])
}

// --- qBittorrent nova3 .py import (doc 07 section 4.3) -------------------
//
// A nova3 plugin is procedural Python: nothing in it is converted and none
// of it ever runs (ADR-0010). The importer is a literal-only reader — it
// scans the text for class-scope assignments of string, integer and dict
// literals, records name, url, supported_categories and the #VERSION:
// header, and produces a disabled indexer row whose settings_json keeps the
// whole file as an inert blob. Anything that is not a literal assignment is
// skipped, never evaluated.

// MaxNovaPluginBytes is the upload cap for a nova3 .py plugin.
const MaxNovaPluginBytes = 512 << 10 // 512 KiB

// ProvenanceQbtPy marks a row created from an uploaded nova3 plugin.
const ProvenanceQbtPy = "imported:qbt-py"

// novaPluginWarning is the doc 07 section 4.3 message pointing the user at
// dlsearch/v1 — the only import outcome for a nova3 plugin.
const novaPluginWarning = "dl-tool does not run Python search plugins. Re-express this plugin as a dl-tool YAML engine (dlsearch/v1)."

// novaPicturesWarning is the mandated warning for the one friendly name
// that has no documented newznab id.
const novaPicturesWarning = `category "pictures" has no newznab equivalent and is imported unmapped`

// novaVersionLineBytes is qBittorrent's per-line read cap for the version
// header; a longer line counts as absent (doc 07 section 4.3).
const novaVersionLineBytes = 16

// NovaCategories maps the nine friendly names of a supported_categories
// dict onto newznab ids, exactly as doc 07 section 2.3 gives the jackett.py
// mapping. "all" means no category filter, and "pictures" has no documented
// newznab id: it is imported as a declared site value with no mapping and
// raises a warning.
var NovaCategories = map[string][]int{
	"all":      nil,
	"anime":    {5070},
	"books":    {8000},
	"games":    {1000, 4000},
	"movies":   {2000},
	"music":    {3000},
	"pictures": nil,
	"software": {4000},
	"tv":       {5000},
}

// novaCategoryNames is the canonical iteration order of the friendly
// names, derived from NovaCategories sorted so the two can never drift;
// warnings and shared-id labels stay deterministic.
var novaCategoryNames = func() []string {
	names := make([]string, 0, len(NovaCategories))
	for name := range NovaCategories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}()

// ImportNovaPlugin extracts metadata from a qBittorrent nova3 plugin. It
// never runs the file. The result always has Converted=false and
// Definition=nil: a nova3 plugin is procedural code and no mechanical
// conversion to dlsearch/v1 exists.
//
// Provenance is "imported:qbt-py"; Origin is the uploaded file name, whose
// stem must equal the class name qBittorrent would resolve with
// getattr(module, module_name).
func ImportNovaPlugin(data []byte, filename string) (ImportResult, error) {
	if len(data) > MaxNovaPluginBytes {
		return ImportResult{}, fmt.Errorf(
			"py: upload is %d bytes, over the %d-byte plugin limit", len(data), MaxNovaPluginBytes)
	}
	if !utf8.Valid(data) {
		return ImportResult{}, errors.New("py: the plugin source is not valid UTF-8")
	}

	stem := novaStem(filename)
	if stem == "" {
		return ImportResult{}, errors.New("py: the file name has no stem; a nova3 plugin is named <class>.py")
	}
	// The UTF-8 BOM CPython accepts transparently must not hide a first
	// line's class definition or #VERSION: header from the reader. Source
	// keeps the uploaded bytes verbatim; only the parse view is stripped.
	src := bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

	classes := pyClassNames(src)
	if len(classes) == 0 {
		return ImportResult{}, errors.New("py: no class definition found; a nova3 plugin defines a class named after its file")
	}

	name, site, siteCategories, err := pyLiterals(src)
	if err != nil {
		return ImportResult{}, err
	}

	res := ImportResult{
		Name:           name,
		Kind:           "dlsearch", // metadata only: Definition stays nil on this path
		Provenance:     ProvenanceQbtPy,
		Origin:         filename, // display-only provenance; never used as a path
		Source:         data,
		Version:        parsePluginVersion(src),
		URL:            site,
		SiteCategories: siteCategories,
		Warnings:       []string{novaPluginWarning},
	}
	if res.Name == "" {
		res.Name = stem
	}
	if !slices.Contains(classes, stem) {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"class %q does not match the file stem %q; qBittorrent resolves the plugin as getattr(module, %q)",
			classes[0], stem, stem))
	}
	if siteCategories != nil && len(siteCategories) == 0 {
		res.Warnings = append(res.Warnings,
			"supported_categories yielded no quoted-string mappings; no categories were imported")
	}
	res.Categories = novaCategories(siteCategories, &res.Warnings)
	return res, nil
}

// novaStem is the module name qBittorrent derives: the file's base name
// minus the .py extension the upload was dispatched on.
func novaStem(filename string) string {
	base := filename
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if strings.HasSuffix(strings.ToLower(base), ".py") {
		base = base[:len(base)-len(".py")]
	}
	return base
}

// novaCategories folds the supported_categories keys through
// NovaCategories into the flat, sorted newznab list the indexer row
// caches. "all" declares no filter and contributes nothing; "pictures" has
// no documented newznab id and lands its mandated warning; a key outside
// the nine friendly names is imported unmapped with a warning naming it.
func novaCategories(siteCategories map[string]string, warnings *[]string) []Category {
	seen := map[int]bool{}
	out := []Category{}
	for _, key := range novaCategoryOrder(siteCategories) {
		mapped, known := NovaCategories[key]
		switch {
		case !known:
			*warnings = append(*warnings, fmt.Sprintf(
				"category %q is not one of the nine nova3 names and is imported unmapped", key))
		case key == "all":
			// no category filter: contributes no ids
		case key == "pictures":
			*warnings = append(*warnings, novaPicturesWarning)
		default:
			for _, id := range mapped {
				if seen[id] {
					continue
				}
				seen[id] = true
				out = append(out, Category{ID: id, Name: key})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// novaCategoryOrder lists the declared keys in canonical order first and
// any unknown keys after, sorted — deterministic warnings and labels.
func novaCategoryOrder(categories map[string]string) []string {
	out := make([]string, 0, len(categories))
	seen := make(map[string]bool, len(categories))
	for _, key := range novaCategoryNames {
		if _, ok := categories[key]; ok {
			out = append(out, key)
			seen[key] = true
		}
	}
	rest := []string{}
	for key := range categories {
		if !seen[key] {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// pyClassRe matches a top-level class definition; the class name is what
// qBittorrent resolves with getattr(module, module_name).
var pyClassRe = regexp.MustCompile(`^class\s+([A-Za-z_][A-Za-z0-9_]*)`)

// pyAssignRe matches `ident = rhs` on a trimmed class-scope line. Only a
// bare `=` binds: in `==`, `<=` and `>=` the second character lands in the
// right-hand side, where the literal readers refuse it.
var pyAssignRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$`)

// parsePluginVersion implements doc 07 section 4.3 exactly: scan every
// line, strip ALL spaces, take the first line that starts with "#VERSION:"
// case-insensitively and read the remainder after 9 characters. qBittorrent
// reads only 16 bytes per line, so a longer line counts as absent and this
// returns "".
func parsePluginVersion(src []byte) string {
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if len(line) > novaVersionLineBytes {
			continue
		}
		line = strings.ReplaceAll(line, " ", "")
		if len(line) >= len("#VERSION:") && strings.EqualFold(line[:len("#VERSION:")], "#VERSION:") {
			return line[len("#VERSION:"):]
		}
	}
	return ""
}

// pyClassNames lists the names of the file's top-level class definitions,
// in source order.
func pyClassNames(src []byte) []string {
	names := []string{}
	for _, line := range strings.Split(string(src), "\n") {
		if m := pyClassRe.FindStringSubmatch(line); m != nil {
			names = append(names, m[1])
		}
	}
	return names
}

// pyLiterals reads class-scope assignments of string, integer and dict
// literals and nothing else. Any other construct on the right-hand side is
// ignored, never evaluated. Returns the values of name, url and
// supported_categories when present; the categories map is non-nil when a
// supported_categories assignment was seen, even one that did not parse.
// Assignments nested inside class-body if/try blocks sit below the body
// indent and are not read, though qBittorrent would see them at runtime.
func pyLiterals(src []byte) (name, url string, categories map[string]string, err error) {
	if !utf8.Valid(src) {
		return "", "", nil, errors.New("py: the plugin source is not valid UTF-8")
	}
	lines := strings.Split(string(src), "\n")
	inClass := false
	classIndent, bodyIndent := 0, -1
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if pyClassRe.MatchString(line) {
			// A top-level class opens the scope the reader cares about;
			// an indented "class" line fails the anchored regexp and is
			// handled like any other nested statement below.
			inClass = true
			classIndent = indent
			bodyIndent = -1
			continue
		}
		if !inClass || indent <= classIndent {
			inClass = false
			continue
		}
		if bodyIndent < 0 {
			bodyIndent = indent
		}
		if indent != bodyIndent {
			continue // inside a method or nested block, not class scope
		}
		m := pyAssignRe.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		switch m[1] {
		case "name":
			if v, ok := pyString(m[2]); ok {
				name = v
			}
		case "url":
			if v, ok := pyString(m[2]); ok {
				url = v
			}
		case "supported_categories":
			var text strings.Builder
			var scan pyBraceScan
			rhs := m[2]
			// A `= \` line continuation (or a bare `=`) puts the literal
			// on the next line; follow it before balancing braces.
			for i+1 < len(lines) &&
				strings.TrimSuffix(strings.TrimSpace(rhs), "\\") == "" {
				i++
				rhs = lines[i]
			}
			text.WriteString(rhs)
			scan.feed(rhs)
			// The dict literal may span lines; keep consuming until its
			// braces balance. An unterminated dict is skipped like any
			// other non-literal construct.
			for !scan.balanced() && i+1 < len(lines) {
				i++
				text.WriteByte('\n')
				text.WriteString(lines[i])
				scan.feed(lines[i])
			}
			categories = map[string]string{} // the assignment was seen
			if d, ok := pyStringDict(text.String()); ok {
				categories = d
			}
		default:
			// Other class attributes — integer literals included — are
			// recognized as assignments but carry no imported value.
		}
	}
	return name, url, categories, nil
}

// pyString accepts a single- or double-quoted string literal optionally
// followed by a comment, and returns its contents. A concatenation, call or
// any other trailing construct makes the right-hand side a non-literal and
// the assignment is skipped.
func pyString(rhs string) (string, bool) {
	d := &pyDictScanner{s: rhs}
	v, ok := d.quoted()
	if !ok {
		return "", false
	}
	d.skipTrivia()
	if d.pos != len(d.s) {
		return "", false
	}
	return v, true
}

// pyBraceScan tracks dict-literal depth across the lines a
// supported_categories value may span, keeping the multi-line consume
// linear in the input. Quotes are skipped so a "}" inside a string does
// not close the dict and comments so a "{" inside one does not open it; a
// quote still open at end of line does not continue — Python's one-line
// strings cannot either.
type pyBraceScan struct {
	depth  int
	opened bool
}

// feed scans one line.
func (s *pyBraceScan) feed(line string) {
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\'', '"':
			q := line[i]
			i++
			for i < len(line) && line[i] != q {
				if line[i] == '\\' {
					i++
				}
				i++
			}
		case '#':
			return // the rest of the line is a comment
		case '{':
			s.depth++
			s.opened = true
		case '}':
			s.depth--
		}
	}
}

// balanced reports that no dict was opened or every opened brace closed.
// A negative depth means a stray "}" ended the literal early; the dict
// reader rejects the text either way, so consuming stops here.
func (s *pyBraceScan) balanced() bool { return !s.opened || s.depth <= 0 }

// pyDictScanner walks a dict or string literal: it accepts quoted strings,
// whitespace and comments and nothing else. It never evaluates — a token it
// does not understand ends the parse, not the input.
type pyDictScanner struct {
	s   string
	pos int
}

// skipTrivia consumes whitespace and # comments.
func (d *pyDictScanner) skipTrivia() {
	for d.pos < len(d.s) {
		switch d.s[d.pos] {
		case '#':
			for d.pos < len(d.s) && d.s[d.pos] != '\n' {
				d.pos++
			}
		case ' ', '\t', '\n', '\r':
			d.pos++
		default:
			return
		}
	}
}

// quoted consumes one single- or double-quoted string literal and returns
// its contents. Escaped quotes are read verbatim; the contents are never
// interpreted beyond quote termination.
func (d *pyDictScanner) quoted() (string, bool) {
	d.skipTrivia()
	if d.pos >= len(d.s) || (d.s[d.pos] != '\'' && d.s[d.pos] != '"') {
		return "", false
	}
	q := d.s[d.pos]
	d.pos++
	var b strings.Builder
	for d.pos < len(d.s) && d.s[d.pos] != q {
		// Only the quote escapes and \\ are honored — in either quote
		// style, as Python does. Every other sequence (\n included) keeps
		// its backslash verbatim rather than being interpreted.
		if d.s[d.pos] == '\\' && d.pos+1 < len(d.s) &&
			(d.s[d.pos+1] == '\'' || d.s[d.pos+1] == '"' || d.s[d.pos+1] == '\\') {
			d.pos++
		}
		b.WriteByte(d.s[d.pos])
		d.pos++
	}
	if d.pos >= len(d.s) {
		return "", false // unterminated
	}
	d.pos++ // the closing quote
	return b.String(), true
}

// pyStringDict parses a `{ 'key': 'value', ... }` literal — the shape
// supported_categories uses. Only quoted keys and quoted values are
// accepted; anything else makes the whole right-hand side a non-literal and
// the assignment is skipped.
func pyStringDict(s string) (map[string]string, bool) {
	d := &pyDictScanner{s: s}
	d.skipTrivia()
	if d.pos >= len(d.s) || d.s[d.pos] != '{' {
		return nil, false
	}
	d.pos++
	out := map[string]string{}
	for {
		d.skipTrivia()
		if d.pos < len(d.s) && d.s[d.pos] == '}' {
			d.pos++
			break
		}
		key, ok := d.quoted()
		if !ok {
			return nil, false
		}
		d.skipTrivia()
		if d.pos >= len(d.s) || d.s[d.pos] != ':' {
			return nil, false
		}
		d.pos++
		value, ok := d.quoted()
		if !ok {
			return nil, false
		}
		out[key] = value
		d.skipTrivia()
		if d.pos < len(d.s) && d.s[d.pos] == ',' {
			d.pos++
			continue
		}
		if d.pos < len(d.s) && d.s[d.pos] == '}' {
			d.pos++
			break
		}
		return nil, false
	}
	d.skipTrivia()
	if d.pos != len(d.s) {
		return nil, false // trailing construct after the closing brace
	}
	return out, true
}
