// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/inventory/inspector"
)

func main() {
	config := inspector.Config{}

	rootCmd := &cobra.Command{
		Use:   "inventory-inspector",
		Short: "Inspect collected inventory data",
		RunE: func(cmd *cobra.Command, args []string) error {
			return inspector.Execute(cmd.Context(), config)
		},
	}

	rootCmd.Flags().BoolVar(&config.Debug, "debug", false, "Enable debug output")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
