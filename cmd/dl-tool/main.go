package main

import (
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"runtime/debug"

	"github.com/danielgtaylor/huma/v2/humacli"
	"github.com/spf13/cobra"

	"github.com/L-K-M/dl-tool/internal/obs"
)

const unknownRevision = "unknown"

// Options are the humacli-bound flags and DLTOOL_* variables.
type Options struct {
	Host string `help:"listen address" default:":8080"`
}

var version = "dev"

func main() {
	slog.SetDefault(obs.NewLogger(os.Stdout, "info", "json"))
	stopped := make(chan struct{})

	cli := humacli.New(func(hooks humacli.Hooks, _ *Options) {
		hooks.OnStart(func() {
			slog.Info("started")
			<-stopped
		})
		hooks.OnStop(func() {
			slog.Info("stopped")
			close(stopped)
		})
	})
	cli.Root().AddCommand(versionCmd())
	cli.Run()
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
