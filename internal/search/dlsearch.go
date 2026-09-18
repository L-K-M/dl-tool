// This file is the dlsearch/v1 evaluator: the closed placeholder set, the
// twelve transform ops, the rss and json row extractors, the guarded fetch
// and the one-request indexer probe. Every template token and every op is
// dispatched by an explicit switch — there is no text/template, no os/exec
// and no JSONPath dependency (docs/07-search-and-indexers.md sections 3.3
// through 3.5, ADR-0010).
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/L-K-M/dl-tool/internal/secure"
)

// The hard limits of docs/07-search-and-indexers.md section 3.5 that this
// file owns. The byte caps live beside the fetch; the expansion caps beside
// Expand.
const (
	maxExpandOutput = 64 << 10 // template expansion output
	maxExpandIters  = 1000     // total {{ range }} iterations per expansion
	maxExpandDepth  = 8        // if/range nesting
	maxParsedDoc    = 2 << 20  // parsed document, under the 8 MiB body cap
	maxResultRows   = 1000     // results per page
	engineDeadline  = 15 * time.Second
)

// ErrRateLimited is returned when the engine's own rate limit would be
// exceeded — the runner never sleeps past the 15 s deadline to wait a bucket
// out, so the call fails instead.
var ErrRateLimited = errors.New("search: engine rate limit exceeded")

// Scope is the only data a template can see. There is no reflection over
// anything else.
type Scope struct {
	Keywords   string
	Page       int
	Limit      int
	Offset     int
	Categories []string          // site-side values mapped from the requested newznab ids
	Query      map[string]string // Q, Season, Ep, Year, Genre, IMDBID, IMDBIDShort, TMDBID,
	// TVDBID, TVMazeID, Artist, Album, Label, Track, Author,
	// Title, Publisher
	Config    map[string]string // declared settings[].name values; a checkbox is "true" or ""
	Result    map[string]string // fields already resolved for the current row
	TodayYear string
}

// Expand walks the template with an explicit switch over the closed token
// set. It never calls text/template. Output is capped at 64 KiB, loops at
// 1000 iterations and nesting at depth 8; exceeding any of them returns an
// error naming the limit.
func Expand(tmpl string, s Scope) (string, error) {
	toks, err := scanTemplate(tmpl)
	if err != nil {
		return "", err
	}
	e := &expander{}
	pos := 0
	term, err := e.block(toks, &pos, evalCtx{s: s}, 0)
	if err != nil {
		return "", err
	}
	if term != "" {
		return "", fmt.Errorf("search: template: {{ %s }} without a block", term)
	}
	return e.out.String(), nil
}

// templateTok is one literal run or one lexed {{ ... }} action.
type templateTok struct {
	lit    string
	action []string // nil for a literal
}

// scanTemplate splits tmpl into literals and actions, reusing the loader's
// actionEnd/lexTemplate pair so quoting and trim-marker rules stay identical
// between validation and expansion.
func scanTemplate(tmpl string) ([]templateTok, error) {
	var out []templateTok
	rest := tmpl
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			if rest != "" {
				out = append(out, templateTok{lit: rest})
			}
			return out, nil
		}
		if i > 0 {
			out = append(out, templateTok{lit: rest[:i]})
		}
		end := actionEnd(rest[i+2:])
		if end < 0 {
			return nil, errors.New(`search: template: unclosed "{{"`)
		}
		tokens, err := lexTemplate(rest[i+2 : i+2+end])
		if err != nil {
			return nil, fmt.Errorf("search: template: %w", err)
		}
		if len(tokens) == 0 {
			return nil, errors.New("search: template: empty {{ }} action")
		}
		out = append(out, templateTok{action: tokens})
		rest = rest[i+2+end+2:]
	}
}

// evalCtx carries the scope plus the range element a bare {{ . }} resolves
// to; inRange distinguishes "inside a range over an empty-string element"
// from "outside any range", where {{ . }} is invalid.
type evalCtx struct {
	s       Scope
	dot     string
	inRange bool
}

// expander accumulates the output and counts loop iterations against the
// caps of section 3.5.
type expander struct {
	out   strings.Builder
	iters int
}

func (e *expander) write(s string) error {
	if e.out.Len()+len(s) > maxExpandOutput {
		return fmt.Errorf("search: template expansion over the %d-byte output cap", maxExpandOutput)
	}
	e.out.WriteString(s)
	return nil
}

// block evaluates toks[*pos:] until an unmatched {{ else }} or {{ end }}
// terminator, which it returns. Running off the end of the slice returns "".
func (e *expander) block(toks []templateTok, pos *int, cx evalCtx, depth int) (string, error) {
	if depth > maxExpandDepth {
		return "", fmt.Errorf("search: template nesting deeper than %d", maxExpandDepth)
	}
	for *pos < len(toks) {
		t := toks[*pos]
		*pos++
		if t.action == nil {
			if err := e.write(t.lit); err != nil {
				return "", err
			}
			continue
		}
		switch t.action[0] {
		case "else", "end":
			return t.action[0], nil
		case "if":
			then, els, next, err := splitBranches(toks, *pos)
			if err != nil {
				return "", err
			}
			*pos = next
			cond, err := evalCond(t.action[1:], cx)
			if err != nil {
				return "", err
			}
			body := els
			if cond {
				body = then
			}
			p := 0
			term, err := e.block(body, &p, cx, depth+1)
			if err != nil {
				return "", err
			}
			if term != "" {
				return "", fmt.Errorf("search: template: {{ %s }} outside its block", term)
			}
		case "range":
			if len(t.action) != 2 || t.action[1] != ".Categories" {
				return "", errors.New("search: template: {{ range }} may only walk .Categories")
			}
			end, err := matchingEnd(toks, *pos)
			if err != nil {
				return "", err
			}
			body := toks[*pos:end]
			*pos = end + 1
			for _, item := range cx.s.Categories {
				e.iters++
				if e.iters > maxExpandIters {
					return "", fmt.Errorf("search: template loop over %d iterations", maxExpandIters)
				}
				p := 0
				term, err := e.block(body, &p, evalCtx{s: cx.s, dot: item, inRange: true}, depth+1)
				if err != nil {
					return "", err
				}
				if term != "" {
					return "", fmt.Errorf("search: template: {{ %s }} outside its block", term)
				}
			}
		default:
			v, err := evalAction(t.action, cx)
			if err != nil {
				return "", err
			}
			if err := e.write(v); err != nil {
				return "", err
			}
		}
	}
	return "", nil
}

// splitBranches returns the then- and else-spans of the {{ if }} whose body
// starts at start, plus the index just past its {{ end }}. The else branch is
// mandatory per section 3.3.
func splitBranches(toks []templateTok, start int) (then, els []templateTok, next int, err error) {
	depth := 0
	elseIdx := -1
	for i := start; i < len(toks); i++ {
		a := toks[i].action
		if a == nil {
			continue
		}
		switch a[0] {
		case "if", "range":
			depth++
		case "end":
			if depth == 0 {
				if elseIdx < 0 {
					return nil, nil, 0, errors.New(`search: template: {{ if }} without the mandatory {{ else }}`)
				}
				return toks[start:elseIdx], toks[elseIdx+1 : i], i + 1, nil
			}
			depth--
		case "else":
			if depth == 0 {
				if elseIdx >= 0 {
					return nil, nil, 0, errors.New("search: template: duplicate {{ else }}")
				}
				elseIdx = i
			}
		}
	}
	return nil, nil, 0, errors.New(`search: template: unclosed {{ if }} block`)
}

// matchingEnd returns the index of the {{ end }} that closes the {{ range }}
// whose body starts at start.
func matchingEnd(toks []templateTok, start int) (int, error) {
	depth := 0
	for i := start; i < len(toks); i++ {
		a := toks[i].action
		if a == nil {
			continue
		}
		switch a[0] {
		case "if", "range":
			depth++
		case "end":
			if depth == 0 {
				return i, nil
			}
			depth--
		}
	}
	return 0, errors.New(`search: template: unclosed {{ range }} block`)
}

// value is a resolved operand: a scalar, or a list when the token named
// .Categories — the only list the closed set exposes.
type value struct {
	s   string
	lst []string
}

func (v value) text() string {
	if v.lst != nil {
		return strings.Join(v.lst, ",")
	}
	return v.s
}

// truthy is the if-condition rule: an empty list, an empty string and the
// literal "false" or "0" are false; anything else is true.
func (v value) truthy() bool {
	if v.lst != nil {
		return len(v.lst) > 0
	}
	return v.s != "" && v.s != "false" && v.s != "0"
}

// evalAction renders a non-control action to the output string.
func evalAction(tokens []string, cx evalCtx) (string, error) {
	if len(tokens) == 1 {
		v, err := resolve(tokens[0], cx)
		if err != nil {
			return "", err
		}
		return v.text(), nil
	}
	if templateFuncs[tokens[0]] {
		return evalFunc(tokens[0], tokens[1:], cx)
	}
	return "", fmt.Errorf("search: template: unknown token %q", tokens[0])
}

// evalCond evaluates an {{ if }} condition: a single operand's truthiness, or
// one of the closed-set functions.
func evalCond(tokens []string, cx evalCtx) (bool, error) {
	if len(tokens) == 0 {
		return false, errors.New("search: template: {{ if }} needs a condition")
	}
	if len(tokens) == 1 && !templateFuncs[tokens[0]] {
		v, err := resolve(tokens[0], cx)
		if err != nil {
			return false, err
		}
		return v.truthy(), nil
	}
	if !templateFuncs[tokens[0]] {
		return false, fmt.Errorf("search: template: unknown token %q", tokens[0])
	}
	out, err := evalFunc(tokens[0], tokens[1:], cx)
	if err != nil {
		return false, err
	}
	return value{s: out}.truthy(), nil
}

// evalFunc runs one of the six closed-set functions. join takes the list and
// the separator in either order — the disambiguation is by operand type.
func evalFunc(name string, args []string, cx evalCtx) (string, error) {
	vals := make([]value, len(args))
	for i, a := range args {
		v, err := resolve(a, cx)
		if err != nil {
			return "", err
		}
		vals[i] = v
	}
	b2s := func(b bool) string {
		if b {
			return "true"
		}
		return ""
	}
	switch name {
	case "join":
		if len(vals) != 2 {
			return "", fmt.Errorf("search: template: join takes a list and a separator, got %d args", len(vals))
		}
		switch {
		case vals[0].lst != nil:
			return strings.Join(vals[0].lst, vals[1].s), nil
		case vals[1].lst != nil:
			return strings.Join(vals[1].lst, vals[0].s), nil
		default:
			return "", errors.New("search: template: join needs a list operand")
		}
	case "eq", "ne":
		if len(vals) != 2 {
			return "", fmt.Errorf("search: template: %s takes two args, got %d", name, len(vals))
		}
		return b2s((name == "eq") == valsEqual(vals[0], vals[1])), nil
	case "and":
		for _, v := range vals {
			if !v.truthy() {
				return "", nil
			}
		}
		return "true", nil
	case "or":
		for _, v := range vals {
			if v.truthy() {
				return "true", nil
			}
		}
		return "", nil
	case "not":
		if len(vals) != 1 {
			return "", fmt.Errorf("search: template: not takes one arg, got %d", len(vals))
		}
		return b2s(!vals[0].truthy()), nil
	}
	return "", fmt.Errorf("search: template: unknown function %q", name)
}

// valsEqual compares lists elementwise and scalars numerically when both
// parse as numbers, else as strings.
func valsEqual(a, b value) bool {
	if a.lst != nil || b.lst != nil {
		if a.lst == nil || b.lst == nil || len(a.lst) != len(b.lst) {
			return false
		}
		for i := range a.lst {
			if a.lst[i] != b.lst[i] {
				return false
			}
		}
		return true
	}
	an, aerr := strconv.ParseFloat(a.s, 64)
	bn, berr := strconv.ParseFloat(b.s, 64)
	if aerr == nil && berr == nil {
		return an == bn
	}
	return a.s == b.s
}

// resolve evaluates one operand token: a placeholder, a literal, a number, a
// boolean or the range dot.
func resolve(tok string, cx evalCtx) (value, error) {
	switch {
	case tok == ".":
		if !cx.inRange {
			return value{}, errors.New(`search: template: {{ . }} is only valid inside {{ range .Categories }}`)
		}
		return value{s: cx.dot}, nil
	case strings.HasPrefix(tok, "."):
		return resolvePlaceholder(tok, cx)
	case len(tok) >= 2 && tok[0] == '"' && tok[len(tok)-1] == '"':
		unq, err := strconv.Unquote(tok)
		if err != nil {
			return value{}, fmt.Errorf("search: template: bad string literal %q", tok)
		}
		return value{s: unq}, nil
	case len(tok) >= 2 && tok[0] == '`' && tok[len(tok)-1] == '`':
		return value{s: tok[1 : len(tok)-1]}, nil
	case tok == "true" || tok == "false":
		return value{s: tok}, nil
	default:
		if _, err := strconv.ParseFloat(tok, 64); err == nil {
			return value{s: tok}, nil
		}
		return value{}, fmt.Errorf("search: template: unknown token %q", tok)
	}
}

// resolvePlaceholder maps a ".Head[.Sub]" token onto Scope. A head outside
// the closed set — .Env, .Secrets, .Config chains deeper than one member — is
// an error naming the token, never a zero value.
func resolvePlaceholder(tok string, cx evalCtx) (value, error) {
	parts := strings.Split(tok[1:], ".")
	head, subs := parts[0], parts[1:]
	scalar := func(s string, ok bool) (value, error) {
		if !ok {
			return value{}, fmt.Errorf("search: template: unknown token %q", tok)
		}
		return value{s: s}, nil
	}
	switch head {
	case "Keywords":
		return scalar(cx.s.Keywords, len(subs) == 0)
	case "Page":
		return scalar(strconv.Itoa(cx.s.Page), len(subs) == 0)
	case "Limit":
		return scalar(strconv.Itoa(cx.s.Limit), len(subs) == 0)
	case "Offset":
		return scalar(strconv.Itoa(cx.s.Offset), len(subs) == 0)
	case "Categories":
		if len(subs) != 0 {
			return value{}, fmt.Errorf("search: template: unknown token %q", tok)
		}
		return value{lst: cx.s.Categories}, nil
	case "Query":
		if len(subs) != 1 || !queryFields[subs[0]] {
			return value{}, fmt.Errorf("search: template: unknown token %q", tok)
		}
		return value{s: cx.s.Query[subs[0]]}, nil
	case "Config":
		if len(subs) != 1 {
			return value{}, fmt.Errorf("search: template: unknown token %q", tok)
		}
		return value{s: cx.s.Config[subs[0]]}, nil
	case "Result":
		if len(subs) != 1 {
			return value{}, fmt.Errorf("search: template: unknown token %q", tok)
		}
		return value{s: cx.s.Result[subs[0]]}, nil
	case "Today":
		return scalar(cx.s.TodayYear, len(subs) == 1 && subs[0] == "Year")
	}
	return value{}, fmt.Errorf("search: template: unknown token %q", tok)
}

// usesKeywords reports whether any request.query template references
// {{ .Keywords }} — when none does, the engine is browse-style and the rows
// are filtered in process (doc 07 section 3.6).
func usesKeywords(query map[string]string) bool {
	for _, v := range query {
		toks, err := scanTemplate(v)
		if err != nil {
			continue
		}
		for _, t := range toks {
			for _, tok := range t.action {
				if tok == ".Keywords" {
					return true
				}
			}
		}
	}
	return false
}

// ApplyTransforms runs the ordered op list. Every op is dispatched by an
// explicit switch; an unknown op is an error, never a no-op.
func ApplyTransforms(v string, ops []TransformOp, s Scope) (string, error) {
	var err error
	for _, op := range ops {
		v, err = applyOp(v, op, s)
		if err != nil {
			return "", err
		}
	}
	return v, nil
}

func applyOp(v string, op TransformOp, s Scope) (string, error) {
	// LoadDefinition enforces these arities, but ApplyTransforms is exported
	// and reachable without the loader — an op missing its argument must be
	// an error here too, never an index-out-of-range panic.
	switch op.Op {
	case "prepend", "append", "regex_capture", "query_param":
		if len(op.Args) < 1 {
			return "", fmt.Errorf("search: transform %q needs an argument", op.Op)
		}
	case "replace", "split":
		if len(op.Args) < 2 {
			return "", fmt.Errorf("search: transform %q needs two arguments", op.Op)
		}
	}
	switch op.Op {
	case "trim":
		if len(op.Args) == 1 {
			return strings.Trim(v, op.Args[0]), nil
		}
		return strings.TrimSpace(v), nil
	case "lower":
		return strings.ToLower(v), nil
	case "upper":
		return strings.ToUpper(v), nil
	case "html_decode":
		return html.UnescapeString(v), nil
	case "url_decode":
		// PathUnescape keeps a literal '+' — the values this op sees come
		// from path-style percent-encoding, matching Prowlarr/Jackett's
		// UnescapeDataString semantics.
		out, err := url.PathUnescape(v)
		if err != nil {
			return "", fmt.Errorf("search: url_decode: %w", err)
		}
		return out, nil
	case "prepend", "append":
		arg, err := Expand(op.Args[0], s)
		if err != nil {
			return "", err
		}
		if op.Op == "prepend" {
			return arg + v, nil
		}
		return v + arg, nil
	case "replace":
		return strings.ReplaceAll(v, op.Args[0], op.Args[1]), nil
	case "regex_capture":
		re, err := compilePattern(op.Args[0])
		if err != nil {
			return "", fmt.Errorf("search: regex_capture: %w", err)
		}
		m := re.FindStringSubmatch(v)
		if m == nil {
			return "", nil
		}
		if len(m) < 2 {
			return "", fmt.Errorf("search: regex_capture: pattern %q has no capture group", op.Args[0])
		}
		return m[1], nil
	case "split":
		idx, err := strconv.Atoi(op.Args[1])
		if err != nil {
			return "", fmt.Errorf("search: split: %w", err)
		}
		parts := strings.Split(v, op.Args[0])
		if idx < 0 {
			idx += len(parts)
		}
		if idx < 0 || idx >= len(parts) {
			return "", fmt.Errorf("search: split: index %d out of range for %d parts", idx, len(parts))
		}
		return parts[idx], nil
	case "query_param":
		// A value without "?" is a bare query string, not a URL — url.Parse
		// would file it under Path and Query() would come back empty.
		if !strings.Contains(v, "?") {
			q, err := url.ParseQuery(v)
			if err != nil {
				return "", fmt.Errorf("search: query_param: %w", err)
			}
			return q.Get(op.Args[0]), nil
		}
		u, err := url.Parse(v)
		if err != nil {
			return "", fmt.Errorf("search: query_param: %w", err)
		}
		return u.Query().Get(op.Args[0]), nil
	case "strip_html":
		return stripTags(v), nil
	}
	return "", fmt.Errorf("search: unknown transform op %q", op.Op)
}

// regexCache memoises compiled regex_capture patterns: the op runs per field
// per row, and the pattern set is bounded by the loaded definitions, each at
// most MaxPatternBytes.
var regexCache sync.Map // pattern -> *regexp.Regexp

func compilePattern(pattern string) (*regexp.Regexp, error) {
	if len(pattern) > MaxPatternBytes {
		return nil, fmt.Errorf("search: regex pattern is %d bytes, over the %d-byte limit", len(pattern), MaxPatternBytes)
	}
	if re, ok := regexCache.Load(pattern); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	regexCache.Store(pattern, re)
	return re, nil
}

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// stripTags removes markup runs and keeps the text between them.
func stripTags(v string) string {
	return strings.TrimSpace(htmlTagRe.ReplaceAllString(v, ""))
}

// Coerce converts a raw string to the declared field type. bytes accepts
// "1.4 GiB", "1400000000" and a plain integer; datetime accepts iso8601,
// rfc1123, unix, relative and strptime:<fmt>, and returns RFC 3339.
func Coerce(raw, typ, format string) (string, error) {
	switch typ {
	case "", "string":
		return raw, nil
	case "int":
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return "", fmt.Errorf("search: cannot coerce %q to int", raw)
		}
		return strconv.FormatInt(n, 10), nil
	case "float":
		f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return "", fmt.Errorf("search: cannot coerce %q to float", raw)
		}
		return strconv.FormatFloat(f, 'g', -1, 64), nil
	case "bytes":
		n, err := parseBytes(raw)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(n, 10), nil
	case "datetime":
		t, err := parseDatetime(raw, format)
		if err != nil {
			return "", err
		}
		return t.UTC().Format(time.RFC3339), nil
	}
	return "", fmt.Errorf("search: unknown field type %q", typ)
}

// byteUnits is the IEC-and-SI unit table of the bytes type: bare numbers and
// "B" are bytes, the IEC units are binary, the SI units decimal.
var byteUnits = map[string]float64{
	"": 1, "b": 1,
	"kb": 1e3, "kib": 1 << 10,
	"mb": 1e6, "mib": 1 << 20,
	"gb": 1e9, "gib": 1 << 30,
	"tb": 1e12, "tib": 1 << 40,
	"pb": 1e15, "pib": 1 << 50,
	"eb": 1e18, "eib": 1 << 60,
}

var byteSizeRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([A-Za-z]{0,3})$`)

func parseBytes(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	m := byteSizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("search: cannot coerce %q to bytes", raw)
	}
	mult, ok := byteUnits[strings.ToLower(m[2])]
	if !ok {
		return 0, fmt.Errorf("search: cannot coerce %q to bytes: unknown unit %q", raw, m[2])
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("search: cannot coerce %q to bytes", raw)
	}
	return int64(math.Round(f * mult)), nil
}

// iso8601Layouts covers the strict RFC 3339 shape plus the date and
// space-separated variants real endpoints emit.
var iso8601Layouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// strptimeLayouts maps the strptime verbs a definition may use to Go
// reference-time fragments.
var strptimeLayouts = map[string]string{
	"%Y": "2006", "%y": "06",
	"%m": "01", "%d": "02", "%e": "_2",
	"%H": "15", "%I": "03", "%M": "04", "%S": "05",
	"%p": "PM", "%b": "Jan", "%B": "January",
	"%a": "Mon", "%A": "Monday",
	"%z": "-0700", "%Z": "MST",
	"%%": "%",
}

var relativeRe = regexp.MustCompile(`^([0-9]+)\s*(second|minute|hour|day|week|month|year)s?\s+ago$`)

// parseDatetime renders raw as a time by the declared format. relative is
// "N <unit> ago" (plus "yesterday") measured from now — month and year are
// the 30- and 365-day approximations every relative timestamp carries.
func parseDatetime(raw, format string) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, errors.New("search: cannot coerce empty value to datetime")
	}
	switch {
	case format == "iso8601":
		for _, l := range iso8601Layouts {
			if t, err := time.Parse(l, s); err == nil {
				return t, nil
			}
		}
	case format == "rfc1123":
		for _, l := range pubDateLayouts {
			if t, err := time.Parse(l, s); err == nil {
				return t, nil
			}
		}
	case format == "unix":
		n, err := strconv.ParseInt(s, 10, 64)
		if err == nil {
			return time.Unix(n, 0), nil
		}
	case format == "relative":
		if t, ok := parseRelative(s, time.Now()); ok {
			return t, nil
		}
	case strings.HasPrefix(format, strptimePfx):
		layout := translateStrptime(format[len(strptimePfx):])
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	default:
		return time.Time{}, fmt.Errorf("search: unknown datetime format %q", format)
	}
	return time.Time{}, fmt.Errorf("search: cannot coerce %q to datetime %q", raw, format)
}

func parseRelative(s string, now time.Time) (time.Time, bool) {
	if s == "yesterday" {
		return now.Add(-24 * time.Hour), true
	}
	m := relativeRe.FindStringSubmatch(strings.ToLower(s))
	if m == nil {
		return time.Time{}, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return time.Time{}, false
	}
	var d time.Duration
	switch m[2] {
	case "second":
		d = time.Duration(n) * time.Second
	case "minute":
		d = time.Duration(n) * time.Minute
	case "hour":
		d = time.Duration(n) * time.Hour
	case "day":
		d = time.Duration(n) * 24 * time.Hour
	case "week":
		d = time.Duration(n) * 7 * 24 * time.Hour
	case "month":
		d = time.Duration(n) * 30 * 24 * time.Hour
	case "year":
		d = time.Duration(n) * 365 * 24 * time.Hour
	}
	return now.Add(-d), true
}

// translateStrptime rewrites %verbs into a Go reference layout; a literal
// that is not a verb passes through.
func translateStrptime(format string) string {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] == '%' && i+1 < len(format) {
			if frag, ok := strptimeLayouts[format[i:i+2]]; ok {
				b.WriteString(frag)
				i++
				continue
			}
		}
		b.WriteByte(format[i])
	}
	return b.String()
}

// UpstreamError reports a request that reached the engine but did not get a
// usable answer: Status carries a non-2xx HTTP status, or 0 for a transport
// failure whose redacted message is Detail. The probe renders it as data;
// Search returns it as the call error.
type UpstreamError struct {
	Status     int
	Detail     string
	RetryAfter time.Duration
}

func (e *UpstreamError) Error() string {
	if e.Status == 0 {
		return "search: " + e.Detail
	}
	if e.Detail == "" {
		return fmt.Sprintf("search: http %d", e.Status)
	}
	return fmt.Sprintf("search: http %d: %s", e.Status, e.Detail)
}

// ProbeResult is the report of a single indexer probe.
type ProbeResult struct {
	Ok              bool
	ElapsedMS       int64
	CategoriesFound int
	Server          string
	Error           string
}

// bucket is a per-engine token bucket of capacity one: it spaces requests
// 60/rate apart, so an engine can never front-load its whole per-minute
// allowance into a burst. next doubles as the Retry-After hold a 429 or 503
// reply pushes forward.
type bucket struct {
	interval time.Duration
	next     time.Time
}

// Runner executes a definition against the network through the single
// guarded client. The limiter is a hand-rolled token bucket per engine id;
// no new dependency.
type Runner struct {
	hc        *http.Client
	log       *slog.Logger
	userAgent string

	mu      sync.Mutex
	limiter map[string]*bucket
}

// NewRunner stores the guarded client it is given — like TorznabClient it
// never builds one of its own for the public case; the private-network
// branch of clientFor rebuilds a scoped guard the way the API's probeCaps
// does.
func NewRunner(hc *http.Client, log *slog.Logger, userAgent string) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{hc: hc, log: log, userAgent: userAgent, limiter: map[string]*bucket{}}
}

// admit takes one token from the engine's bucket or returns ErrRateLimited.
func (r *Runner) admit(engineID string, perMinute int) error {
	if perMinute <= 0 {
		perMinute = defaultRateLimitPerMinute
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.limiter[engineID]
	if b == nil {
		b = &bucket{interval: time.Minute / time.Duration(perMinute)}
		r.limiter[engineID] = b
	}
	now := time.Now()
	if now.Before(b.next) {
		return ErrRateLimited
	}
	b.next = now.Add(b.interval)
	return nil
}

// hold pushes the engine's next token out by d — the Retry-After a 429 or
// 503 carried.
func (r *Runner) hold(engineID string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b := r.limiter[engineID]; b != nil {
		if t := time.Now().Add(d); t.After(b.next) {
			b.next = t
		}
	}
}

// clientFor returns the guarded client for this call: the injected one, or a
// private-allowing guard scoped to the configured origin when the
// definition's allow_private_network flag or the indexer row's setting lifts
// the private-range denial.
func (r *Runner) clientFor(def *Definition, cfg map[string]string, origin string) *http.Client {
	if !def.AllowPrivateNetwork && !isTruthy(cfg["allow_private_network"]) {
		return r.hc
	}
	u, err := url.Parse(origin)
	if err != nil {
		return r.hc
	}
	return secure.NewClient(secure.NewGuard(r.log, true).ForOrigin(u))
}

// fetchOutcome is the successful transport result: the capped body and the
// two response fields callers read.
type fetchOutcome struct {
	body   []byte
	status int
	server string
}

// fetch builds and issues the one GET the definition's request block
// describes: base_url joined with path, expanded query and header values, an
// honest User-Agent. The per-engine bucket is taken before the wire; the
// 8 MiB body cap is enforced while streaming and the parsed document over
// 2 MiB is refused. A non-2xx answer is an *UpstreamError; an SSRF denial
// and ErrRateLimited are plain errors so the caller can map them to problems.
func (r *Runner) fetch(ctx context.Context, def *Definition, cfg map[string]string, scope Scope) (fetchOutcome, error) {
	req := def.Request
	deadline := engineDeadline
	if d := time.Duration(req.TimeoutSeconds) * time.Second; d > 0 && d < deadline {
		deadline = d
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	rawURL := req.BaseURL
	if req.Path != "" {
		rawURL = strings.TrimRight(req.BaseURL, "/") + "/" + strings.TrimLeft(req.Path, "/")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fetchOutcome{}, fmt.Errorf("search: build request url: %w", err)
	}
	q := u.Query()
	for _, k := range sortedKeys(req.Query) {
		expanded, err := Expand(req.Query[k], scope)
		if err != nil {
			return fetchOutcome{}, fmt.Errorf("search: request.query.%s: %w", k, err)
		}
		q.Set(k, expanded)
	}
	u.RawQuery = q.Encode()

	if err := r.admit(def.ID, req.RateLimitPerMinute); err != nil {
		return fetchOutcome{}, err
	}

	httpreq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fetchOutcome{}, fmt.Errorf("search: build request: %w", secure.RedactError(err))
	}
	for _, name := range sortedKeys(req.Headers) {
		expanded, err := Expand(req.Headers[name], scope)
		if err != nil {
			return fetchOutcome{}, fmt.Errorf("search: request.headers.%s: %w", name, err)
		}
		httpreq.Header.Set(name, expanded)
	}
	// The honest UA always wins over a definition's own header.
	httpreq.Header.Set("User-Agent", r.userAgent)

	resp, err := r.clientFor(def, cfg, req.BaseURL).Do(httpreq)
	if err != nil {
		if errors.Is(err, secure.ErrSSRFBlocked) {
			return fetchOutcome{}, fmt.Errorf("search: fetch refused: %w", secure.RedactError(err))
		}
		return fetchOutcome{}, &UpstreamError{Detail: secure.RedactError(err).Error()}
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			r.log.Debug("search: close engine response body", "error", err)
		}
	}()
	body, err := secure.ReadCapped(resp, secure.MetadataFetchCap)
	if err != nil {
		return fetchOutcome{}, err
	}
	out := fetchOutcome{body: body, status: resp.StatusCode, server: resp.Header.Get("Server")}
	// A non-2xx answer is an *UpstreamError even when its error page crosses
	// the parsed-document cap — the cap gates parsing, not status handling,
	// and the 429/503 Retry-After hold must run either way.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var retryAfter time.Duration
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
			r.hold(def.ID, retryAfter)
		}
		return out, &UpstreamError{Status: resp.StatusCode, Detail: bodySnippet(body), RetryAfter: retryAfter}
	}
	if len(body) > maxParsedDoc {
		return fetchOutcome{}, fmt.Errorf(
			"search: document is %d bytes, over the %d-byte parsed-document cap", len(body), maxParsedDoc)
	}
	return out, nil
}

// bodySnippet renders the leading text of a non-2xx body for the probe's
// error string, whitespace-collapsed and truncated.
func bodySnippet(body []byte) string {
	// The snippet is 240 runes of collapsed whitespace — cap the input first
	// so a multi-megabyte error page does not cost full-body conversions.
	if len(body) > 1024 {
		body = body[:1024]
	}
	s := strings.Join(strings.Fields(string(body)), " ")
	if runes := []rune(s); len(runes) > 240 {
		s = string(runes[:240])
	}
	return s
}

// scopeFor builds the expansion scope of one search call: the page math of
// the query, the site-side category values the requested newznab ids map to,
// and the declared settings with their defaults applied (a checkbox is
// "true" or "").
func scopeFor(def *Definition, cfg map[string]string, q Query) Scope {
	scope := Scope{
		Keywords:   q.Q,
		Limit:      q.Limit,
		Offset:     q.Offset,
		Page:       1,
		Categories: siteCategories(def, q.Categories),
		Query: map[string]string{
			"Q": q.Q, "Season": q.Season, "Ep": q.Ep, "IMDBID": q.IMDBID,
		},
		Config:    resolvedSettings(def, cfg),
		Result:    map[string]string{},
		TodayYear: strconv.Itoa(time.Now().Year()),
	}
	if q.Limit > 0 {
		scope.Page = q.Offset/q.Limit + 1
	}
	return scope
}

// resolvedSettings overlays the stored per-engine values on the declared
// defaults; undeclared stored keys (the reserved allow_private_network and
// origin among them) pass through so clientFor can read the flag.
func resolvedSettings(def *Definition, cfg map[string]string) map[string]string {
	out := make(map[string]string, len(cfg)+len(def.Settings))
	for k, v := range cfg {
		out[k] = v
	}
	for _, s := range def.Settings {
		v, ok := cfg[s.Name]
		if !ok {
			v = s.Default
		}
		if s.Type == "checkbox" {
			if isTruthy(v) {
				v = "true"
			} else {
				v = ""
			}
		}
		out[s.Name] = v
	}
	return out
}

// isTruthy is the one checkbox-style truth test, shared by resolvedSettings
// and the reserved allow_private_network read in clientFor so the two paths
// cannot drift: true, 1, yes and on, case-insensitive, whitespace-tolerant.
func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// siteCategories maps the requested newznab ids to the site-side values of
// caps.categories, sorted for a deterministic template input.
func siteCategories(def *Definition, ids []int) []string {
	if len(ids) == 0 {
		return nil
	}
	var out []string
	for _, site := range sortedKeys(def.Caps.Categories) {
		id := def.Caps.Categories[site]
		for _, want := range ids {
			if id == want {
				out = append(out, site)
				break
			}
		}
	}
	return out
}

// Search fetches and maps one page. cfg holds the engine's stored settings
// values. Limits applied per call: 8 MiB body, 2 MiB parsed document, 5
// redirects, ports 80 and 443, http and https only, the definition's rate
// limit, and a 15 s total deadline covering redirects and parsing.
func (r *Runner) Search(ctx context.Context, def *Definition, cfg map[string]string, q Query) ([]SearchResult, error) {
	if def == nil {
		return nil, errors.New("search: nil definition")
	}
	// A static engine answers from def.Entries; dispatch before any request
	// is built so it never opens a socket during a search (doc 07 §3.8).
	if def.Kind == "static" {
		return searchStatic(def, q), nil
	}
	if def.Request == nil || def.Response == nil {
		return nil, errors.New("search: definition has no request/response block")
	}
	if def.Request.Method != "" && def.Request.Method != http.MethodGet {
		return nil, fmt.Errorf("search: request method %q is not GET", def.Request.Method)
	}
	var extract func(context.Context, []byte, *Definition, Scope) ([]map[string]string, int, error)
	switch def.Kind {
	case "rss":
		extract = extractRSS
	case "json":
		extract = extractJSON
	case "html":
		// Probe fetches html, but row extraction is out of scope for v1 —
		// say so explicitly rather than falling through to the generic error.
		return nil, fmt.Errorf("search: kind %q row extraction is not implemented yet", def.Kind)
	default:
		return nil, fmt.Errorf("search: kind %q has no row extraction", def.Kind)
	}

	// The section 3.5 total deadline covers redirects AND parsing, so it is
	// applied here around the whole call, not only inside fetch.
	deadline := engineDeadline
	if d := time.Duration(def.Request.TimeoutSeconds) * time.Second; d > 0 && d < deadline {
		deadline = d
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	scope := scopeFor(def, cfg, q)
	browse := !usesKeywords(def.Request.Query)

	out, err := r.fetch(ctx, def, cfg, scope)
	if err != nil {
		return nil, err
	}
	rows, skipped, extractErr := extract(ctx, out.body, def, scope)
	if extractErr != nil {
		return nil, extractErr
	}
	if len(rows) > maxResultRows {
		rows = rows[:maxResultRows]
	}
	if skipped > 0 {
		r.log.Warn("search rows dropped: field extraction failed",
			"engine_id", def.ID, "dropped", skipped)
	}

	results := make([]SearchResult, 0, len(rows))
	for _, fields := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		results = append(results, mapResult(def, fields))
	}
	final, dropped := Finalise(results)
	if dropped > 0 {
		r.log.Warn("search rows dropped: no download url, magnet or infohash",
			"engine_id", def.ID, "dropped", dropped)
	}

	// The browse-style client-side filter of doc 07 section 3.6: an engine
	// whose query never references {{ .Keywords }} returns its whole feed,
	// and the keyword match runs in process against the title.
	if browse && q.Q != "" {
		needle := strings.ToLower(q.Q)
		kept := final[:0]
		for _, res := range final {
			if strings.Contains(strings.ToLower(res.Title), needle) {
				kept = append(kept, res)
			}
		}
		final = kept
	}
	return final, nil
}

// Probe issues exactly one request and reports what came back. A reachable
// indexer that answers with an error document yields Ok=false and a
// populated Error, never a Go error — the Go error return is reserved for a
// probe that could not be attempted at all (no usable definition, an SSRF
// refusal, the rate limiter).
func (r *Runner) Probe(ctx context.Context, def *Definition, cfg map[string]string) (res ProbeResult, err error) {
	start := time.Now()
	if def == nil {
		return res, errors.New("search: probe needs a definition")
	}
	// res is a named return so the deferred stamp reaches the caller.
	defer func() { res.ElapsedMS = time.Since(start).Milliseconds() }()
	switch def.Kind {
	case "torznab", "newznab":
		base := cfg["base_url"]
		if base == "" {
			return res, errors.New("search: torznab definition needs a base_url setting")
		}
		tc, err := NewTorznabClient(
			r.clientFor(def, cfg, base), base,
			secure.Secret(cfg["api_key"]), def.ID, r.userAgent,
		)
		if err != nil {
			return res, err
		}
		caps, err := tc.Caps(ctx)
		if err != nil {
			if errors.Is(err, secure.ErrSSRFBlocked) {
				return res, err
			}
			res.Error = err.Error()
			return res, nil
		}
		res.Ok = true
		res.CategoriesFound = len(FlattenCategories(caps.Categories))
		res.Server = caps.ServerTitle
		return res, nil
	case "rss", "json", "html":
		if def.Request == nil {
			return res, fmt.Errorf("search: kind %q has no request block", def.Kind)
		}
		scope := scopeFor(def, cfg, Query{Limit: 1})
		out, err := r.fetch(ctx, def, cfg, scope)
		if err != nil {
			var ue *UpstreamError
			if errors.As(err, &ue) {
				res.Error = ue.Error()
				return res, nil
			}
			return res, err
		}
		res.Server = out.server
		res.CategoriesFound = len(def.Caps.Categories)
		// A 2xx that does not parse as the declared kind is a broken
		// indexer, reported as data like a non-2xx.
		switch def.Kind {
		case "rss":
			if _, err := gofeed.NewParser().Parse(bytes.NewReader(out.body)); err != nil {
				res.Error = "search: response is not a feed: " + err.Error()
				return res, nil
			}
		case "json":
			var doc any
			if err := json.NewDecoder(bytes.NewReader(out.body)).Decode(&doc); err != nil {
				res.Error = "search: response is not json: " + err.Error()
				return res, nil
			}
		}
		res.Ok = true
		return res, nil
	case "static":
		return r.probeStatic(ctx, def)
	}
	return res, fmt.Errorf("search: cannot probe kind %q", def.Kind)
}

// searchStatic answers from def.Entries with no HTTP request. Rows are
// filtered by case-insensitive substring of q.Q against Entry.Title, so
// every static engine is browse-style in the UI. A missing size stays 0,
// which the UI renders as an em dash, and Seeders and Leechers are always
// nil.
func searchStatic(def *Definition, q Query) []SearchResult {
	results := make([]SearchResult, 0, len(def.Entries))
	for _, e := range def.Entries {
		res := SearchResult{
			EngineID:    def.ID,
			Title:       e.Title,
			DownloadURL: e.Download,
			MagnetURI:   e.Magnet,
			Infohash:    e.Infohash,
			SizeBytes:   e.Size,
			DetailsURL:  e.Details,
			// The site-side value stays the description; caps.categories
			// supplies the newznab id.
			CategoryDesc:         e.Category,
			DownloadVolumeFactor: 1.0,
			UploadVolumeFactor:   1.0,
		}
		if id, ok := def.Caps.Categories[e.Category]; ok {
			res.CategoryIDs = []int{id}
		}
		if e.Published != "" {
			// entries[] declares no format; iso8601 is the only shape a
			// curated list writes, and a value that does not parse stays
			// unknown rather than failing the list.
			if s, err := Coerce(e.Published, "datetime", "iso8601"); err == nil {
				res.PublishedAt = &s
			}
		}
		results = append(results, res)
	}
	// LoadDefinition already requires an acquisition handle per entry;
	// Finalise is the belt for a Definition that bypassed the loader.
	final, dropped := Finalise(results)
	if dropped > 0 {
		slog.Default().Warn("search rows dropped: no download url, magnet or infohash",
			"engine_id", def.ID, "dropped", dropped)
	}
	if q.Q != "" {
		needle := strings.ToLower(q.Q)
		kept := final[:0]
		for _, res := range final {
			if strings.Contains(strings.ToLower(res.Title), needle) {
				kept = append(kept, res)
			}
		}
		final = kept
	}
	return final
}

// probeStatic validates the definition, then issues one HEAD per distinct
// download URL through the guarded client, with the same 15 s total
// deadline as a live engine. Ok is true only when every URL answered 2xx;
// Error names each URL that did not, with its status. It follows the same
// redirect and port rules as every other fetch.
func (r *Runner) probeStatic(ctx context.Context, def *Definition) (ProbeResult, error) {
	var res ProbeResult
	if len(def.Entries) == 0 {
		return res, errors.New("search: static definition has no entries")
	}
	res.CategoriesFound = len(def.Caps.Categories)

	ctx, cancel := context.WithTimeout(ctx, engineDeadline)
	defer cancel()

	// One HEAD per distinct download URL — entries may share a mirror, so
	// de-duplicate first (doc 07 section 3.8).
	var urls []string
	seen := map[string]bool{}
	for _, e := range def.Entries {
		if e.Download != "" && !seen[e.Download] {
			seen[e.Download] = true
			urls = append(urls, e.Download)
		}
	}

	var failures []string
	for _, raw := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, raw, nil)
		if err != nil {
			// A URL that cannot even form a request is reported per-URL,
			// like a dial failure, not as a probe-level error.
			failures = append(failures, fmt.Sprintf("%s: %s", raw, secure.RedactError(err)))
			continue
		}
		req.Header.Set("User-Agent", r.userAgent)
		resp, err := r.clientFor(def, nil, raw).Do(req)
		if err != nil {
			if errors.Is(err, secure.ErrSSRFBlocked) {
				return res, fmt.Errorf("search: fetch refused: %w", secure.RedactError(err))
			}
			failures = append(failures, fmt.Sprintf("%s: %s", raw, secure.RedactError(err)))
			continue
		}
		if res.Server == "" {
			res.Server = resp.Header.Get("Server")
		}
		if err := resp.Body.Close(); err != nil {
			r.log.Debug("search: close probe response body", "error", err)
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			failures = append(failures, fmt.Sprintf("%s: http %d", raw, resp.StatusCode))
		}
	}
	if len(failures) > 0 {
		res.Error = strings.Join(failures, "; ")
		return res, nil
	}
	res.Ok = true
	return res, nil
}

// xmlElement is one node of the generic tree the rss extractor walks —
// attr access and extension elements such as <infohash> are outside
// gofeed's model, so the tree is built by encoding/xml after gofeed has
// proven the document is a feed.
type xmlElement struct {
	name     string
	attrs    map[string]string
	children []*xmlElement
	text     strings.Builder
}

// parseXMLTree decodes the whole document into an element tree. Element and
// attribute names are matched by local name — the namespace prefix is
// dropped, so torznab:attr and newznab:attr both surface as "attr".
func parseXMLTree(doc []byte) (*xmlElement, error) {
	dec := xml.NewDecoder(bytes.NewReader(doc))
	var root *xmlElement
	var stack []*xmlElement
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			el := &xmlElement{name: t.Name.Local}
			for _, a := range t.Attr {
				if el.attrs == nil {
					el.attrs = map[string]string{}
				}
				el.attrs[a.Name.Local] = a.Value
			}
			if len(stack) == 0 {
				// Token() does not enforce a single root: a trailing element
				// after the real feed root must not replace it.
				if root == nil {
					root = el
				}
			} else {
				top := stack[len(stack)-1]
				top.children = append(top.children, el)
			}
			stack = append(stack, el)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text.Write(t)
			}
		}
	}
	if root == nil {
		return nil, errors.New("search: document has no root element")
	}
	return root, nil
}

// selectElements walks the "a > b > c" element path of response.rows. A
// leading segment naming the root element is consumed first; every later
// segment selects the matching children of the current set.
func selectElements(root *xmlElement, path string) []*xmlElement {
	segs := strings.Split(path, ">")
	for i := range segs {
		segs[i] = strings.TrimSpace(segs[i])
	}
	cur := []*xmlElement{root}
	if len(segs) > 0 && segs[0] == root.name {
		segs = segs[1:]
	}
	for _, seg := range segs {
		if seg == "" {
			continue
		}
		var next []*xmlElement
		for _, el := range cur {
			for _, c := range el.children {
				if c.name == seg {
					next = append(next, c)
				}
			}
		}
		cur = next
	}
	return cur
}

// childPath resolves a field's relative path ("a > b") against one row
// element, first match per segment.
func childPath(el *xmlElement, path string) *xmlElement {
	for _, seg := range strings.Split(path, ">") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		var next *xmlElement
		for _, c := range el.children {
			if c.name == seg {
				next = c
				break
			}
		}
		if next == nil {
			return nil
		}
		el = next
	}
	return el
}

// extractRSS parses the feed, selects the row elements by response.rows and
// resolves every declared field per row — a field's path names a child
// element, its attr an attribute of that element.
func extractRSS(ctx context.Context, body []byte, def *Definition, scope Scope) ([]map[string]string, int, error) {
	if _, err := gofeed.NewParser().Parse(bytes.NewReader(body)); err != nil {
		return nil, 0, fmt.Errorf("search: response is not a feed: %w", err)
	}
	root, err := parseXMLTree(body)
	if err != nil {
		return nil, 0, fmt.Errorf("search: parse xml: %w", err)
	}
	items := selectElements(root, def.Response.Rows)
	rows := make([]map[string]string, 0, len(items))
	skipped := 0
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, skipped, err
		}
		fields, err := resolveFields(def, scope, func(f Field) (string, error) {
			el := childPath(item, f.Path)
			if el == nil {
				return "", nil
			}
			if f.Attr != "" {
				return el.attrs[f.Attr], nil
			}
			return strings.TrimSpace(el.text.String()), nil
		})
		if err != nil {
			skipped++
			continue
		}
		rows = append(rows, fields)
	}
	return rows, skipped, nil
}

// validJSONPath checks a path's shape without touching data. Syntax errors —
// a bad segment, a non-integer index, [*] anywhere but the end — are
// definition bugs that must fail loudly, unlike traversal mismatches, which
// depend on the row's data.
func validJSONPath(path string) error {
	p := strings.TrimSpace(path)
	switch {
	case strings.HasPrefix(p, "$"):
	case strings.HasPrefix(p, "."):
		p = "$" + p
	default:
		p = "$." + p
	}
	for i := 1; i < len(p); {
		switch p[i] {
		case '.':
			j := i + 1
			for j < len(p) && p[j] != '.' && p[j] != '[' {
				j++
			}
			if p[i+1:j] == "" {
				return fmt.Errorf("search: bad json path %q", path)
			}
			i = j
		case '[':
			j := strings.IndexByte(p[i:], ']')
			if j < 0 {
				return fmt.Errorf("search: bad json path %q", path)
			}
			inner := p[i+1 : i+j]
			if inner == "*" {
				if i+j+1 < len(p) {
					return fmt.Errorf("search: json path %q: [*] must be the last segment", path)
				}
			} else if n, err := strconv.Atoi(inner); err != nil || n < 0 {
				return fmt.Errorf("search: json path %q: bad index %q", path, inner)
			}
			i += j + 1
		default:
			return fmt.Errorf("search: bad json path %q", path)
		}
	}
	return nil
}

// jsonPath evaluates the in-repo subset of JSONPath — $, .field, [n] and
// [*] — over a decoded document. A path without a leading $ is relative to
// the value it is evaluated on, which is how field paths address a row.
func jsonPath(doc any, path string) (any, error) {
	p := strings.TrimSpace(path)
	switch {
	case strings.HasPrefix(p, "$"):
	case strings.HasPrefix(p, "."):
		p = "$" + p
	default:
		p = "$." + p
	}
	cur := doc
	i := 1 // past the $
	for i < len(p) {
		switch p[i] {
		case '.':
			j := i + 1
			for j < len(p) && p[j] != '.' && p[j] != '[' {
				j++
			}
			name := p[i+1 : j]
			if name == "" {
				return nil, fmt.Errorf("search: bad json path %q", path)
			}
			obj, ok := cur.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("search: json path %q: %q on a non-object", path, name)
			}
			cur = obj[name]
			i = j
		case '[':
			j := strings.IndexByte(p[i:], ']')
			if j < 0 {
				return nil, fmt.Errorf("search: bad json path %q", path)
			}
			inner := p[i+1 : i+j]
			arr, ok := cur.([]any)
			if !ok {
				return nil, fmt.Errorf("search: json path %q: index on a non-array", path)
			}
			if inner == "*" {
				// [*] is the identity on the array; anything after it would
				// evaluate on the whole list and fail confusingly.
				if i+j+1 < len(p) {
					return nil, fmt.Errorf("search: json path %q: [*] must be the last segment", path)
				}
				cur = arr
			} else {
				n, err := strconv.Atoi(inner)
				if err != nil || n < 0 || n >= len(arr) {
					return nil, fmt.Errorf("search: json path %q: bad index %q", path, inner)
				}
				cur = arr[n]
			}
			i += j + 1
		default:
			return nil, fmt.Errorf("search: bad json path %q", path)
		}
	}
	return cur, nil
}

// jsonScalar renders one field value: strings pass through, numbers and
// booleans render, null and anything composite are empty — a field that
// addresses an object is a definition bug, not a result value.
func jsonScalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	}
	return ""
}

// extractJSON decodes the document, evaluates response.rows into the row
// list and resolves every declared field against its row.
func extractJSON(ctx context.Context, body []byte, def *Definition, scope Scope) ([]map[string]string, int, error) {
	var doc any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, 0, fmt.Errorf("search: parse json: %w", err)
	}
	rowsVal, err := jsonPath(doc, def.Response.Rows)
	if err != nil {
		return nil, 0, err
	}
	arr, ok := rowsVal.([]any)
	if !ok {
		return nil, 0, fmt.Errorf("search: response.rows %q does not select an array", def.Response.Rows)
	}
	// Field paths are shape-checked once up front: a syntax error is a
	// definition bug and fails the call, while a traversal mismatch inside
	// the row loop below is data-dependent and blanks only that field.
	for _, name := range def.Response.OrderedFields() {
		if f := def.Response.Fields[name]; f.Path != "" {
			if err := validJSONPath(f.Path); err != nil {
				return nil, 0, fmt.Errorf("search: field %s: %w", name, err)
			}
		}
	}

	rows := make([]map[string]string, 0, len(arr))
	skipped := 0
	for _, item := range arr {
		if err := ctx.Err(); err != nil {
			return nil, skipped, err
		}
		fields, err := resolveFields(def, scope, func(f Field) (string, error) {
			v, err := jsonPath(item, f.Path)
			if err != nil {
				// A missing key never reaches this branch — map lookups
				// yield nil without an error — and a valid-syntax path
				// that errors here hit a data-shape mismatch in this one
				// row, so only the field is blanked, not the row.
				return "", nil
			}
			return jsonScalar(v), nil
		})
		if err != nil {
			skipped++
			continue
		}
		rows = append(rows, fields)
	}
	return rows, skipped, nil
}

// resolveFields runs the per-row resolution of doc 07 section 3.3: fields
// resolve in declaration order so {{ .Result.<field> }} sees earlier ones;
// transforms apply to the extracted value, an empty optional field falls
// back to its expanded default, and Coerce renders the declared type.
// scope.Result aliases the returned map, so template fields read
// already-resolved values.
func resolveFields(def *Definition, scope Scope, get func(f Field) (string, error)) (map[string]string, error) {
	res := make(map[string]string, len(def.Response.Fields))
	scope.Result = res
	for _, name := range def.Response.OrderedFields() {
		f := def.Response.Fields[name]
		var raw string
		var err error
		switch {
		case f.Const != "":
			raw = f.Const
		case f.Template != "":
			raw, err = Expand(f.Template, scope)
		default:
			raw, err = get(f)
		}
		if err != nil {
			return nil, fmt.Errorf("search: field %s: %w", name, err)
		}
		raw, err = ApplyTransforms(raw, def.Response.Transforms[name], scope)
		if err != nil {
			return nil, fmt.Errorf("search: field %s: %w", name, err)
		}
		if raw == "" && f.Optional && f.Default != "" {
			raw, err = Expand(f.Default, scope)
			if err != nil {
				return nil, fmt.Errorf("search: field %s: %w", name, err)
			}
		}
		if raw != "" || !f.Optional {
			raw, err = Coerce(raw, f.Type, f.Format)
			if err != nil {
				return nil, fmt.Errorf("search: field %s: %w", name, err)
			}
		}
		res[name] = raw
	}
	return res, nil
}

// mapResult projects the resolved field map onto a SearchResult. Names
// outside the known set — and every _-prefixed temporary — stay off the
// result but remain visible to later fields through .Result.
func mapResult(def *Definition, fields map[string]string) SearchResult {
	r := SearchResult{
		EngineID: def.ID,
		// dlsearch/v1 defines no factor fields; freeleech and multipliers
		// stay at the documented 1.0 default.
		DownloadVolumeFactor: 1.0,
		UploadVolumeFactor:   1.0,
	}
	for name, v := range fields {
		if v == "" || strings.HasPrefix(name, "_") {
			continue
		}
		s := v // per-iteration copy for the pointer fields
		switch name {
		case "title":
			r.Title = s
		case "size":
			r.SizeBytes = parseInt64(s)
		case "published":
			r.PublishedAt = &s
		case "details":
			r.DetailsURL = s
		case "download":
			// URI schemes are case-insensitive (RFC 3986); some sites emit
			// MAGNET:?xt=...
			if len(s) >= len("magnet:") && strings.EqualFold(s[:len("magnet:")], "magnet:") {
				r.MagnetURI = s
			} else {
				r.DownloadURL = s
			}
		case "magnet":
			r.MagnetURI = s
		case "infohash":
			r.Infohash = s
		case "seeders":
			if !def.Caps.SeedersUnknown {
				r.Seeders = parseIntPtr(s)
			}
		case "leechers":
			if !def.Caps.SeedersUnknown {
				r.Leechers = parseIntPtr(s)
			}
		case "peers":
			r.peers = parseIntPtr(s)
		case "grabs":
			r.Grabs = parseIntPtr(s)
		case "category":
			r.CategoryDesc = s
			if id, ok := def.Caps.Categories[s]; ok {
				r.CategoryIDs = []int{id}
			}
		case "imdb", "imdbid":
			r.IMDBID = s
		case "tmdbid":
			r.TMDBID = s
		case "tvdbid":
			r.TVDBID = s
		case "year":
			r.Year = parseIntPtr(s)
		case "genre":
			r.Genre = s
		case "language":
			r.Language = s
		case "publisher":
			r.Publisher = s
		case "author":
			r.Author = s
		case "album":
			r.Album = s
		case "artist":
			r.Artist = s
		}
	}
	return r
}
