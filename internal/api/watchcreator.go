// The jobs.TaskCreator adapter of T083: the watch-folder loader hands a
// dropped .torrent to the same creation path an upload takes.
package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/L-K-M/dl-tool/internal/jobs"
	"github.com/L-K-M/dl-tool/internal/store"
	"github.com/L-K-M/dl-tool/internal/uri"
)

// watchTaskCreator adapts TaskHandlers.CreateTasks to jobs.TaskCreator:
// the blob rides the same uploadedFilesKey context slot the multipart
// middleware fills, so normalisation, routing, the destination jail,
// dedup and the requested_destination echo stay the T020/T033 path
// unchanged.
type watchTaskCreator struct{ tasks *TaskHandlers }

// CreateFromTorrent first parses blob with uri.InspectTorrent and answers
// jobs.ErrTorrentDuplicate when c.tasks.FindByInfohash finds a live task —
// the same lookup duplicateRejection runs. It then calls CreateTasks with
// ctx carrying []UploadedFile{{Name: name, Bytes: blob}} and the body
// holding dest and category. A returned *huma.ErrorModel maps Type
// SlugPathRejected onto jobs.ErrDestinationRejected and a Detail prefixed
// by duplicateDetail onto jobs.ErrTorrentDuplicate (the
// commit-between-check-and-insert race); every other error passes through.
func (c watchTaskCreator) CreateFromTorrent(ctx context.Context, name string, blob []byte, dest, category string) (string, error) {
	manifest, err := uri.InspectTorrent(blob)
	if err != nil {
		return "", fmt.Errorf("api: watch folder file %q: %w", name, err)
	}

	if _, err := c.tasks.tasks.FindByInfohash(ctx, manifest.InfohashV1, manifest.InfohashV2); err == nil {
		return "", fmt.Errorf("%w: %s", jobs.ErrTorrentDuplicate, name)
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("api: check duplicate torrent of %q: %w", name, err)
	}

	input := &CreateTasksInput{}
	input.Body.Destination = dest
	input.Body.Category = category
	ctx = context.WithValue(ctx, uploadedFilesKey{}, []UploadedFile{{Name: name, Bytes: blob}})

	output, err := c.tasks.CreateTasks(ctx, input)
	if err != nil {
		return "", mapCreateError(err)
	}
	if len(output.Body.Created) != 1 {
		return "", fmt.Errorf("api: watch folder file %q created %d tasks, want 1", name, len(output.Body.Created))
	}

	return output.Body.Created[0].ID, nil
}

// mapCreateError translates CreateTasks' failures back into the watcher's
// classified errors. The all-refused answer flattens the rejected[] entry's
// slug into its detail text, so the commit-between-check-and-insert
// duplicate crosses back by the detail prefix duplicateRejection writes;
// the path-rejected problem crosses back by type. Every other error
// passes through unchanged.
func mapCreateError(err error) error {
	var model *huma.ErrorModel
	if errors.As(err, &model) {
		switch {
		case model.Type == SlugPathRejected:
			return fmt.Errorf("%w: %s", jobs.ErrDestinationRejected, model.Detail)
		case strings.HasPrefix(model.Detail, duplicateDetail):
			return fmt.Errorf("%w: %s", jobs.ErrTorrentDuplicate, model.Detail)
		}
	}

	return err
}
