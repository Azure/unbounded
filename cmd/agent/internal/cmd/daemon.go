// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/daemon"
)

func newCmdDaemon(cmdCtx *CommandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Watch the Machine CR and reconcile the node to the desired state",
		Long: "Long-running daemon that watches the Machine custom resource on the " +
			"control plane and performs node updates when the desired state diverges " +
			"from the locally applied configuration.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()

			cmdCtx.Setup()

			return daemon.Run(ctx, cmdCtx.Logger)
		},
	}

	return cmd
}
