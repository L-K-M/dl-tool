package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/L-K-M/dl-tool/internal/rss"
	"github.com/L-K-M/dl-tool/internal/store"
)

// createRule posts one rule with the test bearer credential.
func (e *tasksTestEnv) createRule(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Post("/rules", body, "Authorization: Bearer "+e.bearer)
}

// getRules calls GET /rules with the test bearer credential.
func (e *tasksTestEnv) getRules(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Get("/rules", "Authorization: Bearer "+e.bearer)
}

// patchRule patches one rule by id with the test bearer credential.
func (e *tasksTestEnv) patchRule(t *testing.T, id string, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Patch("/rules/"+id, body, "Authorization: Bearer "+e.bearer)
}

// deleteRule deletes one rule by id with the test bearer credential.
func (e *tasksTestEnv) deleteRule(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Delete("/rules/"+id, "Authorization: Bearer "+e.bearer)
}

// decodeRuleBody decodes the flat rule object POST and PATCH return.
func decodeRuleBody(t *testing.T, recorder *httptest.ResponseRecorder) RuleDTO {
	t.Helper()

	var body RuleDTO
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body
}

// decodeRuleList decodes the GET /rules envelope.
func decodeRuleList(t *testing.T, recorder *httptest.ResponseRecorder) []RuleDTO {
	t.Helper()

	var body struct {
		Rules []RuleDTO `json:"rules"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body.Rules
}

// validRuleBody is one legal POST /rules body; mutate the returned maps to
// build the invalid cases.
func validRuleBody(name string) map[string]any {
	return map[string]any{
		"name": name,
		"definition": map[string]any{
			"name":   name,
			"match":  map[string]any{"any_of": []string{"ubuntu *desktop*"}},
			"action": map[string]any{"paused": true},
		},
	}
}

// TestRuleCrud pins doc 05 section 10.2: create returns 201 with the rule
// object and its defaults applied, patch merges the provided members and
// re-validates, delete is 204, and a repeated delete is 404.
func TestRuleCrud(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.createRule(t, validRuleBody("Ubuntu LTS ISOs"))
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	created := decodeRuleBody(t, response)
	if !strings.HasPrefix(created.ID, store.PrefixRule) ||
		created.Name != "Ubuntu LTS ISOs" ||
		!created.Enabled ||
		created.Priority != 0 {
		t.Errorf("created = %+v, want the submitted fields with enabled by default", created)
	}
	if created.Definition.Match.Mode != rss.MatchModeWildcard ||
		len(created.Definition.Match.Fields) != 1 ||
		created.Definition.Match.Fields[0] != "title" ||
		created.Definition.Enabled == nil || !*created.Definition.Enabled {
		t.Errorf("definition = %+v, want ApplyDefaults filled mode, fields and enabled", created.Definition)
	}
	if created.LastMatchAt != nil {
		t.Errorf("last_match_at = %v, want null before the first grab", *created.LastMatchAt)
	}
	if _, err := time.Parse(time.RFC3339, created.CreatedAt); err != nil {
		t.Errorf("created_at = %q, want RFC 3339: %v", created.CreatedAt, err)
	}

	// The patch merges: a priority-only write keeps the stored document
	// and mirrors the new column into it.
	response = env.patchRule(t, created.ID, map[string]any{"priority": 10})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	patched := decodeRuleBody(t, response)
	if patched.Priority != 10 || patched.Definition.Priority != 10 {
		t.Errorf("patched = %+v, want priority 10 mirrored into the document", patched)
	}
	if !patched.Enabled || len(patched.Definition.Match.AnyOf) != 1 {
		t.Errorf("patched = %+v, want enabled and the stored match block untouched", patched)
	}

	response = env.deleteRule(t, created.ID)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusNoContent, response.Body.String())
	}
	response = env.deleteRule(t, created.ID)
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
	response = env.patchRule(t, created.ID, map[string]any{"enabled": false})
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
}

// TestEpisodeFilterMissingSemicolonIsRejected pins FR-074 and the doc 08
// section 6.3 row: "1x01" — no trailing ';' — is 422 at save time with
// errors[0].location naming the member, never a stored rule that silently
// matches nothing. The accepted twin "1x01-;" stores, and a PATCH carrying
// the malformed filter is re-validated exactly like a create.
func TestEpisodeFilterMissingSemicolonIsRejected(t *testing.T) {
	env := newTasksTestEnv(t)

	body := validRuleBody("shows")
	body["definition"].(map[string]any)["episode"] = map[string]any{"filter": "1x01"}
	response := env.createRule(t, body)
	problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	if len(problem.Errors) == 0 || problem.Errors[0].Location != "body.definition.episode.filter" {
		t.Fatalf("errors = %+v, want errors[0].location body.definition.episode.filter", problem.Errors)
	}

	body["definition"].(map[string]any)["episode"] = map[string]any{"filter": "1x01-;"}
	response = env.createRule(t, body)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	ruleID := decodeRuleBody(t, response).ID

	response = env.patchRule(t, ruleID, map[string]any{
		"definition": map[string]any{
			"name":    "shows",
			"match":   map[string]any{},
			"action":  map[string]any{},
			"episode": map[string]any{"filter": "2x03"},
		},
	})
	problem = assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	if len(problem.Errors) == 0 || problem.Errors[0].Location != "body.definition.episode.filter" {
		t.Fatalf("patch errors = %+v, want errors[0].location body.definition.episode.filter", problem.Errors)
	}
}

// TestEmptyPatternInNoneOfIsRejected pins the doc 08 section 4.3 trap: an
// empty string inside none_of is qBittorrent's reject-everything typo, so
// it is 422 at save time and never reaches the store.
func TestEmptyPatternInNoneOfIsRejected(t *testing.T) {
	env := newTasksTestEnv(t)

	body := validRuleBody("no-daily")
	body["definition"].(map[string]any)["match"] = map[string]any{
		"none_of": []string{"web-dl", ""},
	}
	response := env.createRule(t, body)
	problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	if len(problem.Errors) != 1 || problem.Errors[0].Location != "body.definition.match.none_of[1]" {
		t.Fatalf("errors = %+v, want one entry at body.definition.match.none_of[1]", problem.Errors)
	}

	if rules := decodeRuleList(t, env.getRules(t)); len(rules) != 0 {
		t.Errorf("rules = %+v, want the rejected document never stored", rules)
	}
}

// TestParseEpisodeFilterNormalisesSeason pins the doc 08 section 6.1
// normalisation qBittorrent lacks: "01x05;" parses to season 1, the three
// token forms of section 6.2 parse, and an inverted range is skipped — not
// an error.
func TestParseEpisodeFilterNormalisesSeason(t *testing.T) {
	filter, err := rss.ParseEpisodeFilter("01x05;")
	if err != nil {
		t.Fatalf("ParseEpisodeFilter: %v", err)
	}
	if filter.Season != 1 {
		t.Errorf("season = %d, want 1 — leading zeros stripped at parse time", filter.Season)
	}
	if len(filter.Tokens) != 1 || filter.Tokens[0].From != 5 || filter.Tokens[0].To != 0 || filter.Tokens[0].Open {
		t.Errorf("tokens = %+v, want one single-number token 5", filter.Tokens)
	}

	filter, err = rss.ParseEpisodeFilter("2x5;09;12-14;14-12;01-;")
	if err != nil {
		t.Fatalf("ParseEpisodeFilter: %v", err)
	}
	want := []rss.EpisodeToken{
		{From: 5},
		{From: 9}, // leading zeros stripped from every token
		{From: 12, To: 14},
		{From: 1, To: -1, Open: true},
	}
	if len(filter.Tokens) != len(want) {
		t.Fatalf("tokens = %+v, want %+v — the inverted range skipped", filter.Tokens, want)
	}
	for i, token := range want {
		if filter.Tokens[i] != token {
			t.Errorf("tokens[%d] = %+v, want %+v", i, filter.Tokens[i], token)
		}
	}

	// The empty filter is a no-op, and "1x;" parses — every token empty —
	// because rejecting it is the matcher's job, not the parser's.
	for _, s := range []string{"", "1x;"} {
		if _, err := rss.ParseEpisodeFilter(s); err != nil {
			t.Errorf("ParseEpisodeFilter(%q): %v, want nil", s, err)
		}
	}
	if _, err := rss.ParseEpisodeFilter("1x01"); err == nil {
		t.Error(`ParseEpisodeFilter("1x01") = nil error, want the missing-';' rejection`)
	}
}

// TestValidateReportsEveryProblem pins the validator's all-at-once
// contract: a document with three faults — a bad mode, an empty none_of
// entry and a malformed episode.filter — yields three errors[] entries, so
// the editor can highlight every clause in one round trip.
func TestValidateReportsEveryProblem(t *testing.T) {
	env := newTasksTestEnv(t)

	body := validRuleBody("broken")
	body["definition"].(map[string]any)["match"] = map[string]any{
		"mode":    "glob",
		"none_of": []string{""},
	}
	body["definition"].(map[string]any)["episode"] = map[string]any{"filter": "1x01"}
	response := env.createRule(t, body)
	problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	if len(problem.Errors) != 3 {
		t.Fatalf("errors = %+v, want one entry per fault — mode, none_of[0], episode.filter", problem.Errors)
	}
	locations := []string{
		problem.Errors[0].Location, problem.Errors[1].Location, problem.Errors[2].Location,
	}
	for _, want := range []string{
		"body.definition.match.mode", "body.definition.match.none_of[0]", "body.definition.episode.filter",
	} {
		found := false
		for _, location := range locations {
			if location == want {
				found = true
			}
		}
		if !found {
			t.Errorf("errors = %+v, want an entry at %s", problem.Errors, want)
		}
	}
}

// TestRuleDocumentValidation pins the remaining 422s of the validator's
// contract: an uncompilable regex, a pattern past the 1024-byte cap, a
// score.formats list past 32 entries, an unparseable size or
// published_after, and a content_layout outside its enum — each with the
// field path that caused it.
func TestRuleDocumentValidation(t *testing.T) {
	env := newTasksTestEnv(t)

	longPattern := strings.Repeat("a", 1025)
	tooManyFormats := make([]map[string]any, 33)
	for i := range tooManyFormats {
		tooManyFormats[i] = map[string]any{"name": fmt.Sprintf("f%d", i), "pattern": "x", "weight": 1}
	}

	cases := []struct {
		name     string
		match    map[string]any
		extra    map[string]any
		location string
	}{
		{"uncompilable regex", map[string]any{"mode": "regex", "any_of": []string{"a["}}, nil, "body.definition.match.any_of[0]"},
		{"regex in none_of", map[string]any{"mode": "regex", "none_of": []string{"(*"}}, nil, "body.definition.match.none_of[0]"},
		{"overlong pattern", map[string]any{"any_of": []string{longPattern}}, nil, "body.definition.match.any_of[0]"},
		{"bad min_size", map[string]any{"min_size": "1024"}, nil, "body.definition.match.min_size"},
		{"bad max_size", map[string]any{"max_size": "8 gigs"}, nil, "body.definition.match.max_size"},
		{"bad published_after", map[string]any{"published_after": "2026-01-01"}, nil, "body.definition.match.published_after"},
		{"bad field", map[string]any{"fields": []string{"title", "comment"}}, nil, "body.definition.match.fields[1]"},
		{"unservable field", map[string]any{"fields": []string{"description"}}, nil, "body.definition.match.fields[0]"},
		{"empty any_of entry", map[string]any{"any_of": []string{""}}, nil, "body.definition.match.any_of[0]"},
		{"too many formats", map[string]any{}, map[string]any{"score": map[string]any{"formats": tooManyFormats}}, "body.definition.score.formats"},
		{"bad score pattern", map[string]any{}, map[string]any{"score": map[string]any{"formats": []map[string]any{{"name": "x", "pattern": "a[", "weight": 1}}}}, "body.definition.score.formats[0].pattern"},
		{"bad content_layout", map[string]any{}, map[string]any{"action": map[string]any{"content_layout": "flat"}}, "body.definition.action.content_layout"},
		{"inverted size window", map[string]any{"min_size": "2GiB", "max_size": "1GiB"}, nil, "body.definition.match.max_size"},
		{"nan size", map[string]any{"min_size": "nanb"}, nil, "body.definition.match.min_size"},
		{"infinite size", map[string]any{"max_size": "infb"}, nil, "body.definition.match.max_size"},
		{"overflowing size", map[string]any{"max_size": "1e30b"}, nil, "body.definition.match.max_size"},
		{"huge eib size", map[string]any{"min_size": "16EiB"}, nil, "body.definition.match.min_size"},
		{"negative cooldown", map[string]any{}, map[string]any{"throttle": map[string]any{"cooldown_days": -1}}, "body.definition.throttle.cooldown_days"},
		{"negative max_per_run", map[string]any{}, map[string]any{"throttle": map[string]any{"max_per_run": -1}}, "body.definition.throttle.max_per_run"},
	}
	for _, tc := range cases {
		body := validRuleBody("case-" + tc.name)
		definition := body["definition"].(map[string]any)
		definition["match"] = tc.match
		for key, value := range tc.extra {
			definition[key] = value
		}
		response := env.createRule(t, body)
		problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
		if len(problem.Errors) == 0 || problem.Errors[0].Location != tc.location {
			t.Errorf("%s: errors = %+v, want an entry at %s", tc.name, problem.Errors, tc.location)
		}
	}

	// The documented boundary values pass: every enum value, an IEC size
	// of each unit, a 1024-byte pattern and exactly 32 formats.
	formats := make([]map[string]any, 32)
	for i := range formats {
		formats[i] = map[string]any{"name": fmt.Sprintf("f%d", i), "pattern": "x", "weight": 1}
	}
	body := validRuleBody("boundary")
	body["definition"].(map[string]any)["match"] = map[string]any{
		"mode": "plain", "fields": []string{"title"},
		"any_of":   []string{strings.Repeat("a", 1024)},
		"min_size": "1KiB", "max_size": "8GiB", "published_after": "2026-01-01T00:00:00Z",
	}
	body["definition"].(map[string]any)["score"] = map[string]any{"minimum": -10, "formats": formats}
	body["definition"].(map[string]any)["action"] = map[string]any{"content_layout": "no_subfolder"}
	body["definition"].(map[string]any)["episode"] = map[string]any{"filter": "1x01-;", "smart": true}
	body["definition"].(map[string]any)["throttle"] = map[string]any{"cooldown_days": 3, "max_per_run": 5}
	response := env.createRule(t, body)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
}

// TestDuplicateRuleNameConflicts pins the 409 of doc 05 section 10.2: a
// name already taken conflicts on create and on rename — never a silent
// merge.
func TestDuplicateRuleNameConflicts(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.createRule(t, validRuleBody("taken"))
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	seed := env.createRule(t, validRuleBody("second"))
	if seed.Code != http.StatusCreated {
		t.Fatalf("seed status = %d, want %d; body %s", seed.Code, http.StatusCreated, seed.Body.String())
	}
	second := decodeRuleBody(t, seed)

	response = env.createRule(t, validRuleBody("taken"))
	assertProblem(t, response, http.StatusConflict, SlugConflict)

	response = env.patchRule(t, second.ID, map[string]any{"name": "taken"})
	assertProblem(t, response, http.StatusConflict, SlugConflict)

	if rules := decodeRuleList(t, env.getRules(t)); len(rules) != 2 {
		t.Errorf("rules = %+v, want both rows intact", rules)
	}
}

// TestRuleListOrderedByPriorityThenName pins the evaluation order of doc
// 08 section 5: priority ascending, name ascending inside a tie.
func TestRuleListOrderedByPriorityThenName(t *testing.T) {
	env := newTasksTestEnv(t)

	for _, seed := range []struct {
		name     string
		priority int
	}{
		{"zebra", 0}, {"alpha", 10}, {"middle", 10}, {"first", -5},
	} {
		body := validRuleBody(seed.name)
		body["definition"].(map[string]any)["priority"] = seed.priority
		response := env.createRule(t, body)
		if response.Code != http.StatusCreated {
			t.Fatalf("seed %s: status = %d; body %s", seed.name, response.Code, response.Body.String())
		}
	}

	rules := decodeRuleList(t, env.getRules(t))
	if len(rules) != 4 {
		t.Fatalf("rules = %+v, want the four seeds", rules)
	}
	order := []string{rules[0].Name, rules[1].Name, rules[2].Name, rules[3].Name}
	if order[0] != "first" || order[1] != "zebra" || order[2] != "alpha" || order[3] != "middle" {
		t.Errorf("order = %v, want [first(-5) zebra(0) alpha(10) middle(10)]", order)
	}
}

// TestStoredDefinitionRoundTrips pins the storage contract: what lands in
// rules.definition_json is the compact marshal of the validated document,
// so reading it back through RuleDoc and re-encoding it is a fixed point —
// no field silently drops or mutates.
func TestStoredDefinitionRoundTrips(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.createRule(t, validRuleBody("roundtrip"))
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	ruleID := decodeRuleBody(t, response).ID

	var stored string
	if err := env.db.GetContext(
		t.Context(), &stored, `SELECT definition_json FROM rules WHERE id = ?`, ruleID,
	); err != nil {
		t.Fatalf("read definition_json: %v", err)
	}
	var doc rss.RuleDoc
	if err := json.Unmarshal([]byte(stored), &doc); err != nil {
		t.Fatalf("stored definition_json does not decode: %v", err)
	}
	reencoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if string(reencoded) != stored {
		t.Errorf("round trip changed the document: stored %s, re-encoded %s", stored, reencoded)
	}
}

// TestRuleNameMismatchRejected pins the mirrored-name rule of the task:
// definition.name must equal the request's name, so a document smuggling a
// second name is 422 on create and on patch.
func TestRuleNameMismatchRejected(t *testing.T) {
	env := newTasksTestEnv(t)

	body := validRuleBody("outer")
	body["definition"].(map[string]any)["name"] = "inner"
	response := env.createRule(t, body)
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	seed := env.createRule(t, validRuleBody("outer"))
	if seed.Code != http.StatusCreated {
		t.Fatalf("seed status = %d, want %d; body %s", seed.Code, http.StatusCreated, seed.Body.String())
	}
	ruleID := decodeRuleBody(t, seed).ID
	response = env.patchRule(t, ruleID, map[string]any{
		"name":       "outer-renamed",
		"definition": map[string]any{"name": "different"},
	})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
}

// TestAutoPrefixNameRejected pins the reserved namespace of doc 05 section
// 10.1: POST /rules answers 422 to an auto:-prefixed name, PATCH answers
// 422 to one that differs from the stored name — covering creates and
// renames — while echoing an auto: rule's own name or omitting name
// entirely stays legal.
func TestAutoPrefixNameRejected(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.createRule(t, validRuleBody("auto:fed_01JKQ7AAAA"))
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	seed := env.createRule(t, validRuleBody("mine"))
	if seed.Code != http.StatusCreated {
		t.Fatalf("seed status = %d, want %d; body %s", seed.Code, http.StatusCreated, seed.Body.String())
	}
	ruleID := decodeRuleBody(t, seed).ID
	response = env.patchRule(t, ruleID, map[string]any{"name": "auto:fed_01JKQ7BBBB"})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	response = env.patchRule(t, ruleID, map[string]any{
		"definition": map[string]any{"name": "auto:fed_01JKQ7CCCC"},
	})
	assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)

	// A rule that already carries an auto: name — seeded by the feed
	// lifecycle — is edited like any other: echoing its own name and
	// omitting name entirely both stay legal.
	feedResponse := env.createFeed(t, map[string]any{
		"url": "https://archlinux.org/feeds/releases/", "auto_download": true,
	})
	feedID := decodeFeedBody(t, feedResponse).ID
	autoRule, err := store.RuleByName(t.Context(), env.db, autoRuleName(feedID))
	if err != nil {
		t.Fatalf("resolve auto rule: %v", err)
	}

	response = env.patchRule(t, autoRule.ID, map[string]any{"name": autoRuleName(feedID)})
	if response.Code != http.StatusOK {
		t.Fatalf("echo status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	response = env.patchRule(t, autoRule.ID, map[string]any{"enabled": false})
	if response.Code != http.StatusOK {
		t.Fatalf("toggle status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if patched := decodeRuleBody(t, response); patched.Enabled {
		t.Error("enabled toggle on an auto: rule did not apply")
	}
}

// TestAutoRuleDeletesLikeAnyOther pins doc 09 section 8.1: DELETE
// /rules/{id} on an auto: rule is 204 — the operator's way to disable
// auto-download without touching the feed — and a later PATCH
// auto_download: true recreates it.
func TestAutoRuleDeletesLikeAnyOther(t *testing.T) {
	env := newTasksTestEnv(t)

	feedResponse := env.createFeed(t, map[string]any{
		"url": "https://archlinux.org/feeds/releases/", "auto_download": true,
	})
	feedID := decodeFeedBody(t, feedResponse).ID
	autoRule, err := store.RuleByName(t.Context(), env.db, autoRuleName(feedID))
	if err != nil {
		t.Fatalf("resolve auto rule: %v", err)
	}

	response := env.deleteRule(t, autoRule.ID)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusNoContent, response.Body.String())
	}
	if _, err := store.RuleByName(t.Context(), env.db, autoRuleName(feedID)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("auto rule survived its delete: %v", err)
	}

	response = env.patchFeed(t, feedID, map[string]any{"auto_download": true})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	recreated, err := store.RuleByName(t.Context(), env.db, autoRuleName(feedID))
	if err != nil {
		t.Fatalf("auto rule not recreated: %v", err)
	}
	if recreated.ID == autoRule.ID {
		t.Error("recreated rule kept the deleted row's id, want a fresh rul_ id")
	}
}

// TestPatchRuleName pins the mirrored-name rules of PATCH: an explicit ""
// is 422 at body.name — a stored rule can never carry an empty name — a
// replacement document whose name is empty keeps the stored name rather
// than blank it, and a definition-only rename still works.
func TestPatchRuleName(t *testing.T) {
	env := newTasksTestEnv(t)

	response := env.createRule(t, validRuleBody("original"))
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	ruleID := decodeRuleBody(t, response).ID

	response = env.patchRule(t, ruleID, map[string]any{"name": ""})
	problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	if len(problem.Errors) == 0 || problem.Errors[0].Location != "body.name" {
		t.Fatalf("errors = %+v, want errors[0].location body.name", problem.Errors)
	}

	// A replacement document carrying an empty name keeps the stored
	// name rather than blank it — name is required in the document
	// schema, so "" is the degenerate spelling of "no rename".
	response = env.patchRule(t, ruleID, map[string]any{
		"definition": map[string]any{"name": "", "match": map[string]any{}, "action": map[string]any{}},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if patched := decodeRuleBody(t, response); patched.Name != "original" || patched.Definition.Name != "original" {
		t.Errorf("patched = %+v, want the stored name kept and mirrored into the document", patched)
	}

	// A definition-only rename still applies, mirrored into the column.
	response = env.patchRule(t, ruleID, map[string]any{
		"definition": map[string]any{"name": "renamed", "match": map[string]any{}, "action": map[string]any{}},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if patched := decodeRuleBody(t, response); patched.Name != "renamed" || patched.Definition.Name != "renamed" {
		t.Errorf("patched = %+v, want renamed in the column and the document", patched)
	}
}

// TestRuleFeedURLsAreRedacted pins the section 10.1 credential rule on the
// rule document's feeds member: GET /rules renders userinfo and secret
// query values __redacted__ while the stored document keeps them verbatim
// for matching — and a write echoing a redacted entry is 422, never a
// stored non-address.
func TestRuleFeedURLsAreRedacted(t *testing.T) {
	env := newTasksTestEnv(t)
	const rawURL = "https://user:pass@tracker.example.com/feed.xml?apikey=sekret&genre=iso"

	response := env.createFeed(t, map[string]any{"url": rawURL, "auto_download": true})
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	feedID := decodeFeedBody(t, response).ID

	rules := decodeRuleList(t, env.getRules(t))
	if len(rules) != 1 {
		t.Fatalf("rules = %+v, want the one auto rule", rules)
	}
	if len(rules[0].Definition.Feeds) != 1 {
		t.Fatalf("feeds = %v, want one entry", rules[0].Definition.Feeds)
	}
	rendered := rules[0].Definition.Feeds[0]
	if strings.Contains(rendered, "user:pass") || strings.Contains(rendered, "sekret") {
		t.Errorf("rendered feeds entry leaks a credential: %q", rendered)
	}
	if !strings.Contains(rendered, redactedValue) || !strings.Contains(rendered, "genre=iso") {
		t.Errorf("rendered feeds entry = %q, want __redacted__ members and the genre member kept", rendered)
	}

	// The stored document keeps the raw url: matching compares it
	// verbatim against feed urls.
	if doc := env.autoRuleDocument(t, feedID); len(doc.Feeds) != 1 || doc.Feeds[0] != rawURL {
		t.Errorf("stored feeds = %v, want the verbatim credential url", doc.Feeds)
	}

	// A write echoing a redacted entry is 422 on create and on patch.
	body := validRuleBody("scoped")
	body["definition"].(map[string]any)["feeds"] = []string{
		"https://" + redactedValue + "@tracker.example.com/feed.xml",
	}
	response = env.createRule(t, body)
	problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	if len(problem.Errors) == 0 || problem.Errors[0].Location != "body.definition.feeds[0]" {
		t.Fatalf("errors = %+v, want errors[0].location body.definition.feeds[0]", problem.Errors)
	}

	seed := env.createRule(t, validRuleBody("scoped"))
	if seed.Code != http.StatusCreated {
		t.Fatalf("seed status = %d, want %d; body %s", seed.Code, http.StatusCreated, seed.Body.String())
	}
	response = env.patchRule(t, decodeRuleBody(t, seed).ID, map[string]any{
		"definition": map[string]any{
			"name":  "scoped",
			"feeds": []string{"https://tracker.example.com/feed.xml?token=" + redactedValue},
			"match": map[string]any{}, "action": map[string]any{},
		},
	})
	problem = assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	if len(problem.Errors) == 0 || problem.Errors[0].Location != "body.definition.feeds[0]" {
		t.Fatalf("patch errors = %+v, want errors[0].location body.definition.feeds[0]", problem.Errors)
	}
}

// TestRuleLastMatchAtIsMonotonic pins the dedup watermark's direction:
// SetRuleLastMatchAt never moves last_match_at backwards, so an
// out-of-order or retried write cannot re-open the consumed window and
// re-grab items the rule already committed.
func TestRuleLastMatchAtIsMonotonic(t *testing.T) {
	env := newTasksTestEnv(t)
	ctx := t.Context()

	response := env.createRule(t, validRuleBody("watermarked"))
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	ruleID := decodeRuleBody(t, response).ID

	// The first write on a NULL column stores the given value.
	if err := store.SetRuleLastMatchAt(ctx, env.db, ruleID, 2000); err != nil {
		t.Fatalf("set last_match_at: %v", err)
	}
	rule, err := store.RuleByID(ctx, env.db, ruleID)
	if err != nil {
		t.Fatalf("read rule: %v", err)
	}
	if rule.LastMatchAt == nil || *rule.LastMatchAt != 2000 {
		t.Fatalf("last_match_at = %v, want 2000", rule.LastMatchAt)
	}
	updatedAt := rule.UpdatedAt

	// An older write does not regress the watermark — and does not churn
	// updated_at for a row whose content did not change.
	if err := store.SetRuleLastMatchAt(ctx, env.db, ruleID, 1000); err != nil {
		t.Fatalf("set older last_match_at: %v", err)
	}
	rule, err = store.RuleByID(ctx, env.db, ruleID)
	if err != nil {
		t.Fatalf("re-read rule: %v", err)
	}
	if rule.LastMatchAt == nil || *rule.LastMatchAt != 2000 {
		t.Errorf("last_match_at = %v, want it held at 2000", rule.LastMatchAt)
	}
	if rule.UpdatedAt != updatedAt {
		t.Errorf("updated_at moved on a non-advancing write: %d -> %d", updatedAt, rule.UpdatedAt)
	}

	// A newer write advances it, stamping updated_at.
	time.Sleep(2 * time.Millisecond)
	if err := store.SetRuleLastMatchAt(ctx, env.db, ruleID, 3000); err != nil {
		t.Fatalf("set newer last_match_at: %v", err)
	}
	rule, err = store.RuleByID(ctx, env.db, ruleID)
	if err != nil {
		t.Fatalf("re-read rule: %v", err)
	}
	if rule.LastMatchAt == nil || *rule.LastMatchAt != 3000 {
		t.Errorf("last_match_at = %v, want 3000", rule.LastMatchAt)
	}
	if rule.UpdatedAt <= updatedAt {
		t.Errorf("updated_at = %d, want a stamp after %d on an advancing write", rule.UpdatedAt, updatedAt)
	}
}

// TestCheckRuleFeedsRejectsRedactedForms pins the write-guard/read-render
// contract: every url shape redactFeedURL emits for a credential-bearing
// feed is refused by checkRuleFeeds, so a rendered form can never be
// written back as a non-address — if the redaction format ever changes,
// this test catches the two drifting apart.
func TestCheckRuleFeedsRejectsRedactedForms(t *testing.T) {
	for _, raw := range []string{
		"https://user:secret@tracker.example.com/feed.xml",
		"https://tracker.example.com/feed.xml?apikey=sekret&genre=iso",
		"https://tracker.example.com/feed.xml?token=%zz",
	} {
		doc := rss.RuleDoc{Feeds: []string{redactFeedURL(raw)}}
		if err := checkRuleFeeds(doc); err == nil {
			t.Errorf("checkRuleFeeds accepted the redacted form of %q", raw)
		}
	}
}

// testRule posts one unsaved document to POST /rules/test with the test
// bearer credential.
func (e *tasksTestEnv) testRule(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Post("/rules/test", body, "Authorization: Bearer "+e.bearer)
}

// validTestRuleBody is one legal POST /rules/test body; mutate the
// returned maps to build the invalid cases.
func validTestRuleBody() map[string]any {
	return map[string]any{
		"rule": map[string]any{
			"name":   "preview",
			"match":  map[string]any{"any_of": []string{"*ubuntu*"}},
			"action": map[string]any{"destination": "/data/iso", "category": "linux"},
		},
	}
}

// TestTestRuleMalformedFilterIsRejected pins the 422 of doc 05 section
// 10.3: a malformed episode.filter fails Validate before any database
// access, with errors[].location naming the member under body.rule.
func TestTestRuleMalformedFilterIsRejected(t *testing.T) {
	env := newTasksTestEnv(t)

	body := validTestRuleBody()
	body["rule"].(map[string]any)["episode"] = map[string]any{"filter": "1x01"}
	response := env.testRule(t, body)
	problem := assertProblem(t, response, http.StatusUnprocessableEntity, SlugValidationFailed)
	if len(problem.Errors) == 0 || problem.Errors[0].Location != "body.rule.episode.filter" {
		t.Fatalf("errors = %+v, want errors[0].location body.rule.episode.filter", problem.Errors)
	}
}

// TestTestRuleUnknownFeedIsNotFound pins the 404 of doc 05 section 10.3: a
// named feed id that addresses no row is /problems/not-found.
func TestTestRuleUnknownFeedIsNotFound(t *testing.T) {
	env := newTasksTestEnv(t)

	body := validTestRuleBody()
	body["feeds"] = []string{"fed_does_not_exist"}
	response := env.testRule(t, body)
	assertProblem(t, response, http.StatusNotFound, SlugNotFound)
}

// TestTestRuleReturnsEveryItem pins the shape the editor's live preview
// needs: 200 whatever the per-item outcomes, evaluated, matched and
// elapsed_ms always present, and every stored item in results — matched
// rows carry matched_by and would_do, unmatched rows carry reason.
func TestTestRuleReturnsEveryItem(t *testing.T) {
	env := newTasksTestEnv(t)

	feedID := env.seedFeed(t, "https://example.com/dryrun.xml")
	published := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	downloadURL := "https://example.com/t/x.torrent"
	items := []store.FeedItem{
		{FeedID: feedID, Identity: "i1", Title: "ubuntu 26.04 desktop amd64", TitleNorm: "ubuntu 26.04 desktop amd64", DownloadURL: &downloadURL, PublishedAt: &published},
		{FeedID: feedID, Identity: "i2", Title: "debian 13 netinst", TitleNorm: "debian 13 netinst", DownloadURL: &downloadURL, PublishedAt: &published},
	}
	if _, err := store.UpsertFeedItems(t.Context(), env.db, items, published); err != nil {
		t.Fatalf("seed feed items: %v", err)
	}

	response := env.testRule(t, validTestRuleBody())
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	var report rss.DryRunReport
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode response body %q: %v", response.Body.String(), err)
	}
	if report.Evaluated != 2 || report.Matched != 1 || len(report.Results) != 2 {
		t.Fatalf("report = %+v, want evaluated=2 matched=1 and every item in results", report)
	}
	for _, row := range report.Results {
		if row.Matched {
			if row.WouldDo == nil || row.WouldDo.Destination != "/data/iso" || len(row.MatchedBy) == 0 {
				t.Errorf("matched row = %+v, want matched_by and would_do", row)
			}
		} else if row.Reason == "" || row.ReasonDetail == "" {
			t.Errorf("unmatched row = %+v, want reason and reason_detail", row)
		}
	}
	if !strings.Contains(response.Body.String(), `"elapsed_ms":`) {
		t.Errorf("body %s, want elapsed_ms present", response.Body.String())
	}
}

// TestTestRuleIgnoreStateFalseIsAccepted pins the handler's explicit-false
// branch: body.ignore_state=false must dereference through the pointer and
// reach rss.DryRun as false — observable because the stored rule_matches
// row collides with the item's info hash, so a stateful run rejects it
// where a stateless one would match.
func TestTestRuleIgnoreStateFalseIsAccepted(t *testing.T) {
	env := newTasksTestEnv(t)

	feedID := env.seedFeed(t, "https://example.com/ignore-state.xml")
	published := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	downloadURL := "https://example.com/t/x.torrent"
	hash := "dcb9178653b651c7ca4526e11fa8e22f74e2fd7a"
	items := []store.FeedItem{
		{FeedID: feedID, Identity: "i1", Title: "ubuntu 26.04 desktop amd64", TitleNorm: "ubuntu 26.04 desktop amd64", DownloadURL: &downloadURL, InfoHash: &hash, PublishedAt: &published},
	}
	if _, err := store.UpsertFeedItems(t.Context(), env.db, items, published); err != nil {
		t.Fatalf("seed feed items: %v", err)
	}
	if err := store.CreateRule(t.Context(), env.db, store.Rule{
		ID: "rul_01DRYRUNSTATE00000000000", Name: "grabbed", Enabled: true, DefinitionJSON: "{}",
	}); err != nil {
		t.Fatalf("seed rule: %v", err)
	}
	if _, err := env.db.ExecContext(t.Context(),
		`INSERT INTO rule_matches
		(id, rule_id, info_hash, title, status, score, matched_at, created_at, updated_at)
		VALUES ('rm_ignore_state', 'rul_01DRYRUNSTATE00000000000', ?, 'ubuntu 26.04 desktop amd64', 'sent', 0, ?, ?, ?)`,
		hash, published, published, published); err != nil {
		t.Fatalf("seed rule_matches: %v", err)
	}

	body := validTestRuleBody()
	body["ignore_state"] = false
	response := env.testRule(t, body)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	var report rss.DryRunReport
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode response body %q: %v", response.Body.String(), err)
	}
	if report.Evaluated != 1 || len(report.Results) != 1 {
		t.Fatalf("report = %+v, want evaluated=1 with the item in results", report)
	}
	if report.Results[0].Matched || report.Results[0].Reason != rss.ReasonDuplicateInfoHash {
		t.Errorf("row = %+v, want the stateful duplicate_infohash rejection — explicit false must reach DryRun", report.Results[0])
	}

	// The stateless default still matches the same item.
	response = env.testRule(t, validTestRuleBody())
	if response.Code != http.StatusOK {
		t.Fatalf("default status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode response body %q: %v", response.Body.String(), err)
	}
	if report.Matched != 1 {
		t.Errorf("default report = %+v, want the stateless run matching the item", report)
	}
}

// TestTestRuleTitlesEvaluateWithoutStoredItems pins the docked-test path
// of doc 05 section 10.3: titles replaces stored items entirely — the
// results come back in request order, one verdict per title, and the
// feed-shaped members marshal as null.
func TestTestRuleTitlesEvaluateWithoutStoredItems(t *testing.T) {
	env := newTasksTestEnv(t)

	// A stored item whose title matches the rule would enter the results
	// if the titles path ever read the item tables.
	feedID := env.seedFeed(t, "https://example.com/titles.xml")
	published := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	downloadURL := "https://example.com/t/x.torrent"
	if _, err := store.UpsertFeedItems(t.Context(), env.db, []store.FeedItem{
		{FeedID: feedID, Identity: "stored", Title: "ubuntu stored", TitleNorm: "ubuntu stored", DownloadURL: &downloadURL, PublishedAt: &published},
	}, published); err != nil {
		t.Fatalf("seed feed items: %v", err)
	}

	body := validTestRuleBody()
	body["titles"] = []string{"debian netinst", "ubuntu desktop"}
	body["rule"].(map[string]any)["action"].(map[string]any)["tags"] = []string{"iso"}
	response := env.testRule(t, body)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	var report rss.DryRunReport
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode response body %q: %v", response.Body.String(), err)
	}
	if report.Evaluated != 2 || report.Matched != 1 || len(report.Results) != 2 {
		t.Fatalf("report = %+v, want evaluated=2 matched=1", report)
	}
	if report.Results[0].Title != "debian netinst" || report.Results[1].Title != "ubuntu desktop" {
		t.Errorf("results order = %q, %q, want request order",
			report.Results[0].Title, report.Results[1].Title)
	}
	for _, row := range report.Results {
		if row.FeedID != nil || row.Feed != nil || row.DownloadURL != nil || row.PublishedAt != nil {
			t.Errorf("titles row = %+v, want feed_id, feed, download_url and published_at null", row)
		}
	}
	if tags := report.Results[1].WouldDo.Tags; !slices.Equal(tags, []string{"iso"}) {
		t.Errorf("would_do.tags = %v, want the rule's action.tags echoed", tags)
	}
	if !strings.Contains(response.Body.String(), `"feed_id":null`) {
		t.Errorf("body %s, want feed_id marshalled as null", response.Body.String())
	}
}

// TestTestRuleTitlesBounds pins the request validation of doc 05 section
// 10.3: a present-but-empty array, more than 50 titles and a title past
// 500 UTF-8 bytes each answer 422 with errors[].location body.titles.
func TestTestRuleTitlesBounds(t *testing.T) {
	env := newTasksTestEnv(t)

	// doc 05 section 10.3: with titles present, feeds are not resolved —
	// an unknown feed id must still answer 200 with one verdict per title.
	titlesBody := validTestRuleBody()
	titlesBody["titles"] = []string{"ubuntu desktop"}
	titlesBody["feeds"] = []string{"fed_does_not_exist"}
	response := env.testRule(t, titlesBody)
	if response.Code != http.StatusOK {
		t.Fatalf("titles + unknown feed status = %d, want %d; body %s",
			response.Code, http.StatusOK, response.Body.String())
	}
	var titlesReport rss.DryRunReport
	if err := json.Unmarshal(response.Body.Bytes(), &titlesReport); err != nil {
		t.Fatalf("decode titles body %q: %v", response.Body.String(), err)
	}
	if len(titlesReport.Results) != 1 {
		t.Fatalf("titles + unknown feed verdicts = %d, want 1", len(titlesReport.Results))
	}

	cases := map[string][]string{
		"empty":     {},
		"fifty-one": make([]string, 51),
		"oversized": {strings.Repeat("x", 501)},
	}
	for name, titles := range cases {
		body := validTestRuleBody()
		body["titles"] = titles
		problem := assertProblem(t, env.testRule(t, body), http.StatusUnprocessableEntity, SlugValidationFailed)
		if len(problem.Errors) == 0 || problem.Errors[0].Location != "body.titles" {
			t.Errorf("%s: errors = %+v, want errors[0].location body.titles", name, problem.Errors)
		}
	}
}

// TestRunRuleCarriesTagsToTheTask pins the grab hand-off of doc 04
// section 5: action.tags rides GrabRequest into POST /tasks, so the
// committed task lists the rule's tags.
func TestRunRuleCarriesTagsToTheTask(t *testing.T) {
	env := newTasksTestEnv(t)

	feedID := env.seedFeed(t, "https://example.com/tagged.xml")
	published := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	env.seedRunItems(t, feedID, []store.FeedItem{
		runItem(feedID, "i1", "ubuntu 26.04 desktop amd64", "", published),
	})

	body := validRuleBody("Tagged")
	body["definition"].(map[string]any)["action"].(map[string]any)["tags"] = []string{"iso", "linux"}
	response := env.createRule(t, body)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	rule := decodeRuleBody(t, response)

	response = env.runRule(t, rule.ID)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	report := decodeRunRuleBody(t, response)
	if len(report.CreatedTaskIDs) != 1 {
		t.Fatalf("report = %+v, want one created task", report)
	}

	task := env.getTask(t, report.CreatedTaskIDs[0])
	if task.Code != http.StatusOK {
		t.Fatalf("get task status = %d, want %d; body %s", task.Code, http.StatusOK, task.Body.String())
	}
	var dto TaskDTO
	if err := json.Unmarshal(task.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode task body %q: %v", task.Body.String(), err)
	}
	if !slices.Equal(dto.Tags, []string{"iso", "linux"}) {
		t.Errorf("task tags = %v, want [iso linux]", dto.Tags)
	}
}

// runRule posts to POST /rules/{id}/run with the test bearer credential.
func (e *tasksTestEnv) runRule(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()

	return e.api.Post("/rules/"+id+"/run", "Authorization: Bearer "+e.bearer)
}

// runRuleBody is the decoded POST /rules/{id}/run report.
type runRuleBody struct {
	Evaluated      int      `json:"evaluated"`
	Matched        int      `json:"matched"`
	CreatedTaskIDs []string `json:"created_task_ids"`
	ElapsedMS      int64    `json:"elapsed_ms"`
}

// decodeRunRuleBody decodes the run report envelope.
func decodeRunRuleBody(t *testing.T, recorder *httptest.ResponseRecorder) runRuleBody {
	t.Helper()

	var body runRuleBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}

	return body
}

// seedRunItems upserts routable .torrent items on one feed — the same
// fixture shape the dry-run tests use, because RunRule reads the stored
// items exactly like DryRun does.
func (e *tasksTestEnv) seedRunItems(t *testing.T, feedID string, items []store.FeedItem) {
	t.Helper()

	if _, err := store.UpsertFeedItems(t.Context(), e.db, items, time.Now().UnixMilli()); err != nil {
		t.Fatalf("seed feed items: %v", err)
	}
}

// runItem is one stored item carrying a routable .torrent download URL.
func runItem(feedID, identity, title, hash string, published int64) store.FeedItem {
	item := store.FeedItem{
		FeedID:      feedID,
		Identity:    identity,
		Title:       title,
		TitleNorm:   title,
		DownloadURL: strPtrAPI("https://example.com/t/" + identity + ".torrent"),
		PublishedAt: &published,
	}
	if hash != "" {
		item.InfoHash = &hash
	}

	return item
}

func strPtrAPI(s string) *string { return &s }

// TestRunRuleCommitsGrabsAsTasks pins the doc 05 section 10.2 run report:
// the saved rule evaluates the stored items, commits every winner through
// the ordinary create path and answers evaluated, matched and the created
// task ids.
func TestRunRuleCommitsGrabsAsTasks(t *testing.T) {
	env := newTasksTestEnv(t)

	feedID := env.seedFeed(t, "https://example.com/run.xml")
	published := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	env.seedRunItems(t, feedID, []store.FeedItem{
		runItem(feedID, "i1", "ubuntu 26.04 desktop amd64", "", published),
		runItem(feedID, "i2", "debian 13 netinst", "", published+1),
	})

	response := env.createRule(t, validRuleBody("Ubuntu desktops"))
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	rule := decodeRuleBody(t, response)

	response = env.runRule(t, rule.ID)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	report := decodeRunRuleBody(t, response)
	if report.Evaluated != 2 || report.Matched != 1 || len(report.CreatedTaskIDs) != 1 {
		t.Fatalf("report = %+v, want evaluated=2 matched=1 and one created task", report)
	}
	if env.countTasks(t) != 1 {
		t.Errorf("countTasks = %d, want the one committed grab", env.countTasks(t))
	}

	// The committed match row names the created task and carries 'sent'.
	var status, taskID string
	if err := env.db.QueryRowContext(t.Context(),
		`SELECT status, task_id FROM rule_matches WHERE rule_id = ?`, rule.ID).
		Scan(&status, &taskID); err != nil {
		t.Fatalf("read rule_matches: %v", err)
	}
	if status != "sent" || taskID != report.CreatedTaskIDs[0] {
		t.Errorf("rule_matches = (%s, %s), want sent row naming %s", status, taskID, report.CreatedTaskIDs[0])
	}

	// A second run over unchanged items creates nothing and reports [].
	response = env.runRule(t, rule.ID)
	if response.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	if body := decodeRunRuleBody(t, response); len(body.CreatedTaskIDs) != 0 {
		t.Errorf("second run report = %+v, want no created task ids", body)
	}
	if !strings.Contains(response.Body.String(), `"created_task_ids":[]`) {
		t.Errorf("second run body %s, want created_task_ids serialised as []", response.Body.String())
	}
}

// TestRunRuleUnknownIsNotFound pins the 404 of POST /rules/{id}/run.
func TestRunRuleUnknownIsNotFound(t *testing.T) {
	env := newTasksTestEnv(t)

	assertProblem(t, env.runRule(t, "rul_01JKQ8Z9YV6M3P0R2S4T6V8W0X"), http.StatusNotFound, SlugNotFound)
}

// TestRunRuleCreatedIDsShorterThanMatched pins the dedup row of the
// contract: two releases sharing one content key are both matched, but
// only the winner grabs — created_task_ids is shorter than matched.
func TestRunRuleCreatedIDsShorterThanMatched(t *testing.T) {
	env := newTasksTestEnv(t)

	feedID := env.seedFeed(t, "https://example.com/dedup.xml")
	published := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	hash := "dcb9178653b651c7ca4526e11fa8e22f74e2fd7a"
	env.seedRunItems(t, feedID, []store.FeedItem{
		runItem(feedID, "i1", "ubuntu 26.04 desktop amd64", hash, published+100),
		runItem(feedID, "i2", "ubuntu 26.04 desktop i386", hash, published),
	})

	response := env.createRule(t, validRuleBody("Ubuntu desktops"))
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d; body %s", response.Code, http.StatusCreated, response.Body.String())
	}
	rule := decodeRuleBody(t, response)

	response = env.runRule(t, rule.ID)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", response.Code, http.StatusOK, response.Body.String())
	}
	report := decodeRunRuleBody(t, response)
	if report.Evaluated != 2 || report.Matched != 2 || len(report.CreatedTaskIDs) != 1 {
		t.Fatalf("report = %+v, want evaluated=2 matched=2 with one created task", report)
	}
	if env.countTasks(t) != 1 {
		t.Errorf("countTasks = %d, want the one winner's task", env.countTasks(t))
	}
}
