// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func Run() {
	cmdCtx := &CommandContext{
		LogFormat: "text",
	}

	root := &cobra.Command{
		Use:   "agent",
		Short: "Unbounded Kubernetes Node Agent",
	}

	root.PersistentFlags().BoolVar(&cmdCtx.Debug, "debug", false, "enable debug-level logging")
	root.PersistentFlags().StringVar(&cmdCtx.LogFormat, "log-format", cmdCtx.LogFormat, "log format: text or json")
	root.PersistentFlags().BoolVar(&cmdCtx.LogNoColor, "no-color", false, "disable color in log output")

	root.AddCommand(
		newCmdStart(cmdCtx),
		newCmdDaemon(cmdCtx),
		newCmdReset(cmdCtx),
		newCmdVersion(),
	)

	if err := root.Execute(); err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
}
