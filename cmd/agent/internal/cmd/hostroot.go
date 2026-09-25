// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// newCmdHostRoot prints where this agent keeps its host-side files on this host,
// as it will once migrated. It does not migrate or otherwise change the host.
//
// Its existence is what the install script and AgentUpgrade check for: an
// agent without it predates the host root and still installs to the legacy
// root.
func newCmdHostRoot() *cobra.Command {
	return &cobra.Command{
		Use:    "host-root",
		Short:  "Print the directory that holds the agent's host-side files",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), goalstates.PlannedHostPaths().Root)
			return err
		},
	}
}
