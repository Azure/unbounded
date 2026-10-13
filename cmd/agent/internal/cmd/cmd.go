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
	cmdCtx := newCommandContext()

	root := &cobra.Command{
		Use:   "agent",
		Short: "Unbounded Kubernetes Node Agent",
	}

	bindLogFlags(root, cmdCtx)

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

// bindLogFlags registers the logging flags on root for every subcommand.
//
// --debug is the agent's original level flag. It stays, hidden, because the
// install script (internal/provision/assets/unbounded-agent-install.sh) passes
// it to whichever agent release it downloads.
func bindLogFlags(root *cobra.Command, cmdCtx *CommandContext) {
	flags := root.PersistentFlags()
	cmdCtx.LogOpts.BindFlags(flags)
	cmdCtx.LogOpts.BindDebugFlag(flags)
	flags.BoolVar(&cmdCtx.LogOpts.NoColor, "no-color", false, "Disable color in prettytext log output")

	if err := flags.MarkHidden("debug"); err != nil {
		panic(fmt.Sprintf("hide --debug: %v", err))
	}
}
