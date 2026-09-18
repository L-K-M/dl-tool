package store

import (
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"
)

// seedSearchFixture inserts one indexer row, one search job and the given
// result rows through the store APIs, returning the job id and the res_ ids
// InsertResults assigned, keyed by title — the generated ULIDs are random,
// not insert-ordered.
func seedSearchFixture(t *testing.T, db *sqlx.DB, rows []SearchResultRow) (string, map[string]string) {
	t.Helper()

	indexerID := NewID(PrefixIndexer)
	_, err := db.ExecContext(t.Context(), `INSERT INTO indexers
(id, name, kind, enabled, url, priority, created_at, updated_at)
VALUES (?, 'stub', 'torznab', 1, 'https://x.test', 50, 0, 0)`, indexerID)
	require.NoError(t, err)

	job, err := CreateSearchJob(t.Context(), db, SearchJob{
		Query:          "fixture",
		IndexerIDsJSON: `["` + indexerID + `"]`,
	})
	require.NoError(t, err)

	n, err := InsertResults(t.Context(), db, job.ID, indexerID, rows)
	require.NoError(t, err)
	require.Len(t, rows, n)

	var pairs []struct {
		ID    string `db:"id"`
		Title string `db:"title"`
	}
	require.NoError(t, db.SelectContext(t.Context(), &pairs,
		`SELECT id, title FROM search_results WHERE search_job_id = ?`, job.ID))
	ids := make(map[string]string, len(pairs))
	for _, p := range pairs {
		ids[p.Title] = p.ID
	}

	return job.ID, ids
}

// TestGetSearchResultResolvesLiveRow pins the resolution contract of doc 05
// section 9.2: a res_ id returns the stored row including the server-only
// acquisition fields the API layer resolves into a task source.
func TestGetSearchResultResolvesLiveRow(t *testing.T) {
	db, _, _ := openTestStore(t)

	magnet := "magnet:?xt=urn:btih:8f9c3a2b1d4e5f60718293a4b5c6d7e8f9a0b1c2"
	download := "https://provider.example/dl?t=1&passkey=k3y"
	_, ids := seedSearchFixture(t, db, []SearchResultRow{
		{Title: "one", MagnetURI: &magnet, DownloadURL: &download},
	})

	row, err := GetSearchResult(t.Context(), db, ids["one"])
	require.NoError(t, err)
	require.Equal(t, ids["one"], row.ID)
	require.Equal(t, "one", row.Title)
	require.Equal(t, magnet, *row.MagnetURI)
	require.Equal(t, download, *row.DownloadURL)
}

// TestGetSearchResultGoneJobIsNotFound: deleting the owning job makes its
// result ids indistinguishable from unknown ids — the IN guard keeps even a
// result row orphaned by a missing cascade from resolving.
func TestGetSearchResultGoneJobIsNotFound(t *testing.T) {
	db, _, _ := openTestStore(t)

	magnet := "magnet:?xt=urn:btih:8f9c3a2b1d4e5f60718293a4b5c6d7e8f9a0b1c2"
	jobID, ids := seedSearchFixture(t, db, []SearchResultRow{
		{Title: "one", MagnetURI: &magnet},
	})

	require.NoError(t, DeleteSearchJob(t.Context(), db, jobID))

	_, err := GetSearchResult(t.Context(), db, ids["one"])
	require.ErrorIs(t, err, ErrNotFound)

	// An id that never existed answers identically.
	_, err = GetSearchResult(t.Context(), db, "res_00000000000000000000000000")
	require.ErrorIs(t, err, ErrNotFound)
}

// TestGetSearchResultRequiresAcquisitionSource: a row holding neither
// magnet_uri nor download_url can never become a task, so it resolves as
// not-found — the caller cannot tell it apart from a deleted result.
func TestGetSearchResultRequiresAcquisitionSource(t *testing.T) {
	db, _, _ := openTestStore(t)

	_, ids := seedSearchFixture(t, db, []SearchResultRow{{Title: "metadata only"}})

	_, err := GetSearchResult(t.Context(), db, ids["metadata only"])
	require.ErrorIs(t, err, ErrNotFound)

	// And a second job whose result does carry a source resolves, proving
	// the predicate is the missing source, not the fixture shape.
	magnet := "magnet:?xt=urn:btih:8f9c3a2b1d4e5f60718293a4b5c6d7e8f9a0b1c2"
	_, okIDs := seedSearchFixture(t, db, []SearchResultRow{{Title: "ok", MagnetURI: &magnet}})
	row, err := GetSearchResult(t.Context(), db, okIDs["ok"])
	require.NoError(t, err)
	require.Equal(t, "ok", row.Title)
}
