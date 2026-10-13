// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/metalman/commands"
	"github.com/Azure/unbounded/internal/version"
)

func main() {
	root := &cobra.Command{
		Use:   "metalman",
		Short: "Bare metal provisioning for Kubernetes",
	}
	root.AddCommand(commands.ServePXECmd())
	root.AddCommand(version.Command())

	root.CompletionOptions.DisableDefaultCmd = true
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
