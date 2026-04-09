package app

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/project-unbounded/unbounded-kube/internal/cloudprovider"
	"github.com/project-unbounded/unbounded-kube/internal/kube"
	"github.com/project-unbounded/unbounded-kube/internal/provision"
)

//go:embed assets/node-bootstrap/script.sh
var manualBootstrapTemplate string

//go:embed assets/node-bootstrap/cloud-init.yaml
var manualBootstrapCloudInitTemplate string

// bootstrapVariant controls the output format of the manual-bootstrap command.
type bootstrapVariant string

const (
	// variantScript produces a self-contained bash script (default).
	variantScript bootstrapVariant = "script"

	// variantCloudInit produces a cloud-init user-data document.
	variantCloudInit bootstrapVariant = "cloud-init"
)

func parseBootstrapVariant(s string) (bootstrapVariant, error) {
	switch bootstrapVariant(s) {
	case variantScript:
		return variantScript, nil
	case variantCloudInit:
		return variantCloudInit, nil
	default:
		return "", fmt.Errorf("unknown variant %q (valid: script, cloud-init)", s)
	}
}

// manualBootstrapHandler generates a self-contained bootstrap script that can
// be executed on a bare-metal or VM host to join it to the cluster as a worker
// node. The script embeds the agent JSON config inline so no additional files
// need to be transferred to the target machine.
type manualBootstrapHandler struct {
	// siteName is the name of the site whose bootstrap token will be used.
	siteName string

	// machineName is the name to assign to the node.
	machineName string

	// nodeLabels are key=value pairs passed through to kubelet --node-labels.
	nodeLabels []string

	// taints are taint strings passed through to kubelet --register-with-taints.
	taints []string

	// ociImage is an optional OCI image reference for the agent. When set,
	// it is included in the AgentConfig JSON so the agent uses a container
	// image to bootstrap the machine rootfs instead of debootstrap.
	ociImage string

	// kubernetesVersion overrides the Kubernetes version that would otherwise
	// be auto-detected from the API server. When empty the version is resolved
	// via the discovery client.
	kubernetesVersion string

	// variant controls the output format. Defaults to "script".
	variant string

	// kubeconfigPath is the path to the kubeconfig used to contact the cluster.
	kubeconfigPath string

	// out is the writer where the rendered script is emitted.
	// Defaults to os.Stdout.
	out io.Writer

	// kubeCli is the kubernetes client interface. Populated during execute.
	kubeCli kubernetes.Interface

	// kubeConfig is the REST config derived from the kubeconfig file.
	kubeConfig *rest.Config

	logger *slog.Logger
}

func (h *manualBootstrapHandler) execute(ctx context.Context) error {
	if h.logger == nil {
		h.logger = slog.Default()
	}

	if h.out == nil {
		h.out = os.Stdout
	}

	if err := h.validate(); err != nil {
		return fmt.Errorf("validating manual-bootstrap input: %w", err)
	}

	// Build kubernetes clients unless pre-injected (tests).
	if h.kubeCli == nil {
		kubeCli, kubeConfig, err := kube.ClientAndConfigFromFile(h.kubeconfigPath)
		if err != nil {
			return fmt.Errorf("creating Kubernetes client: %w", err)
		}

		h.kubeCli = kubeCli
		h.kubeConfig = kubeConfig
	}

	cfg, err := h.buildAgentConfig(ctx)
	if err != nil {
		return fmt.Errorf("building agent config: %w", err)
	}

	var output string

	switch bootstrapVariant(h.variant) {
	case variantCloudInit:
		output, err = h.renderCloudInit(cfg)
	default:
		output, err = h.renderScript(cfg)
	}

	if err != nil {
		return fmt.Errorf("rendering bootstrap output: %w", err)
	}

	_, err = fmt.Fprint(h.out, output)

	return err
}

func (h *manualBootstrapHandler) validate() error {
	if isEmpty(h.siteName) {
		return errors.New("site name is required")
	}

	if isEmpty(h.machineName) {
		return errors.New("machine name is required")
	}

	if _, err := parseNodeLabels(h.nodeLabels); err != nil {
		return fmt.Errorf("invalid --node-label: %w", err)
	}

	if h.variant == "" {
		h.variant = string(variantScript)
	}

	if _, err := parseBootstrapVariant(h.variant); err != nil {
		return err
	}

	h.kubeconfigPath = getKubeconfigPath(h.kubeconfigPath)

	if !isReadableFile(h.kubeconfigPath) {
		return fmt.Errorf("kubeconfig %q is not readable", h.kubeconfigPath)
	}

	return nil
}

// buildAgentConfig resolves cluster information and assembles the provision.AgentConfig
// that the unbounded-agent expects.
func (h *manualBootstrapHandler) buildAgentConfig(ctx context.Context) (*provision.AgentConfig, error) {
	tok, err := resolveBootstrapToken(ctx, h.logger, h.kubeCli, h.siteName)
	if err != nil {
		return nil, err
	}

	bootstrapToken := fmt.Sprintf("%s.%s", tok.ID, tok.Secret)

	// Resolve the API server endpoint.
	apiServer := h.kubeConfig.Host

	// Resolve CA certificate from kube-root-ca.crt ConfigMap.
	cm, err := h.kubeCli.CoreV1().ConfigMaps(metav1.NamespacePublic).Get(ctx, "kube-root-ca.crt", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get kube-root-ca.crt ConfigMap: %w", err)
	}

	caCert, ok := cm.Data["ca.crt"]
	if !ok {
		return nil, fmt.Errorf("ca.crt key not found in kube-root-ca.crt ConfigMap")
	}

	caCertBase64 := base64.StdEncoding.EncodeToString([]byte(caCert))

	// Resolve Kubernetes version from flag override or API server.
	k8sVersion := h.kubernetesVersion
	if k8sVersion == "" {
		sv, err := h.kubeCli.Discovery().ServerVersion()
		if err != nil {
			return nil, fmt.Errorf("resolving Kubernetes version: %w", err)
		}

		k8sVersion = sv.GitVersion
	}

	// Resolve cluster DNS from kube-dns Service.
	dnsSvc, err := h.kubeCli.CoreV1().Services(metav1.NamespaceSystem).Get(ctx, "kube-dns", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get kube-dns Service: %w", err)
	}

	clusterDNS := dnsSvc.Spec.ClusterIP
	if clusterDNS == "" {
		return nil, fmt.Errorf("kube-dns Service has no ClusterIP")
	}

	// Parse node labels.
	labels, err := parseNodeLabels(h.nodeLabels)
	if err != nil {
		return nil, fmt.Errorf("parsing node labels: %w", err)
	}

	// Detect cloud provider and merge its default labels. Provider labels
	// override user-supplied labels, matching the machina controller behaviour.
	provider, err := cloudprovider.DetectProvider(ctx, h.kubeCli)
	if err != nil {
		h.logger.Warn("cloud provider detection failed, continuing without provider labels", "error", err)
	}

	if provider != nil {
		h.logger.Info("detected cloud provider", "provider", provider.ID())

		if labels == nil {
			labels = make(map[string]string)
		}

		maps.Copy(labels, provider.DefaultLabels())
	}

	// Common labels are applied unconditionally to every node provisioned
	// by unbounded, regardless of the detected cloud provider.
	if labels == nil {
		labels = make(map[string]string)
	}

	maps.Copy(labels, cloudprovider.CommonDefaultLabels())

	return &provision.AgentConfig{
		MachineName: h.machineName,
		Cluster: provision.AgentClusterConfig{
			CaCertBase64: caCertBase64,
			ClusterDNS:   clusterDNS,
			Version:      k8sVersion,
		},
		Kubelet: provision.AgentKubeletConfig{
			ApiServer:          apiServer,
			BootstrapToken:     bootstrapToken,
			Labels:             labels,
			RegisterWithTaints: h.taints,
		},
		OCIImage: h.ociImage,
	}, nil
}

// manualBootstrapTemplateData holds the values injected into the
// node-bootstrap/script.sh template.
type manualBootstrapTemplateData struct {
	// MachineName is the name assigned to the node.
	MachineName string

	// AgentConfigJSON is the indented JSON representation of the agent config.
	AgentConfigJSON string

	// InstallScript is the full install script embedded verbatim inside a
	// heredoc that is piped to bash.
	InstallScript string
}

// renderScript produces a self-contained bash script that writes the agent
// config JSON to a temporary file and then executes the standard install
// script. It uses the embedded node-bootstrap/script.sh template.
func (h *manualBootstrapHandler) renderScript(cfg *provision.AgentConfig) (string, error) {
	configJSON, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshalling agent config: %w", err)
	}

	data := manualBootstrapTemplateData{
		MachineName:     cfg.MachineName,
		AgentConfigJSON: string(configJSON),
		InstallScript:   provision.UnboundedAgentInstallScript(),
	}

	t, err := template.New("node-bootstrap").Parse(manualBootstrapTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing node-bootstrap template: %w", err)
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("rendering node-bootstrap template: %w", err)
	}

	return buf.String(), nil
}

// renderCloudInit produces a cloud-init user-data document that writes the
// agent config JSON file and runs the install script on first boot via runcmd.
func (h *manualBootstrapHandler) renderCloudInit(cfg *provision.AgentConfig) (string, error) {
	configJSON, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshalling agent config: %w", err)
	}

	data := manualBootstrapTemplateData{
		MachineName:     cfg.MachineName,
		AgentConfigJSON: string(configJSON),
		InstallScript:   provision.UnboundedAgentInstallScript(),
	}

	funcMap := template.FuncMap{
		"indent": func(n int, s string) string {
			pad := strings.Repeat(" ", n)
			lines := strings.Split(s, "\n")

			for i, line := range lines {
				if line != "" {
					lines[i] = pad + line
				}
			}

			return strings.Join(lines, "\n")
		},
	}

	t, err := template.New("cloud-init").Funcs(funcMap).Parse(manualBootstrapCloudInitTemplate)
	if err != nil {
		return "", fmt.Errorf("parsing cloud-init template: %w", err)
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("rendering cloud-init template: %w", err)
	}

	return buf.String(), nil
}

func machineManualBootstrapCommand() *cobra.Command {
	handler := manualBootstrapHandler{}

	cmd := &cobra.Command{
		Use:   "manual-bootstrap NAME",
		Short: "Generate a bootstrap script or cloud-init config for provisioning a machine",
		Long: `Generate a self-contained bootstrap payload that provisions a bare-metal or VM
host as an unbounded-kube worker node. The payload embeds the agent JSON
configuration inline and the install script for the target architecture.

Use --variant to choose the output format:

  script      (default) A bash script that can be piped directly to a host.
  cloud-init  A cloud-init user-data document for VM provisioning APIs.

Examples:

  # Pipe a bash script to a remote host via SSH:
  kubectl unbounded machine manual-bootstrap my-node --site my-site | ssh root@host bash

  # As a non-root user with passwordless sudo:
  kubectl unbounded machine manual-bootstrap my-node --site my-site | ssh user@host sudo bash

  # Generate cloud-init user-data for a cloud provider API:
  kubectl unbounded machine manual-bootstrap my-node --site my-site --variant cloud-init > user-data.yaml`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			handler.machineName = args[0]
			return handler.execute(cmd.Context())
		},
	}

	cmd.Flags().StringVar(&handler.siteName, "site", "", "Name of the site")
	cmd.Flags().StringVar(&handler.kubeconfigPath, "kubeconfig", "", "Path to kubeconfig file")
	cmd.Flags().StringArrayVar(&handler.nodeLabels, "node-label", nil, "Label in key=value format to pass to kubelet (can be repeated)")
	cmd.Flags().StringArrayVar(&handler.taints, "register-with-taint", nil, "Taint to register on the node (can be repeated)")
	cmd.Flags().StringVar(&handler.ociImage, "oci-image", "", "OCI image reference for the agent rootfs")
	cmd.Flags().StringVar(&handler.kubernetesVersion, "kubernetes-version", "", "Override the Kubernetes version (default: auto-detected from API server)")
	cmd.Flags().StringVar(&handler.variant, "variant", "script", "Output format: script or cloud-init")

	if err := cmd.MarkFlagRequired("site"); err != nil {
		panic(err)
	}

	return cmd
}

// resolveBootstrapToken tries to find a bootstrap token for the given site.
// It first looks for a site-scoped token. If that fails, it logs a warning
// and falls back to the first valid bootstrap token secret in kube-system.
func resolveBootstrapToken(ctx context.Context, logger *slog.Logger, kubeCli kubernetes.Interface, siteName string) (*kube.BootstrapToken, error) {
	tok, err := kube.GetBootstrapTokenForSite(ctx, kubeCli, siteName)
	if err == nil {
		return tok, nil
	}

	logger.Warn("site-scoped bootstrap token not found, falling back to first available token", "site", siteName, "error", err)

	l, err := kubeCli.CoreV1().Secrets(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing secrets in kube-system: %w", err)
	}

	for i := range l.Items {
		secret := &l.Items[i]

		if secret.Type != "bootstrap.kubernetes.io/token" {
			continue
		}

		tokenID, ok := secret.Data["token-id"]
		if !ok {
			continue
		}

		tokenSecret, ok := secret.Data["token-secret"]
		if !ok {
			continue
		}

		return &kube.BootstrapToken{
			ID:     string(tokenID),
			Secret: string(tokenSecret),
			Labels: secret.Labels,
		}, nil
	}

	return nil, fmt.Errorf("no bootstrap token found for site %q and no tokens available in the cluster (run 'kubectl unbounded site init' first)", siteName)
}
