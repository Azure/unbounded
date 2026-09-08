// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import "github.com/spf13/cobra"

func configCommandGroup() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage MachineConfigurations and MachineConfigurationVersions",
	}

	cmd.AddCommand(
		configCreateCommand(),
		configGetCommand(),
		configVersionsCommand(),
		configAssignCommand(),
	)

	return cmd
}
