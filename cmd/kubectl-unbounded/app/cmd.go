// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"

	netcmd "github.com/Azure/unbounded/cmd/kubectl-unbounded/app/net"
	"github.com/Azure/unbounded/internal/version"
)

func Run() {
	ctrl.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

	root := &cobra.Command{
		Use:          "kubectl-unbounded",
		SilenceUsage: true,
	}

	root.AddCommand(siteCommandGroup())
	root.AddCommand(machineCommandGroup())
	root.AddCommand(netcmd.Command())
	root.AddCommand(overridesCommandGroup())
	root.AddCommand(installCommand())
	root.AddCommand(version.Command())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
