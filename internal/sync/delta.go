// Delta computation: the projection of store rows onto the canonical Task
// object of docs/05-api-contract.md section 3, and the snapshot diff that
// feeds Hub.Publish. The wire envelope (Delta, Stats, Ring, Hub) and the
// 1 Hz loop that drives the publish belong to ring.go and hub.go.

package sync

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/L-K-M/dl-tool/internal/store"
)

// Snapshot is the current wire state of every task, keyed by task id. The
// inner map holds the field names of the canonical Task object in
// docs/05-api-contract.md section 3; a field absent from the inner map is a
// field this projection cannot derive from the row, never a field the client
// must treat as null.
type Snapshot map[string]map[string]any

// taskStateQueued and the activePair below mirror the sidebar vocabulary the
// store already owns: `queued` counts the queue, and "active" is exactly the
// states of the sidebar's active filter (docs/04-data-model.md section 4.1),
// so the stats block and the sidebar can never disagree.
const taskStateQueued = "queued"

var activeStates = map[string]bool{
	"downloading": true,
	"seeding":     true,
}

// Project turns one store row into its wire representation: bytes as
// integers, timestamps as RFC 3339 UTC strings, progress as
// completed_bytes / total_bytes and 0.0 when total_bytes is null. Fields the
// row cannot answer for — the category name, tag names, ratio, the peer and
// limit columns, file_count, requested_destination — are absent, which in a
// delta means "unchanged" and never "null".
func Project(t store.Task) map[string]any {
	return map[string]any{
		"id":              t.ID,
		"engine":          t.Engine,
		"source_kind":     t.SourceKind,
		"source_uri":      nilOrValue(DisplaySourceURI(t)),
		"infohash_v1":     nilOrValue(t.InfohashV1),
		"infohash_v2":     nilOrValue(t.InfohashV2),
		"name":            t.Name,
		"state":           t.State,
		"error_code":      nilOrValue(t.ErrorCode),
		"error_message":   nilOrValue(t.ErrorMessage),
		"destination":     t.Destination,
		"content_path":    nilOrValue(t.ContentPath),
		"total_bytes":     nilOrValue(t.TotalBytes),
		"completed_bytes": t.CompletedBytes,
		"uploaded_bytes":  t.UploadedBytes,
		"progress":        progress(t),
		"download_rate":   t.DownloadRate,
		"upload_rate":     t.UploadRate,
		"eta_seconds":     nilOrValue(t.ETASeconds),
		"sequential":      t.Sequential == 1,
		"queue_position":  nilOrValue(t.QueuePosition),
		"added_at":        rfc3339(t.AddedAt),
		"started_at":      nilOrValue(derefTime(t.StartedAt)),
		"completed_at":    nilOrValue(derefTime(t.CompletedAt)),
		"updated_at":      rfc3339(t.UpdatedAt),
	}
}

// DisplaySourceURI renders the API-safe source reference every task-emitting
// path shares — the SSE projection and the REST DTO both call it, so the
// sanitizing rule exists exactly once.
//
// A non-empty source_display_uri wins verbatim: it is written at insert and
// is already API-safe (search-result:<res_id> carries no provider data). A
// NULL or empty column falls back to the server-only source_uri, sanitized
// fail-closed: userinfo is stripped, and a source that cannot be parsed or
// whose credentials ride in opaque form ("user:pass@host", where u.User is
// nil and the secret sits in u.Opaque) is dropped whole. For every scheme
// except magnet: the raw query goes too — a Torznab passkey rides there, not
// in userinfo — so a row written before the column existed can never leak a
// query-string secret.
//
// A magnet's query is untrusted but load-bearing: dn, tr, xs and ws values
// commonly embed tracker passkeys and indexer API keys, yet stripping the
// query whole would render a bare, useless magnet:. Only xt parameters whose
// value parses as a URI with scheme urn — compared case-insensitively — and
// carries no ? or # of its own are kept; an RFC 8141 q-/f-component is not
// part of the content identity. A magnet left with no surviving xt is dropped
// like an unparsable source.
func DisplaySourceURI(t store.Task) *string {
	if t.SourceDisplayURI != nil && *t.SourceDisplayURI != "" {
		return t.SourceDisplayURI
	}
	if t.SourceURI == nil {
		return nil
	}

	u, err := url.Parse(*t.SourceURI)
	if err != nil || u.Opaque != "" {
		return nil
	}
	u.User = nil

	if !strings.EqualFold(u.Scheme, "magnet") {
		u.RawQuery = ""
		display := u.String()

		return &display
	}

	kept := keepURNextParams(u.RawQuery)
	if len(kept) == 0 {
		return nil
	}
	u.RawQuery = strings.Join(kept, "&")
	u.Fragment = ""
	display := u.String()

	return &display
}

// keepURNextParams filters one magnet query to its xt pairs whose decoded
// value is a content URN: a URI whose scheme is urn, compared
// case-insensitively, and that carries no ? or # of its own. The surviving
// pairs are re-emitted verbatim, so a valid xt keeps its original encoding.
func keepURNextParams(rawQuery string) []string {
	var kept []string
	for _, pair := range strings.Split(rawQuery, "&") {
		name, rawValue, _ := strings.Cut(pair, "=")
		if name != "xt" {
			continue
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil || strings.ContainsAny(value, "?#") {
			continue
		}
		xt, err := url.Parse(value)
		if err != nil || !strings.EqualFold(xt.Scheme, "urn") {
			continue
		}
		kept = append(kept, pair)
	}

	return kept
}

// progress derives progress as completed_bytes / total_bytes, clamped to 1.0
// and 0.0 while total_bytes is null or zero (docs/05-api-contract.md
// section 3).
func progress(t store.Task) float64 {
	if t.TotalBytes == nil || *t.TotalBytes == 0 {
		return 0
	}

	return min(float64(t.CompletedBytes)/float64(*t.TotalBytes), 1.0)
}

// rfc3339 renders a database millisecond stamp as an RFC 3339 UTC string.
func rfc3339(unixMilli int64) string {
	return time.UnixMilli(unixMilli).UTC().Format(time.RFC3339)
}

// derefTime renders an optional stamp, nil staying nil.
func derefTime(unixMilli *int64) *string {
	if unixMilli == nil {
		return nil
	}
	rendered := rfc3339(*unixMilli)

	return &rendered
}

// nilOrValue maps an optional column onto the wire: nil stays nil, a value
// passes through.
func nilOrValue[T any](v *T) any {
	if v == nil {
		return nil
	}

	return *v
}

// Diff returns, per task id, only the fields whose value changed between
// prev and next, plus the ids present in prev and absent from next. An
// unchanged task is absent from changed entirely. Each changed task is
// marshaled exactly once, so a ring entry and a live delivery can never
// render the same patch differently.
func Diff(prev, next Snapshot) (changed map[string]json.RawMessage, removed []string) {
	changed = make(map[string]json.RawMessage, len(next))

	for id, after := range next {
		before, existed := prev[id]
		if !existed {
			changed[id] = mustMarshal(after)

			continue
		}

		patch := make(map[string]any, len(after))
		for field, value := range after {
			// The projection is a fixed field set, so a field missing from
			// before can only mean a projection built by an older process —
			// sending it is the safe direction.
			previous, known := before[field]
			if !known || previous != value {
				patch[field] = value
			}
		}
		if len(patch) > 0 {
			changed[id] = mustMarshal(patch)
		}
	}

	for id := range prev {
		if _, stillPresent := next[id]; !stillPresent {
			removed = append(removed, id)
		}
	}
	slices.Sort(removed)

	return changed, removed
}

// mustMarshal renders one projected task or patch. The values are the scalars
// Project emits, all of which JSON marshals; a failure means a non-scalar
// sneaked into a Snapshot and is a programmer error, not a runtime condition.
func mustMarshal(v map[string]any) json.RawMessage {
	rendered, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("sync: marshal projected task: %v", err))
	}

	return rendered
}

// Aggregate computes the stats block: the summed rates and the counts of
// active and queued tasks.
func Aggregate(s Snapshot) Stats {
	var stats Stats

	for _, fields := range s {
		stats.SpeedDown += intField(fields, "download_rate")
		stats.SpeedUp += intField(fields, "upload_rate")

		state, _ := fields["state"].(string)
		if activeStates[state] {
			stats.Active++
		}
		if state == taskStateQueued {
			stats.Queued++
		}
	}

	return stats
}

// intField reads an integer field a Project-ed map carries, 0 when absent.
func intField(fields map[string]any, name string) int64 {
	value, _ := fields[name].(int64)

	return value
}
