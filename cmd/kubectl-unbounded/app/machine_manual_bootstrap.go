// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/cloudprovider"
	"github.com/Azure/unbounded/internal/kube"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/config"
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

	// nodeIP is passed through to kubelet --node-ip.
	nodeIP string

	// localDNS enables the nspawn-local CoreDNS cache.
	localDNS bool

	// ociImage is an optional OCI image reference for the agent. When set,
	// it is included in the AgentConfig JSON so the agent uses a container
	// image to bootstrap the machine rootfs.
	ociImage string

	// sandboxImage overrides the containerd CRI sandbox image.
	sandboxImage string

	// kubernetesVersion overrides the Kubernetes version that would otherwise
	// be auto-detected from the API server. When empty the version is resolved
	// via the discovery client.
	kubernetesVersion string

	// agentVersion pins the unbounded-agent release tag to download on the
	// target host. When empty (the default) the install script tracks the
	// latest published release.
	agentVersion string

	// agentURL is a fully qualified override for the unbounded-agent download
	// URL. When set it takes precedence over agentVersion and agentBaseURL.
	agentURL string

	// agentBaseURL overrides the base URL used to construct the download URL
	// for the unbounded-agent. Useful for self-hosted release mirrors. Must
	// follow the same layout as GitHub releases
	// (<base>/latest/download/<asset> and <base>/download/<tag>/<asset>).
	agentBaseURL string

	// offlineArtifactsSource configures a complete offline source for rootfs
	// binary artifacts installed by the agent.
	offlineArtifactsSource string

	// additionalHostMounts is a list of extra host bind-mounts for the nspawn
	// machine. Each entry uses the format "source[:target][:ro]" where target
	// defaults to source when omitted and ":ro" marks the mount read-only.
	additionalHostMounts []string

	// additionalHostDevices is a list of extra host device nodes under /dev or
	// systemd device group specifiers to expose in the nspawn machine.
	additionalHostDevices []string

	// Download override flags for rootfs binaries installed by the agent.
	// See `kubectl unbounded machine register --help` for the equivalent
	// flags on the machina controller path.
	kubernetesBaseURL, kubernetesURL, kubernetesBinaryVersion string
	containerdBaseURL, containerdURL, containerdVersion       string
	runcBaseURL, runcURL, runcVersion                         string
	cniBaseURL, cniURL, cniVersion                            string
	crictlBaseURL, crictlURL, crictlVersion                   string

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

func validateOfflineArtifactsSource(source string) error {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil
	}

	if strings.HasPrefix(source, "oci://") {
		return validateOCIArtifactsSource(source)
	}

	if strings.HasPrefix(source, "https://") {
		return validateHTTPSArtifactsSource(source)
	}

	if strings.HasPrefix(source, "file://") {
		return validateFileArtifactsSource(source)
	}

	return validatePathArtifactsSource(source)
}

func validateOCIArtifactsSource(source string) error {
	u, err := url.Parse(source)
	if err != nil {
		return fmt.Errorf("parse OCI URL: %w", err)
	}

	if u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return errors.New("OCI URL must include registry and repository")
	}

	if u.Fragment != "" {
		return errors.New("OCI URL must not include a fragment")
	}

	last := u.Path[strings.LastIndex(u.Path, "/")+1:]
	if !strings.Contains(last, ":") && !strings.Contains(u.Path, "@") {
		return errors.New("OCI URL must include a tag or digest")
	}

	return nil
}

func validateHTTPSArtifactsSource(source string) error {
	u, err := url.Parse(source)
	if err != nil {
		return errors.New("parse HTTPS URL")
	}

	if u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return errors.New("HTTPS URL must include a host and archive path")
	}

	if u.User != nil || u.Fragment != "" {
		return errors.New("HTTPS URL must not include user info or a fragment")
	}

	return nil
}

func validateFileArtifactsSource(source string) error {
	u, err := url.Parse(source)
	if err != nil {
		return fmt.Errorf("parse file URL: %w", err)
	}

	if u.Host != "" && u.Host != "localhost" {
		return fmt.Errorf("file URL must not include host %q", u.Host)
	}

	if u.Path == "" || !filepath.IsAbs(u.Path) {
		return errors.New("file URL path must be absolute")
	}

	return nil
}

func validatePathArtifactsSource(source string) error {
	if strings.Contains(source, "://") {
		return fmt.Errorf("unsupported scheme in %q; supported sources are absolute paths, file:// URLs, HTTPS URLs, and oci:// URLs", source)
	}

	if !filepath.IsAbs(source) {
		return errors.New("source without a scheme must be an absolute path")
	}

	return nil
}

// parseAdditionalHostMount parses a single --additional-host-mount flag value.
// The accepted format is "source[:target][:ro]" where:
//   - source is a clean absolute host path.
//   - target is an optional clean absolute path inside the container; defaults to source.
//   - ":ro" marks the mount read-only.
func parseAdditionalHostMount(value string) (config.AdditionalHostMount, error) {
	if value == "" {
		return config.AdditionalHostMount{}, errors.New("mount spec must not be empty")
	}

	readOnly := false

	spec := value
	if strings.HasSuffix(spec, ":ro") {
		readOnly = true
		spec = strings.TrimSuffix(spec, ":ro")
	}

	var source, target string

	if i := strings.Index(spec, ":"); i >= 0 {
		source = spec[:i]
		target = spec[i+1:]
	} else {
		source = spec
	}

	mount := config.AdditionalHostMount{
		Source:   source,
		Target:   target,
		ReadOnly: readOnly,
	}

	if err := config.ValidateAdditionalHostMounts([]config.AdditionalHostMount{mount}); err != nil {
		return config.AdditionalHostMount{}, fmt.Errorf("invalid --additional-host-mount %q: %w", value, err)
	}

	return mount, nil
}

// parseAdditionalHostDevice validates a single --additional-host-device flag value.
// The accepted format is a clean absolute path under /dev (e.g. /dev/uinput)
// or a systemd device group specifier (e.g. char-input, block-*).
func parseAdditionalHostDevice(value string) (string, error) {
	if value == "" {
		return "", errors.New("device spec must not be empty")
	}

	if err := config.ValidateAdditionalHostDevices([]string{value}); err != nil {
		return "", fmt.Errorf("invalid --additional-host-device %q: %w", value, err)
	}

	return value, nil
}

func (h *manualBootstrapHandler) validate() error {
	if isEmpty(h.siteName) {
		return errors.New("site name is required")
	}

	// The machine name is optional. When omitted, the unbounded-agent resolves
	// it at startup from the AGENT_MACHINE_NAME environment variable or the host
	// hostname, which lets a single bootstrap payload be reused across many
	// instances (e.g. Azure VMSS or AWS Auto Scaling). When supplied, it must be
	// a valid Kubernetes node name because it becomes the Machine CR name.
	if !isEmpty(h.machineName) {
		if errs := validation.IsDNS1123Subdomain(h.machineName); len(errs) > 0 {
			return fmt.Errorf("invalid machine name %q: %s", h.machineName, strings.Join(errs, "; "))
		}
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

	if err := validateOfflineArtifactsSource(h.offlineArtifactsSource); err != nil {
		return fmt.Errorf("invalid --offline-artifacts-source: %w", err)
	}

	h.kubeconfigPath = getKubeconfigPath(h.kubeconfigPath)

	if !isReadableFile(h.kubeconfigPath) {
		return fmt.Errorf("kubeconfig %q is not readable", h.kubeconfigPath)
	}

	return nil
}

// buildAgentConfig resolves cluster information and assembles the provision.AgentConfig
// that the unbounded-agent expects.
func (h *manualBootstrapHandler) buildAgentConfig(ctx context.Context) (*provision.UnboundedAgentConfig, error) {
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

	// Parse node labels from CLI flags.
	labels, err := parseNodeLabels(h.nodeLabels)
	if err != nil {
		return nil, fmt.Errorf("parsing node labels: %w", err)
	}

	// Detect cloud provider for default node labels.
	var providerLabels map[string]string

	provider, err := cloudprovider.DetectProvider(ctx, h.kubeCli)
	if err != nil {
		h.logger.Warn("cloud provider detection failed, continuing without provider labels", "error", err)
	}

	if provider != nil {
		h.logger.Info("detected cloud provider", "provider", provider.ID())
		providerLabels = provider.DefaultLabels()
	}

	// Build a Machine object from CLI flags for the canonical config builder.
	machine := &unboundedv1alpha3.Machine{}
	machine.Name = h.machineName
	machine.Spec.Kubernetes = &unboundedv1alpha3.KubernetesSpec{
		NodeLabels:         labels,
		RegisterWithTaints: h.taints,
	}

	if h.ociImage != "" {
		machine.Spec.Agent = &unboundedv1alpha3.AgentSpec{Image: h.ociImage}
	}

	if h.localDNS {
		if machine.Spec.Agent == nil {
			machine.Spec.Agent = &unboundedv1alpha3.AgentSpec{}
		}

		machine.Spec.Agent.LocalDNS = &unboundedv1alpha3.LocalDNSSpec{Enabled: true}
	}

	if downloads := h.buildDownloadsSpec(); downloads != nil {
		if machine.Spec.Agent == nil {
			machine.Spec.Agent = &unboundedv1alpha3.AgentSpec{}
		}

		machine.Spec.Agent.Downloads = downloads
	}

	cfg := provision.BuildAgentConfig(provision.BuildAgentConfigParams{
		Machine: machine,
		Cluster: provision.ClusterEndpoint{
			APIServer:    apiServer,
			CACertBase64: caCertBase64,
			ClusterDNS:   clusterDNS,
			KubeVersion:  k8sVersion,
		},
		ProviderLabels: providerLabels,
		BootstrapToken: bootstrapToken,
	})

	cfg.Kubelet.NodeIP = strings.TrimSpace(h.nodeIP)
	if source := strings.TrimSpace(h.offlineArtifactsSource); source != "" {
		cfg.OfflineArtifacts = &provision.AgentOfflineArtifacts{Source: source}
	}

	cfg.CRI.Containerd.SandboxImage = strings.TrimSpace(h.sandboxImage)

	for _, raw := range h.additionalHostMounts {
		mount, err := parseAdditionalHostMount(raw)
		if err != nil {
			return nil, err
		}

		cfg.AdditionalHostMounts = append(cfg.AdditionalHostMounts, mount)
	}

	for _, raw := range h.additionalHostDevices {
		device, err := parseAdditionalHostDevice(raw)
		if err != nil {
			return nil, err
		}

		cfg.AdditionalHostDevices = append(cfg.AdditionalHostDevices, device)
	}

	return &cfg, nil
}

// manualBootstrapTemplateData holds the values injected into the
// node-bootstrap/script.sh template.
type manualBootstrapTemplateData struct {
	// MachineName is the name assigned to the node.
	MachineName string

	// MachineNameDisplay is the value shown in the rendered comment header.
	// It is the configured machine name, or "(resolved at runtime)" when the
	// name is left empty and resolved on the host by the unbounded-agent.
	MachineNameDisplay string

	// AgentConfigJSON is the indented JSON representation of the agent config.
	AgentConfigJSON string

	// InstallScript is the full install script embedded verbatim inside a
	// heredoc that is piped to bash.
	InstallScript string

	// InstallEnv is an optional list of "KEY=VALUE" strings that are exported
	// in the generated script's shell (or cloud-init runcmd) immediately
	// before the embedded install script runs. Used to forward agent
	// download overrides like AGENT_VERSION, AGENT_URL, and AGENT_BASE_URL.
	InstallEnv []string
}

// buildDownloadsSpec returns a non-nil AgentDownloadsSpec when any rootfs
// download override flag has been set; otherwise nil.
func (h *manualBootstrapHandler) buildDownloadsSpec() *unboundedv1alpha3.AgentDownloadsSpec {
	out := &unboundedv1alpha3.AgentDownloadsSpec{
		Kubernetes: downloadSourceFromFlags(h.kubernetesBaseURL, h.kubernetesURL, h.kubernetesBinaryVersion),
		Containerd: downloadSourceFromFlags(h.containerdBaseURL, h.containerdURL, h.containerdVersion),
		Runc:       downloadSourceFromFlags(h.runcBaseURL, h.runcURL, h.runcVersion),
		CNI:        downloadSourceFromFlags(h.cniBaseURL, h.cniURL, h.cniVersion),
		Crictl:     downloadSourceFromFlags(h.crictlBaseURL, h.crictlURL, h.crictlVersion),
	}

	if out.Kubernetes == nil && out.Containerd == nil && out.Runc == nil && out.CNI == nil && out.Crictl == nil {
		return nil
	}

	return out
}

// installEnv returns the KEY=VALUE pairs that should be exported before the
// embedded install script runs. Only non-empty overrides are included.
func (h *manualBootstrapHandler) installEnv() []string {
	return provision.AgentInstallEnv(&unboundedv1alpha3.AgentSpec{
		Version: h.agentVersion,
		BaseURL: h.agentBaseURL,
		URL:     h.agentURL,
	})
}

// machineNameDisplay returns the value rendered into the comment header of the
// generated payload. When the machine name is empty it indicates that the name
// is resolved on the host at runtime by the unbounded-agent.
func machineNameDisplay(name string) string {
	if strings.TrimSpace(name) == "" {
		return "(resolved at runtime)"
	}

	return name
}

// renderScript produces a self-contained bash script that writes the agent
// config JSON to a temporary file and then executes the standard install
// script. It uses the embedded node-bootstrap/script.sh template.
func (h *manualBootstrapHandler) renderScript(cfg *provision.UnboundedAgentConfig) (string, error) {
	configJSON, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshalling agent config: %w", err)
	}

	data := manualBootstrapTemplateData{
		MachineName:        cfg.MachineName,
		MachineNameDisplay: machineNameDisplay(cfg.MachineName),
		AgentConfigJSON:    string(configJSON),
		InstallScript:      provision.UnboundedAgentInstallScript(),
		InstallEnv:         h.installEnv(),
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
func (h *manualBootstrapHandler) renderCloudInit(cfg *provision.UnboundedAgentConfig) (string, error) {
	configJSON, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshalling agent config: %w", err)
	}

	data := manualBootstrapTemplateData{
		MachineName:        cfg.MachineName,
		MachineNameDisplay: machineNameDisplay(cfg.MachineName),
		AgentConfigJSON:    string(configJSON),
		InstallScript:      provision.UnboundedAgentInstallScript(),
		InstallEnv:         h.installEnv(),
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

func newMachineManualBootstrapCommand(handler *manualBootstrapHandler) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "manual-bootstrap [NAME]",
		Short: "Generate a bootstrap script or cloud-init config for provisioning a machine",
		Long: `Generate a self-contained bootstrap payload that provisions a bare-metal or VM
host as an unbounded worker node. The payload embeds the agent JSON
configuration inline and the install script for the target architecture.

NAME is optional. When omitted, the unbounded-agent resolves the machine name
on the host at startup from the AGENT_MACHINE_NAME environment variable, falling
back to the host hostname. This lets a single payload be reused across many
instances, for example an Azure VMSS or AWS Auto Scaling group, where each
instance derives its own name. The resolved name is logged by the agent and can
be inspected with 'journalctl -u unbounded-agent'.

Use --variant to choose the output format:

  script      (default) A bash script that can be piped directly to a host.
  cloud-init  A cloud-init user-data document for VM provisioning APIs.

Examples:

  # Pipe a bash script to a remote host via SSH:
  kubectl unbounded machine manual-bootstrap my-node --site my-site | ssh root@host bash

  # As a non-root user with passwordless sudo:
  kubectl unbounded machine manual-bootstrap my-node --site my-site | ssh user@host sudo bash

  # Generate cloud-init user-data for a cloud provider API:
  kubectl unbounded machine manual-bootstrap my-node --site my-site --variant cloud-init > user-data.yaml

  # Generate a reusable cloud-init payload for an autoscaling group (no NAME):
  # each instance resolves its own machine name from its hostname at boot.
  kubectl unbounded machine manual-bootstrap --site my-site --variant cloud-init > user-data.yaml

  # Pin the agent to a specific release instead of tracking "latest":
  kubectl unbounded machine manual-bootstrap my-node --site my-site --agent-version v0.0.10

  # Self-host / mirror the release assets (expects the same layout as
  # GitHub releases under the base URL):
  kubectl unbounded machine manual-bootstrap my-node --site my-site \
    --agent-base-url https://releases.example.com/unbounded`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				handler.machineName = args[0]
			}

			return handler.execute(cmd.Context())
		},
	}

	cmd.Flags().StringVar(&handler.siteName, "site", "", "Name of the site")
	cmd.Flags().StringVar(&handler.kubeconfigPath, "kubeconfig", "", "Path to kubeconfig file")
	cmd.Flags().StringArrayVar(&handler.nodeLabels, "node-label", nil, "Label in key=value format to pass to kubelet (can be repeated)")
	cmd.Flags().StringArrayVar(&handler.taints, "register-with-taint", nil, "Taint to register on the node (can be repeated)")
	cmd.Flags().StringVar(&handler.nodeIP, "node-ip", "", "IP address to pass to kubelet")
	cmd.Flags().BoolVar(&handler.localDNS, "local-dns", false, "Enable the nspawn-local CoreDNS cache")
	cmd.Flags().StringVar(&handler.ociImage, "oci-image", "", "OCI image source for the agent rootfs (registry reference, HTTPS OCI layout archive, or oci-layout:// URL)")
	cmd.Flags().StringVar(&handler.sandboxImage, "sandbox-image", "", "Containerd CRI sandbox image reference")
	cmd.Flags().StringVar(&handler.offlineArtifactsSource, "offline-artifacts-source", "", "Offline rootfs binary artifact source to embed in agent config (absolute path, file:// URL, HTTPS archive, or oci:// artifact reference)")
	cmd.Flags().StringArrayVar(&handler.additionalHostMounts, "additional-host-mount", nil, `Extra host bind-mount for the nspawn machine in "source[:target][:ro]" format (can be repeated). target defaults to source; append :ro for a read-only mount`)
	cmd.Flags().StringArrayVar(&handler.additionalHostDevices, "additional-host-device", nil, `Extra host device node or systemd device group specifier to expose in the nspawn machine (can be repeated). Accepts absolute /dev/* paths and systemd device group specifiers like char-input or block-*`)
	cmd.Flags().StringVar(&handler.kubernetesVersion, "kubernetes-version", "", "Override the Kubernetes version (default: auto-detected from API server)")
	cmd.Flags().StringVar(&handler.variant, "variant", "script", "Output format: script or cloud-init")
	cmd.Flags().StringVar(&handler.agentVersion, "agent-version", "", "Pin the unbounded-agent release tag to download on the host (default: latest GitHub release)")
	cmd.Flags().StringVar(&handler.agentURL, "agent-url", "", "Fully qualified download URL for the unbounded-agent tarball (overrides --agent-version and --agent-base-url)")
	cmd.Flags().StringVar(&handler.agentBaseURL, "agent-base-url", "", "Base URL for unbounded-agent release downloads (default: https://github.com/Azure/unbounded/releases). Use this to self-host or mirror release assets")

	// Rootfs binary download overrides. See `kubectl unbounded machine register --help`
	// for the equivalent flags on the machina controller path.
	cmd.Flags().StringVar(&handler.kubernetesBaseURL, "kubernetes-base-url", "",
		"Base URL for kubelet/kubectl/kube-proxy downloads (default: https://dl.k8s.io). Mirrors must preserve the <base>/v<ver>/bin/linux/<arch>/ layout")
	cmd.Flags().StringVar(&handler.kubernetesURL, "kubernetes-url", "",
		"Full URL template for kubernetes binary downloads (fmt placeholders: version, arch, binary)")
	cmd.Flags().StringVar(&handler.kubernetesBinaryVersion, "kubernetes-binary-version", "",
		"Override the Kubernetes binary version installed in the rootfs (defaults to the cluster Kubernetes version)")
	cmd.Flags().StringVar(&handler.containerdBaseURL, "containerd-base-url", "",
		"Base URL for containerd release downloads (default: https://github.com/containerd/containerd/releases/download)")
	cmd.Flags().StringVar(&handler.containerdURL, "containerd-url", "",
		"Full URL template for containerd downloads (fmt placeholders: version, version, arch)")
	cmd.Flags().StringVar(&handler.containerdVersion, "containerd-version", "",
		"Override the containerd version installed in the rootfs (defaults to agent's built-in version)")
	cmd.Flags().StringVar(&handler.runcBaseURL, "runc-base-url", "",
		"Base URL for runc release downloads (default: https://github.com/opencontainers/runc/releases/download)")
	cmd.Flags().StringVar(&handler.runcURL, "runc-url", "",
		"Full URL template for runc downloads (fmt placeholders: version, arch)")
	cmd.Flags().StringVar(&handler.runcVersion, "runc-version", "",
		"Override the runc version installed in the rootfs (defaults to agent's built-in version)")
	cmd.Flags().StringVar(&handler.cniBaseURL, "cni-base-url", "",
		"Base URL for CNI plugins release downloads (default: https://github.com/containernetworking/plugins/releases/download)")
	cmd.Flags().StringVar(&handler.cniURL, "cni-url", "",
		"Full URL template for CNI plugins downloads (fmt placeholders: version, arch, version)")
	cmd.Flags().StringVar(&handler.cniVersion, "cni-version", "",
		"Override the CNI plugins version installed in the rootfs (defaults to agent's built-in version)")
	cmd.Flags().StringVar(&handler.crictlBaseURL, "crictl-base-url", "",
		"Base URL for cri-tools (crictl) release downloads (default: https://github.com/kubernetes-sigs/cri-tools/releases/download)")
	cmd.Flags().StringVar(&handler.crictlURL, "crictl-url", "",
		"Full URL template for crictl downloads (fmt placeholders: version, version, os, arch)")
	cmd.Flags().StringVar(&handler.crictlVersion, "crictl-version", "",
		"Override the cri-tools/crictl version installed in the rootfs (defaults to the cluster Kubernetes minor, patch 0)")

	if err := cmd.MarkFlagRequired("site"); err != nil {
		panic(err)
	}

	return cmd
}

func machineManualBootstrapCommand() *cobra.Command {
	handler := &manualBootstrapHandler{}

	return newMachineManualBootstrapCommand(handler)
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
