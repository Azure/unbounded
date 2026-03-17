package setup

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"text/template"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/project-unbounded/unbounded-kube/cmd/kubectl-unbounded/internal/util/utilcli"
	"github.com/project-unbounded/unbounded-kube/cmd/kubectl-unbounded/internal/util/utilk8s"
)

//go:embed assets/config.yaml
var configYAML string

//go:embed assets/unbounded-cni/gatewaypool.yaml
var gatewayPoolYAML string

//go:embed assets/unbounded-cni/site.yaml
var siteYAML string

//go:embed assets/unbounded-cni/sitegatewaypoolassignment.yaml
var siteGatewayPoolAssignmentYAML string

const (
	setupExample = `
	# Setup the cluster for joining unbounded machines
	%[1]s setup --node-cidr 10.1.0.0/16

	# Print the generated resources to stdout without applying
	%[1]s setup --node-cidr 10.1.0.0/16 --print-only

	# Specify a custom service subnet and pod CIDR
	%[1]s setup --node-cidr 10.1.0.0/16 --pod-cidr 10.244.0.0/16 --service-subnet 10.96.0.0/12

	# Skip SSH key generation
	%[1]s setup --node-cidr 10.1.0.0/16 --no-ssh-key

	# Use an existing SSH private key instead of generating a new one
	%[1]s setup --node-cidr 10.1.0.0/16 --ssh-private-key ~/.ssh/id_ed25519

	# Skip unbounded-cni resource generation
	%[1]s setup --no-unbounded-cni
`
)

// Params holds the values needed to render the bootstrap config template.
type Params struct {
	CertificateAuthorityData string
	Server                   string
	KubernetesVersion        string
	ServiceSubnet            string
	TokenID                  string
	TokenSecret              string
	NodeCIDRs                []string
	PodCIDRs                 []string
}

// Render renders the RBAC + ConfigMap + bootstrap token Secret YAML template
// with the given parameters.
func Render(p Params) ([]byte, error) {
	t, err := template.New("config").Parse(configYAML)
	if err != nil {
		return nil, fmt.Errorf("parsing config template: %w", err)
	}

	buf := &bytes.Buffer{}
	if err := t.Execute(buf, p); err != nil {
		return nil, fmt.Errorf("rendering config template: %w", err)
	}

	return buf.Bytes(), nil
}

// RenderCNI renders the unbounded-cni resources (GatewayPool, Site,
// SiteGatewayPoolAssignment) with the given parameters.
func RenderCNI(p Params) ([]byte, error) {
	templates := []struct {
		name    string
		content string
	}{
		{"gatewaypool", gatewayPoolYAML},
		{"site", siteYAML},
		{"sitegatewaypoolassignment", siteGatewayPoolAssignmentYAML},
	}

	buf := &bytes.Buffer{}
	for _, tmpl := range templates {
		t, err := template.New(tmpl.name).Parse(tmpl.content)
		if err != nil {
			return nil, fmt.Errorf("parsing %s template: %w", tmpl.name, err)
		}

		if err := t.Execute(buf, p); err != nil {
			return nil, fmt.Errorf("rendering %s template: %w", tmpl.name, err)
		}

		// Ensure each rendered document ends with a newline so YAML
		// document separators (---) in the next template are valid.
		if buf.Len() > 0 && buf.Bytes()[buf.Len()-1] != '\n' {
			buf.WriteByte('\n')
		}
	}

	return buf.Bytes(), nil
}

// RandomString generates a random alphanumeric string of the given length.
func RandomString(n int) (string, error) {
	const letters = "0123456789abcdefghijklmnopqrstuvwxyz"

	b := make([]byte, 0, n)
	for range n {
		r, err := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		if err != nil {
			return "", err
		}

		b = append(b, letters[r.Int64()])
	}

	return string(b), nil
}

// SetupOptions holds flags and configuration for the setup command.
type SetupOptions struct {
	configFlags *genericclioptions.ConfigFlags

	ServiceSubnet  string
	PrintOnly      bool
	NoSSHKey       bool
	SSHPrivateKey  string
	NoUnboundedCNI bool
	NodeCIDR       string
	PodCIDR        string
}

// NewSetupOptions returns a SetupOptions with defaults.
func NewSetupOptions() *SetupOptions {
	return &SetupOptions{
		configFlags: genericclioptions.NewConfigFlags(true),
	}
}

// AddFlags registers CLI flags on the given command.
func (opts *SetupOptions) AddFlags(cmd *cobra.Command) {
	opts.configFlags.AddFlags(cmd.Flags())

	// TODO: detect from cluster if possible instead of requiring user input
	cmd.Flags().StringVar(&opts.ServiceSubnet, "service-subnet", "10.0.0.0/16", "The Kubernetes service subnet CIDR")
	cmd.Flags().BoolVar(&opts.PrintOnly, "print-only", false, "If true, print the resource YAML to stdout instead of applying it")
	cmd.Flags().BoolVar(&opts.NoSSHKey, "no-ssh-key", false, "If true, skip generating the default SSH key secret")
	cmd.Flags().StringVar(&opts.SSHPrivateKey, "ssh-private-key", "", "Path to an existing SSH private key file to use instead of generating a new one")
	cmd.Flags().BoolVar(&opts.NoUnboundedCNI, "no-unbounded-cni", false, "If true, skip generating unbounded-cni resources (GatewayPool, Site, SiteGatewayPoolAssignment)")
	cmd.Flags().StringVar(&opts.NodeCIDR, "node-cidr", "", "Node CIDR for the unbounded-cni Site resource (required unless --no-unbounded-cni)")
	cmd.Flags().StringVar(&opts.PodCIDR, "pod-cidr", "100.125.0.0/16", "Pod CIDR for the unbounded-cni Site resource")
}

// Run executes the setup command.
func (opts *SetupOptions) Run(ctx context.Context, streams genericiooptions.IOStreams) error {
	if opts.NoSSHKey && opts.SSHPrivateKey != "" {
		return fmt.Errorf("--no-ssh-key and --ssh-private-key are mutually exclusive")
	}

	if !opts.NoUnboundedCNI {
		if opts.NodeCIDR == "" {
			return fmt.Errorf("--node-cidr is required (use --no-unbounded-cni to skip unbounded-cni resource generation)")
		}
	}

	params, err := opts.resolveParams(ctx, streams)
	if err != nil {
		return err
	}

	if !opts.NoUnboundedCNI {
		params.NodeCIDRs = []string{opts.NodeCIDR}
		params.PodCIDRs = []string{opts.PodCIDR}
	}

	data, err := Render(*params)
	if err != nil {
		return err
	}

	if !opts.NoUnboundedCNI {
		cniData, err := RenderCNI(*params)
		if err != nil {
			return err
		}

		data = append(data, cniData...)
	}

	if !opts.NoSSHKey {
		data, err = ensureSSHKeySecret(ctx, opts.configFlags, opts.PrintOnly, opts.SSHPrivateKey, streams, data)
		if err != nil {
			return err
		}
	}

	if opts.PrintOnly {
		_, err = streams.Out.Write(data)
		return err
	}

	k8sClient, err := utilk8s.NewClientFromCLIOpts(opts.configFlags)
	if err != nil {
		return fmt.Errorf("creating kubernetes client: %w", err)
	}

	if err := applyResources(ctx, k8sClient, data, streams.Out); err != nil {
		return err
	}

	return nil
}

const fieldManager = "kubectl-unbounded"

// applyResources decodes multi-document YAML and applies each resource to
// the cluster using server-side apply.
func applyResources(ctx context.Context, k8sClient client.Client, data []byte, out io.Writer) error {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if err == io.EOF {
				break
			}

			return fmt.Errorf("decoding resource: %w", err)
		}

		if obj.Object == nil {
			continue
		}

		applyCfg := client.ApplyConfigurationFromUnstructured(obj)
		if err := k8sClient.Apply(ctx, applyCfg, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
			return fmt.Errorf("applying %s %q: %w", obj.GetKind(), obj.GetName(), err)
		}

		fmt.Fprintf(out, "%s/%s applied\n", obj.GetKind(), obj.GetName())
	}

	return nil
}

// resolveParams resolves template parameters from the live cluster.
func (opts *SetupOptions) resolveParams(ctx context.Context, streams genericiooptions.IOStreams) (*Params, error) {
	rawConfig, err := opts.configFlags.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	currentContext := rawConfig.CurrentContext
	if opts.configFlags.Context != nil && *opts.configFlags.Context != "" {
		currentContext = *opts.configFlags.Context
	}

	ctxCfg, ok := rawConfig.Contexts[currentContext]
	if !ok {
		return nil, fmt.Errorf("context %q not found in kubeconfig", currentContext)
	}

	cluster, ok := rawConfig.Clusters[ctxCfg.Cluster]
	if !ok {
		return nil, fmt.Errorf("cluster %q not found in kubeconfig", ctxCfg.Cluster)
	}

	caData := base64.StdEncoding.EncodeToString(cluster.CertificateAuthorityData)
	server := cluster.Server

	clientset, err := utilk8s.NewClientsetFromCLIOpts(opts.configFlags)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes clientset: %w", err)
	}

	sv, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("resolving kubernetes version: %w", err)
	}

	tokenID, err := RandomString(6)
	if err != nil {
		return nil, fmt.Errorf("generating token ID: %w", err)
	}

	tokenSecret, err := RandomString(16)
	if err != nil {
		return nil, fmt.Errorf("generating token secret: %w", err)
	}

	return &Params{
		CertificateAuthorityData: caData,
		Server:                   server,
		KubernetesVersion:        sv.GitVersion,
		ServiceSubnet:            opts.ServiceSubnet,
		TokenID:                  tokenID,
		TokenSecret:              tokenSecret,
	}, nil
}

// New creates the "setup" cobra command.
func New(streams genericiooptions.IOStreams) *cobra.Command {
	opts := NewSetupOptions()

	cmd := &cobra.Command{
		Use:          "setup",
		Short:        "Setup the cluster for joining unbounded machines",
		SilenceUsage: true,
		Example:      utilcli.Example(setupExample),
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return opts.Run(cmd.Context(), streams)
		},
	}

	opts.AddFlags(cmd)

	return cmd
}
