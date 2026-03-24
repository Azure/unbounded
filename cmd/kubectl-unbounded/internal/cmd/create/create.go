package create

import (
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/project-unbounded/unbounded-kube/cmd/kubectl-unbounded/internal/util/utilcli"
)

const (
	createExample = `
	# Create a machine on host myserver (SSH port 22)
	%[1]s create mymachine --host myserver

	# Create a machine with a custom SSH port
	%[1]s create mymachine --host myserver --port 2222

	# Create a machine with SSH credentials and bootstrap config
	%[1]s create mymachine --host myserver --ssh-username azureuser --ssh-secret-name my-ssh-key --bootstrap-token-secret my-token

	# Create a machine with a bastion host
	%[1]s create mymachine --host myserver --bastion-host bastion.example.com

	# Create a machine with node labels
	%[1]s create mymachine --host myserver --bootstrap-token-secret my-token --node-labels "env=prod,region=us-east"

	# Print the machine YAML to stdout without creating it in the cluster
	%[1]s create mymachine --host myserver --print-only
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

	return cmd
}
