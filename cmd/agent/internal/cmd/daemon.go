// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded-kube/internal/provision"
)

func newCmdDaemon(cmdCtx *CommandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the long-lived agent daemon",
		Long: "Long-running daemon that registers the Machine CR and then " +
			"blocks the process from exiting. It is intended to be managed " +
			"by systemd on the host after the machine has been started.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()

			cmdCtx.Setup()
			log := cmdCtx.Logger

			agentCfg, err := loadConfig()
			if err != nil {
				return err
			}

			daemonCfg, err := buildDaemonConfig(agentCfg)
			if err != nil {
				return err
			}

			return daemon.Run(ctx, log, daemonCfg)
		},
	}

	return cmd
}

// buildDaemonConfig converts an AgentConfig into the daemon-specific Config
// struct, decoding the base64 CA certificate into PEM bytes.
func buildDaemonConfig(cfg *provision.AgentConfig) (*daemon.Config, error) {
	caCert, err := base64.StdEncoding.DecodeString(cfg.Cluster.CaCertBase64)
	if err != nil {
		return nil, fmt.Errorf("decode CaCertBase64: %w", err)
	}

	return &daemon.Config{
		MachineName:        cfg.MachineName,
		APIServer:          cfg.Kubelet.ApiServer,
		CACertData:         caCert,
		BootstrapToken:     cfg.Kubelet.BootstrapToken,
		NodeLabels:         cfg.Kubelet.Labels,
		RegisterWithTaints: cfg.Kubelet.RegisterWithTaints,
	}, nil
}
