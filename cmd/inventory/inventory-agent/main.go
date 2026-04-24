// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	inventory "github.com/Azure/unbounded/internal/inventory/agent"
)

func main() {
	config := inventory.ExecuteInventoryConfig{}

	rootCmd := &cobra.Command{
		Use:   "inventory",
		Short: "Collect node inventory data",
		RunE: func(cmd *cobra.Command, args []string) error {
			return inventory.Execute(config)
		},
	}

	rootCmd.Flags().BoolVar(&config.Debug, "debug", false, "Enable debug output")
	rootCmd.Flags().StringVar(&config.DbPath, "db", "./inventory.db", "Path to the output database file")
	rootCmd.Flags().StringVar(&config.CollectorAddr, "collector", "inventory-collector:50051", "Address of the inventory collector")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
