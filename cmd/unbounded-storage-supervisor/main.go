// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Command unbounded-storage-supervisor installs and supervises the
// unbounded-storage daemon on a node.
//
// It is intended to run as a Kubernetes DaemonSet that shares a single image
// across two roles selected by subcommand:
//
//   - install: the init-container role. It installs (or upgrades) the
//     unbounded-storage daemon on the host.
//   - run: the runtime-container role. It supervises the installed daemon for
//     the lifetime of the pod.
package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/storagesupervisor"
	"github.com/Azure/unbounded/internal/version"
)

func main() {
	rootCmd := &cobra.Command{
		Use:               "unbounded-storage-supervisor",
		Short:             "Install and supervise the unbounded-storage daemon",
		SilenceUsage:      true,
		PersistentPreRunE: setupLogging,
	}

	rootCmd.AddCommand(installCmd())
	rootCmd.AddCommand(runCmd())
	rootCmd.AddCommand(version.Command())

	if err := rootCmd.Execute(); err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}

// setupLogging installs a JSON slog handler as the process default so all
// subcommands emit structured logs.
func setupLogging(_ *cobra.Command, _ []string) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	return nil
}

// installCmd is the init-container entrypoint. It loads configuration from the
// environment, checks host-level preconditions, and runs the native install
// workflow (a port of hack/scripts/install-unbounded-storage.sh).
func installCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install or upgrade the unbounded-storage daemon on the host",
		RunE: func(cmd *cobra.Command, _ []string) error {
			slog.Info("unbounded-storage-supervisor install", "version", version.String())

			cfg, err := storagesupervisor.LoadConfig()
			if err != nil {
				return err
			}

			if err := storagesupervisor.Preconditions(cfg); err != nil {
				return err
			}

			return storagesupervisor.Install(cmd.Context(), cfg)
		},
	}
}

// runCmd is the runtime-container entrypoint. It loads configuration from the
// environment, renders the projected ConfigMap into the daemon's config file,
// and supervises it (re-rendering on change) until the process is signaled.
func runCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Supervise the installed unbounded-storage daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			slog.Info("unbounded-storage-supervisor run", "version", version.String())

			cfg, err := storagesupervisor.LoadConfig()
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			return storagesupervisor.Run(ctx, cfg)
		},
	}
}
