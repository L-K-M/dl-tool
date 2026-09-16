package search

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, name string) (*Definition, error) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return LoadDefinition(data)
}

func wantDefErr(t *testing.T, err error, substrs ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("LoadDefinition succeeded, want error containing %q", substrs)
	}
	var de *DefinitionError
	if !errors.As(err, &de) {
		t.Fatalf("error type %T, want *DefinitionError (%v)", err, err)
	}
	for _, s := range substrs {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not contain %q", err.Error(), s)
		}
	}
}

// baseJSON is a valid kind: json definition; each TestDefinitionRules row mutates
// exactly one thing so the expected rule is the one that fires.
const baseJSON = `dlsearch: 1
id: fixture-json
name: Fixture JSON
description: "A valid json definition used as the mutation base."
homepage: https://example.org/
version: "1.0.0"
legal_tier: legitimate
kind: json
caps:
  modes: {search: [q], tv-search: [q, season, ep]}
  categories: {books: 7000, films: 2000}
  seeders_unknown: true
settings:
  - name: safe
    type: checkbox
    label: Safe only
request:
  base_url: https://example.org/
  path: api/search
  method: GET
  query:
    q: '{{ if .Config.safe }}safe:({{ .Keywords }}){{ else }}{{ .Keywords }}{{ end }}'
    cats: '{{ range .Categories }}{{ . }},{{ end }}'
    qf: '{{ .Query.Title }} {{ .Today.Year }}'
  headers:
    X-Token: "t-{{ .Config.safe }}"
response:
  rows: "$.rows"
  total: "$.total"
  fields:
    _id:       {path: "id"}
    _hash:     {path: "hash", optional: true}
    title:     {path: "name", optional: true, default: "Untitled {{ .Result._id }}"}
    size:      {path: "bytes", type: bytes}
    category:  {path: "cat"}
    published: {path: "ts", type: datetime, format: unix}
    download:  {template: "https://example.org/d/{{ .Result._id }}.torrent"}
    magnet:    {template: "magnet:?xt=urn:btih:{{ .Result._hash }}", optional: true}
  transforms:
    title:
      - {op: trim}
      - {op: replace, args: ["a", "b"]}
      - {op: regex_capture, args: ["v(\\d+)"]}
    download:
      - {op: query_param, args: ["id"]}
`

// baseStatic is a minimal valid kind: static definition for entry-rule mutations.
const baseStatic = `dlsearch: 1
id: fixture-static
name: Fixture Static
description: "A valid static definition used as the mutation base."
homepage: https://example.org/
version: "1.0.0"
legal_tier: legitimate
kind: static
refresh_note: "Refreshed on each upstream point release."
caps:
  modes: {search: [q]}
  categories: {iso: 4020}
entries:
  - title: "Example ISO"
    download: "https://example.org/example.iso.torrent"
    details: "https://example.org/"
    category: iso
    size: 1234
`

func mutate(base, old, new string) string {
	if !strings.Contains(base, old) {
		panic("mutate: pattern not found in base document: " + old)
	}
	return strings.Replace(base, old, new, 1)
}

// docWithQueryTemplate returns baseJSON with the q query template replaced by tpl.
// Single quotes are doubled because the scalar sits inside a single-quoted YAML
// string.
func docWithQueryTemplate(tpl string) string {
	return mutate(baseJSON,
		`q: '{{ if .Config.safe }}safe:({{ .Keywords }}){{ else }}{{ .Keywords }}{{ end }}'`,
		"q: '"+strings.ReplaceAll(tpl, "'", "''")+"'")
}

func TestLoadValidRSSDefinition(t *testing.T) {
	def, err := loadFixture(t, "def_valid_rss.yaml")
	if err != nil {
		t.Fatalf("LoadDefinition(def_valid_rss.yaml) = %v", err)
	}
	if def.ID != "arch-linux" || def.Kind != "rss" || def.LegalTier != "legitimate" {
		t.Errorf("unexpected identity: %+v", def)
	}
	if def.Request == nil || def.Request.Method != "GET" || def.Request.RateLimitPerMinute != 6 || def.Request.TimeoutSeconds != 15 {
		t.Errorf("unexpected request block: %+v", def.Request)
	}
	if def.Response == nil || len(def.Response.Fields) != 6 {
		t.Errorf("unexpected response block: %+v", def.Response)
	}
	if !def.Caps.SeedersUnknown || def.Caps.Categories["release"] != 4020 {
		t.Errorf("unexpected caps block: %+v", def.Caps)
	}
}

func TestLoadValidStaticDefinition(t *testing.T) {
	def, err := loadFixture(t, "def_static.yaml")
	if err != nil {
		t.Fatalf("LoadDefinition(def_static.yaml) = %v", err)
	}
	if def.Kind != "static" || len(def.Entries) != 3 || def.RefreshNote == "" {
		t.Errorf("unexpected static definition: %+v", def)
	}
}

func TestRequestDefaults(t *testing.T) {
	doc := mutate(baseJSON, "  method: GET\n", "")
	def, err := LoadDefinition([]byte(doc))
	if err != nil {
		t.Fatalf("LoadDefinition = %v", err)
	}
	if def.Request.Method != "GET" || def.Request.RateLimitPerMinute != 30 || def.Request.TimeoutSeconds != 15 {
		t.Errorf("defaults not applied: %+v", def.Request)
	}
}

func TestUnknownKeyNamesTheKey(t *testing.T) {
	_, err := loadFixture(t, "def_unknown_key.yaml")
	wantDefErr(t, err, "bogus_key")
}

func TestOversizeRejectedBeforeParse(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "def_oversize.yaml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if len(data) <= MaxDefinitionBytes {
		t.Fatalf("fixture is %d bytes, want more than %d", len(data), MaxDefinitionBytes)
	}
	_, err = LoadDefinition(data)
	// The fixture body is deliberately not parseable YAML: the byte-limit error
	// can only come from the pre-parse size check.
	wantDefErr(t, err, strconv.Itoa(MaxDefinitionBytes))
	if strings.Contains(err.Error(), "yaml:") {
		t.Errorf("error %q looks like a decoder error; the document was decoded before the size check", err.Error())
	}
}

func TestLanguageTagRejected(t *testing.T) {
	doc := mutate(baseStatic,
		`description: "A valid static definition used as the mutation base."`,
		`description: !!python/object/new:os.system ["echo pwned"]`)
	_, err := LoadDefinition([]byte(doc))
	wantDefErr(t, err, "tag", "python/object/new")
}

func TestUnknownPlaceholderRejected(t *testing.T) {
	_, err := loadFixture(t, "def_bad_placeholder.yaml")
	wantDefErr(t, err, ".Bogus")
}

func TestUnknownTransformOpRejected(t *testing.T) {
	_, err := loadFixture(t, "def_bad_op.yaml")
	wantDefErr(t, err, "uppercase")
}

func TestPatternOverCapRejected(t *testing.T) {
	big := strings.Repeat("x", 600)
	doc := mutate(baseJSON, `{op: regex_capture, args: ["v(\\d+)"]}`, `{op: regex_capture, args: ["`+big+`"]}`)
	_, err := LoadDefinition([]byte(doc))
	wantDefErr(t, err, "512")

	// A nested-quantifier pattern that would backtrack catastrophically under a
	// backtracking engine compiles and loads under RE2 without measurable cost.
	doc = mutate(baseJSON, `{op: regex_capture, args: ["v(\\d+)"]}`, `{op: regex_capture, args: ["(x+x+)+y"]}`)
	if _, err := LoadDefinition([]byte(doc)); err != nil {
		t.Errorf("RE2-safe nested-quantifier pattern rejected: %v", err)
	}
}

func TestStaticRequiresEntriesAndRefreshNote(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"static without entries", strings.SplitN(baseStatic, "entries:", 2)[0], "entries"},
		{"static without refresh_note", mutate(baseStatic, "refresh_note: \"Refreshed on each upstream point release.\"\n", ""), "refresh_note"},
		{"entries on a json definition", baseJSON + "entries:\n  - {title: x, download: \"https://example.org/f\", category: books}\n", "forbidden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadDefinition([]byte(tc.doc))
			wantDefErr(t, err, tc.want)
		})
	}
}

func TestDefinitionRules(t *testing.T) {
	mergeDoc := strings.Replace(baseStatic, "caps:\n", "caps: &caps\n", 1) +
		"extra:\n  <<: *caps\n"

	var manyEntries strings.Builder
	manyEntries.WriteString(strings.SplitN(baseStatic, "entries:", 2)[0] + "entries:\n")
	for i := 0; i <= MaxStaticEntries; i++ {
		manyEntries.WriteString("  - {title: t, download: \"https://example.org/f\", category: iso}\n")
	}

	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"dlsearch not 1", mutate(baseJSON, "dlsearch: 1", "dlsearch: 2"), "must equal 1"},
		{"id pattern", mutate(baseJSON, "id: fixture-json", "id: Bad_ID"), "must match ^[a-z0-9]"},
		{"missing name", mutate(baseJSON, "name: Fixture JSON", "name: \"\""), "name: is required"},
		{"homepage scheme", mutate(baseJSON, "homepage: https://example.org/", "homepage: ftp://example.org/"), "scheme must be http or https"},
		{"homepage credentials", mutate(baseJSON, "homepage: https://example.org/", "homepage: https://user:pass@example.org/"), "credentials"},
		{"homepage no host", mutate(baseJSON, "homepage: https://example.org/", "homepage: https:///path"), "no host"},
		{"unknown kind", mutate(baseJSON, "kind: json", "kind: bogus"), "one of torznab"},
		{"unknown legal_tier", mutate(baseJSON, "legal_tier: legitimate", "legal_tier: shady"), "user-supplied"},
		{"modes without search", mutate(baseJSON, "{search: [q], tv-search: [q, season, ep]}", "{tv-search: [q]}"), `must contain "search"`},
		{"unknown mode key", mutate(baseJSON, "{search: [q], tv-search: [q, season, ep]}", "{search: [q], bogus-search: [q]}"), `unknown mode "bogus-search"`},
		{"non-positive category id", mutate(baseJSON, "{books: 7000, films: 2000}", "{books: 7000, films: -2}"), "positive integer"},
		{"bad setting type", mutate(baseJSON, "type: checkbox", "type: toggler"), `"toggler"`},
		{"duplicate setting name", mutate(baseJSON, "    label: Safe only", "    label: Safe only\n  - name: safe\n    type: text"), "duplicate"},
		{"unusable setting name", mutate(baseJSON, "name: safe", "name: sa.fe"), ".Config member name"},
		{"hyphenated setting name", mutate(baseJSON, "name: safe", "name: api-key"), ".Config member name"},
		{"digit-first setting name", mutate(baseJSON, "name: safe", "name: 2fa"), ".Config member name"},
		{"select without options", mutate(baseJSON, "type: checkbox", "type: select"), "type is select"},
		{"base_url scheme", mutate(baseJSON, "base_url: https://example.org/", "base_url: file:///etc/"), "scheme must be http or https"},
		{"base_url credentials", mutate(baseJSON, "base_url: https://example.org/", "base_url: https://user:pass@example.org/"), "credentials"},
		{"empty base_url", mutate(baseJSON, "base_url: https://example.org/", "base_url: \"\""), "request.base_url"},
		{"method not GET", mutate(baseJSON, "method: GET", "method: POST"), "must be GET"},
		{"rate limit over cap", mutate(baseJSON, "method: GET", "method: GET\n  rate_limit_per_minute: 121"), "at most 120"},
		{"timeout over cap", mutate(baseJSON, "method: GET", "method: GET\n  timeout_seconds: 16"), "at most 15"},
		{"authorization header", mutate(baseJSON, "X-Token", "Authorization"), "Authorization"},
		{"cookie header lowercase", mutate(baseJSON, "X-Token", "cookie"), "header is not allowed"},
		{"header control characters", mutate(baseJSON, `X-Token: "t-{{ .Config.safe }}"`, `X-Token: "a\r\nb"`), "control characters"},
		{"header name control characters", mutate(baseJSON, `X-Token: "t-{{ .Config.safe }}"`, `"X`+`\n`+`Token": "t"`), "control characters"},
		{"header name with space", mutate(baseJSON, `X-Token: "t-{{ .Config.safe }}"`, `X Token: "t"`), "control characters"},
		{"if without else", mutate(baseJSON, "){{ else }}{{ .Keywords }}{{ end }}", "){{ .Keywords }}{{ end }}"), "mandatory {{ else }}"},
		{"else outside if", mutate(baseJSON, "qf: '{{ .Query.Title }} {{ .Today.Year }}'", "qf: '{{ else }}'"), "outside an {{ if }}"},
		{"end without block", mutate(baseJSON, "qf: '{{ .Query.Title }} {{ .Today.Year }}'", "qf: '{{ end }}'"), "without a block"},
		{"unclosed action", mutate(baseJSON, "qf: '{{ .Query.Title }} {{ .Today.Year }}'", "qf: '{{ .Keywords'"), `unclosed "{{"`},
		{"unclosed block", mutate(baseJSON, "){{ else }}{{ .Keywords }}{{ end }}", "){{ else }}{{ .Keywords }}"), "unclosed {{ if }} block"},
		{"unterminated literal", docWithQueryTemplate(`{{ "unterminated`), `unclosed "{{"`},
		{"unterminated raw literal", docWithQueryTemplate("{{ `unterminated"), `unclosed "{{"`},
		{"unknown function", mutate(baseJSON, "{{ else }}{{ .Keywords }}", "{{ else }}{{ eval .Keywords }}"), `token "eval"`},
		{"dot outside range", mutate(baseJSON, "qf: '{{ .Query.Title }} {{ .Today.Year }}'", "qf: '{{ . }}'"), "only valid inside"},
		{"range over non-categories", mutate(baseJSON, "{{ range .Categories }}", "{{ range .Keywords }}"), "may only walk"},
		{"nested range", docWithQueryTemplate("{{ range .Categories }}{{ range .Categories }}{{ end }}{{ end }}"), "may not nest"},
		{"bad trim marker suffix", docWithQueryTemplate("{{ .Keywords-}}"), "unknown placeholder"},
		{"bad trim marker prefix", docWithQueryTemplate("{{-end}}"), "unknown template token"},
		{"floating trim marker", docWithQueryTemplate("{{ .Keywords - }}"), `token "-"`},
		{"unknown query field", mutate(baseJSON, "{{ .Query.Title }}", "{{ .Query.Bogus }}"), "query field"},
		{"undeclared config setting", mutate(baseJSON, "t-{{ .Config.safe }}", "t-{{ .Config.missing }}"), ".Config.missing"},
		{"undeclared result field", mutate(baseJSON, "Untitled {{ .Result._id }}", "Untitled {{ .Result.nosuch }}"), ".Result.nosuch"},
		{"today member", mutate(baseJSON, "{{ .Today.Year }}", "{{ .Today.Month }}"), ".Year member"},
		{"member on scalar placeholder", mutate(baseJSON, "safe:({{ .Keywords }})", "safe:({{ .Keywords.Length }})"), ".Keywords.Length"},
		{"two field sources", mutate(baseJSON, `title:     {path: "name", optional: true, default: "Untitled {{ .Result._id }}"}`, `title:     {path: "name", template: "x"}`), "exactly one"},
		{"attr without path", mutate(baseJSON, `_id:       {path: "id"}`, `_id:       {template: "x", attr: "href"}`), "requires path"},
		{"unknown field type", mutate(baseJSON, `{path: "bytes", type: bytes}`, `{path: "bytes", type: bignum}`), `"bignum"`},
		{"datetime without format", mutate(baseJSON, `{path: "ts", type: datetime, format: unix}`, `{path: "ts", type: datetime}`), "needs one of iso8601"},
		{"format without datetime", mutate(baseJSON, `{path: "bytes", type: bytes}`, `{path: "bytes", type: bytes, format: unix}`), "only valid with type datetime"},
		{"default without optional", mutate(baseJSON, `title:     {path: "name", optional: true, default: "Untitled {{ .Result._id }}"}`, `title:     {path: "name", default: "x"}`), "optional"},
		{"missing title field", mutate(baseJSON, "    title:     {path: \"name\", optional: true, default: \"Untitled {{ .Result._id }}\"}\n", ""), `missing required field "title"`},
		{"missing size field", mutate(baseJSON, "    size:      {path: \"bytes\", type: bytes}\n", ""), `missing required field "size"`},
		{"no download source", mutate(baseJSON, "    download:  {template: \"https://example.org/d/{{ .Result._id }}.torrent\"}\n    magnet:    {template: \"magnet:?xt=urn:btih:{{ .Result._hash }}\", optional: true}\n", ""), "at least one of download"},
		{"missing category field", mutate(baseJSON, "    category:  {path: \"cat\"}\n", ""), `missing required field "category"`},
		{"replace arity", mutate(baseJSON, `{op: replace, args: ["a", "b"]}`, `{op: replace, args: ["a"]}`), "exactly 2"},
		{"split arity", mutate(baseJSON, `{op: query_param, args: ["id"]}`, `{op: split, args: [","]}`), "exactly 2"},
		{"split index not int", mutate(baseJSON, `{op: query_param, args: ["id"]}`, `{op: split, args: [",", "x"]}`), "integer index"},
		{"regex does not compile", mutate(baseJSON, `args: ["v(\\d+)"]`, `args: ["v("]`), "does not compile"},
		{"transform undeclared field", mutate(baseJSON, "    download:\n      - {op: query_param", "    nosuch:\n      - {op: query_param"), "not declared"},
		{"torznab forbids request", mutate(baseJSON, "kind: json", "kind: torznab"), `forbidden for kind "torznab"`},
		{"static forbids response", mutate(baseStatic, "kind: static\nrefresh_note", "kind: static\nresponse: {}\nrefresh_note"), `forbidden for kind "static"`},
		{"rss missing rows", mutate(baseJSON, `rows: "$.rows"`, `rows: ""`), "response.rows"},
		{"second document", baseJSON + "---\ndlsearch: 1\n", "more than one"},
		{"merge key tag", mergeDoc, "!!merge"},
		{"timestamp tag", mutate(baseStatic, `version: "1.0.0"`, `version: 2024-01-01`), "!!timestamp"},
		{"too many entries", manyEntries.String(), "500-entry limit"},
		{"entry http download", mutate(baseStatic, `download: "https://example.org/example.iso.torrent"`, `download: "http://example.org/example.iso.torrent"`), "scheme must be https"},
		{"entry http details", mutate(baseStatic, `details: "https://example.org/"`, `details: "http://example.org/"`), "scheme must be https"},
		{"entry credentials in download", mutate(baseStatic, `download: "https://example.org/example.iso.torrent"`, `download: "https://user:pass@example.org/x.torrent"`), "credentials"},
		{"entry undeclared category", mutate(baseStatic, "category: iso", "category: movie"), "not a declared caps.categories key"},
		{"entry no source", mutate(baseStatic, `    download: "https://example.org/example.iso.torrent"`+"\n", ""), "at least one"},
		{"entry bad magnet", mutate(baseStatic, `size: 1234`, `size: 1234`+"\n    magnet: \"http://example.org/x\""), "magnet: scheme"},
		{"entry bad infohash", mutate(baseStatic, `size: 1234`, `size: 1234`+"\n    infohash: \"zzzz\""), "lowercase hex"},
		{"entry missing title", mutate(baseStatic, `title: "Example ISO"`, `title: ""`), "entries.0.title"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadDefinition([]byte(tc.doc))
			wantDefErr(t, err, tc.want)
		})
	}
}

// TestValidDefinitionVariants covers documents that must still load: escaped
// quotes and "}}" inside string literals, backquoted raw literals, both
// trim-marker spellings, a stray "}}" in literal text, a negative split index
// (doc 07 section 3.4 counts from the end), a template-expanded prepend arg, an
// uppercase MAGNET scheme and a single-category definition with no category
// field.
func TestValidDefinitionVariants(t *testing.T) {
	magnetEntry := mutate(baseStatic,
		`    download: "https://example.org/example.iso.torrent"`,
		`    magnet: "MAGNET:?xt=urn:btih:0b1ec1a478f2b3c793bf88a45e9c0d6d81f2a3b4"`)
	splitFromEnd := mutate(baseJSON,
		`{op: query_param, args: ["id"]}`,
		`{op: split, args: [",", "-1"]}`)
	prependTemplated := mutate(baseJSON,
		`{op: query_param, args: ["id"]}`,
		`{op: prepend, args: ["{{ .Result._id }}-"]}`)
	noCategoryField := mutate(
		mutate(baseJSON, "{books: 7000, films: 2000}", "{books: 7000}"),
		"    category:  {path: \"cat\"}\n", "")

	templates := []string{
		`{{ "say \"hi\"" }}`,
		`{{ "it's" }}`,
		`{{ "}}" }}`,
		"{{ `a}}b` }}",
		"{{ if eq .Keywords `a}}b` }}x{{ else }}y{{ end }}",
		`{{- .Keywords -}}`,
		`{{ .Keywords -}}`,
		`{{- .Keywords }}`,
		`a }} b {{ .Query.Q }}`,
		`{{ .Query.Q }} a }}`,
		`{{ if eq .Page 1 }}p1{{ else }}pn{{ end }}`,
		`{{ range .Categories }}{{ if . }}{{ . }}{{ else }}-{{ end }}{{ end }}`,
	}
	for _, tpl := range templates {
		t.Run("template "+tpl, func(t *testing.T) {
			if _, err := LoadDefinition([]byte(docWithQueryTemplate(tpl))); err != nil {
				t.Errorf("template %q rejected: %v", tpl, err)
			}
		})
	}

	docs := map[string]string{
		"uppercase magnet scheme":   magnetEntry,
		"negative split index":      splitFromEnd,
		"template prepend argument": prependTemplated,
		"single category, no field": noCategoryField,
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadDefinition([]byte(doc)); err != nil {
				t.Errorf("rejected: %v", err)
			}
		})
	}
}
