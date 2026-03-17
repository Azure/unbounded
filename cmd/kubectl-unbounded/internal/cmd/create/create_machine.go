package create

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/project-unbounded/unbounded-kube/cmd/kubectl-unbounded/internal/util/utilk8s"
	machinav1alpha2 "github.com/project-unbounded/unbounded-kube/cmd/machina/machina/api/v1alpha2"
)

type CreateMachineOptions struct {
	configFlags *genericclioptions.ConfigFlags

	Host string
	Port int32

	ModelName string

	PrintOnly bool
}

func NewCreateMachineOptions() *CreateMachineOptions {
	return &CreateMachineOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
	}
}

func (opts *CreateMachineOptions) AddFlags(cmd *cobra.Command) {
	flags := cmd.Flags()

	opts.configFlags.AddFlags(flags)

	flags.StringVar(&opts.Host, "host", "", "The hostname or IP address of the machine")
	flags.Int32Var(&opts.Port, "port", 22, "The SSH port of the machine")
	flags.StringVar(&opts.ModelName, "model", "", "The name of the MachineModel to use for this machine")
	flags.BoolVar(&opts.PrintOnly, "print-only", false, "If true, print the Machine YAML to stdout instead of creating it in the cluster")

	cmd.MarkFlagRequired("host")
}

func (opts *CreateMachineOptions) buildMachine(name string) *machinav1alpha2.Machine {
	machine := &machinav1alpha2.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: machinav1alpha2.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: machinav1alpha2.MachineSpec{
			SSH: machinav1alpha2.MachineSSHSpec{
				Host: opts.Host,
				Port: opts.Port,
			},
		},
	}

	if opts.ModelName != "" {
		machine.Spec.ModelRef = &machinav1alpha2.LocalObjectReference{
			Name: opts.ModelName,
		}
	}

	return machine
}

func (opts *CreateMachineOptions) resolveModelName(ctx context.Context, streams genericiooptions.IOStreams) error {
	if opts.ModelName != "" {
		return nil
	}

	k8sClient, err := utilk8s.NewClientFromCLIOpts(opts.configFlags)
	if err != nil {
		return fmt.Errorf("creating kubernetes client: %w", err)
	}

	var models machinav1alpha2.MachineModelList
	if err := k8sClient.List(ctx, &models, &client.ListOptions{Limit: 1}); err != nil {
		return fmt.Errorf("listing machine models: %w", err)
	}

	if len(models.Items) == 0 {
		fmt.Fprintf(streams.ErrOut, "Warning: no MachineModel found in the cluster. "+
			"Specify --model or run 'kubectl unbounded create machinemodel' first.\n")
		return nil
	}

	opts.ModelName = models.Items[0].Name
	return nil
}

// TODO: add support for watching the machine after creation
func (opts *CreateMachineOptions) Run(ctx context.Context, name string, streams genericiooptions.IOStreams) error {
	if err := opts.resolveModelName(ctx, streams); err != nil {
		return err
	}

	machine := opts.buildMachine(name)

	if opts.PrintOnly {
		data, err := yaml.Marshal(machine)
		if err != nil {
			return fmt.Errorf("marshalling machine: %w", err)
		}
		_, err = streams.Out.Write(data)
		return err
	}

	k8sClient, err := utilk8s.NewClientFromCLIOpts(opts.configFlags)
	if err != nil {
		return fmt.Errorf("creating kubernetes client: %w", err)
	}

	if err := k8sClient.Create(ctx, machine, &client.CreateOptions{}); err != nil {
		return fmt.Errorf("creating machine %q: %w", name, err)
	}

	fmt.Fprintf(streams.Out, "machine/%s created\n", name)
	return nil
}
