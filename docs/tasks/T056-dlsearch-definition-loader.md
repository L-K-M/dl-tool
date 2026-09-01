# T056 — Load and validate `dlsearch/v1` definitions

| Field | Value |
|---|---|
| **ID** | T056 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T004, T005 |
| **Blocks** | T057, T058, T059, T060, T105 |
| **Parallel-safe** | no — it also edits the shared file `internal/search/testdata/` |
| **Implements** | [FR-051](../02-requirements.md#fr-051-load-declarative-dlsearchv1-engines), [NFR-019](../02-requirements.md#nfr-019-parse-untrusted-definitions-and-regexes-defensively) |
| **Decisions** | [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md), [ADR-0008](../decisions/0008-torznab-first-declarative-yaml-second.md) |
| **Est. size** | 2 new files, ~430 LOC |

## Goal
`search.LoadDefinition` turns one `dlsearch/v1` YAML document into a validated `*Definition` or into an
error naming the offending key and line. It rejects an unknown key, a document over 512 KiB, a placeholder
outside the closed set, a transform op outside the closed set, and a pattern over 512 bytes. It parses only;
nothing is fetched and nothing is executed.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/07-search-and-indexers.md` §3.1 Field table](../07-search-and-indexers.md#31-field-table) — every
   path, type, required flag and rule.
2. [`docs/07-search-and-indexers.md` §3.2 Kinds](../07-search-and-indexers.md#32-kinds) — which blocks each
   kind requires and which it forbids.
3. [`docs/07-search-and-indexers.md` §3.3 Template placeholders](../07-search-and-indexers.md#33-template-placeholders--a-fixed-closed-set)
   and [§3.4 Transform ops](../07-search-and-indexers.md#34-transform-ops--a-fixed-closed-set) — both closed sets.
4. [`docs/07-search-and-indexers.md` §3.5 Hard limits](../07-search-and-indexers.md#35-hard-limits) and
   [§3.8 Worked example — `kind: static`](../07-search-and-indexers.md#38-worked-example--kind-static) — the
   `entries[]` record table and the 500-entry cap.
5. [`docs/12-security-and-threat-model.md` §5.1 YAML parsing](../12-security-and-threat-model.md#51-yaml-parsing)
   and [§5.2 Selectors and regexes](../12-security-and-threat-model.md#52-selectors-and-regexes).
6. [`docs/14-conventions.md` §8.2 Add a `dlsearch/v1` definition kind](../14-conventions.md#82-add-a-dlsearchv1-definition-kind).

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/search/definition.go` | create | `Definition`, `LoadDefinition`, `DefinitionError` and the closed sets. |
| `internal/search/definition_test.go` | create | Valid, invalid and limit cases over the fixtures. |
| `internal/search/testdata/` | modify | Add `def_valid_rss.yaml`, `def_unknown_key.yaml`, `def_bad_placeholder.yaml`, `def_bad_op.yaml`, `def_static.yaml` and a 600 KiB `def_oversize.yaml`. |

No other file may be modified.

## Interface contract

```go
package search

// Definition is one dlsearch/v1 document, 07-search-and-indexers.md section 3.1 field
// for field. Every struct field carries a yaml tag; the decoder runs with
// KnownFields(true), so an unknown key is an error.
type Definition struct {
	DLSearch    int    `yaml:"dlsearch"`     // must equal 1
	ID          string `yaml:"id"`           // ^[a-z0-9][a-z0-9-]{1,63}$
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Homepage    string `yaml:"homepage"`
	Kind        string `yaml:"kind"`         // torznab | rss | json | html | static
	Version     string `yaml:"version"`
	LegalTier   string `yaml:"legal_tier"`   // legitimate | user-supplied
	Maintainer  string `yaml:"maintainer"`
	LicenseNote string `yaml:"license_note"`
	AllowPrivateNetwork bool `yaml:"allow_private_network"`
	Caps        DefCaps    `yaml:"caps"`
	Settings    []Setting  `yaml:"settings"`
	Request     *Request   `yaml:"request"`
	Response    *Response  `yaml:"response"`
	Entries     []Entry    `yaml:"entries"`
	RefreshNote string     `yaml:"refresh_note"`
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
	Type    string            `yaml:"type"`  // info|text|password|checkbox|select
	Label   string            `yaml:"label"`
	Default string            `yaml:"default"`
	Options map[string]string `yaml:"options"`
}

type Request struct {
	BaseURL            string            `yaml:"base_url"`
	Path               string            `yaml:"path"`
	Method             string            `yaml:"method"`  // GET only in v1
	Query              map[string]string `yaml:"query"`
	Headers            map[string]string `yaml:"headers"` // Authorization and Cookie are rejected
	RateLimitPerMinute int               `yaml:"rate_limit_per_minute"` // default 30, max 120
	TimeoutSeconds     int               `yaml:"timeout_seconds"`       // default 20, max 30
}

type Response struct {
	Rows       string                     `yaml:"rows"`
	Total      string                     `yaml:"total"`
	Fields     map[string]Field           `yaml:"fields"`
	Transforms map[string][]TransformOp   `yaml:"transforms"`
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

// Placeholders and TransformOps are the two closed sets. A token outside them is a
// load-time error; there is no fallthrough to text/template.
var Placeholders = []string{
	"Keywords", "Page", "Limit", "Offset", "Categories", "Query", "Config", "Result", "Today",
}
var TransformOps = []string{
	"trim", "lower", "upper", "html_decode", "url_decode", "prepend", "append",
	"replace", "regex_capture", "split", "query_param", "strip_html",
}

// LoadDefinition validates size, then decodes into Definition, then runs the field,
// kind, placeholder, op and limit rules. err is always a *DefinitionError.
func LoadDefinition(data []byte) (*Definition, error)

// DefinitionError names the limit or key that was violated, with the YAML line when the
// decoder supplied one.
type DefinitionError struct {
	Line int
	Path string // e.g. "response.fields.title.type"
	Msg  string
}

func (e *DefinitionError) Error() string
```

## Steps
1. Create `internal/search/definition.go` with the structs above and the limit constants, exactly as the
   contract gives them.
2. Reject `len(data) > MaxDefinitionBytes` before parsing, and run the decode under a
   `context.WithTimeout(ctx, MaxParseDuration)` guard.
3. Decode with `yaml.NewDecoder(bytes.NewReader(data))` and `Decoder.KnownFields(true)` into the concrete
   struct — never into `map[string]any` — and reject any node whose tag is not one of `!!str`, `!!int`,
   `!!bool`, `!!float`, `!!map`, `!!seq` or `!!null`.
4. Validate the ubiquitous rules: `dlsearch == 1`, `id` against `^[a-z0-9][a-z0-9-]{1,63}$`, `homepage` and
   `request.base_url` limited to `http`/`https`, `legal_tier` and `kind` inside their enums, `caps.modes`
   containing `search`, and every `caps.categories` value a positive integer.
5. Validate per kind, per doc 07 §3.2: `torznab` and `static` forbid `request` and `response`; `rss`, `json`
   and `html` require `request.base_url`, `response.rows` and `response.fields`; `static` requires
   `entries[]` and `refresh_note`; `entries[]` is forbidden everywhere else.
6. Validate `response.fields`: exactly one of `path`, `template` or `const`; `title`, `size` and at least one
   of `download`, `magnet`, `infohash`; `category` unless `caps.categories` has exactly one entry; `default`
   only together with `optional: true`; `datetime` only with a `format` from the closed list.
7. Validate every template string in `request.query`, `request.headers`, `field.template` and
   `field.default` by tokenising `{{ … }}` and checking the head against `Placeholders`, the control words
   `if`, `else`, `end`, `range` and the functions `join`, `eq`, `ne`, `and`, `or`, `not`. Reject a
   `{{ if }}` with no `{{ else }}`.
8. Validate transforms: every `op` in `TransformOps`, `replace` with exactly two args, `split` with two,
   `regex_capture` with one pattern that compiles with `regexp.Compile` and is at most `MaxPatternBytes`.
9. Clamp and validate the request limits — `rate_limit_per_minute` default 30 maximum 120, `timeout_seconds`
   default 20 maximum 30 — and reject the headers `Authorization` and `Cookie` case-insensitively.
10. Validate `entries[]`: at most `MaxStaticEntries`, `title` and `category` present, `category` a declared
    `caps.categories` key, at least one of `download`, `magnet`, `infohash`, and every URL `https`.
11. Create `internal/search/definition_test.go` with `TestLoadValidRSSDefinition`,
    `TestUnknownKeyNamesTheKey`, `TestOversizeRejectedBeforeParse`, `TestLanguageTagRejected`,
    `TestUnknownPlaceholderRejected`, `TestUnknownTransformOpRejected`, `TestPatternOverCapRejected` and
    `TestStaticRequiresEntriesAndRefreshNote`.
12. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestUnknownKeyNamesTheKey` asserts the error text contains the offending key name.
- [ ] `TestOversizeRejectedBeforeParse` asserts the 600 KiB fixture is rejected with the byte limit in the message and that no decode was attempted.
- [ ] `TestLanguageTagRejected` asserts a document containing a language-specific tag is refused.
- [ ] `TestPatternOverCapRejected` asserts a 600-byte `regex_capture` pattern is refused, and the suite completes in under two seconds with a catastrophically backtracking pattern present.
- [ ] `LoadDefinition` performs no network I/O: the package's test binary passes with `-race` and no `httptest` server started.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/search/... && echo DEFINITION_OK
```
Expected: `ok  	github.com/L-K-M/dl-tool/internal/search`, every test named in step 11 listed as passing,
and the final line of stdout exactly `DEFINITION_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT expand a template or run a transform here; T058 owns `Expand` and `ApplyTransforms`.
- Do NOT fetch anything, and do NOT add `kind: html` row extraction; T058 owns fetching.
- Do NOT read from `/config/engines` or embed the bundled files; T057 owns the registry.
- Do NOT add a placeholder, a control word, a function or a transform op beyond the two closed sets.
- Do NOT accept `request.paths[]` or a second request path; that is deferred to v2.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>
