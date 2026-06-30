// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"github.com/spf13/cobra"
)

func siteCommandGroup() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "site",
		Short: "Manage unbounded sites",
	}

	cmd.AddCommand(
		siteInitCommand())

	return cmd
}
