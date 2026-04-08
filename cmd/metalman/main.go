package main

import (
	"log/slog"
	"os"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/project-unbounded/unbounded-kube/internal/metalman/commands"
	"github.com/project-unbounded/unbounded-kube/internal/version"
)

func main() {
	ctrl.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

	root := &cobra.Command{
		Use:   "metalman",
		Short: "Bare metal provisioning for Kubernetes",
	}
	root.AddCommand(commands.ServePXECmd())
	root.AddCommand(version.Command())

	root.CompletionOptions.DisableDefaultCmd = true
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
