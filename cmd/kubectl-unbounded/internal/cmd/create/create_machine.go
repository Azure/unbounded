package create

import (
	"context"
	"fmt"
	"io"
	"net"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/project-unbounded/unbounded-kube/cmd/kubectl-unbounded/internal/defaults"
	"github.com/project-unbounded/unbounded-kube/cmd/kubectl-unbounded/internal/util/utilk8s"
	unboundedv1alpha3 "github.com/project-unbounded/unbounded-kube/cmd/machina/machina/api/v1alpha3"
)

type CreateMachineOptions struct {
	configFlags *genericclioptions.ConfigFlags

	Host string
	Port int32

	// SSH options.
	SSHUsername   string
	SSHSecretName string
	SSHSecretKey  string

	// Bastion options.
	BastionHost          string
	BastionUsername      string
	BastionSSHSecretName string
	BastionSSHSecretKey  string

	// Kubernetes options.
	KubernetesVersion        string
	BootstrapTokenSecretName string
	NodeLabels               map[string]string

	// Provider annotation.
	Provider string

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
	flags.StringVar(&opts.SSHUsername, "ssh-username", "", "The SSH username for the machine")
	flags.StringVar(&opts.SSHSecretName, "ssh-secret-name", "", "The name of the secret containing the SSH private key")
	flags.StringVar(&opts.SSHSecretKey, "ssh-secret-key", "ssh-privatekey", "The key within the secret that contains the SSH private key")

	flags.StringVar(&opts.BastionHost, "bastion-host", "", "The hostname or IP address of the bastion host")
	flags.StringVar(&opts.BastionUsername, "bastion-username", "azureuser", "The SSH username for the bastion host")
	flags.StringVar(&opts.BastionSSHSecretName, "bastion-ssh-secret-name", "", "The name of the secret containing the bastion SSH private key")
	flags.StringVar(&opts.BastionSSHSecretKey, "bastion-ssh-secret-key", "ssh-privatekey", "The key within the secret that contains the bastion SSH private key")

	flags.StringVar(&opts.KubernetesVersion, "kubernetes-version", "", "The Kubernetes version to install (defaults to cluster version)")
	flags.StringVar(&opts.BootstrapTokenSecretName, "bootstrap-token-secret", "", "The name of the bootstrap token secret in kube-system")
	flags.StringToStringVar(&opts.NodeLabels, "node-labels", nil, "Labels to pass to kubelet's --node-labels (key=value pairs)")

	flags.StringVar(&opts.Provider, "provider", "", "Provider name for the machine (sets the unbounded-kube.io/provider annotation)")

	flags.BoolVar(&opts.PrintOnly, "print-only", false, "If true, print the Machine YAML to stdout instead of creating it in the cluster")

	cmd.MarkFlagRequired("host") //nolint:errcheck // Flag is defined just above; error is impossible.
}

func (opts *CreateMachineOptions) buildMachine(name string) *unboundedv1alpha3.Machine {
	// Build host:port string.
	host := opts.Host
	if opts.Port != 0 && opts.Port != 22 {
		// If opts.Host already includes a port, leave it as-is and ignore opts.Port.
		if _, _, err := net.SplitHostPort(opts.Host); err != nil {
			host = net.JoinHostPort(opts.Host, fmt.Sprintf("%d", opts.Port))
		}
	}

	machine := &unboundedv1alpha3.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: unboundedv1alpha3.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:     host,
				Username: opts.SSHUsername,
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{
					Name: opts.SSHSecretName,
					Key:  opts.SSHSecretKey,
				},
			},
		},
	}

	// Add provider annotation if set.
	if opts.Provider != "" {
		machine.Annotations = map[string]string{
			unboundedv1alpha3.AnnotationProvider: opts.Provider,
		}
	}

	// Add bastion if configured.
	if opts.BastionHost != "" {
		machine.Spec.SSH.Bastion = &unboundedv1alpha3.BastionSSHSpec{
			Host:     opts.BastionHost,
			Username: opts.BastionUsername,
		}
		if opts.BastionSSHSecretName != "" {
			machine.Spec.SSH.Bastion.PrivateKeyRef = &unboundedv1alpha3.SecretKeySelector{
				Name: opts.BastionSSHSecretName,
				Key:  opts.BastionSSHSecretKey,
			}
		}
	}

	// Add kubernetes configuration if bootstrap token is set.
	if opts.BootstrapTokenSecretName != "" {
		machine.Spec.Kubernetes = &unboundedv1alpha3.KubernetesSpec{
			Version: opts.KubernetesVersion,
			BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{
				Name: opts.BootstrapTokenSecretName,
			},
		}
		if len(opts.NodeLabels) > 0 {
			machine.Spec.Kubernetes.NodeLabels = opts.NodeLabels
		}
	}

	return machine
}

func (opts *CreateMachineOptions) resolveSSHSecret(ctx context.Context, clientset kubernetes.Interface, errOut io.Writer) error {
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
		fmt.Fprintf(errOut, "Warning: no default SSH secret found in %s (label %s=%s). "+ //nolint:errcheck // Best-effort warning to stderr.
			"Specify --ssh-secret-name or run 'kubectl unbounded setup' first.\n",
			defaults.SSHSecretNamespace, defaults.LabelKeyDefaultSSHSecret, defaults.LabelValueDefaultSSHSecret)

		return nil
	}

	opts.SSHSecretName = secrets.Items[0].Name

	return nil
}

func (opts *CreateMachineOptions) resolveBootstrapTokenSecret(ctx context.Context, clientset kubernetes.Interface, errOut io.Writer) error {
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
		fmt.Fprintf(errOut, "Warning: no default bootstrap token secret found in kube-system (label %s=%s). "+ //nolint:errcheck // Best-effort warning to stderr.
			"Specify --bootstrap-token-secret or run 'kubectl unbounded setup' first.\n",
			defaults.LabelKeyDefaultBootstrapTokenSecret, defaults.LabelValueDefaultBootstrapTokenSecret)

		return nil
	}

	opts.BootstrapTokenSecretName = secrets.Items[0].Name

	return nil
}

// TODO: add support for watching the machine after creation
func (opts *CreateMachineOptions) Run(ctx context.Context, name string, streams genericiooptions.IOStreams) error {
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

	// Resolve kubernetes version from cluster if not specified.
	if opts.KubernetesVersion == "" && opts.BootstrapTokenSecretName != "" {
		sv, err := clientset.Discovery().ServerVersion()
		if err != nil {
			return fmt.Errorf("resolving kubernetes version: %w", err)
		}

		opts.KubernetesVersion = sv.GitVersion
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

	fmt.Fprintf(streams.Out, "machine/%s created\n", name) //nolint:errcheck // Best-effort status message to stdout.

	return nil
}
