// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/agent/internal/attest"
	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/internal/version"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
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

			cfg, err := loadConfig(cmdCtx.Logger)
			if err != nil {
				return err
			}

			log := cmdCtx.Logger

			gs, err := goalstates.ResolveMachine(log, &cfg.AgentConfig, goalstates.NSpawnMachineKube1, provision.ResolveDownloadOverrides(cfg.Downloads))
			if err != nil {
				return err
			}

			rootFSGoalState := gs.RootFS
			nodeStartGoalState := gs.NodeStart

			// Run host setup and attestation first. Metalman bootstrap tokens are
			// only available after attestation, so Machine status reporting starts
			// after this block.
			preBootstrapTasks := []phases.Task{
				// Phase 1: host
				host.InstallPackages(log),
				phases.Parallel(log,
					host.ConfigureOS(log),
					host.ConfigureNFTables(log),
					host.DisableDocker(log),
					host.DisableSwap(log),
					host.HardenAPT(log),
				),

				// TPM Attestation (no-op when not configured).
				attest.ApplyAttestation(log, cfg.Attest, cfg.MachineName, nodeStartGoalState),
			}

			if err := phases.Serial(log, preBootstrapTasks...).Do(ctx); err != nil {
				return err
			}

			syncAttestedKubeletConfig(&cfg.AgentConfig, nodeStartGoalState)

			reporter := daemon.NewBootstrapStatusReporter(ctx, log, &cfg.AgentConfig)
			reporter.Running(ctx)

			if err := runBootstrapTask(ctx, log, reporter, "RootFSFailed", rootfs.Provision(log, rootFSGoalState)); err != nil {
				return err
			}

			if err := phases.ExecuteTask(ctx, log, nodestart.StartNode(log, nodeStartGoalState)); err != nil {
				reporter.Failed(ctx, classifyNodeStartFailure(err), err)
				return err
			}

			if err := runBootstrapTask(ctx, log, reporter, "KubeletBootstrapFailed", nodestart.WaitForKubeletBootstrap(log, nodeStartGoalState.MachineName)); err != nil {
				return err
			}

			if err := phases.Serial(log,
				// Phase 4: Persist the applied config for drift detection.
				daemon.PersistAppliedConfig(log, nodeStartGoalState.MachineName, &cfg.AgentConfig),

				// Phase 5: Enable and start the daemon that watches the
				// Machine CR for drift detection and reconciliation.
				daemon.EnableDaemon(log),
			).Do(ctx); err != nil {
				reporter.Failed(ctx, "Failed", err)
				return err
			}

			reporter.Succeeded(ctx)

			return nil
		},
	}

	return cmd
}

func syncAttestedKubeletConfig(cfg *provision.AgentConfig, nodeStart *goalstates.NodeStart) {
	if nodeStart.Kubelet.BootstrapToken != "" {
		cfg.Kubelet.Auth.BootstrapToken = nodeStart.Kubelet.BootstrapToken
	}

	if len(nodeStart.Kubelet.CACertData) > 0 {
		cfg.Cluster.CaCertBase64 = base64.StdEncoding.EncodeToString(nodeStart.Kubelet.CACertData)
	}
}

func runBootstrapTask(ctx context.Context, log *slog.Logger, reporter *daemon.BootstrapStatusReporter, reason string, task phases.Task) error {
	if err := phases.ExecuteTask(ctx, log, task); err != nil {
		reporter.Failed(ctx, reason, err)
		return err
	}

	return nil
}

func classifyNodeStartFailure(err error) string {
	message := err.Error()
	switch {
	case strings.Contains(message, "start-kubelet"):
		return "KubeletBootstrapFailed"
	case strings.Contains(message, "start-nspawn-machine"):
		return "NSpawnFailed"
	default:
		return "Failed"
	}
}
