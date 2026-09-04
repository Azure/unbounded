// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/version"
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
