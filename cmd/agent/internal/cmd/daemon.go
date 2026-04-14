// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
)

func newCmdDaemon(cmdCtx *CommandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the long-lived agent daemon",
		Long: "Long-running daemon that blocks the process from exiting. " +
			"It is intended to be managed by systemd on the host after " +
			"the machine has been started.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()

			cmdCtx.Setup()
			log := cmdCtx.Logger

			return runDaemon(ctx, log)
		},
	}

	return cmd
}

// runDaemon blocks until the context is cancelled (e.g. via SIGINT).
func runDaemon(ctx context.Context, log *slog.Logger) error {
	log.Info("daemon starting")

	// Block until the context is done.
	<-ctx.Done()

	log.Info("daemon shutting down")

	return nil
}
