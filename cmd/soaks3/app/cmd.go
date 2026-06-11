// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package app implements the soaks3 command tree.
package app

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/version"
)

// Run executes the soaks3 root command.
func Run() {
	root := &cobra.Command{
		Use:          "soaks3",
		Short:        "S3 load generator for unbounded-storage",
		Long:         "soaks3 seeds deterministic test data and drives read load against an unbounded-storage S3 frontend.",
		SilenceUsage: true,
	}

	root.AddCommand(newSeedCommand())
	root.AddCommand(newRunCommand())
	root.AddCommand(version.Command())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
