package obs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	// metricsOff is the exact value of DLTOOL_METRICS_ADDR that disables
	// metrics entirely; an empty addr means unset and falls back to the
	// default (docs/11-config-reference.md section 2).
	metricsOff         = "off"
	defaultMetricsAddr = "127.0.0.1:9090"
	metricsPath        = "/metrics"

	metricsReadHeaderTimeout = 5 * time.Second
	metricsShutdownTimeout   = 5 * time.Second

	tasksSampleInterval = 15 * time.Second
)

// queryTaskCounts is the sampler query fixed by the T010 interface contract.
const queryTaskCounts = `SELECT state, engine, count(*) AS count FROM tasks GROUP BY 1, 2`

// Metrics owns a private registry — the process exposes only the series
// below plus the Go and process collectors — and the second listener that
// serves them. Every series is registered here and written by the component
// that observes the events (the reconciler, the task store, the worker
// pool, the SSE hub); each owning task hands its component this *Metrics at
// the composition root. A registered series that nothing ever sets is a
// defect, not an empty gauge.
type Metrics struct {
	Registry *prometheus.Registry

	// tasks publishes the sampler's snapshot atomically; a GaugeVec's
	// Reset-then-Set sequence would let a concurrent scrape observe a
	// half-empty label set.
	tasks                 *tasksTotalCollector
	TaskTransitionsTotal  *prometheus.CounterVec   // labels: from, to, engine
	BytesTransferredTotal *prometheus.CounterVec   // labels: direction
	EnginePollDuration    *prometheus.HistogramVec // labels: engine
	EngineErrorsTotal     *prometheus.CounterVec   // labels: engine, kind
	JobsTotal             *prometheus.CounterVec   // labels: kind, outcome
	SSEClients            prometheus.Gauge
}

// NewMetrics builds the registry and registers the fixed metric set.
func NewMetrics() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		tasks: &tasksTotalCollector{
			desc: prometheus.NewDesc(
				"dltool_tasks_total",
				"Current number of tasks by canonical state and engine.",
				[]string{"state", "engine"},
				nil,
			),
		},
		TaskTransitionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dltool_task_transitions_total",
			Help: "Task state transitions by source state, target state and engine.",
		}, []string{"from", "to", "engine"}),
		BytesTransferredTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dltool_bytes_transferred_total",
			Help: "Bytes transferred, by direction.",
		}, []string{"direction"}),
		EnginePollDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "dltool_engine_poll_duration_seconds",
			Help: "Duration of engine status polls.",
		}, []string{"engine"}),
		EngineErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dltool_engine_errors_total",
			Help: "Engine call failures by engine and error kind.",
		}, []string{"engine", "kind"}),
		JobsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dltool_jobs_total",
			Help: "Jobs executed by kind and outcome.",
		}, []string{"kind", "outcome"}),
		SSEClients: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dltool_sse_clients",
			Help: "Currently connected event-stream clients.",
		}),
	}

	// Static, uniquely named collectors: a registration failure is a
	// programmer error and belongs at boot, not on an error path.
	m.Registry.MustRegister(
		m.tasks,
		m.TaskTransitionsTotal,
		m.BytesTransferredTotal,
		m.EnginePollDuration,
		m.EngineErrorsTotal,
		m.JobsTotal,
		m.SSEClients,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return m
}

// ListenAndServe serves GET /metrics on its own listener at addr. The exact
// value "off" disables metrics and returns nil without binding; an empty
// addr means unset and falls back to the default of
// docs/11-config-reference.md section 2. It blocks until ctx is cancelled,
// then drains the listener and returns. The endpoint is unauthenticated by
// design: the default address is loopback-only.
func (m *Metrics) ListenAndServe(ctx context.Context, addr string) error {
	if addr == metricsOff {
		return nil
	}

	listenAddr := cmp.Or(addr, defaultMetricsAddr)

	mux := http.NewServeMux()
	mux.Handle(metricsPath, promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: metricsReadHeaderTimeout,
	}

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("obs: metrics listener %q: %w", listenAddr, err)
	}

	// The endpoint carries no authentication; make a non-loopback bind
	// visible in the log instead of silently exposing the series. The bound
	// address is authoritative: it resolves hostnames and an empty host means
	// all interfaces, neither of which the string parse sees.
	if tcpAddr, ok := listener.Addr().(*net.TCPAddr); ok && !tcpAddr.IP.IsLoopback() {
		slog.Warn("metrics endpoint is unauthenticated and bound to a non-loopback address",
			"addr", listenAddr)
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return fmt.Errorf("obs: metrics server: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), metricsShutdownTimeout)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("obs: metrics shutdown: %w", err)
		}

		return nil
	}
}

// RunTasksTotalSampler refreshes dltool_tasks_total every 15 seconds from
// SELECT state, engine, count(*) FROM tasks GROUP BY 1, 2, until ctx is
// cancelled. It samples once immediately so a fresh process publishes task
// counts without waiting out the first tick; a failed sample keeps the last
// snapshot and is logged at debug.
func (m *Metrics) RunTasksTotalSampler(ctx context.Context, db *sqlx.DB) {
	if db == nil {
		return
	}

	// SampleTasksTotal logs a failure at debug and leaves the last snapshot
	// standing; the next tick retries.
	m.SampleTasksTotal(ctx, db)

	ticker := time.NewTicker(tasksSampleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.SampleTasksTotal(ctx, db)
		}
	}
}

// SampleTasksTotal runs the sampler query once and publishes the rows as
// the new dltool_tasks_total snapshot. A row set that no longer contains a
// state drops that series from the exposition on the next scrape. A failed
// query is logged at debug and leaves the previous snapshot in place.
func (m *Metrics) SampleTasksTotal(ctx context.Context, db *sqlx.DB) {
	var rows []taskCountRow
	if err := db.SelectContext(ctx, &rows, queryTaskCounts); err != nil {
		slog.DebugContext(ctx, "tasks_total sample failed", "err", err)

		return
	}

	m.tasks.update(rows)
}

// taskCountRow is one row of the sampler query.
type taskCountRow struct {
	State  string `db:"state"`
	Engine string `db:"engine"`
	Count  int64  `db:"count"`
}

// tasksTotalCollector emits the sampler's last snapshot as gauge series;
// swapping the whole snapshot under one lock keeps every scrape consistent.
type tasksTotalCollector struct {
	mu   sync.Mutex
	rows []taskCountRow
	desc *prometheus.Desc
}

func (c *tasksTotalCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c *tasksTotalCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	rows := c.rows
	c.mu.Unlock()

	for _, row := range rows {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(row.Count), row.State, row.Engine)
	}
}

func (c *tasksTotalCollector) update(rows []taskCountRow) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.rows = rows
}
