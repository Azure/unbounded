package app

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/project-unbounded/unbounded-kube/internal/version"
)

func Run() {
	root := &cobra.Command{
		Use:          "kubectl-unbounded",
		SilenceUsage: true,
	}

	root.AddCommand(siteCommandGroup())
	root.AddCommand(machineCommandGroup())
	root.AddCommand(version.Command())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
