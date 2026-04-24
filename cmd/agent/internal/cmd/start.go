// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/agent/goalstates"
	"github.com/Azure/unbounded/agent/phases"
	"github.com/Azure/unbounded/agent/phases/host"
	"github.com/Azure/unbounded/agent/phases/nodestart"
	"github.com/Azure/unbounded/agent/phases/rootfs"
	"github.com/Azure/unbounded/cmd/agent/internal/attest"
	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/internal/version"
)

func newCmdStart(cmdCtx *CommandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Bootstrap the host, rootfs, and start the node",
		Long:  "Run all three phases (host, rootfs, node-start) in sequence to fully bootstrap a machine and join it to the cluster.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()

			cmdCtx.Setup()

			cmdCtx.Logger.Info("starting unbounded-agent",
				"version", version.Version,
				"commit", version.GitCommit,
			)

			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			log := cmdCtx.Logger

			gs, err := goalstates.ResolveMachine(log, &cfg.AgentConfig, goalstates.NSpawnMachineKube1)
			if err != nil {
				return err
			}

			rootFSGoalState := gs.RootFS
			nodeStartGoalState := gs.NodeStart

			// Build the list of phases to execute.
			tasks := []phases.Task{
				// Phase 1: host
				host.InstallPackages(log),
				phases.Parallel(log,
					host.ConfigureOS(log),
					host.ConfigureNFTables(log),
					host.DisableDocker(log),
					host.DisableSwap(log),
				),

				// TPM Attestation (no-op when not configured).
				attest.ApplyAttestation(log, cfg.Attest, cfg.MachineName, nodeStartGoalState),

				// Phase 2: rootfs
				rootfs.Provision(log, rootFSGoalState),

				// Phase 3: node-start.
				nodestart.StartNode(log, nodeStartGoalState),

				// Phase 4: Persist the applied config for drift detection.
				daemon.PersistAppliedConfig(log, cfg.MachineName, &cfg.AgentConfig),

				// Phase 5: Enable and start the daemon that watches the
				// Machine CR for drift detection and reconciliation.
				host.EnableDaemon(log),
			}

			return phases.Serial(log, tasks...).Do(ctx)
		},
	}

	return cmd
}
