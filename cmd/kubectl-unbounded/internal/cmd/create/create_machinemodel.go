package create

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/project-unbounded/unbounded-kube/cmd/agent"
	"github.com/project-unbounded/unbounded-kube/cmd/kubectl-unbounded/internal/defaults"
	"github.com/project-unbounded/unbounded-kube/cmd/kubectl-unbounded/internal/util/utilk8s"
	machinav1alpha2 "github.com/project-unbounded/unbounded-kube/cmd/machina/machina/api/v1alpha2"
)

type CreateMachineModelOptions struct {
	configFlags *genericclioptions.ConfigFlags

	SSHUsername            string
	SSHSecretName          string
	SSHSecretKey           string
	AgentInstallScript     string
	AgentInstallScriptFile string

	JumpboxHost          string
	JumpboxPort          int32
	JumpboxSSHUsername   string
	JumpboxSSHSecretName string
	JumpboxSSHSecretKey  string

	BootstrapTokenSecretName string

	PrintOnly bool
}

func NewCreateMachineModelOptions() *CreateMachineModelOptions {
	return &CreateMachineModelOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
	}
}

func (opts *CreateMachineModelOptions) AddFlags(cmd *cobra.Command) {
	flags := cmd.Flags()

	opts.configFlags.AddFlags(flags)

	flags.StringVar(&opts.SSHUsername, "ssh-username", "", "The SSH username for machines using this model")
	flags.StringVar(&opts.SSHSecretName, "ssh-secret-name", "", "The name of the secret containing the SSH private key")
	flags.StringVar(&opts.SSHSecretKey, "ssh-secret-key", "ssh-privatekey", "The key within the secret that contains the SSH private key")
	flags.StringVar(&opts.AgentInstallScript, "agent-install-script", "", "Content of the install script to run on the target machine to install the agent")
	flags.StringVar(&opts.AgentInstallScriptFile, "agent-install-script-file", "", "Path to a file containing the install script to run on the target machine to install the agent")

	flags.StringVar(&opts.JumpboxHost, "jumpbox-host", "", "The hostname or IP address of the jumpbox")
	flags.Int32Var(&opts.JumpboxPort, "jumpbox-port", 22, "The SSH port of the jumpbox")
	flags.StringVar(&opts.JumpboxSSHUsername, "jumpbox-ssh-username", "azureuser", "The SSH username for the jumpbox")
	flags.StringVar(&opts.JumpboxSSHSecretName, "jumpbox-ssh-secret-name", "", "The name of the secret containing the jumpbox SSH private key")
	flags.StringVar(&opts.JumpboxSSHSecretKey, "jumpbox-ssh-secret-key", "ssh-privatekey", "The key within the secret that contains the jumpbox SSH private key")

	flags.StringVar(&opts.BootstrapTokenSecretName, "bootstrap-token-secret", "", "The name of the bootstrap token secret in kube-system")

	flags.BoolVar(&opts.PrintOnly, "print-only", false, "If true, print the MachineModel YAML to stdout instead of creating it in the cluster")
	cmd.MarkFlagsMutuallyExclusive("agent-install-script", "agent-install-script-file")
}

func (opts *CreateMachineModelOptions) buildMachineModel(name string, kubeVersion string) *machinav1alpha2.MachineModel {
	model := &machinav1alpha2.MachineModel{
		TypeMeta: metav1.TypeMeta{
			APIVersion: machinav1alpha2.GroupVersion.String(),
			Kind:       "MachineModel",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername: opts.SSHUsername,
			SSHPrivateKeyRef: machinav1alpha2.SecretKeySelector{
				Name: opts.SSHSecretName,
				Key:  opts.SSHSecretKey,
			},
			AgentInstallScript: opts.AgentInstallScript,
			KubernetesProfile: &machinav1alpha2.KubernetesProfile{
				Version: kubeVersion,
				BootstrapTokenRef: machinav1alpha2.LocalObjectReference{
					Name: opts.BootstrapTokenSecretName,
				},
			},
		},
	}

	if opts.JumpboxHost != "" {
		model.Spec.Jumpbox = &machinav1alpha2.JumpboxConfig{
			Host:        opts.JumpboxHost,
			Port:        opts.JumpboxPort,
			SSHUsername: opts.JumpboxSSHUsername,
		}
		if opts.JumpboxSSHSecretName != "" {
			model.Spec.Jumpbox.SSHPrivateKeyRef = &machinav1alpha2.SecretKeySelector{
				Name: opts.JumpboxSSHSecretName,
				Key:  opts.JumpboxSSHSecretKey,
			}
		}
	}

	return model
}

func (opts *CreateMachineModelOptions) resolveAgentInstallScript() error {
	switch {
	case opts.AgentInstallScript != "":
		return nil
	case opts.AgentInstallScriptFile != "":
		data, err := os.ReadFile(opts.AgentInstallScriptFile)
		if err != nil {
			return fmt.Errorf("reading agent install script file: %w", err)
		}
		opts.AgentInstallScript = string(data)
	default:
		opts.AgentInstallScript = agent.AKSFlexInstallScript()
	}
	return nil
}

func (opts *CreateMachineModelOptions) resolveSSHSecret(ctx context.Context, clientset kubernetes.Interface, errOut io.Writer) error {
	if opts.SSHSecretName != "" {
		return nil
	}

	secrets, err := clientset.CoreV1().Secrets(defaults.SSHSecretNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: defaults.LabelKeyDefaultSSHSecret + "=" + defaults.LabelValueDefaultSSHSecret,
		Limit:         1,
	})
	if err != nil {
		return fmt.Errorf("listing default SSH secrets: %w", err)
	}

	if len(secrets.Items) == 0 {
		fmt.Fprintf(errOut, "Warning: no default SSH secret found in %s (label %s=%s). "+
			"Specify --ssh-secret-name or run 'kubectl unbounded setup' first.\n",
			defaults.SSHSecretNamespace, defaults.LabelKeyDefaultSSHSecret, defaults.LabelValueDefaultSSHSecret)
		return nil
	}

	opts.SSHSecretName = secrets.Items[0].Name
	return nil
}

func (opts *CreateMachineModelOptions) resolveBootstrapTokenSecret(ctx context.Context, clientset kubernetes.Interface, errOut io.Writer) error {
	if opts.BootstrapTokenSecretName != "" {
		return nil
	}

	secrets, err := clientset.CoreV1().Secrets("kube-system").List(ctx, metav1.ListOptions{
		LabelSelector: defaults.LabelKeyDefaultBootstrapTokenSecret + "=" + defaults.LabelValueDefaultBootstrapTokenSecret,
		Limit:         1,
	})
	if err != nil {
		return fmt.Errorf("listing default bootstrap token secrets: %w", err)
	}

	if len(secrets.Items) == 0 {
		fmt.Fprintf(errOut, "Warning: no default bootstrap token secret found in kube-system (label %s=%s). "+
			"Specify --bootstrap-token-secret or run 'kubectl unbounded setup' first.\n",
			defaults.LabelKeyDefaultBootstrapTokenSecret, defaults.LabelValueDefaultBootstrapTokenSecret)
		return nil
	}

	opts.BootstrapTokenSecretName = secrets.Items[0].Name
	return nil
}

func (opts *CreateMachineModelOptions) Run(ctx context.Context, name string, streams genericiooptions.IOStreams) error {
	if err := opts.resolveAgentInstallScript(); err != nil {
		return err
	}

	clientset, err := utilk8s.NewClientsetFromCLIOpts(opts.configFlags)
	if err != nil {
		return fmt.Errorf("creating kubernetes clientset: %w", err)
	}

	if err := opts.resolveSSHSecret(ctx, clientset, streams.ErrOut); err != nil {
		return err
	}

	if err := opts.resolveBootstrapTokenSecret(ctx, clientset, streams.ErrOut); err != nil {
		return err
	}

	sv, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return fmt.Errorf("resolving kubernetes version: %w", err)
	}

	model := opts.buildMachineModel(name, sv.GitVersion)

	if opts.PrintOnly {
		data, err := yaml.Marshal(model)
		if err != nil {
			return fmt.Errorf("marshalling machine model: %w", err)
		}
		_, err = streams.Out.Write(data)
		return err
	}

	k8sClient, err := utilk8s.NewClientFromCLIOpts(opts.configFlags)
	if err != nil {
		return fmt.Errorf("creating kubernetes client: %w", err)
	}

	if err := k8sClient.Create(ctx, model, &client.CreateOptions{}); err != nil {
		return fmt.Errorf("creating machinemodel %q: %w", name, err)
	}

	fmt.Fprintf(streams.Out, "machinemodel/%s created\n", name)
	return nil
}

func createMachineModelCommand(streams genericiooptions.IOStreams) *cobra.Command {
	opts := NewCreateMachineModelOptions()

	cmd := &cobra.Command{
		Use:          "machinemodel NAME",
		Short:        "Create a MachineModel resource",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return opts.Run(cmd.Context(), args[0], streams)
		},
	}

	opts.AddFlags(cmd)

	return cmd
}
