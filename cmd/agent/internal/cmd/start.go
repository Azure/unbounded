// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/host"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/nodestart"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/rootfs"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilio"
	"github.com/Azure/unbounded-kube/internal/provision"
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
				rootfs.EnsureNSpawnWorkspace(log, rootFSGoalState),
				phases.Parallel(log,
					rootfs.DownloadKubeBinaries(log, rootFSGoalState),
					rootfs.DownloadCRIBinaries(log, rootFSGoalState),
					rootfs.DownloadCNIBinaries(log, rootFSGoalState),
					rootfs.ConfigureOS(rootFSGoalState),
					rootfs.DisableResolved(rootFSGoalState),
				),

				// Phase 3: node-start
				phases.Parallel(log,
					nodestart.ConfigureContainerd(nodeStartGoalState),
					nodestart.ConfigureKubelet(nodeStartGoalState),
				),
				nodestart.StartNSpawnMachine(log, nodeStartGoalState),
				nodestart.SetupNVIDIA(log, nodeStartGoalState),
				nodestart.StartContainerd(log, nodeStartGoalState),
				nodestart.StartKubelet(log, nodeStartGoalState),
			}

			if err := phases.Serial(log, tasks...).Do(ctx); err != nil {
				return err
			}

			// Persist the applied config so the daemon can detect drift.
			// This MUST happen before the daemon starts, because the
			// daemon reads the applied config on task arrival.
			nspawnMachineName := goalstates.NSpawnMachineKube1
			if err := persistAppliedConfig(cfg, nspawnMachineName); err != nil {
				return err
			}
			log.Info("applied config persisted",
				"path", goalstates.AppliedConfigPath(nspawnMachineName),
			)

			// Phase 4 (optional): enable the agent daemon when a task
			// server endpoint is configured. Runs after applied config
			// is persisted so the daemon can find it immediately.
			if cfg.TaskServer != nil && cfg.TaskServer.Endpoint != "" {
				log.Info("task server configured, enabling daemon",
					"endpoint", cfg.TaskServer.Endpoint,
				)

				daemonTask := host.EnableDaemon(log, cfg.TaskServer.Endpoint)
				if err := daemonTask.Do(ctx); err != nil {
					return err
				}
			}

			return nil
		},
	}

	return cmd
}

// persistAppliedConfig writes the agent config to the applied config file
// for the given nspawn machine. This is used for drift detection by the
// daemon when a NodeUpdateSpec task arrives.
func persistAppliedConfig(cfg *provision.AgentConfig, machineName string) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal applied config: %w", err)
	}

	path := goalstates.AppliedConfigPath(machineName)
	if err := utilio.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write applied config to %s: %w", path, err)
	}

	return nil
}
