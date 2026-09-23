// The shared delete-data executor of docs/05-api-contract.md section 5.6:
// the re-check of every recorded path and the unlinks run identically for
// the single delete and for every id of a bulk remove, so the only
// irreversible operation in the product has exactly one implementation.
package fsx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
)

// DeleteResult is the body of DELETE /tasks/{id}, doc 05 §5.6.
type DeleteResult struct {
	Deleted       bool  `json:"deleted"`
	DeleteData    bool  `json:"delete_data"`
	FilesUnlinked int   `json:"files_unlinked"`
	BytesUnlinked int64 `json:"bytes_unlinked"`
	Missing       int   `json:"missing"` // a recorded file that no longer exists is not an error
}

// Target is one recorded file: a task_files row resolved against
// tasks.destination. The executor is given targets; it never globs, never
// walks a directory and never uses content_path alone.
type Target struct {
	Path  string
	Bytes int64
}

// DeleteData performs the re-check and the unlink of the delete sequence
// of doc 05 §5.6 and nothing else. Enumerating the targets, stopping the
// task at its engine, recording the event and tombstoning the row belong
// to the caller, which must run them in the documented order.
//
// The re-check runs to completion BEFORE any unlink: every target is
// resolved, symlinks included, and must lie inside one of the configured
// roots. One failing target aborts the whole call with ErrPathRejected
// naming it and NOTHING AT ALL is unlinked.
//
// The unlink is one unlink(2) per recorded file, then a single attempt to
// remove the task's own directory, which succeeds only while it is empty.
// A non-empty directory is left in place.
//
// Hardlinks are expected and safe: a file dl-tool downloaded and then
// hardlinked into a media library is one inode with two names, so
// unlinking dl-tool's name leaves the library copy byte-for-byte intact.
// This is the normal outcome of the single /data mount (ADR-0012).
// Nothing here detects, warns about or refuses a delete because of it.
func DeleteData(ctx context.Context, roots []string, taskDir string, targets []Target) (DeleteResult, error) {
	if err := ctx.Err(); err != nil {
		return DeleteResult{}, err
	}

	// The complete re-check pass, through the same containment check the
	// destinations take, so a task_files row tampered into an escape is
	// refused here even when a caller skipped its own validation. No
	// unlink may run while a target's containment is unproven, so one
	// failure discards the whole batch.
	for _, target := range targets {
		if _, err := ResolveDestination(roots, target.Path); err != nil {
			// Wrap the resolver's verdict verbatim: a real rejection
			// stays errors.Is ErrPathRejected and any detail the
			// resolver wrapped is not erased.
			return DeleteResult{}, fmt.Errorf("fsx: delete target %s: %w", target.Path, err)
		}
	}

	result := DeleteResult{Deleted: true, DeleteData: true}
	for _, target := range targets {
		switch err := os.Remove(target.Path); {
		case err == nil:
			result.FilesUnlinked++
			result.BytesUnlinked += target.Bytes
		case errors.Is(err, fs.ErrNotExist):
			result.Missing++
		default:
			// The operation is irreversible, so it continues and the
			// counts report what was actually unlinked.
			slog.WarnContext(ctx, "fsx: unlink of recorded task file failed",
				slog.String("path", target.Path), slog.Any("err", err))
		}
	}

	removeTaskDir(ctx, roots, taskDir)

	return result, nil
}

// removeTaskDir takes the task's own directory with it, only while it is
// empty: os.Remove of a directory fails while anything lives inside it,
// which is exactly that rule, so a non-empty directory is simply left in
// place. The directory still has to resolve inside the roots like every
// other path; one that does not is left alone rather than removed.
func removeTaskDir(ctx context.Context, roots []string, taskDir string) {
	if taskDir == "" {
		return
	}
	if _, err := ResolveDestination(roots, taskDir); err != nil {
		slog.WarnContext(ctx, "fsx: task's own directory does not resolve inside the data roots; leaving it in place",
			slog.String("dir", taskDir), slog.Any("err", err))

		return
	}

	if err := os.Remove(taskDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.WarnContext(ctx, "fsx: removal of the task's own directory failed",
			slog.String("dir", taskDir), slog.Any("err", err))
	}
}
