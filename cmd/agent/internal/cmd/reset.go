// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"log/slog"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/internal/version"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func newCmdReset(cmdCtx *CommandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset the host by removing the agent and all associated resources",
		Long: `Fully reverse the bootstrap process: stop and remove the nspawn machines,
clean up network interfaces, remove configuration files, and restore the host
to its original state. This is the inverse of 'unbounded-agent start'.

Both possible nspawn machine names (kube1 and kube2) are stopped and removed.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()

			cmdCtx.Setup()

			cmdCtx.Logger.Info("starting unbounded-agent reset",
				"version", version.Version,
				"commit", version.GitCommit,
			)

			return resetAgent(cmdCtx.Logger).Do(ctx)
		},
	}

	return cmd
}

// resetAgent returns a task that resets the host by stopping the daemon and
// removing the unbounded-agent and all associated resources.
func resetAgent(log *slog.Logger) phases.Task {
	return phases.Serial(log,
		// CLI reset runs outside the daemon, so it can stop the daemon first to
		// keep it from reconciling while files are removed. The daemon operation
		// path stops the daemon last because stopping the unit terminates the
		// reconciler before it can mark the MachineOperation complete.
		daemon.StopDaemon(log),
		daemon.ResetAgentResources(log),
	)
}
