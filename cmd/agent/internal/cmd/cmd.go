// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
)

func Run() {
	cmdCtx := &CommandContext{
		LogFormat: "text",
	}

	root := &cobra.Command{
		Use:   "agent",
		Short: "Unbounded Kubernetes Node Agent",
	}

	root.PersistentFlags().BoolVar(&cmdCtx.Debug, "debug", false, "enable debug-level logging")
	root.PersistentFlags().StringVar(&cmdCtx.LogFormat, "log-format", cmdCtx.LogFormat, "log format: text or json")
	root.PersistentFlags().BoolVar(&cmdCtx.LogNoColor, "no-color", false, "disable color in log output")

	root.AddCommand(
		newCmdStart(cmdCtx),
		newCmdPreflight(cmdCtx),
		newCmdDaemon(cmdCtx),
		newCmdReset(cmdCtx),
		newCmdVersion(),
		newCmdNSpawnLifecycle(cmdCtx),
		newCmdHostAgentUpgrade(cmdCtx),
		newCmdRecordAgentUpgradeFailureSignal(cmdCtx),
	)

	if err := root.Execute(); err != nil {
		// The daemon standing down for an unfinished installation is an
		// ordinary state, not a fault. It has already said so in the journal,
		// and the unit treats this code as success so systemd leaves it alone
		// rather than restarting it into its start limit.
		if errors.Is(err, daemon.ErrDeferred) {
			os.Exit(daemon.DeferredExitCode)
		}

		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
}
