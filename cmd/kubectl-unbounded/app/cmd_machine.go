package app

import "github.com/spf13/cobra"

func machineCommandGroup() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "machine",
		Short: "Manage unbounded-kube machines",
	}

	cmd.AddCommand(
		machineRebootCommand(),
		machineReimageCommand(),
	)

	return cmd
}
