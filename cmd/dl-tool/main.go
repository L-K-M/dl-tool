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
	"time"

	"github.com/danielgtaylor/huma/v2/humacli"
	"github.com/spf13/cobra"

	"github.com/L-K-M/dl-tool/internal/api"
	"github.com/L-K-M/dl-tool/internal/config"
	"github.com/L-K-M/dl-tool/internal/obs"
	"github.com/L-K-M/dl-tool/internal/store"
)

const (
	unknownRevision    = "unknown"
	bootstrapLogLevel  = "info"
	bootstrapLogFormat = "json"
	fallbackErrorCode  = "config_malformed"
	exitFailure        = 1
	backupsDirName     = "backups"

	// readHeaderTimeout bounds slow-header exposure on the main listener;
	// shutdownTimeout bounds the graceful drain on stop.
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// Options are the humacli-bound flags.
type Options struct {
	Host string `help:"listen address" default:":8080"`
}

var version = "dev"

func main() {
	slog.SetDefault(obs.NewLogger(os.Stdout, bootstrapLogLevel, bootstrapLogFormat))
	stopped := make(chan struct{})
	drained := make(chan struct{})

	var httpServer *http.Server

	cli := humacli.New(func(hooks humacli.Hooks, _ *Options) {
		hooks.OnStart(func() {
			ctx := context.Background()

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

			server, err := api.NewServer(cfg, db, logger)
			if err != nil {
				logger.Error("server build failed", "err", err)
				os.Exit(exitFailure)
			}

			httpServer = &http.Server{
				Addr:              cfg.HTTPAddr,
				Handler:           server.Router,
				ReadHeaderTimeout: readHeaderTimeout,
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
			if err := db.Close(); err != nil {
				slog.Error("database close failed", "err", err)
			}

			close(drained)
		})
		hooks.OnStop(func() {
			slog.Info("stopped")
			close(stopped)
			<-drained
		})
	})
	cli.Root().AddCommand(versionCmd())
	cli.Root().AddCommand(openapiCmd())
	cli.Run()
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
