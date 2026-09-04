// The live-update endpoints of docs/05-api-contract.md sections 6.1 and 6.2:
// GET /events frames hub deltas as SSE, GET /sync returns the identical
// envelope without framing. One Delta value feeds both, so the two renderings
// cannot drift apart.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"
	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/store"
	"github.com/L-K-M/dl-tool/internal/sync"
)

const (
	operationStreamEvents = "stream-events"
	operationGetSync      = "get-sync"

	// The wire rules of doc 05 section 6.1: one reconnect delay on the first
	// message of every connection, and a keep-alive every 15 s while idle.
	sseRetryMillis    = 3000
	sseBeatInterval   = 15 * time.Second
	sseEventSync      = "sync"
	sseEventHeartbeat = "hb"
	sseBeatComment    = "hb"

	// snapshotTaskFilter is the store's `all` sidebar filter: every canonical
	// state except removed, resolved in SQL and nowhere else.
	snapshotTaskFilter = "all"
	// snapshotPageSize is the largest page TaskStore.ListTasks accepts; the
	// snapshot walks every page, so a store with more tasks than one page is
	// still projected completely.
	snapshotPageSize = 500
)

// SyncInput is the query of GET /sync. A rid of 0, or one outside the ring,
// forces a full update.
type SyncInput struct {
	RID int64 `query:"rid" minimum:"0" doc:"The last rid the client holds; 0 forces a full update"`
}

// SyncOutput is the identical JSON object an SSE data line carries, with no
// event framing.
type SyncOutput struct {
	Body sync.Delta
}

// EventsInput carries the reconnect key of doc 05 section 6.1: the
// Last-Event-ID request header is the rid, with no separate client-side
// bookkeeping.
type EventsInput struct {
	LastEventID string `header:"Last-Event-ID" doc:"The rid to resume from; absent, unparseable or evicted forces a full update"`
}

// heartbeatEvent is the payload of the `hb` keep-alive: an empty JSON object.
type heartbeatEvent struct{}

// ClientsGauge is the subset of prometheus.Gauge the event stream maintains:
// one Inc per connection, one Dec when it ends.
type ClientsGauge interface {
	Inc()
	Dec()
}

// SSEHandlerOption adjusts an SSEHandlers constructor default.
type SSEHandlerOption func(*SSEHandlers)

// WithHeartbeatEvery overrides the keep-alive interval; tests use it to
// observe a beat without waiting out the wire's 15 seconds.
func WithHeartbeatEvery(interval time.Duration) SSEHandlerOption {
	return func(h *SSEHandlers) {
		h.beatEvery = interval
	}
}

// SSEHandlers owns the two live-update endpoints and the snapshot source
// their hub diffs. db may be nil — the openapi subcommand and router-only
// tests register the operations without a store to read; Snapshot is then an
// empty stub and the loop is never started.
type SSEHandlers struct {
	hub   *sync.Hub
	tasks *store.TaskStore

	beatEvery time.Duration
	gauge     ClientsGauge

	// latest is the most recent Snapshot the source produced. Subscribe's
	// seed runs under the hub lock, so it reads this copy — at most one tick
	// stale — instead of walking the store there.
	latest atomic.Pointer[sync.Snapshot]
}

// NewSSEHandlers builds the handlers over one hub. The hub is handed in
// because cmd/dl-tool's composition root owns its lifetime.
func NewSSEHandlers(hub *sync.Hub, db *sqlx.DB, opts ...SSEHandlerOption) *SSEHandlers {
	h := &SSEHandlers{
		hub:       hub,
		beatEvery: sseBeatInterval,
	}
	if db != nil {
		h.tasks = store.NewTaskStore(db)
	}
	for _, opt := range opts {
		opt(h)
	}

	return h
}

// SetClientsGauge connects the live connection count to the
// dltool_sse_clients gauge T010 registered. The *obs.Metrics instance owns
// that gauge and is constructed in cmd/dl-tool/main.go, outside this
// package; the composition root hands the gauge over once.
func (h *SSEHandlers) SetClientsGauge(g ClientsGauge) {
	h.gauge = g
}

// clientConnected moves the connection gauge up; clientDisconnected moves
// it back down.
func (h *SSEHandlers) clientConnected() {
	if h.gauge != nil {
		h.gauge.Inc()
	}
}

func (h *SSEHandlers) clientDisconnected() {
	if h.gauge != nil {
		h.gauge.Dec()
	}
}

// Snapshot is the 1 Hz source Hub.Loop diffs: every task in every state
// except removed, projected onto the canonical wire fields. A successful
// load also refreshes the copy Subscribe seeds from; a failed one leaves it
// standing, and the next successful diff folds the whole missed window into
// one delta.
func (h *SSEHandlers) Snapshot(ctx context.Context) (sync.Snapshot, error) {
	if h.tasks == nil {
		return sync.Snapshot{}, nil
	}

	filter := store.TaskFilter{State: snapshotTaskFilter, Limit: snapshotPageSize}
	snap := make(sync.Snapshot)
	for {
		rows, cursor, _, err := h.tasks.ListTasks(ctx, filter)
		if err != nil {
			return nil, fmt.Errorf("sse snapshot: list tasks: %w", err)
		}
		for _, row := range rows {
			snap[row.ID] = sync.Project(row)
		}
		if cursor == "" {
			break
		}
		filter.Cursor = cursor
	}

	h.latest.Store(&snap)

	return snap, nil
}

// seedDelta is the full side of the miss path: every task the last snapshot
// carried, each marshaled once through the same Diff path a delta uses, so a
// seed and a diff render one task identically.
func (h *SSEHandlers) seedDelta() sync.Delta {
	d := sync.Delta{
		Tasks:        map[string]json.RawMessage{},
		TasksRemoved: []string{},
	}

	cached := h.latest.Load()
	if cached == nil {
		return d
	}

	d.Tasks, _ = sync.Diff(sync.Snapshot{}, *cached)
	d.Stats = sync.Aggregate(*cached)

	return d
}

// Events serves GET /events: one `sync` event per published delta, the seed
// first, a `retry` on the first message, and the `hb` keep-alive while idle.
func (h *SSEHandlers) Events(ctx context.Context, input *EventsInput, send sse.Sender) {
	ch, cancel := h.hub.Subscribe(ctx, input.LastEventID, h.seedDelta)
	defer cancel()

	h.clientConnected()
	defer h.clientDisconnected()

	first := true
	beats := time.NewTicker(h.beatEvery)
	defer beats.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case d, ok := <-ch:
			if !ok {
				// Evicted as a slow subscriber, or cancelled: the client
				// reconnects through the seq_gap path.
				return
			}
			beats.Reset(h.beatEvery)

			// The id line is the rid inside the payload; rid 0 (nothing
			// published yet) has no id line at all.
			msg := sse.Message{ID: int(d.RID), Data: d}
			if first {
				msg.Retry = sseRetryMillis
				first = false
			}
			if err := send(msg); err != nil {
				return
			}
		case <-beats.C:
			// The keep-alive of doc 05 section 6.1: a comment line proxies
			// respect, and the named event is what an EventSource can
			// actually observe.
			if err := send(sse.Message{Comment: sseBeatComment, Data: heartbeatEvent{}}); err != nil {
				return
			}
		}
	}
}

// Sync serves GET /sync: the identical envelope the stream carries, without
// event framing.
func (h *SSEHandlers) Sync(_ context.Context, in *SyncInput) (*SyncOutput, error) {
	return &SyncOutput{Body: h.hub.Snapshot(in.RID, h.seedDelta)}, nil
}

// RegisterOperations mounts both live-update operations on a Huma API;
// Server.registerOperations is the production call site.
func (h *SSEHandlers) RegisterOperations(hapi huma.API) {
	sse.Register(hapi, huma.Operation{
		OperationID: operationStreamEvents,
		Method:      http.MethodGet,
		Path:        "/events",
		Summary:     "Stream task deltas over SSE",
		Description: "One `sync` event per second at most, and only when something changed. The `id` line is the rid inside the payload; Last-Event-ID is the reconnect key. An absent, unparseable or evicted key yields one message with full_update and seq_gap true carrying every task. The `hb` event is the 15-second keep-alive.",
		Tags:        []string{"events"},
		Security:    credentialRequired,
	}, map[string]any{
		sseEventSync:      sync.Delta{},
		sseEventHeartbeat: heartbeatEvent{},
	}, h.Events)

	huma.Register(hapi, huma.Operation{
		OperationID: operationGetSync,
		Method:      http.MethodGet,
		Path:        "/sync",
		Summary:     "Poll the delta since a rid",
		Description: "The byte-identical JSON object the event stream's `sync` data line carries for the same rid, without event framing. A rid of 0, or one outside the ring, forces a full update.",
		Tags:        []string{"events"},
		Security:    credentialRequired,
		// Same strictness as every other query-carrying operation: a
		// mistyped query key is 422, never silently ignored.
		RejectUnknownQueryParameters: true,
	}, h.Sync)
}
