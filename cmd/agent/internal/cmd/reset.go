// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/internal/version"
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

			return daemon.ResetAgent(cmdCtx.Logger).Do(ctx)
		},
	}

	return cmd
}
