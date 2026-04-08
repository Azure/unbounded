package app

import (
	"fmt"
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
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println(version.String())
		},
	})

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
