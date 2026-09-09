// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/internal/version"
	"github.com/Azure/unbounded/pkg/agent/bootstrap"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/installstate"
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

			return runStart(ctx, cmdCtx.Logger)
		},
	}

	return cmd
}

// runStart resolves what to install and hands the sequencing to the bootstrap
// coordinator.
//
// The ordering and recovery rules live in pkg/agent/bootstrap so they can be
// tested without a host; this function is the wiring that gives the coordinator
// something real to run.
func runStart(ctx context.Context, log *slog.Logger) error {
	cfg, err := loadConfig(log)
	if err != nil {
		return err
	}

	downloads, containerImageArchives, err := provision.ResolveDownloadOverridesWithOfflineArtifacts(ctx, cfg)
	if err != nil {
		return err
	}

	gs, err := goalstates.ResolveMachine(log, &cfg.AgentConfig, goalstates.NSpawnMachineKube1, downloads)
	if err != nil {
		return err
	}

	identity, err := bootstrapIdentity(cfg)
	if err != nil {
		return err
	}

	stages := &agentStages{
		log:                    log,
		cfg:                    cfg,
		rootFS:                 gs.RootFS,
		nodeStar:               gs.NodeStart,
		containerImageArchives: containerImageArchives,
	}

	reporter := &bootstrapReporter{
		reporter: daemon.NewBootstrapStatusReporter(ctx, log, &cfg.AgentConfig),
	}

	coordinator := bootstrap.New(log, installstate.DefaultStore(), stages, reporter)

	outcome, err := coordinator.Run(ctx, identity)
	if err != nil {
		return err
	}

	switch {
	case outcome.AlreadyComplete:
		log.Info("host is already bootstrapped, nothing to do")
	case outcome.Resumed:
		log.Info("resumed and completed an unfinished installation")
		reporter.reporter.Succeeded(ctx)
	default:
		reporter.reporter.Succeeded(ctx)
	}

	return nil
}

func syncAttestedKubeletConfig(cfg *provision.AgentConfig, nodeStart *goalstates.NodeStart) {
	if nodeStart.Kubelet.BootstrapToken != "" {
		cfg.Kubelet.Auth.BootstrapToken = nodeStart.Kubelet.BootstrapToken
	}

	if len(nodeStart.Kubelet.CACertData) > 0 {
		cfg.Cluster.CaCertBase64 = base64.StdEncoding.EncodeToString(nodeStart.Kubelet.CACertData)
	}
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
