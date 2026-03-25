package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	"github.com/project-unbounded/unbounded-kube/cmd/metalman/commands"
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
)

var version = "dev"

func main() {
	ctrl.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

	root := &cobra.Command{
		Use:   "metalman",
		Short: "Bare metal provisioning for Kubernetes",
	}
	root.AddCommand(commands.RebootCmd(), commands.ServePXECmd())
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println(version)
		},
	})

	root.CompletionOptions.DisableDefaultCmd = true
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
