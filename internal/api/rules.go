package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/rss"
	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	operationListRules  = "list-rules"
	operationCreateRule = "create-rule"
	operationPatchRule  = "patch-rule"
	operationDeleteRule = "delete-rule"
	operationTestRule   = "test-rule"
	operationRunRule    = "run-rule"

	ruleConflictDetail      = "a rule with that name already exists"
	ruleNameDetail          = "definition.name must equal the request's name"
	ruleAutoNameDetail      = "names beginning auto: are reserved for the feed auto-download lifecycle"
	ruleEmptyNameDetail     = "the name must not be empty"
	ruleRedactedFeedsDetail = "a feeds entry containing __redacted__ is a rendered form, not a feed url"

	// autoRulePrefix is the reserved name prefix of doc 05 section 10.1's
	// feed lifecycle: auto:<feed_id> rules are created and removed by the
	// /feeds write paths, so a user-authored rule can never carry the name
	// and be silently adopted.
	autoRulePrefix = "auto:"
)

// RuleDTO is the rule object of docs/05-api-contract.md section 10.2:
// name, enabled and priority are the mirrored columns, definition is the
// stored document, and the timestamps render RFC 3339 or null.
type RuleDTO struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Enabled     bool        `json:"enabled"`
	Priority    int         `json:"priority"`
	Definition  rss.RuleDoc `json:"definition"`
	LastMatchAt *string     `json:"last_match_at" format:"date-time"`
	CreatedAt   string      `json:"created_at"    format:"date-time"`
	UpdatedAt   string      `json:"updated_at"    format:"date-time"`
}

// CreateRuleInput is the JSON body of POST /rules: the document in the
// definition member, never a YAML string (doc 05 section 10.2).
type CreateRuleInput struct {
	Body struct {
		Name       string      `json:"name"       required:"true" minLength:"1" doc:"Unique rule name; names beginning auto: are reserved"`
		Definition rss.RuleDoc `json:"definition" required:"true"               doc:"The rule document; its name must equal the body's name"`
	}
}

// PatchRuleInput addresses one rule by id. The body fields are pointers: an
// omitted field is nil and stays untouched. A provided definition replaces
// the whole document; provided name, enabled and priority members merge
// onto it so the stored document and the mirrored columns cannot diverge.
type PatchRuleInput struct {
	ID   string `path:"id" doc:"The rul_ id of the rule"`
	Body struct {
		Name       *string      `json:"name,omitempty"       doc:"Rename; a duplicate is 409 and an auto: rename is 422"`
		Enabled    *bool        `json:"enabled,omitempty"`
		Priority   *int         `json:"priority,omitempty"`
		Definition *rss.RuleDoc `json:"definition,omitempty" doc:"Replacement rule document"`
	}
}

// DeleteRuleInput addresses one rule by id.
type DeleteRuleInput struct {
	ID string `path:"id" doc:"The rul_ id of the rule"`
}

// ListRulesOutput is the GET /rules body.
type ListRulesOutput struct {
	Body struct {
		Rules []RuleDTO `json:"rules"`
	}
}

// The titles bounds of docs/05-api-contract.md section 10.3: the editor's
// docked test panel sends one; the request may carry at most 50 of at
// most 500 UTF-8 bytes each.
const (
	testRuleMaxTitles      = 50
	testRuleMaxTitleBytes  = 500
	testRuleTitlesLocation = "body.titles"
)

// TestRuleInput is the body of POST /rules/test (docs/05-api-contract.md
// section 10.3). The rule arrives as raw JSON so a malformed document is
// this handler's 422 — errors[].location naming the member — never a
// schema-level rejection with a shape the editor cannot point at.
type TestRuleInput struct {
	Body struct {
		Rule        json.RawMessage `json:"rule"                    required:"true"             doc:"The full rule document, unsaved"`
		Feeds       []string        `json:"feeds,omitempty"         doc:"Feed ids; the default is rule.feeds resolved by url, else every enabled feed"`
		Limit       int             `json:"limit,omitempty"         minimum:"1" maximum:"500" default:"200" doc:"Items per feed, newest first"`
		IgnoreState *bool           `json:"ignore_state,omitempty"  default:"true" doc:"Default true: bypass rule_matches and rule_seen_episodes so the preview repeats byte for byte"`
		Titles      []string        `json:"titles,omitempty"        doc:"At most 50 titles of at most 500 UTF-8 bytes each, evaluated statelessly in place of stored items; feeds, limit and ignore_state do not apply"`
	}
}

// TestRuleOutput carries the 200 dry-run report.
type TestRuleOutput struct {
	Body rss.DryRunReport
}

// RuleOutput carries 201 from Create and 200 from Patch.
type RuleOutput struct {
	Status int `json:"-"`
	Body   RuleDTO
}

// RunRuleInput addresses one rule by id for POST /rules/{id}/run.
type RunRuleInput struct {
	ID string `path:"id" doc:"The rul_ id of the rule"`
}

// RunRuleOutput carries the 200 run report of docs/05-api-contract.md
// section 10.2.
type RunRuleOutput struct {
	Body struct {
		Evaluated      int      `json:"evaluated"`
		Matched        int      `json:"matched"`
		CreatedTaskIDs []string `json:"created_task_ids"`
		ElapsedMS      int64    `json:"elapsed_ms"`
	}
}

// ruleTaskCreator adapts the task handlers to rss.TaskCreator: it builds
// the same body POST /tasks accepts and calls CreateTasks, so a rule grab
// passes the same normalisation, routing, destination containment and
// concurrency checks a pasted URI passes. It never writes the tasks table
// directly.
type ruleTaskCreator struct{ tasks *TaskHandlers }

// CreateForRule turns one grabbed item into a one-URI POST /tasks body.
// action.engine is an engine id (eng_qbittorrent) while the body's engine
// member takes the bare kind, so the prefix is stripped. A submission the
// create path refuses is a grab failure — its detail lands in the match
// row's reason — never a fabricated task id.
func (c ruleTaskCreator) CreateForRule(ctx context.Context, g rss.GrabRequest) (string, error) {
	input := &CreateTasksInput{}
	input.Body.URIs = []string{g.URI}
	input.Body.Destination = g.Destination
	input.Body.Category = g.Category
	input.Body.Tags = g.Tags
	input.Body.Paused = g.Paused
	// content_layout "subfolder" maps onto create_subfolder; original and
	// no_subfolder both leave it false — the finer distinction has no
	// POST /tasks equivalent (F551 records the gap).
	input.Body.CreateSubfolder = g.ContentLayout == "subfolder"
	input.Body.Engine = strings.TrimPrefix(g.Engine, store.EngineIDPrefix)

	output, err := c.tasks.CreateTasks(ctx, input)
	if err != nil {
		return "", err
	}
	if len(output.Body.Created) == 1 {
		return output.Body.Created[0].ID, nil
	}
	if len(output.Body.Rejected) > 0 {
		return "", errors.New(output.Body.Rejected[0].Detail)
	}

	return "", errors.New("the submission created no task")
}

// RuleHandlers owns the rule CRUD of docs/05-api-contract.md section 10.2
// over the rules table. creator is the one ruleTaskCreator the composition
// root built — the same instance the feed pollers carry; a nil creator
// leaves POST /rules/{id}/run answering 503.
type RuleHandlers struct {
	db      *sqlx.DB
	creator rss.TaskCreator
}

// NewRuleHandlers builds the rule handlers over db; tc is the shared
// rss.TaskCreator of T071.
func NewRuleHandlers(db *sqlx.DB, tc rss.TaskCreator) *RuleHandlers {
	return &RuleHandlers{db: db, creator: tc}
}

// Register mounts the four operations on the Huma API;
// Server.registerOperations is the call site.
func (h *RuleHandlers) Register(hapi huma.API) {
	huma.Register(hapi, huma.Operation{
		OperationID: operationListRules,
		Method:      http.MethodGet,
		Path:        "/rules",
		Summary:     "List the rules",
		Description: "Every rule ordered by (priority, name) — the evaluation order — each carrying its stored rule document.",
		Tags:        []string{"rules"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.List)

	huma.Register(hapi, huma.Operation{
		OperationID:   operationCreateRule,
		Method:        http.MethodPost,
		Path:          "/rules",
		DefaultStatus: http.StatusCreated,
		Summary:       "Create a rule",
		Description:   "Stores one validated rule document; every malformed member is rejected at save time with an errors[] entry naming it — a malformed episode.filter is never accepted and silently ignored at match time. A duplicate name is 409 /problems/conflict and an auto:-prefixed name is 422: the prefix is the feed lifecycle's.",
		Tags:          []string{"rules"},
		Security:      credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Create)

	huma.Register(hapi, huma.Operation{
		OperationID: operationPatchRule,
		Method:      http.MethodPatch,
		Path:        "/rules/{id}",
		Summary:     "Update a rule",
		Description: "Partial update of name, enabled, priority and definition; omitted fields are untouched. A provided definition is re-validated like a create's, so a malformed document can never replace a stored one. Renaming onto an existing name is 409 /problems/conflict; renaming onto an auto:-prefixed name is 422.",
		Tags:        []string{"rules"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Patch)

	huma.Register(hapi, huma.Operation{
		OperationID:   operationDeleteRule,
		Method:        http.MethodDelete,
		Path:          "/rules/{id}",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Delete a rule",
		Description:   "Removes the rule row. An auto:<feed_id> rule deletes like any other — this is how an operator disables a feed's auto-download without touching the feed.",
		Tags:          []string{"rules"},
		Security:      credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Delete)

	huma.Register(hapi, huma.Operation{
		OperationID: operationTestRule,
		Method:      http.MethodPost,
		Path:        "/rules/test",
		Summary:     "Dry-run a rule",
		Description: "Evaluates an unsaved rule document against the stored items of the named feeds and returns every evaluated item — matched and unmatched — with its score, the clause that matched, or a reason code and the clause index responsible. Nothing is created or stored, so the editor can call it on a 250 ms debounce. 404 when a named feed id does not exist; 422 when the document fails validation, with one errors[] entry per member.",
		Tags:        []string{"rules"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.TestRule)

	huma.Register(hapi, huma.Operation{
		OperationID: operationRunRule,
		Method:      http.MethodPost,
		Path:        "/rules/{id}/run",
		Summary:     "Run a rule against stored items",
		Description: "Evaluates the saved rule over the items already stored for its feeds — including a disabled rule, which the poller skips — and commits every accepted candidate through the ordinary task-creation path. created_task_ids can be shorter than matched: contested content_keys and the throttle cap both commit fewer tasks than matches. 404 for an unknown rule id; 503 /problems/engine-unavailable when the engine refuses every grab.",
		Tags:        []string{"rules"},
		Security:    credentialRequired,
		// Same strictness as every other operation: a mistyped query key
		// is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.RunRule)
}

// List serves GET /rules: every rule in evaluation order, definition
// decoded from its stored JSON.
func (h *RuleHandlers) List(ctx context.Context, _ *struct{}) (*ListRulesOutput, error) {
	rows, err := store.ListRules(ctx, h.db, false)
	if err != nil {
		return nil, internalFailure(ctx, "list rules", err)
	}

	output := &ListRulesOutput{}
	output.Body.Rules = make([]RuleDTO, 0, len(rows))
	for _, row := range rows {
		dto, err := ruleDTO(row)
		if err != nil {
			return nil, internalFailure(ctx, "decode rule", err)
		}
		output.Body.Rules = append(output.Body.Rules, dto)
	}

	return output, nil
}

// Create serves POST /rules. The name checks run before the document's:
// an auto: prefix is the feed lifecycle's and a definition.name must equal
// the body's name, so a stored rule and its document can never carry two
// names. Then ApplyDefaults and Validate produce one errors[] entry per
// malformed member, and the stored definition_json is the compact marshal
// of the defaulted document — so what round-trips is exactly what was
// validated.
func (h *RuleHandlers) Create(ctx context.Context, in *CreateRuleInput) (*RuleOutput, error) {
	if strings.HasPrefix(in.Body.Name, autoRulePrefix) {
		return nil, ruleFieldProblem("body.name", ruleAutoNameDetail)
	}
	doc := in.Body.Definition
	if doc.Name != in.Body.Name {
		return nil, ruleFieldProblem("body.definition.name", ruleNameDetail)
	}
	if err := checkRuleFeeds(doc); err != nil {
		return nil, err
	}

	doc.ApplyDefaults()
	if errs := doc.Validate(); len(errs) > 0 {
		return nil, ruleValidationProblem(errs)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, internalFailure(ctx, "encode rule document", err)
	}

	rule := store.Rule{
		ID:             store.NewID(store.PrefixRule),
		Name:           in.Body.Name,
		Enabled:        *doc.Enabled,
		Priority:       doc.Priority,
		DefinitionJSON: string(raw),
	}
	if err := store.CreateRule(ctx, h.db, rule); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, Problem(SlugConflict, http.StatusConflict, ruleConflictDetail)
		}

		return nil, internalFailure(ctx, "create rule", err)
	}

	// Read back so the answer carries the timestamps the insert stamped.
	created, err := store.RuleByID(ctx, h.db, rule.ID)
	if err != nil {
		return nil, internalFailure(ctx, "read back rule", err)
	}
	dto, err := ruleDTO(created)
	if err != nil {
		return nil, internalFailure(ctx, "decode rule", err)
	}

	return &RuleOutput{Status: http.StatusCreated, Body: dto}, nil
}

// Patch serves PATCH /rules/{id}. The effective document is the submitted
// definition or the stored one; the provided top-level members then merge
// onto it — name, enabled and priority are the mirrored columns, so the
// stored document is rewritten to match them before Validate. Renaming
// onto an auto:-prefixed name is 422 while echoing an auto: rule's own
// name stays legal: the reservation covers creates and renames, never
// edits.
func (h *RuleHandlers) Patch(ctx context.Context, in *PatchRuleInput) (*RuleOutput, error) {
	rule, err := store.RuleByID(ctx, h.db, in.ID)
	if err != nil {
		return nil, FromStore(err)
	}

	var doc rss.RuleDoc
	if err := json.Unmarshal([]byte(rule.DefinitionJSON), &doc); err != nil {
		return nil, internalFailure(ctx, "decode stored rule", err)
	}
	if in.Body.Definition != nil {
		doc = *in.Body.Definition
		if in.Body.Name != nil && doc.Name != *in.Body.Name {
			return nil, ruleFieldProblem("body.definition.name", ruleNameDetail)
		}
	}

	effectiveName := rule.Name
	switch {
	case in.Body.Name != nil:
		effectiveName = *in.Body.Name
	case in.Body.Definition != nil && doc.Name != "":
		effectiveName = doc.Name
	case in.Body.Definition != nil:
		// A replacement document whose name is empty keeps the stored
		// name rather than blank it.
		effectiveName = rule.Name
	}
	if effectiveName == "" {
		return nil, ruleFieldProblem("body.name", ruleEmptyNameDetail)
	}
	if effectiveName != rule.Name && strings.HasPrefix(effectiveName, autoRulePrefix) {
		return nil, ruleFieldProblem("body.name", ruleAutoNameDetail)
	}
	// The feeds check guards submitted documents only: a PATCH without a
	// definition never rewrites the member, so the stored document's
	// feeds are not the request's to validate.
	if in.Body.Definition != nil {
		if err := checkRuleFeeds(doc); err != nil {
			return nil, err
		}
	}

	doc.ApplyDefaults()
	doc.Name = effectiveName
	if in.Body.Enabled != nil {
		*doc.Enabled = *in.Body.Enabled
	}
	if in.Body.Priority != nil {
		doc.Priority = *in.Body.Priority
	}
	if errs := doc.Validate(); len(errs) > 0 {
		return nil, ruleValidationProblem(errs)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, internalFailure(ctx, "encode rule document", err)
	}

	rule.Name = effectiveName
	rule.Enabled = *doc.Enabled
	rule.Priority = doc.Priority
	rule.DefinitionJSON = string(raw)
	if err := store.UpdateRule(ctx, h.db, rule); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, Problem(SlugConflict, http.StatusConflict, ruleConflictDetail)
		}

		return nil, FromStore(err)
	}

	// Read back so the answer carries the updated_at the write stamped.
	updated, err := store.RuleByID(ctx, h.db, in.ID)
	if err != nil {
		return nil, internalFailure(ctx, "read back rule", err)
	}
	dto, err := ruleDTO(updated)
	if err != nil {
		return nil, internalFailure(ctx, "decode rule", err)
	}

	return &RuleOutput{Status: http.StatusOK, Body: dto}, nil
}

// Delete serves DELETE /rules/{id}: the row goes — an auto:<feed_id> rule
// deletes like any other (doc 09 section 8.1), which is how an operator
// disables a feed's auto-download without touching the feed.
func (h *RuleHandlers) Delete(ctx context.Context, in *DeleteRuleInput) (*struct{}, error) {
	if err := store.DeleteRule(ctx, h.db, in.ID); err != nil {
		return nil, FromStore(err)
	}

	return nil, nil
}

// TestRule serves POST /rules/test: the unsaved document is unmarshalled,
// defaulted and validated before any database access — one errors[] entry
// per FieldError, located under body.rule — then rss.DryRun evaluates it.
// The handler takes no transaction and writes nothing; every pattern was
// compiled during Validate, so evaluation cannot panic.
func (h *RuleHandlers) TestRule(ctx context.Context, in *TestRuleInput) (*TestRuleOutput, error) {
	var doc rss.RuleDoc
	if err := json.Unmarshal(in.Body.Rule, &doc); err != nil {
		return nil, ruleFieldProblem("body.rule", "the rule document is not valid JSON")
	}

	doc.ApplyDefaults()
	if errs := doc.Validate(); len(errs) > 0 {
		return nil, ruleValidationProblemAt("body.rule.", errs)
	}

	// A present-but-empty array is a 422 like an oversized one; absent
	// (nil) leaves the stored-item path untouched.
	if in.Body.Titles != nil {
		if len(in.Body.Titles) == 0 {
			return nil, ruleFieldProblem(testRuleTitlesLocation, "at least one title is required")
		}
		if len(in.Body.Titles) > testRuleMaxTitles {
			return nil, ruleFieldProblem(testRuleTitlesLocation,
				fmt.Sprintf("at most %d titles, got %d", testRuleMaxTitles, len(in.Body.Titles)))
		}
		for _, title := range in.Body.Titles {
			if len(title) > testRuleMaxTitleBytes {
				return nil, ruleFieldProblem(testRuleTitlesLocation,
					fmt.Sprintf("a title is at most %d UTF-8 bytes", testRuleMaxTitleBytes))
			}
		}
	}

	ignoreState := true
	if in.Body.IgnoreState != nil {
		ignoreState = *in.Body.IgnoreState
	}
	report, err := rss.DryRun(ctx, h.db, rss.DryRunRequest{
		Rule:        doc,
		FeedIDs:     in.Body.Feeds,
		Limit:       in.Body.Limit,
		IgnoreState: ignoreState,
		Titles:      in.Body.Titles,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, FromStore(err)
		}

		return nil, internalFailure(ctx, "test rule", err)
	}

	return &TestRuleOutput{Body: report}, nil
}

// RunRule serves POST /rules/{id}/run: the saved rule evaluates the items
// already stored for its feeds and commits every accepted candidate —
// task creation, fallback rows, episode keys and the last_match_at
// watermark all land through rss.RunRule (docs/05-api-contract.md section
// 10.2). A nil creator — the document-only builds — answers 503, and so
// does a run whose every grab the create path refused.
func (h *RuleHandlers) RunRule(ctx context.Context, in *RunRuleInput) (*RunRuleOutput, error) {
	if h.creator == nil {
		return nil, Problem(SlugEngineUnavailable, http.StatusServiceUnavailable, "the rule runner is not configured")
	}

	started := time.Now()
	report, err := rss.RunRule(ctx, h.db, in.ID, 0, h.creator, started.UnixMilli())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, FromStore(err)
		}

		return nil, internalFailure(ctx, "run rule", err)
	}

	// Dedup-suppressed candidates stay inside matched — the fallbacks of a
	// contested content_key included — but the grabs are the winners:
	// every one attempted and none created a task is the documented 503.
	if grabs := report.Matched - report.Fallbacks; grabs > 0 && len(report.CreatedTaskIDs) == 0 {
		return nil, Problem(SlugEngineUnavailable, http.StatusServiceUnavailable, "every grab was refused")
	}

	output := &RunRuleOutput{}
	output.Body.Evaluated = report.Evaluated
	output.Body.Matched = report.Matched
	output.Body.CreatedTaskIDs = report.CreatedTaskIDs
	if output.Body.CreatedTaskIDs == nil {
		// The empty list encodes [], never null — the same rule every
		// other list member of the contract follows.
		output.Body.CreatedTaskIDs = []string{}
	}
	output.Body.ElapsedMS = time.Since(started).Milliseconds()

	return output, nil
}

// ruleValidationProblem renders the 422 of doc 05 section 10.2: one
// errors[] entry per FieldError, its location prefixed into the definition
// member — episode.filter arrives as body.definition.episode.filter.
func ruleValidationProblem(errs []rss.FieldError) error {
	return ruleValidationProblemAt("body.definition.", errs)
}

// ruleValidationProblemAt is ruleValidationProblem against another member:
// POST /rules/test carries the document in body.rule, so its errors[] point
// there instead of body.definition.
func ruleValidationProblemAt(prefix string, errs []rss.FieldError) error {
	details := make([]*huma.ErrorDetail, 0, len(errs))
	for _, fieldErr := range errs {
		details = append(details, &huma.ErrorDetail{
			Message:  fieldErr.Message,
			Location: prefix + fieldErr.Location,
		})
	}

	return &huma.ErrorModel{
		Type:   SlugValidationFailed,
		Title:  http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity,
		Detail: "the rule document is invalid",
		Errors: details,
	}
}

// checkRuleFeeds rejects a feeds entry still carrying the __redacted__
// sentinel: it is a rendered form copied off a GET response, not a
// fetchable address — storing it would silently break matching, the same
// reason POST /feeds refuses a redacted url.
func checkRuleFeeds(doc rss.RuleDoc) error {
	for i, feedURL := range doc.Feeds {
		if strings.Contains(feedURL, redactedValue) {
			location := fmt.Sprintf("body.definition.feeds[%d]", i)

			return ruleFieldProblem(location, ruleRedactedFeedsDetail)
		}
	}

	return nil
}

// ruleFieldProblem is the single-member 422 the name checks produce: it
// rides the same validation slug so a client reads one error shape.
func ruleFieldProblem(location, detail string) error {
	return &huma.ErrorModel{
		Type:   SlugValidationFailed,
		Title:  http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity,
		Detail: detail,
		Errors: []*huma.ErrorDetail{{Message: detail, Location: location}},
	}
}

// ruleDTO renders one store row into the section 10.2 rule object: the
// definition member is the stored document decoded, and the millisecond
// columns become RFC 3339 or null.
func ruleDTO(r store.Rule) (RuleDTO, error) {
	var doc rss.RuleDoc
	if err := json.Unmarshal([]byte(r.DefinitionJSON), &doc); err != nil {
		return RuleDTO{}, fmt.Errorf("decode definition of rule %s: %w", r.ID, err)
	}
	// Feed urls get the member-wise redaction of doc 05 section 10.1 like
	// the feed object's url: the stored document keeps the secret —
	// matching needs it verbatim — but the API never returns it.
	for i := range doc.Feeds {
		doc.Feeds[i] = redactFeedURL(doc.Feeds[i])
	}

	return RuleDTO{
		ID:          r.ID,
		Name:        r.Name,
		Enabled:     r.Enabled,
		Priority:    r.Priority,
		Definition:  doc,
		LastMatchAt: unixMilliToRFC3339(r.LastMatchAt),
		CreatedAt:   time.UnixMilli(r.CreatedAt).UTC().Format(time.RFC3339),
		UpdatedAt:   time.UnixMilli(r.UpdatedAt).UTC().Format(time.RFC3339),
	}, nil
}
