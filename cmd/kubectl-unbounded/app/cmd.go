// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"os"

	"github.com/spf13/cobra"

	netcmd "github.com/Azure/unbounded/cmd/kubectl-unbounded/app/net"
	"github.com/Azure/unbounded/internal/version"
)

func Run() {
	root := &cobra.Command{
		Use:          "kubectl-unbounded",
		SilenceUsage: true,
	}

	root.AddCommand(siteCommandGroup())
	root.AddCommand(machineCommandGroup())
	root.AddCommand(netcmd.Command())
	root.AddCommand(version.Command())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
