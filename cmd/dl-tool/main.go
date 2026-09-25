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
	"github.com/jmoiron/sqlx"
	"github.com/spf13/cobra"

	"github.com/L-K-M/dl-tool/internal/api"
	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/engine"
	"github.com/L-K-M/dl-tool/internal/engine/ytdlp"
	"github.com/L-K-M/dl-tool/internal/fsx"
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
	workerPoolSize     = 2

	// readHeaderTimeout bounds slow-header exposure on the main listener;
	// readTimeout bounds the whole request read (body included); idleTimeout
	// bounds keep-alive idling; shutdownTimeout bounds the graceful drain.
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 60 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 10 * time.Second

	// governorBootTimeout bounds the stored-limits fan-out so a black-holed
	// engine can hold the boot for one window, not per daemon RPC.
	governorBootTimeout = 10 * time.Second

	// engineBootProbeTimeout bounds the yt-dlp "--version" probe the same
	// way connectEngine bounds a daemon probe: one window at boot, never a
	// hang. A frozen binary surfaces as a recorded last_error, not a stall.
	engineBootProbeTimeout = 10 * time.Second

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

			// Doc 17 section 1.3 stage S3: hold the stable process lock
			// beside the database for the process lifetime, so a second
			// server exits database_locked and `dl-tool restore` refuses
			// with restore_server_running instead of swapping the file
			// out from under a live instance. The handle must stay
			// reachable — an unreachable *os.File's finalizer would close
			// the descriptor and silently drop the flock — so it lives in
			// a named variable released when OnStart returns at shutdown.
			processLock, lockErr := store.AcquireProcessLock(cfg.DBPath)
			if lockErr != nil {
				// database_locked names the held-flock refusal; a
				// directory or open failure is a different fault.
				errCode := "database_locked"
				if !errors.Is(lockErr, store.ErrDatabaseLocked) {
					errCode = "database_lock_failed"
				}
				logger.Error("database lock failed", "err_code", errCode, "err", lockErr)
				os.Exit(exitFailure)
			}
			defer processLock.Release()

			// One derivation of the backup directory for the whole
			// composition root: store.Open's pre-migration backups and the
			// scheduler's WithMaintenance attach must agree with the
			// join NewServer hands NewSystemHandlers — so all three go
			// through store.BackupsDirFor.
			backupsDir := store.BackupsDirFor(cfg.ConfigDir)
			db, err := store.Open(ctx, cfg.DBPath, backupsDir)
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
			postprocess.SetConfigDir(cfg.ConfigDir)
			store.SetCompletedHook(func(ctx context.Context, taskID string) {
				// The store detaches cancellation already; the timeout
				// bounds the whole chain — the job enqueues plus a
				// full-duration T078 hook run — so a wedged chain cannot
				// stall the transitioning caller indefinitely. The budget
				// is the jobs package's single source of truth, not a
				// re-derivation of its timings here.
				hookCtx, cancel := context.WithTimeout(ctx, jobs.HookChainBudget)
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

			// The yt-dlp engine joins the same registry beside aria2 and
			// qBittorrent (T090): api.NewServer owns the daemon adapters, so
			// the media lane — a local subprocess configured entirely
			// through DLTOOL_YTDLP_PATH and DLTOOL_JS_RUNTIME_PATH — is
			// composed here, on the registry pointer the task handlers
			// captured. It registers unconditionally: the environment
			// always names a binary, and a missing one is Health's report,
			// never a construction failure. The engines row makes
			// GET /engines list it and POST /engines/{id}/test probe it
			// (FR-143); the boot probe mirrors connectEngine's, a warn and
			// a recorded last_error rather than a boot failure.
			ytdlpEngine := ytdlp.NewEngine(ytdlp.Config{
				BinaryPath:    cfg.YtdlpPath,
				JSRuntimePath: cfg.JSRuntimePath,
				ArchiveDir:    filepath.Join(cfg.ConfigDir, "archives"),
			}, logger)
			server.Engines.Register(ytdlpEngine)

			engineRows := store.NewSettingsStore(db)
			if err := engineRows.EnsureEngine(ctx, store.EngineIDYTDLP, engine.NameYtDlp, ytdlpEngine.Name(), cfg.YtdlpPath, time.Now().UnixMilli()); err != nil {
				logger.Error("engine row sync failed", "engine", engine.NameYtDlp, "err", err)
			} else {
				probeCtx, cancelProbe := context.WithTimeout(ctx, engineBootProbeTimeout)
				probeErr := ytdlpEngine.Connect(probeCtx)
				version := ""
				if probeErr == nil {
					version, probeErr = ytdlpEngine.Health(probeCtx)
				}
				cancelProbe()
				at := time.Now().UnixMilli()
				var touchErr error
				if probeErr != nil {
					logger.Warn("ytdlp engine unavailable", "engine", engine.NameYtDlp, "err", probeErr)
					detail := probeErr.Error()
					touchErr = engineRows.TouchEngine(ctx, store.EngineIDYTDLP, nil, &detail, at)
				} else {
					touchErr = engineRows.TouchEngine(ctx, store.EngineIDYTDLP, &version, nil, at)
				}
				if touchErr != nil {
					logger.Warn("engine probe outcome not recorded", "engine", engine.NameYtDlp, "err", touchErr)
				}
			}

			// store.Open has returned: migrations are applied and the database
			// answers, so /readyz may report ready (doc 05 section 13.1).
			server.Health.MarkReady()

			// The job worker pool shares runCtx, so OnStop cancels it and
			// runDone.Wait blocks until every in-flight handler has finished.
			// The search fan-out (T061) reuses the same Deps collaborators
			// the API holds — one runner, one registry, one guarded client
			// per process.
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

			// The bandwidth governor (T079) pushes the stored global limits to
			// every registered engine before the listener accepts traffic, so
			// the first admitted task already runs under them. A partial
			// fan-out is a warn, never a boot failure: a down engine must not
			// lock the UI out. Nothing re-pushes a missed fan-out yet — the
			// settings write path (T092) owns that call site. The parked-set
			// store (T081) rides the same construction so the schedule's No
			// Download cell can record and release the tasks it pauses.
			governor := engine.NewGovernor(server.Engines, store.NewSettingsStore(db)).
				WithTasks(store.NewTaskStore(db))
			governorCtx, cancelGovernor := context.WithTimeout(ctx, governorBootTimeout)
			if err := governor.LoadAndApply(governorCtx); err != nil {
				logger.Warn("global rate limits not applied to every engine", "err", err)
			}
			cancelGovernor()

			// The watch-folder loader (T083) shares the one create path
			// through server.WatchCreator — a dropped .torrent takes the
			// T020 path unchanged. DLTOOL_WATCH_DIR seeds one enabled row
			// before the loader starts, its destination the data root
			// containing the directory (doc 11 section 2); the value
			// arrives already root-validated, and a resolve or seed error
			// is logged and skipped, never fatal.
			settingsStore := store.NewSettingsStore(db)
			watcher := jobs.NewWatcher(settingsStore, server.WatchCreator)
			if cfg.WatchDir != "" {
				if root, _, err := fsx.ResolveDestinationRoot(cfg.DataRoots, cfg.WatchDir); err != nil {
					logger.Warn("watch directory resolves outside the data roots; skipping seed", "path", cfg.WatchDir, "err", err)
				} else if created, err := settingsStore.SeedWatchFolder(ctx, cfg.WatchDir, root); err != nil {
					logger.Warn("watch folder seed failed", "path", cfg.WatchDir, "err", err)
				} else if created {
					logger.Info("seeded watch folder", "path", cfg.WatchDir, "destination", root)
				}
			}

			// The cron scheduler enqueues the periodic jobs (rss_poll),
			// applies the active schedule cell once a minute with the
			// governor attached (T081), runs the nightly backup and the
			// retention prunes with the maintenance store attached (T091)
			// and drives the watch-folder loader beside the entries on
			// the same context (T083). The attaches land before Start arms
			// the entries, so the first tick already sees the live
			// instances. server.Maintenance is the same store
			// POST /system/backup uses, so the backup lock spans the cron
			// entry and the endpoint. Start blocks until runCtx is
			// cancelled in OnStop, then drains the in-flight entry and
			// the loader.
			scheduler := jobs.NewScheduler(db, logger).
				WithGovernor(governor).
				WithWatcher(watcher).
				WithMaintenance(server.Maintenance, backupsDir)
			runDone.Add(1)
			go func() {
				defer runDone.Done()
				scheduler.Start(runCtx)
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
			shutdownDrain(httpServer, server, db, func() {
				cancelRun()
				runDone.Wait()
			})
		})
		hooks.OnStop(func() {
			slog.Info("stopped")
			// The whole teardown runs in the OnStart goroutine — the one that
			// owns the values — released by stopped and fenced by drained.
			close(stopped)
			<-drained
		})
	})
	cli.Root().AddCommand(versionCmd())
	cli.Root().AddCommand(openapiCmd())
	cli.Root().AddCommand(restoreCmd())
	cli.Run()
}

// shutdownDrain performs the ordered teardown of docs/17 §2 once OnStop has
// signalled: withdraw readiness and close ingress first — step 1 — then run
// drainRuntime, which stops and joins the runtime loops — cron, job workers,
// metrics — then the server's background loops, the engines and the store.
// http.Server.Shutdown closes the listener immediately but waits for
// in-flight connections, and an open SSE stream can hold it for the whole
// budget, so it runs beside the runtime drain rather than serialized ahead
// of it: ingress stops accepting first either way.
func shutdownDrain(httpServer *http.Server, server *api.Server, db *sqlx.DB, drainRuntime func()) {
	server.Health.MarkDraining()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	httpDone := make(chan struct{})
	if httpServer != nil {
		go func() {
			defer close(httpDone)
			if err := httpServer.Shutdown(shutdownCtx); err != nil {
				slog.Error("http shutdown failed", "err", err)
				// Shutdown overran with requests still live. Close reaps
				// the listener and sockets — cancelling request contexts —
				// but cannot join hung handler goroutines.
				if cerr := httpServer.Close(); cerr != nil {
					slog.Error("http force-close failed", "err", cerr)
				}
			}
		}()
	} else {
		close(httpDone)
	}

	drainRuntime()
	<-httpDone
	server.Shutdown()
	// Engine teardown — yt-dlp's subprocess kills, qBittorrent's poll stop,
	// aria2's websocket abort — runs only after the loops that call into
	// engines are dead, and before the store closes so write-back paths
	// like the maindata infohash writer never meet a closed pool. A close
	// error is logged, never fatal: teardown is not abandoned to it.
	if err := server.Engines.CloseAll(); err != nil {
		slog.Error("engine shutdown failed", "err", err)
	}
	if err := db.Close(); err != nil {
		slog.Error("database close failed", "err", err)
	}
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

// restoreCmd is the dl-tool restore --from <file> of
// docs/17-operations-and-runbook.md section 3.4. It never touches the HTTP
// server: store.RestoreFrom owns the four refusal gates and the staged
// atomic replacement, and a refusal or failure exits 1 with the named
// error — humacli's Run drops a RunE return, so the command exits like the
// boot failure paths rather than relying on cobra's error propagation.
func restoreCmd() *cobra.Command {
	var from string
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Replace the database with a backup file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if from == "" {
				fmt.Fprintln(os.Stderr, "restore: --from <file> is required")
				os.Exit(exitFailure)
			}

			ctx := context.Background()
			cfg, err := config.Load(ctx)
			if err != nil {
				logConfigError(err)
				os.Exit(exitFailure)
			}

			tasks, err := store.RestoreFrom(ctx, cfg.DBPath, cfg.ConfigDir, from)
			if err != nil {
				fmt.Fprintln(os.Stderr, "restore:", err)
				os.Exit(exitFailure)
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "restored %d tasks\n", tasks); err != nil {
				fmt.Fprintln(os.Stderr, "restore: write result:", err)
				os.Exit(exitFailure)
			}

			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "backup file inside DLTOOL_CONFIG_DIR")

	return cmd
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
