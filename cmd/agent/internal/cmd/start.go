// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/host"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/nodestart"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/rootfs"
	"github.com/Azure/unbounded-kube/internal/version"
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

			gs, err := goalstates.ResolveMachine(log, cfg, goalstates.NSpawnMachineKube1)
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
				host.ApplyAttestation(log, cfg.Attest, cfg.MachineName, nodeStartGoalState),

				// Phase 2: rootfs
				rootfs.Provision(log, rootFSGoalState),

				// Register a Machine CR for this node if one does not already
				// exist. This supports dynamic environments (manual-bootstrap,
				// cloud-init) where a Machine CR may not have been pre-created.
				// Must run after ApplyAttestation so the bootstrap token is
				// fully resolved, and before kubelet starts.
				nodestart.RegisterMachine(log, nodeStartGoalState),

				// Phase 3: node-start (includes persisting the applied config).
				nodestart.StartNode(log, nodeStartGoalState, cfg),
			}

			if err := phases.Serial(log, tasks...).Do(ctx); err != nil {
				return err
			}

			// Phase 4: Enable and start the daemon that watches the Machine CR
			// for ongoing drift detection and reconciliation.
			if err := host.EnableDaemon(log).Do(ctx); err != nil {
				return err
			}

			return nil
		},
	}

	return cmd
}
