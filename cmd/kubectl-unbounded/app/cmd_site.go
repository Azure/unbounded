package app

import "github.com/spf13/cobra"

func siteCommandGroup() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "site",
		Short: "Manage unbounded-kube sites",
	}

	cmd.AddCommand(
		siteInitCommand(),
		siteAddMachineCommand())

	return cmd
}
