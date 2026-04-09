// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded-kube/internal/inventory"
)

func main() {
	var (
		debug  bool
		dbPath string
	)

	rootCmd := &cobra.Command{
		Use:   "inventory",
		Short: "Collect node inventory data",
		RunE: func(cmd *cobra.Command, args []string) error {
			return inventory.Execute(debug, dbPath)
		},
	}

	rootCmd.Flags().BoolVar(&debug, "debug", false, "Enable debug output")
	rootCmd.Flags().StringVar(&dbPath, "db", "./inventory.db", "Path to the output database file")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
