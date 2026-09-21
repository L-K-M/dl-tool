package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2/humacli"
	"github.com/spf13/cobra"

	"github.com/L-K-M/dl-tool/internal/api"
	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/jobs"
	"github.com/L-K-M/dl-tool/internal/obs"
	"github.com/L-K-M/dl-tool/internal/rss"
	"github.com/L-K-M/dl-tool/internal/search"
	"github.com/L-K-M/dl-tool/internal/secure"
	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	unknownRevision    = "unknown"
	bootstrapLogLevel  = "info"
	bootstrapLogFormat = "json"
	fallbackErrorCode  = "config_malformed"
	exitFailure        = 1
	backupsDirName     = "backups"
	workerPoolSize     = 2

	// readHeaderTimeout bounds slow-header exposure on the main listener;
	// readTimeout bounds the whole request read (body included); idleTimeout
	// bounds keep-alive idling; shutdownTimeout bounds the graceful drain.
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 60 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 10 * time.Second

	// healthcheckTimeout stays under the image HEALTHCHECK's --timeout=5s so
	// the probe process always answers before Docker kills it.
	healthcheckTimeout = 4 * time.Second
	loopbackHost       = "127.0.0.1"
	healthzPath        = "/healthz"
)

// Options are the humacli-bound flags.
type Options struct {
	Host string `help:"listen address" default:":8080"`
}

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve": // the image CMD; the root command is already the server
			os.Args = append(os.Args[:1], os.Args[2:]...)
		case "healthcheck": // the image HEALTHCHECK
			os.Exit(healthcheck())
		}
	}

	slog.SetDefault(obs.NewLogger(os.Stdout, bootstrapLogLevel, bootstrapLogFormat))
	stopped := make(chan struct{})
	drained := make(chan struct{})

	var httpServer *http.Server
	var cancelRun context.CancelFunc
	var runDone sync.WaitGroup

	cli := humacli.New(func(hooks humacli.Hooks, _ *Options) {
		hooks.OnStart(func() {
			// Every exit path — including a boot failure — must release OnStop.
			defer close(drained)

			ctx := context.Background()
			// runCtx spans the metrics listener and the tasks_total sampler;
			// OnStop cancels it so both drain with the main listener.
			runCtx, cancel := context.WithCancel(ctx)
			cancelRun = cancel

			cfg, err := config.Load(ctx)
			if err != nil {
				logConfigError(err)
				os.Exit(exitFailure)
			}

			logger := obs.NewLogger(os.Stdout, cfg.LogLevel, cfg.LogFormat)
			slog.SetDefault(logger)

			db, err := store.Open(ctx, cfg.DBPath, filepath.Join(cfg.ConfigDir, backupsDirName))
			if err != nil {
				logger.Error("database open failed", "err", err)
				os.Exit(exitFailure)
			}

			api.Version = version
			rss.Version = version

			// The process-wide search collaborators, in the order the task
			// contract fixes: the SSRF guard and its outbound client, then
			// the indexer store sealing under cfg.SecretKey, then the
			// definition registry seeding the bundled indexers — built once
			// so the API and the job worker share one of each
			// (docs/14-conventions.md section 8.3). The search worker
			// registration (T061) extends this Deps in its own task.
			searchGuard := secure.NewGuard(logger, cfg.SSRFAllowPrivate)
			searchHTTP := secure.NewClient(searchGuard)
			indexers, err := store.NewIndexerStore(db, cfg.SecretKey)
			if err != nil {
				logger.Error("indexer store build failed", "err", err)
				os.Exit(exitFailure)
			}

			defs, err := search.NewRegistry(logger, filepath.Join(cfg.ConfigDir, "engines"))
			if err != nil {
				logger.Error("definition registry failed", "err", err)
				os.Exit(exitFailure)
			}
			// First boot seeds one enabled row per bundled definition; the
			// unique index on definition_id makes later boots a no-op.
			seeded, err := defs.SeedIndexers(ctx, indexers)
			if err != nil {
				logger.Error("bundled indexer seeding failed", "err", err)
				os.Exit(exitFailure)
			}
			if seeded > 0 {
				logger.Info("seeded bundled indexers", "created", seeded)
			}

			// One runner for the process: its per-engine rate buckets are
			// shared state between the probe endpoint and the search jobs.
			// The search fan-out's torznab calls carry the same honest UA.
			userAgent := "dl-tool/" + version
			runner := search.NewRunner(searchHTTP, logger, userAgent)

			// The notifier (T077) shares the one SSRF-guarded client and
			// opens channel secrets under the same at-rest key the indexer
			// store seals with; the chain's tail step fans out through it.
			// LAN-hosted endpoints (a self-hosted ntfy or gotify on
			// 192.168.x.x) need DLTOOL_SSRF_ALLOW_PRIVATE — the same
			// operator opt-in the indexers and feeds use.
			notifier := jobs.NewNotifier(db, cfg.SecretKey, searchHTTP)

			// The post-processing chain (T074) is installed before the API
			// server runs its boot reconciliation: the completion hook is
			// the chain's single entry point, and tasks the reconciler
			// moves into completed during boot must feed it too. A chain
			// error only strands the enqueue, so it is logged, never raised.
			postprocess := jobs.NewChain(db, store.NewTaskStore(db))
			postprocess.SetNotifier(notifier)
			store.SetCompletedHook(func(ctx context.Context, taskID string) {
				// The store detaches cancellation already; the timeout
				// bounds the enqueue so a wedged chain cannot stall the
				// transitioning caller indefinitely.
				hookCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				if err := postprocess.OnCompleted(hookCtx, taskID); err != nil {
					logger.ErrorContext(ctx, "postprocess chain failed", "task_id", taskID, "err", err)
				}
			})

			server, err := api.NewServer(cfg, db, logger, api.Deps{Indexers: indexers, Defs: defs, Runner: runner, HTTP: searchHTTP, DB: db})
			if err != nil {
				logger.Error("server build failed", "err", err)
				os.Exit(exitFailure)
			}

			// store.Open has returned: migrations are applied and the database
			// answers, so /readyz may report ready (doc 05 section 13.1).
			server.Health.MarkReady()

			// The job worker pool shares runCtx, so OnStop cancels it and
			// runDone.Wait blocks until every in-flight handler has finished.
			// The search fan-out (T061) reuses the same Deps collaborators
			// the API holds — one runner, one registry, one guarded client
			// per process. T066/T091 register their kinds here later.
			worker := jobs.NewWorker(db, logger, workerPoolSize)
			worker.Register(jobs.JobKindSearch, jobs.NewSearchHandler(
				db, logger, defs, runner, indexers, searchHTTP, userAgent,
			))

			// The extract handler claims the jobs the chain installed
			// above enqueues.
			worker.Register(
				jobs.JobKindExtract,
				jobs.NewExtractHandler(store.NewTaskStore(db), cfg.SevenzipPath).Handle,
			)
			// The move handler (T076) claims the relocation jobs the chain
			// enqueues when a completed payload sits outside its resolved
			// destination; it needs the raw db for the content_path write
			// and the configured roots for the destination's min-free floor.
			worker.Register(
				jobs.JobKindMove,
				jobs.NewMoveHandler(db, store.NewTaskStore(db), cfg.DataRoots).Handle,
			)
			// The webhook handler (T077) claims the notification jobs the
			// chain's terminal fan-out enqueues — one per enabled channel
			// whose mask selected the event.
			worker.Register(jobs.JobKindWebhook, notifier.Handle)
			// The feed poller shares the SSRF-guarded client with the search
			// fan-out and the refresh endpoint; the parser is T067's
			// parse.go and the creator the one ruleTaskCreator NewServer
			// built, so a 200 that adds items runs the rules pass through
			// the ordinary task-creation path.
			poller := rss.NewPoller(db, searchHTTP, rss.NewParser(time.Now), server.RuleCreator, logger, time.Now)
			worker.Register(jobs.JobKindRSSPoll, poller.PollDue)
			runDone.Add(1)
			go func() {
				defer runDone.Done()
				if err := worker.Run(runCtx); err != nil {
					logger.Error("job worker failed", "err", err)
				}
			}()

			// The cron scheduler enqueues the periodic jobs (rss_poll now,
			// T081/T083/T091 extend it later). Start blocks until runCtx is
			// cancelled in OnStop, then drains the in-flight entry.
			scheduler := jobs.NewScheduler(db, logger)
			runDone.Add(1)
			go func() {
				defer runDone.Done()
				scheduler.Start(runCtx)
			}()

			metrics := obs.NewMetrics()
			runDone.Add(2)
			go func() {
				defer runDone.Done()
				// A failed metrics listener is degraded, not fatal: /metrics is
				// a loopback-only side channel (doc 05 section 13.1).
				if err := metrics.ListenAndServe(runCtx, cfg.MetricsAddr); err != nil {
					logger.Error("metrics listener failed", "err", err)
				}
			}()
			go func() {
				defer runDone.Done()
				metrics.RunTasksTotalSampler(runCtx, db)
			}()

			httpServer = &http.Server{
				Addr:              cfg.HTTPAddr,
				Handler:           server.Router,
				ReadHeaderTimeout: readHeaderTimeout,
				ReadTimeout:       readTimeout,
				IdleTimeout:       idleTimeout,
			}

			go func() {
				if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("http listener failed", "err", err)
					os.Exit(exitFailure)
				}
			}()

			slog.Info("started")
			<-stopped

			// Drain in the goroutine that owns the values; stopped and drained
			// are the happens-before edges, so there is no racy cross-goroutine
			// read and humacli cannot return before the close completes.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()

			if httpServer != nil {
				if err := httpServer.Shutdown(shutdownCtx); err != nil {
					slog.Error("http shutdown failed", "err", err)
				}
			}
			// The server's background loops — sync hub, reconciler and
			// admission — must stop before the store closes; Shutdown
			// cancels their context and joins each goroutine.
			server.Shutdown()
			if err := db.Close(); err != nil {
				slog.Error("database close failed", "err", err)
			}
		})
		hooks.OnStop(func() {
			slog.Info("stopped")
			// Ask the metrics listener and the tasks_total sampler to stop and
			// join them before signalling the main shutdown path, so both drain
			// before the store closes.
			if cancelRun != nil {
				cancelRun()
			}
			runDone.Wait()
			close(stopped)
			<-drained
		})
	})
	cli.Root().AddCommand(versionCmd())
	cli.Root().AddCommand(openapiCmd())
	cli.Run()
}

// healthcheck GETs {DLTOOL_BASE_PATH}/healthz on DLTOOL_HTTP_ADDR and returns the
// process exit code: 0 on 200, 1 otherwise. It shells out to nothing, so the image
// needs neither curl nor a shell in the health command.
func healthcheck() int {
	addr := os.Getenv("DLTOOL_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = loopbackHost + addr
	}
	url := "http://" + addr + strings.TrimSuffix(os.Getenv("DLTOOL_BASE_PATH"), "/") + healthzPath
	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return exitFailure
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return exitFailure
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck:", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return exitFailure
	}
	return 0
}

func logConfigError(err error) {
	logger := obs.NewLogger(os.Stderr, bootstrapLogLevel, bootstrapLogFormat)

	var fatal *config.FatalError
	if !errors.As(err, &fatal) {
		logger.Error("configuration failed",
			"err_code", fallbackErrorCode,
			"err", err,
		)

		return
	}

	logger.Error("configuration failed",
		"err_code", fatal.Code,
		"variable", fatal.Variable,
		"err", fatal,
	)
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s\n", version, runtime.Version(), revision()); err != nil {
				return fmt.Errorf("write version: %w", err)
			}

			return nil
		},
	}
}

// openapiCmd prints the canonical (empty base path) OpenAPI document; the
// committed api/openapi.json is byte-identical to its output. A deployment
// with a configured base path gets its variant from the running server.
func openapiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "openapi",
		Short: "Print the OpenAPI document",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			api.Version = version

			server, err := api.NewServer(&config.Config{}, nil, slog.Default())
			if err != nil {
				return fmt.Errorf("build server: %w", err)
			}

			spec, err := server.Spec()
			if err != nil {
				return err
			}

			if _, err := cmd.OutOrStdout().Write(spec); err != nil {
				return fmt.Errorf("write openapi document: %w", err)
			}

			return nil
		},
	}
}

func revision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return unknownRevision
	}

	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return setting.Value
		}
	}

	return unknownRevision
}
