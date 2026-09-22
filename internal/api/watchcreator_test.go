package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/L-K-M/dl-tool/internal/jobs"
)

// The adapter tests drive Server.WatchCreator — the field cmd/dl-tool
// hands to jobs.NewWatcher — so they pin the production seam, not a test
// double. Every call goes through the real CreateTasks.

// TestWatchCreatorDuplicateMapsToSentinel covers step 10's first half: a
// live task carrying the blob's infohash answers jobs.ErrTorrentDuplicate.
// The adapter's FindByInfohash pre-check is the same lookup the create
// path's duplicateRejection runs.
func TestWatchCreatorDuplicateMapsToSentinel(t *testing.T) {
	env := newTasksTestEnv(t)
	creator := env.server.WatchCreator
	if creator == nil {
		t.Fatal("Server.WatchCreator is nil — NewServer must fill it")
	}

	taskID, err := creator.CreateFromTorrent(t.Context(), "hello.torrent", []byte(uploadSingleFileTorrent), env.dataRoot, "")
	if err != nil {
		t.Fatalf("first CreateFromTorrent: %v", err)
	}
	if taskID == "" {
		t.Fatal("first CreateFromTorrent returned an empty task id")
	}

	if _, err := creator.CreateFromTorrent(t.Context(), "hello.torrent", []byte(uploadSingleFileTorrent), env.dataRoot, ""); !errors.Is(err, jobs.ErrTorrentDuplicate) {
		t.Fatalf("duplicate CreateFromTorrent error = %v, want jobs.ErrTorrentDuplicate", err)
	}
}

// TestWatchCreatorPathRejectedMapsToSentinel covers step 10's second half:
// a destination outside the data roots answers
// jobs.ErrDestinationRejected, mapped off the problem's
// /problems/path-rejected type — so a reworded detail cannot break the
// watcher's path_rejected skip.
func TestWatchCreatorPathRejectedMapsToSentinel(t *testing.T) {
	env := newTasksTestEnv(t)

	outside := t.TempDir()
	_, err := env.server.WatchCreator.CreateFromTorrent(t.Context(), "hello.torrent", []byte(uploadSingleFileTorrent), outside, "")
	if !errors.Is(err, jobs.ErrDestinationRejected) {
		t.Fatalf("out-of-roots CreateFromTorrent error = %v, want jobs.ErrDestinationRejected", err)
	}
}

// TestWatchCreatorRequestedDestinationEcho pins FR-044 through the watch
// seam: a folder destination that resolves to a different path (here a
// symlink inside the roots) lands both columns on the task row — the
// resolved destination and the requested echo — through the same
// insertPlanned rule an upload follows.
func TestWatchCreatorRequestedDestinationEcho(t *testing.T) {
	env := newTasksTestEnv(t)

	real := filepath.Join(env.dataRoot, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("make real dir: %v", err)
	}
	link := filepath.Join(env.dataRoot, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	requested := filepath.Join(link, "watch")

	taskID, err := env.server.WatchCreator.CreateFromTorrent(t.Context(), "hello.torrent", []byte(uploadSingleFileTorrent), requested, "")
	if err != nil {
		t.Fatalf("CreateFromTorrent: %v", err)
	}

	var row struct {
		Destination          string  `db:"destination"`
		RequestedDestination *string `db:"requested_destination"`
		SourceKind           string  `db:"source_kind"`
	}
	if err := env.db.GetContext(t.Context(), &row,
		`SELECT destination, requested_destination, source_kind FROM tasks WHERE id = ?`, taskID); err != nil {
		t.Fatalf("read task row: %v", err)
	}

	wantResolved := filepath.Join(real, "watch")
	if row.Destination != wantResolved {
		t.Errorf("destination = %q, want the resolved %q", row.Destination, wantResolved)
	}
	if row.RequestedDestination == nil || *row.RequestedDestination != requested {
		t.Errorf("requested_destination = %v, want the requested %q", row.RequestedDestination, requested)
	}
	if row.SourceKind != "torrent" {
		t.Errorf("source_kind = %q, want torrent", row.SourceKind)
	}
}

// TestWatchCreatorSameDestinationLeavesEchoNull pins the other half of the
// echo rule: a folder destination that already resolves to itself carries
// a null requested_destination — the echo exists only when the two differ.
func TestWatchCreatorSameDestinationLeavesEchoNull(t *testing.T) {
	env := newTasksTestEnv(t)

	taskID, err := env.server.WatchCreator.CreateFromTorrent(t.Context(), "hello.torrent", []byte(uploadSingleFileTorrent), env.dataRoot, "")
	if err != nil {
		t.Fatalf("CreateFromTorrent: %v", err)
	}

	var requested *string
	if err := env.db.GetContext(t.Context(), &requested,
		`SELECT requested_destination FROM tasks WHERE id = ?`, taskID); err != nil {
		t.Fatalf("read task row: %v", err)
	}
	if requested != nil {
		t.Errorf("requested_destination = %q, want null — requested and resolved agree", *requested)
	}
}

// TestMapCreateError pins the sentinel translation the real path only
// reaches on the commit-between-check-and-insert race: the all-refused
// answer flattens rejected[] into the top-level detail, so the duplicate
// crosses back by its detail prefix and path rejection by its problem
// type. Both branches must stay pinned against drift in the create
// endpoint's wording.
func TestMapCreateError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{
			name: "duplicate detail prefix",
			err:  &huma.ErrorModel{Type: SlugUnsupportedScheme, Detail: fmt.Sprintf(duplicateDetailFormat, duplicateDetail, "tsk_123")},
			want: jobs.ErrTorrentDuplicate,
		},
		{
			name: "path rejected type",
			err:  &huma.ErrorModel{Type: SlugPathRejected, Detail: "outside"},
			want: jobs.ErrDestinationRejected,
		},
		{
			name: "unrelated problem passes through",
			err:  &huma.ErrorModel{Type: SlugValidationFailed, Detail: "nope"},
			want: nil,
		},
		{
			name: "non-problem error passes through",
			err:  errors.New("engine offline"),
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapCreateError(tc.err)
			if tc.want == nil {
				if !errors.Is(got, tc.err) {
					t.Fatalf("mapCreateError() = %v, want the original error unchanged", got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("mapCreateError() = %v, want errors.Is %v", got, tc.want)
			}
		})
	}
}
