package app

import (
	"os"

	"github.com/spf13/cobra"
)

func Run() {
	root := &cobra.Command{
		Use:          "kubectl-unbounded",
		SilenceUsage: true,
	}

	root.AddCommand(siteCommandGroup())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
