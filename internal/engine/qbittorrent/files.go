// The file listing and selection calls of docs/06-download-engines.md
// section 5.7: GET torrents/files and POST torrents/filePrio. Priorities
// are the identity mapping of section 1.1 — the canonical vocabulary is
// qBittorrent's own — and the two forbidden values never leave this file:
// 4 (libtorrent's internal scale) and -1 (the read-only Mixed aggregate).

package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/L-K-M/dl-tool/internal/engine"
)

const (
	pathTorrentsFiles    = "torrents/files"
	pathTorrentsFilePrio = "torrents/filePrio"

	// fileIDSeparator joins the file ids of one torrents/filePrio group:
	// "File IDs separated by pipes" (docs/06 section 5.7).
	fileIDSeparator = "|"
)

// The WebAPI priority vocabulary of docs/06-download-engines.md section
// 1.1, verified against release-5.2.3 src/base/bittorrent/
// downloadpriority.h. filePriorityMixed is read-only: qBittorrent answers
// it on an aggregate row, and this adapter never sends it.
const (
	filePrioritySkip    = 0
	filePriorityNormal  = 1
	filePriorityHigh    = 6
	filePriorityMaximum = 7
	filePriorityMixed   = -1
)

// fileJSON is one element of GET /api/v2/torrents/files.
type fileJSON struct {
	Index        int     `json:"index"` // 0-based
	Name         string  `json:"name"`  // filename including relative path
	Size         int64   `json:"size"`
	Progress     float64 `json:"progress"` // percentage/100
	Priority     int     `json:"priority"` // 0 | 1 | 6 | 7, and -1 (Mixed) on an aggregate row
	IsSeed       bool    `json:"is_seed"`
	Availability float64 `json:"availability"`
}

// Files returns one FileEntry per file. Priority is the identity mapping
// of 06 section 1.1; a returned -1 (Mixed) is passed through as Priority
// 1 and never treated as an error.
func (c *Client) Files(ctx context.Context, id string) ([]engine.FileEntry, error) {
	body, err := c.do(ctx, http.MethodGet, pathTorrentsFiles, url.Values{"hash": {ref(id)}})
	if err != nil {
		return nil, err
	}

	var files []fileJSON
	if err := json.Unmarshal(body, &files); err != nil {
		return nil, fmt.Errorf("qbittorrent: decode torrents/files: %w", err)
	}

	return toFileEntries(files), nil
}

// toFileEntries maps the WebAPI listing onto engine.FileEntry. A -1
// (Mixed) aggregate degrades to normal: it is a folder-level summary the
// daemon emits, not a priority any caller could act on, and section 5.7
// forbids treating it as an error.
func toFileEntries(files []fileJSON) []engine.FileEntry {
	entries := make([]engine.FileEntry, 0, len(files))
	for _, f := range files {
		priority := f.Priority
		if priority == filePriorityMixed {
			priority = filePriorityNormal
		}

		entry := engine.FileEntry{
			Index:     f.Index,
			Path:      f.Name,
			Size:      f.Size,
			Completed: int64(f.Progress * float64(f.Size)),
			Selected:  priority != filePrioritySkip,
		}
		entry.Priority = &priority
		entries = append(entries, entry)
	}

	return entries
}

// SetFiles applies selection and priorities in one pass. Indices absent
// from both arguments are left untouched. It groups the indices by target
// priority and issues one POST torrents/filePrio per group with id as a
// pipe-separated list. It never sends 4 and never sends -1.
//
// The normalisation: every index in priorities keeps its value; every
// index in selected with no explicit priority becomes normal; every index
// the engine reports that appears in neither — but only when selected is
// non-nil, i.e. the caller named a complete desired selection — becomes
// skip. A nil selected leaves every unlisted index exactly as it stands.
func (c *Client) SetFiles(ctx context.Context, id string, selected []int, priorities map[int]int) error {
	targets, err := c.filePrioTargets(ctx, id, selected, priorities)
	if err != nil {
		return err
	}

	hash := ref(id)
	for _, group := range groupByPriority(targets) {
		ids := make([]string, 0, len(group.indices))
		for _, index := range group.indices {
			ids = append(ids, strconv.Itoa(index))
		}

		form := url.Values{
			"hash":     {hash},
			"id":       {strings.Join(ids, fileIDSeparator)},
			"priority": {strconv.Itoa(group.priority)},
		}
		if _, err := c.do(ctx, http.MethodPost, pathTorrentsFilePrio, form); err != nil {
			var apiErr *apiError
			if errors.As(err, &apiErr) && apiErr.status == http.StatusConflict {
				// The daemon rejects a file id outside the torrent's range
				// with 409; name the ids of the failing group so the caller
				// can tell a stale listing from a transport fault.
				return fmt.Errorf(
					"qbittorrent: torrents/filePrio: the daemon rejected file ids %s as out of range: %w",
					strings.Join(ids, fileIDSeparator), err,
				)
			}

			return err
		}
	}

	return nil
}

// filePrioTargets resolves the index -> priority map SetFiles sends. The
// vocabulary check runs before any request — including the listing read
// the selection path needs — so a caller carrying 4, -1 or any other
// outside value fails without touching the daemon.
func (c *Client) filePrioTargets(
	ctx context.Context,
	id string,
	selected []int,
	priorities map[int]int,
) (map[int]int, error) {
	for index, value := range priorities {
		if value != filePrioritySkip && value != filePriorityNormal &&
			value != filePriorityHigh && value != filePriorityMaximum {
			return nil, fmt.Errorf(
				"qbittorrent: file %d carries priority %d, outside the WebAPI vocabulary {0,1,6,7}; 4 is libtorrent's internal scale and is never sent",
				index, value,
			)
		}
	}

	if selected == nil {
		return maps.Clone(priorities), nil
	}

	targets := maps.Clone(priorities)
	if targets == nil {
		targets = make(map[int]int, len(selected))
	}
	for _, index := range selected {
		if _, explicit := targets[index]; !explicit {
			targets[index] = filePriorityNormal
		}
	}

	// selected is non-nil, so it names the complete desired selection:
	// every reported index outside it that carries no explicit priority
	// is deselected.
	entries, err := c.Files(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if _, explicit := priorities[entry.Index]; explicit {
			continue
		}
		if _, picked := targets[entry.Index]; picked {
			continue
		}
		targets[entry.Index] = filePrioritySkip
	}

	return targets, nil
}

// priorityGroup is one torrents/filePrio request: every index that shares
// a target priority, in ascending index order.
type priorityGroup struct {
	priority int
	indices  []int
}

// groupByPriority folds a target map into one group per priority, groups
// ordered by ascending priority so the request sequence is deterministic.
func groupByPriority(targets map[int]int) []priorityGroup {
	byPriority := make(map[int][]int, len(targets))
	for index, priority := range targets {
		byPriority[priority] = append(byPriority[priority], index)
	}

	priorities := make([]int, 0, len(byPriority))
	for priority := range byPriority {
		priorities = append(priorities, priority)
	}
	slices.Sort(priorities)

	groups := make([]priorityGroup, 0, len(byPriority))
	for _, priority := range priorities {
		indices := byPriority[priority]
		slices.Sort(indices)
		groups = append(groups, priorityGroup{priority: priority, indices: indices})
	}

	return groups
}
