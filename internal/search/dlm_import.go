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
	members := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ImportResult{}, fmt.Errorf("dlm: read tar member: %w", err)
		}
		members++
		if members > MaxDLMMembers {
			return ImportResult{}, fmt.Errorf("dlm: archive has more than %d members", MaxDLMMembers)
		}
		if err := validateDLMMember(hdr); err != nil {
			return ImportResult{}, err
		}
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
	return ImportResult{
		Definition: def,
		Name:       def.Name,
		Kind:       "dlsearch",
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
	for _, s := range phpStringLiterals(php) {
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
		}
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
