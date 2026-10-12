// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/base64"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/agent/internal/bootstrap"
	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/internal/version"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
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

			if err := daemon.MigrateHostRoot(cmdCtx.Logger); err != nil {
				return err
			}

			cfg, err := loadConfig(cmdCtx.Logger)
			if err != nil {
				return err
			}

			log := cmdCtx.Logger

			if err := cfg.Validate(); err != nil {
				return err
			}

			id, err := bootstrapIdentity(cfg)
			if err != nil {
				return err
			}

			stages := &agentStages{log: log, cfg: cfg}

			outcome, err := bootstrap.New(log, installstate.DefaultStore(), stages, stages).Run(ctx, id)
			if err != nil {
				return err
			}

			if outcome.AlreadyComplete {
				log.Info("installation already complete")
			}

			// Safe when bootstrap never reached credential setup: the reporter
			// reports through a nil-receiver check.
			stages.reporter.Succeeded(ctx)

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

// classifyNodeStartFailure maps a node-start failure onto the Machine condition
// reason, by the name of the task that reported it.
//
// wait-for-kubelet-bootstrap is matched as well as start-kubelet because both
// mean the node did not join. That wait is its own task inside this stage, and
// a failure there is the most common real one: a rejected or expired token, an
// unreachable API server, a CA mismatch. Reporting it as a generic failure
// would tell an operator nothing.
func classifyNodeStartFailure(err error) string {
	message := err.Error()
	switch {
	case strings.Contains(message, "start-kubelet"),
		strings.Contains(message, "wait-for-kubelet-bootstrap"):
		return "KubeletBootstrapFailed"
	case strings.Contains(message, "start-nspawn-machine"):
		return "NSpawnFailed"
	default:
		return "Failed"
	}
}
