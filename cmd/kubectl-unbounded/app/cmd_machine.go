// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import "github.com/spf13/cobra"

func machineCommandGroup() *cobra.Command {
	return newMachineCommandGroup(newMachineCommandRuntime())
}

func newMachineCommandGroup(rt *machineCommandRuntime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "machine",
		Short: "Manage unbounded-kube machines",
	}

	cmd.AddCommand(
		configCommandGroup(),
		newMachineOperationCommandGroup(rt),
		machineRegisterCommand(),
		newMachineNodeRebootCommand(rt),
		newMachineHostRebootCommand(rt),
		newMachinePowerOffCommand(rt),
		newMachinePowerOnCommand(rt),
		newMachineAgentUpgradeCommand(rt),
		newMachineAgentResetCommand(rt),
		newMachineReplaceCommand(rt),
		machineRebootCommand(),
		machineHardRebootCommand(),
		machineRepaveCommand(),
		machineSoftRebootCommand(),
		machineManualBootstrapCommand(),
	)

	return cmd
}
