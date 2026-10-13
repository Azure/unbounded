// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"

	"github.com/spf13/cobra"

	netcmd "github.com/Azure/unbounded/cmd/kubectl-unbounded/app/net"
	"github.com/Azure/unbounded/internal/logger"
	"github.com/Azure/unbounded/internal/version"
)

func Run() {
	// The net command has a PersistentPreRun of its own. Without this, cobra
	// runs only the nearest hook and the root's logging setup would be skipped
	// for every net subcommand.
	cobra.EnableTraverseRunHooks = true

	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	logOpts := logger.Options{Format: logger.FormatPrettyText}

	root := &cobra.Command{
		Use:          "kubectl-unbounded",
		SilenceUsage: true,
		// Logs go to stderr; several commands write their results to stdout.
		PersistentPreRun: func(*cobra.Command, []string) {
			logOpts.Setup(os.Stderr)
		},
	}

	logOpts.BindFlags(root.PersistentFlags())

	root.AddCommand(siteCommandGroup())
	root.AddCommand(machineCommandGroup())
	root.AddCommand(netcmd.Command())
	root.AddCommand(overridesCommandGroup())
	root.AddCommand(installCommand())
	root.AddCommand(version.Command())

	return root
}
