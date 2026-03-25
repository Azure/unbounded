package cmd

import (
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/project-unbounded/unbounded-kube/cmd/kubectl-unbounded/internal/cmd/create"
	"github.com/project-unbounded/unbounded-kube/cmd/kubectl-unbounded/internal/cmd/setup"
	metalman "github.com/project-unbounded/unbounded-kube/cmd/metalman/commands"
)

func New(streams genericiooptions.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kubectl-unbounded",
		Short: "unbounded kubectl plugin",
	}

	cmd.AddCommand(
		setup.New(streams),
		create.New(streams),
		metalman.RebootCmd(),
		metalman.ServePXECmd(),
	)

	return cmd
}
