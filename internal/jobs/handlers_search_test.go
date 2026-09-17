package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/L-K-M/dl-tool/internal/search"
	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

// newSearchFixture is one migrated store, its indexer store and the strict
// SSRF-guarded client the handler gets in production; the seeded rows lift
// the private-range denial per origin through allow_private_network.
func newSearchFixture(t *testing.T) (*sqlx.DB, *store.IndexerStore, *http.Client) {
	t.Helper()
	db := newTestDB(t)
	idx, err := store.NewIndexerStore(db, secure.Secret("search-handler-test-key"))
	require.NoError(t, err)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return db, idx, secure.NewClient(secure.NewGuard(log, false))
}

// seedSearchIndexer inserts an enabled torznab row pointed at the stub.
func seedSearchIndexer(t *testing.T, idx *store.IndexerStore, name, baseURL string) store.Indexer {
	t.Helper()
	settings := `{"allow_private_network":true,"origin":"` + baseURL + `"}`
	row, err := idx.Create(t.Context(), store.Indexer{
		Name:         name,
		Kind:         "torznab",
		Enabled:      true,
		URL:          &baseURL,
		SettingsJSON: &settings,
		Priority:     store.DefaultIndexerPriority,
	}, secure.Secret("k"))
	require.NoError(t, err)
	return row
}

// newSearchJob writes the search_jobs row and builds the queue row's decoded
// form the handler receives; the tests run the handler over it directly.
func newSearchJob(t *testing.T, db *sqlx.DB, indexerIDs ...string) (store.SearchJob, store.Job) {
	t.Helper()
	idsJSON, err := json.Marshal(indexerIDs)
	require.NoError(t, err)
	job, err := store.CreateSearchJob(t.Context(), db, store.SearchJob{
		Query:          "q",
		IndexerIDsJSON: string(idsJSON),
	})
	require.NoError(t, err)
	payload, err := json.Marshal(SearchPayload{
		SearchJobID: job.ID,
		Query:       "q",
		IndexerIDs:  indexerIDs,
	})
	require.NoError(t, err)
	t.Cleanup(func() { store.Searches.Forget(job.ID) })
	return job, store.Job{Kind: JobKindSearch, PayloadJSON: string(payload)}
}

// runSearchJob executes the real handler over the built row.
func runSearchJob(t *testing.T, db *sqlx.DB, idx *store.IndexerStore, hc *http.Client, job store.Job) {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := NewSearchHandler(db, log, nil, nil, idx, hc, "dl-tool/test")
	require.NoError(t, h(t.Context(), job))
}

// searchFeedXML renders one RSS item per title, each carrying a .torrent
// enclosure so Finalise keeps the row.
func searchFeedXML(titles ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed"><channel>`)
	for i, title := range titles {
		fmt.Fprintf(&b,
			`<item><title>%s</title><enclosure url="https://x.test/f%d.torrent" length="10%d"/></item>`,
			title, i, i,
		)
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

// engineByID finds one engine's tracked status.
func engineByID(t *testing.T, engines []store.EngineStatus, id string) store.EngineStatus {
	t.Helper()
	for _, e := range engines {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("engine %s missing from snapshot %+v", id, engines)
	return store.EngineStatus{}
}

// engineResultCount counts one engine's persisted rows for the job.
func engineResultCount(t *testing.T, db *sqlx.DB, jobID, indexerID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.GetContext(t.Context(), &n,
		"SELECT COUNT(*) FROM search_results WHERE search_job_id = ? AND indexer_id = ?",
		jobID, indexerID))
	return n
}

// TestFanOutWritesPerEngine is the happy path: every engine's rows land under
// its own indexer id as it answers, and the tracker reports each done with
// its count.
func TestFanOutWritesPerEngine(t *testing.T) {
	db, idx, hc := newSearchFixture(t)

	one := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(searchFeedXML("one-a", "one-b")))
	}))
	defer one.Close()
	two := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(searchFeedXML("two-a")))
	}))
	defer two.Close()

	r1 := seedSearchIndexer(t, idx, "one", one.URL+"/api")
	r2 := seedSearchIndexer(t, idx, "two", two.URL+"/api")

	job, queueRow := newSearchJob(t, db, r1.ID, r2.ID)
	runSearchJob(t, db, idx, hc, queueRow)

	engines, ok := store.Searches.Snapshot(job.ID)
	require.True(t, ok, "tracker must hold the finished job's engines")
	require.Len(t, engines, 2)
	require.Equal(t, store.EngineStatus{ID: r1.ID, Name: "one", Status: store.EngineDone, Count: 2},
		engineByID(t, engines, r1.ID))
	require.Equal(t, store.EngineStatus{ID: r2.ID, Name: "two", Status: store.EngineDone, Count: 1},
		engineByID(t, engines, r2.ID))

	require.Equal(t, 2, engineResultCount(t, db, job.ID, r1.ID))
	require.Equal(t, 1, engineResultCount(t, db, job.ID, r2.ID))

	stored, err := store.GetSearchJob(t.Context(), db, job.ID)
	require.NoError(t, err)
	require.True(t, stored.Finished)
	require.Equal(t, 3, stored.Total)
	require.Nil(t, stored.LastError)
}

// TestOneEngineFailsOthersSucceed is the FR-055 mixed outcome: the failing
// indexer's status is "error" with the upstream status named, the healthy
// engine's results persist, and the job still finishes.
func TestOneEngineFailsOthersSucceed(t *testing.T) {
	db, idx, hc := newSearchFixture(t)

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(searchFeedXML("kept-a", "kept-b")))
	}))
	defer good.Close()

	badRow := seedSearchIndexer(t, idx, "broken", bad.URL+"/api")
	goodRow := seedSearchIndexer(t, idx, "healthy", good.URL+"/api")

	job, queueRow := newSearchJob(t, db, badRow.ID, goodRow.ID)
	runSearchJob(t, db, idx, hc, queueRow)

	engines, ok := store.Searches.Snapshot(job.ID)
	require.True(t, ok)
	require.Len(t, engines, 2)

	failed := engineByID(t, engines, badRow.ID)
	require.Equal(t, store.EngineError, failed.Status)
	require.NotNil(t, failed.Error)
	assert.Contains(t, *failed.Error, "503", "the upstream status must reach the engine error")

	healthy := engineByID(t, engines, goodRow.ID)
	require.Equal(t, store.EngineDone, healthy.Status)
	require.Equal(t, 2, healthy.Count)
	require.Nil(t, healthy.Error)

	require.Equal(t, 0, engineResultCount(t, db, job.ID, badRow.ID))
	require.Equal(t, 2, engineResultCount(t, db, job.ID, goodRow.ID))

	// The settings table shows the same message the poll does.
	row, err := idx.Get(t.Context(), badRow.ID)
	require.NoError(t, err)
	require.NotNil(t, row.LastError)
	assert.Contains(t, *row.LastError, "503")
	require.NotNil(t, row.LastTestAt)

	stored, err := store.GetSearchJob(t.Context(), db, job.ID)
	require.NoError(t, err)
	require.True(t, stored.Finished)
	require.Equal(t, 2, stored.Total)
	require.NotNil(t, stored.LastError)
	assert.Contains(t, *stored.LastError, "1 of 2")
}

// TestAllEnginesFailFinishesWithErrors: a search in which every indexer
// failed still finishes — finished with total 0 and an engine list of
// errors, never left running (doc 05 section 9.2).
func TestAllEnginesFailFinishesWithErrors(t *testing.T) {
	db, idx, hc := newSearchFixture(t)

	one := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer one.Close()
	two := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer two.Close()

	r1 := seedSearchIndexer(t, idx, "one", one.URL+"/api")
	r2 := seedSearchIndexer(t, idx, "two", two.URL+"/api")

	job, queueRow := newSearchJob(t, db, r1.ID, r2.ID)
	runSearchJob(t, db, idx, hc, queueRow)

	engines, ok := store.Searches.Snapshot(job.ID)
	require.True(t, ok)
	require.Len(t, engines, 2)
	for _, e := range engines {
		require.Equal(t, store.EngineError, e.Status, "engine %s must report error", e.ID)
		require.NotNil(t, e.Error, "engine %s must carry a message", e.ID)
	}

	stored, err := store.GetSearchJob(t.Context(), db, job.ID)
	require.NoError(t, err)
	require.True(t, stored.Finished, "an all-failed search is finished, never left running")
	require.Equal(t, 0, stored.Total)
	require.NotNil(t, stored.LastError)
	assert.Contains(t, *stored.LastError, "2 of 2", "last_error names the count of failed engines")
}

// TestTorznabErrorCodeClassification covers every row of doc 07 section 2.5
// plus Prowlarr's non-spec 400/410/429 answers and the untyped transports.
func TestTorznabErrorCodeClassification(t *testing.T) {
	const url = "https://indexer.example/api"

	cases := []struct {
		name        string
		err         error
		status      int
		wantNil     bool // code 300: an empty result set, not a failure
		wantText    string
		wantDisable bool
		wantMode    string
		wantRetry   time.Duration
	}{
		{"100 incorrect credentials", &search.TorznabError{Code: 100, Description: "Incorrect credentials"}, 0, false, "check API key: Incorrect credentials", true, "", 0},
		{"101 account suspended", &search.TorznabError{Code: 101, Description: "Account suspended"}, 0, false, "check API key: Account suspended", true, "", 0},
		{"102 insufficient privileges", &search.TorznabError{Code: 102, Description: "Insufficient privileges"}, 0, false, "check API key: Insufficient privileges", true, "", 0},
		{"200 missing parameter", &search.TorznabError{Code: 200, Description: "Missing parameter (t)", HTTPStatus: 400}, 0, false, "torznab error 200 (http 400): Missing parameter (t)", false, "", 0},
		{"201 incorrect parameter", &search.TorznabError{Code: 201, Description: "Incorrect parameter"}, 0, false, "torznab error 201: Incorrect parameter", false, "", 0},
		{"900 unknown error", &search.TorznabError{Code: 900, Description: "boom"}, 0, false, "torznab error 900: boom", false, "", 0},
		{"202 no such function", &search.TorznabError{Code: 202, Description: "No such function"}, 0, false, "the engine does not support t=search: No such function", false, "search", 0},
		{"203 function not available", &search.TorznabError{Code: 203, Description: "Function not available"}, 0, false, "the engine does not support t=search: Function not available", false, "search", 0},
		{"300 no such item is empty", &search.TorznabError{Code: 300, Description: "No such item"}, 0, true, "", false, "", 0},
		{"910 api disabled", &search.TorznabError{Code: 910, Description: "API disabled"}, 0, false, "check API key: API disabled", true, "", 0},
		{"410 disabled upstream", &search.TorznabError{Code: 410, Description: "Indexer is disabled", HTTPStatus: 410}, 0, false, "the engine is disabled upstream: Indexer is disabled", true, "", 0},
		{"429 honours retry-after", &search.TorznabError{Code: 429, Description: "rate limited", HTTPStatus: 429, RetryAfter: 30 * time.Second}, 0, false, "HTTP 429 from https://indexer.example/api; retry after 30s", false, "", 30 * time.Second},
		{"upstream 503 carries retry-after", &search.UpstreamError{Status: 503, Detail: "maintenance", RetryAfter: 45 * time.Second}, 0, false, "HTTP 503 from https://indexer.example/api; retry after 45s", false, "", 45 * time.Second},
		{"bare status with untyped error", errors.New("read timed out"), 503, false, "HTTP 503 from https://indexer.example/api", false, "", 0},
		{"transport error", errors.New("connection refused"), 0, false, "connection refused", false, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := classify(tc.err, tc.status, url)
			if tc.wantNil {
				assert.Nil(t, f)
				return
			}
			require.NotNil(t, f)
			assert.Equal(t, tc.wantText, f.Text)
			assert.Equal(t, tc.wantDisable, f.Disable)
			assert.Equal(t, tc.wantMode, f.DisableMode)
			assert.Equal(t, tc.wantRetry, f.RetryAfter)
		})
	}
}

// TestRetryAfterIsReported runs the fan-out against a Prowlarr-style 429: the
// engine ends in error and the message carries the wait the Retry-After
// header asked for.
func TestRetryAfterIsReported(t *testing.T) {
	db, idx, hc := newSearchFixture(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`<error code="429" description="query limit reached"/>`))
	}))
	defer srv.Close()

	row := seedSearchIndexer(t, idx, "throttled", srv.URL+"/api")
	job, queueRow := newSearchJob(t, db, row.ID)
	runSearchJob(t, db, idx, hc, queueRow)

	engines, ok := store.Searches.Snapshot(job.ID)
	require.True(t, ok)
	require.Len(t, engines, 1)
	require.Equal(t, store.EngineError, engines[0].Status)
	require.NotNil(t, engines[0].Error)
	assert.Contains(t, *engines[0].Error, "429")
	assert.Contains(t, *engines[0].Error, "30s", "the message must carry the Retry-After wait")

	stored, err := store.GetSearchJob(t.Context(), db, job.ID)
	require.NoError(t, err)
	require.True(t, stored.Finished)
}
