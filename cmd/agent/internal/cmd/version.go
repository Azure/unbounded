// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded-kube/internal/version"
)

func newCmdVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the agent version and build information",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Printf("unbounded-agent %s\n", version.String())
		},
	}
}
