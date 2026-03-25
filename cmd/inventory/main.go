package main

import (
	"fmt"
	"os"

	"github.com/project-unbounded/unbounded-kube/pkg/inventory"
	"github.com/spf13/cobra"
)

func main() {
	var (
		debug  bool
		dbPath string
	)

	rootCmd := &cobra.Command{
		Use:   "inventory",
		Short: "Collect node inventory data",
		Run: func(cmd *cobra.Command, args []string) {
			inventory.CollectInventory(debug, dbPath)
		},
	}

	rootCmd.Flags().BoolVar(&debug, "debug", false, "Enable debug output")
	rootCmd.Flags().StringVar(&dbPath, "db", "./inventory.db", "Path to the output database file")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
