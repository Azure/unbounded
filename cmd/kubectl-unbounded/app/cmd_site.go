package app

import (
	"github.com/spf13/cobra"

	metalmancmd "github.com/project-unbounded/unbounded-kube/internal/metalman/commands"
)

func siteCommandGroup() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "site",
		Short: "Manage unbounded-kube sites",
	}

	cmd.AddCommand(
		siteInitCommand(),
		siteAddMachineCommand(),
		deployPXECommand(),
		metalmancmd.ServePXECmd())

	return cmd
}
