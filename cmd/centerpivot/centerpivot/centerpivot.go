// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package centerpivot is the centerpivot binary: the controller that updates
// Azure Container Linux nodes from Nebraska, and the node-info DaemonSet that
// publishes each node's Nebraska identity. See designs/centerpivot.md.
package centerpivot

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/version"
)

// Run runs the command line.
func Run() {
	root := &cobra.Command{
		Use:   "centerpivot",
		Short: "Update Azure Container Linux nodes from Nebraska",
	}

	root.AddCommand(controllerCommand())
	root.AddCommand(nodeInfoCommand())
	root.AddCommand(version.Command())

	if err := root.Execute(); err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
}
