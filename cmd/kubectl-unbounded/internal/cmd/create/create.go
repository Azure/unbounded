package create

import (
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/project-unbounded/unbounded-kube/cmd/kubectl-unbounded/internal/util/utilcli"
)

const (
	createExample = `
	# Create a machine on host myserver:22
	%[1]s create mymachine --host myserver --port 22

	# Create a machine with custom model
	%[1]s create mymachine --host myserver --port 22 --model mymodel

	# Print the machine YAML to stdout without creating it in the cluster
	%[1]s create mymachine --host myserver --port 22 --print-only

	# Create a machine model with default bootstrap settings
	%[1]s create machinemodel mymodel

	# Create a machine model with custom bootstrap settings
	%[1]s create machinemodel mymodel --agent-install-script 'echo hello'

	# Create a machine model with an install script loaded from a file
	%[1]s create machinemodel mymodel --agent-install-script-file ./agent-install.sh

	# Print the machine model YAML to stdout without creating it in the cluster
	%[1]s create machinemodel mymodel --agent-install-script-file ./agent-install.sh --print-only
`
)

func New(streams genericiooptions.IOStreams) *cobra.Command {
	opts := NewCreateMachineOptions()

	cmd := &cobra.Command{
		Use:          "create NAME",
		Short:        "Create machine resources",
		SilenceUsage: true,
		Example:      utilcli.Example(createExample),
		Args:         cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return opts.Run(cmd.Context(), args[0], streams)
		},
	}

	opts.AddFlags(cmd)

	cmd.AddCommand(createMachineModelCommand(streams))

	return cmd
}
