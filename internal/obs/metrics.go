package obs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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

	TasksTotal            *prometheus.GaugeVec     // labels: state, engine
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
		TasksTotal: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dltool_tasks_total",
			Help: "Current number of tasks by canonical state and engine.",
		}, []string{"state", "engine"}),
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
		m.TasksTotal,
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
// cancelled. The tasks table arrives with T017; until then every sample
// fails and is logged at debug level, leaving the gauge empty.
func (m *Metrics) RunTasksTotalSampler(ctx context.Context, db *sqlx.DB) {
	if db == nil {
		return
	}

	ticker := time.NewTicker(tasksSampleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.sampleTasksTotal(ctx, db)
		}
	}
}

// taskCountRow is one row of the sampler query.
type taskCountRow struct {
	State  string `db:"state"`
	Engine string `db:"engine"`
	Count  int64  `db:"count"`
}

func (m *Metrics) sampleTasksTotal(ctx context.Context, db *sqlx.DB) {
	var rows []taskCountRow
	if err := db.SelectContext(ctx, &rows, queryTaskCounts); err != nil {
		slog.DebugContext(ctx, "tasks_total sample failed", "err", err)
		return
	}

	// Reset first, so a state that no longer has tasks is absent from the
	// exposition instead of keeping its last value.
	m.TasksTotal.Reset()
	for _, row := range rows {
		m.TasksTotal.WithLabelValues(row.State, row.Engine).Set(float64(row.Count))
	}
}
