// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/reset"
	"github.com/Azure/unbounded-kube/internal/version"
)

// defaultConfigPath is the well-known location for the agent config file
// written by cloud-init based bootstrapping.
const defaultConfigPath = "/etc/unbounded-agent/config.json"

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

			log := cmdCtx.Logger

			return phases.Serial(log,
				// Step 1: Stop both nspawn machines.
				phases.Parallel(log,
					reset.StopMachine(log, goalstates.NSpawnMachineKube1),
					reset.StopMachine(log, goalstates.NSpawnMachineKube2),
				),

				// Step 2-3: Remove network interfaces and WireGuard keys.
				phases.Parallel(log,
					reset.RemoveNetworkInterfaces(log),
					reset.RemoveWireGuardKeys(log),
				),

				// Step 4: Remove nspawn configuration files for both machines.
				phases.Parallel(log,
					reset.RemoveNSpawnConfig(log, goalstates.NSpawnMachineKube1),
					reset.RemoveNSpawnConfig(log, goalstates.NSpawnMachineKube2),
				),

				// Step 5: Remove both machine rootfs directories.
				phases.Parallel(log,
					reset.RemoveMachine(log, goalstates.NSpawnMachineKube1),
					reset.RemoveMachine(log, goalstates.NSpawnMachineKube2),
				),

				// Step 6: Clean up policy routing rules.
				reset.CleanupRoutes(log),

				// Step 7: Remove agent binaries and config.
				reset.RemoveAgentArtifacts(log),

				// Step 8: Reload systemd.
				reset.ReloadSystemd(log),
			).Do(ctx)
		},
	}

	return cmd
}
