// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/kube"
)

type machineRegisterHandler struct {
	// siteName is the name of the site that contains the machine.
	siteName string

	// name is the name of the machine. If empty, it is derived from the host.
	// The site name is always prefixed: ${site}-${name}.
	name string

	// nodeLabels are key=value pairs passed to kubelet's --node-labels flag.
	// Each entry must be in the form "key=value".
	nodeLabels []string

	// host is the IP or DNS name and optionally port. If the port is omitted, it defaults to 22.
	host string

	// hostSSHUsername is the username used to connect to the machine.
	hostSSHUsername string

	// hostSSHPrivateKey is the path to the private key file used to connect to the machine.
	hostSSHPrivateKey string

	// sshSecretName is the name of the Kubernetes secret that holds SSH credentials.
	// Defaults to "ssh-${siteName}" in the unbounded-kube namespace.
	sshSecretName string

	// bastionHost is the IP or DNS name and optionally port that Machina connects to first before jumping to host.
	// If the port is omitted, it defaults to 22.
	bastionHost string

	// bastionSSHUsername is the username used to connect to the bastion host.
	bastionSSHUsername string

	// bastionSSHPrivateKey is the path to the private key file used to connect to the bastion host.
	bastionSSHPrivateKey string

	// bastionSSHSecretName is the name of the Kubernetes secret for bastion SSH credentials.
	// Defaults to sshSecretName.
	bastionSSHSecretName string

	// agentVersion pins the unbounded-agent release tag to download on the
	// host. When empty the install script tracks the latest published release.
	agentVersion string

	// agentBaseURL overrides the base URL used to construct the download
	// URL for the unbounded-agent tarball. Useful for self-hosted mirrors.
	agentBaseURL string

	// agentURL overrides the fully-qualified download URL for the
	// unbounded-agent tarball. Takes precedence over agentVersion and
	// agentBaseURL.
	agentURL string

	// Download override flags for rootfs binaries installed by the agent.
	// Each artifact exposes base-url / url / version triples mirroring
	// the agent-* flags above. Empty means upstream defaults apply.
	kubernetesBaseURL, kubernetesURL, kubernetesVersionOverride string
	containerdBaseURL, containerdURL, containerdVersion         string
	runcBaseURL, runcURL, runcVersion                           string
	cniBaseURL, cniURL, cniVersion                              string
	crictlBaseURL, crictlURL, crictlVersion                     string

	// kubeCli is the kubernetes client interface.
	kubeCli kubernetes.Interface

	kubeConfig *rest.Config

	// kubeconfigPath is the path to the kubeconfig file to use for connecting to the cluster.
	kubeconfigPath string

	// kubeResourcesCli is the controller-runtime client used for server-side apply of manifests.
	kubeResourcesCli client.Client

	// kubectl is function that creates a kubectl command pointed to the correct KUBECONFIG for the cluster.
	kubectl kube.KubectlFunc

	logger *slog.Logger
}

func (h *machineRegisterHandler) execute(ctx context.Context) error {
	if h.logger == nil {
		h.logger = slog.Default()
	}

	h.setDefaults()

	if err := h.validate(); err != nil {
		return fmt.Errorf("validating machine input: %w", err)
	}

	return h.executeAfterValidation(ctx)
}

// executeAfterValidation contains the core logic that runs after setDefaults and validate.
// It is separated so tests can pre-inject clients and skip kubeconfig validation.
func (h *machineRegisterHandler) executeAfterValidation(ctx context.Context) error {
	// Allow tests to pre-inject clients by skipping creation when already set.
	if h.kubeCli == nil {
		kubeCli, kubeConfig, err := kube.ClientAndConfigFromFile(h.kubeconfigPath)
		if err != nil {
			return fmt.Errorf("creating Kubernetes client: %w", err)
		}

		h.kubeCli = kubeCli
		h.kubeConfig = kubeConfig
		h.kubectl = kube.Kubectl(nil, h.kubeconfigPath)

		kubeResourcesCli, err := client.New(kubeConfig, client.Options{})
		if err != nil {
			return fmt.Errorf("creating controller-runtime client: %w", err)
		}

		h.kubeResourcesCli = kubeResourcesCli
	}

	ao := metav1.ApplyOptions{
		FieldManager: fieldManagerID,
	}

	// Read and apply the SSH private key secret when a key file is provided.
	if !isEmpty(h.hostSSHPrivateKey) {
		keyData, err := os.ReadFile(h.hostSSHPrivateKey)
		if err != nil {
			return fmt.Errorf("reading SSH private key %s: %w", h.hostSSHPrivateKey, err)
		}

		s := v1.Secret(h.sshSecretName, machinaNamespace).
			WithData(map[string][]byte{
				"ssh-private-key": keyData,
			})

		if err := kube.ApplySecret(ctx, h.kubeCli, s, ao); err != nil {
			return fmt.Errorf("applying SSH secret %s: %w", h.sshSecretName, err)
		}

		h.logger.Info("SSH secret applied", "name", h.sshSecretName, "namespace", machinaNamespace)
	}

	// Apply a separate bastion SSH secret when the bastion uses a different key file
	// and a different secret name than the host.
	if !isEmpty(h.bastionHost) && !isEmpty(h.bastionSSHPrivateKey) &&
		h.bastionSSHSecretName != h.sshSecretName {
		keyData, err := os.ReadFile(h.bastionSSHPrivateKey)
		if err != nil {
			return fmt.Errorf("reading bastion SSH private key %s: %w", h.bastionSSHPrivateKey, err)
		}

		s := v1.Secret(h.bastionSSHSecretName, machinaNamespace).
			WithData(map[string][]byte{
				"ssh-private-key": keyData,
			})

		if err := kube.ApplySecret(ctx, h.kubeCli, s, ao); err != nil {
			return fmt.Errorf("applying bastion SSH secret %s: %w", h.bastionSSHSecretName, err)
		}

		h.logger.Info("bastion SSH secret applied", "name", h.bastionSSHSecretName, "namespace", machinaNamespace)
	}

	// Resolve the bootstrap token for this site.
	bootstrapToken, err := kube.GetBootstrapTokenForSite(ctx, h.kubeCli, h.siteName)
	if err != nil {
		return fmt.Errorf("getting bootstrap token for site %s: %w (run 'kubectl unbounded site init' first)", h.siteName, err)
	}

	// Resolve the Kubernetes version from the cluster.
	sv, err := h.kubeCli.Discovery().ServerVersion()
	if err != nil {
		return fmt.Errorf("resolving Kubernetes version: %w", err)
	}

	// Parse node labels from the repeated --node-label flags.
	nodeLabels, err := parseNodeLabels(h.nodeLabels)
	if err != nil {
		return fmt.Errorf("parsing node labels: %w", err)
	}

	// Build the Machine resource.
	m := v1alpha3.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha3.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: h.name,
		},
		Spec: v1alpha3.MachineSpec{
			SSH: &v1alpha3.SSHSpec{
				Host:     h.host,
				Username: h.hostSSHUsername,
				PrivateKeyRef: v1alpha3.SecretKeySelector{
					Name:      h.sshSecretName,
					Namespace: machinaNamespace,
					Key:       "ssh-private-key",
				},
			},
			Kubernetes: &v1alpha3.KubernetesSpec{
				Version:    sv.GitVersion,
				NodeLabels: nodeLabels,
				BootstrapTokenRef: v1alpha3.LocalObjectReference{
					Name: fmt.Sprintf("bootstrap-token-%s", bootstrapToken.ID),
				},
			},
			Agent: h.buildAgentSpec(),
		},
	}

	// Add bastion configuration if a bastion host is specified.
	if !isEmpty(h.bastionHost) {
		m.Spec.SSH.Bastion = &v1alpha3.BastionSSHSpec{
			Host:     h.bastionHost,
			Username: h.bastionSSHUsername,
		}

		// Only set the bastion privateKeyRef when it differs from the parent
		// SSH key ref, so the controller falls back to the parent key otherwise.
		if h.bastionSSHSecretName != h.sshSecretName {
			m.Spec.SSH.Bastion.PrivateKeyRef = &v1alpha3.SecretKeySelector{
				Name:      h.bastionSSHSecretName,
				Namespace: machinaNamespace,
				Key:       "ssh-private-key",
			}
		}
	}

	// Marshal to YAML and apply via server-side apply.
	data, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshalling machine %s: %w", h.name, err)
	}

	if err := kube.ApplyManifests(ctx, h.logger, h.kubeResourcesCli, fieldManagerID, data); err != nil {
		return fmt.Errorf("applying machine %s: %w", h.name, err)
	}

	h.logger.Info("machine applied", "name", h.name)

	return nil
}

// buildAgentSpec returns an AgentSpec when any agent-related override has
// been set on the handler, otherwise nil so Spec.Agent stays empty.
func (h *machineRegisterHandler) buildAgentSpec() *v1alpha3.AgentSpec {
	agent := &v1alpha3.AgentSpec{
		Version: h.agentVersion,
		BaseURL: h.agentBaseURL,
		URL:     h.agentURL,
	}

	downloads := h.buildDownloadsSpec()
	if downloads != nil {
		agent.Downloads = downloads
	}

	if agent.Version == "" && agent.BaseURL == "" && agent.URL == "" && agent.Image == "" && agent.Downloads == nil {
		return nil
	}

	return agent
}

// buildDownloadsSpec returns a non-nil AgentDownloadsSpec when any rootfs
// download override flag has been set.
func (h *machineRegisterHandler) buildDownloadsSpec() *v1alpha3.AgentDownloadsSpec {
	out := &v1alpha3.AgentDownloadsSpec{
		Kubernetes: downloadSourceFromFlags(h.kubernetesBaseURL, h.kubernetesURL, h.kubernetesVersionOverride),
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

// downloadSourceFromFlags builds a DownloadSource from the given flag
// values. Returns nil when all values are empty.
func downloadSourceFromFlags(baseURL, url, version string) *v1alpha3.DownloadSource {
	if baseURL == "" && url == "" && version == "" {
		return nil
	}

	return &v1alpha3.DownloadSource{
		BaseURL: baseURL,
		URL:     url,
		Version: version,
	}
}

func (h *machineRegisterHandler) setDefaults() {
	h.kubeconfigPath = getKubeconfigPath(h.kubeconfigPath)

	// Default the SSH secret name to "ssh-${site}".
	if isEmpty(h.sshSecretName) {
		h.sshSecretName = fmt.Sprintf("ssh-%s", h.siteName)
	}

	// Derive the machine name from the host when not explicitly provided.
	if isEmpty(h.name) {
		// Strip port from host if present, so "10.0.0.5:2222" becomes "10.0.0.5-2222"
		// after sanitization. We use the raw host string for sanitization so the colon
		// gets replaced with a dash.
		h.name = fmt.Sprintf("%s-%s", h.siteName, kube.SanitizeK8sName(h.host))
	} else {
		h.name = fmt.Sprintf("%s-%s", h.siteName, h.name)
	}

	// Apply bastion defaults only when a bastion host is configured.
	if !isEmpty(h.bastionHost) {
		if isEmpty(h.bastionSSHUsername) {
			h.bastionSSHUsername = h.hostSSHUsername
		}

		if isEmpty(h.bastionSSHPrivateKey) {
			h.bastionSSHPrivateKey = h.hostSSHPrivateKey
		}

		if isEmpty(h.bastionSSHSecretName) {
			h.bastionSSHSecretName = h.sshSecretName
		}
	}
}

func (h *machineRegisterHandler) validate() error {
	if isEmpty(h.siteName) {
		return errors.New("site name is required")
	}

	if isEmpty(h.host) {
		return errors.New("host is required")
	}

	// Validate host format: must be a valid IP or host, optionally with port.
	if host, _, err := net.SplitHostPort(h.host); err == nil {
		// Had a port — check the host part is non-empty.
		if isEmpty(host) {
			return errors.New("host address is empty in host:port value")
		}
	}

	if isEmpty(h.hostSSHUsername) {
		return errors.New("ssh username is required")
	}

	// SSH private key is required unless bastion flags are set.
	hasBastionFlags := !isEmpty(h.bastionHost)
	if !hasBastionFlags && isEmpty(h.hostSSHPrivateKey) {
		return errors.New("--ssh-private-key is required when no bastion flags are set")
	}

	// Validate SSH key files are readable when provided.
	if !isEmpty(h.hostSSHPrivateKey) && !isReadableFile(h.hostSSHPrivateKey) {
		return fmt.Errorf("SSH private key file %q is not readable", h.hostSSHPrivateKey)
	}

	if !isEmpty(h.bastionSSHPrivateKey) && !isReadableFile(h.bastionSSHPrivateKey) {
		return fmt.Errorf("bastion SSH private key file %q is not readable", h.bastionSSHPrivateKey)
	}

	if !isReadableFile(h.kubeconfigPath) {
		return fmt.Errorf("kubeconfig %q is not readable", h.kubeconfigPath)
	}

	if _, err := parseNodeLabels(h.nodeLabels); err != nil {
		return fmt.Errorf("invalid --node-label: %w", err)
	}

	return nil
}

// parseNodeLabels converts a slice of "key=value" strings into a map.
// It returns an error if any entry is missing an "=" separator or if
// duplicate keys are detected.
func parseNodeLabels(entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	labels := make(map[string]string, len(entries))

	for _, entry := range entries {
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("%q is not a valid key=value pair", entry)
		}

		if _, exists := labels[k]; exists {
			return nil, fmt.Errorf("duplicate label key %q", k)
		}

		labels[k] = v
	}

	return labels, nil
}

func machineRegisterCommand() *cobra.Command {
	handler := machineRegisterHandler{}

	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register a machine to the site",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return handler.execute(cmd.Context())
		},
	}

	cmd.Flags().StringVar(&handler.siteName, "site", "", "Name of the site")
	cmd.Flags().StringVar(&handler.name, "name", "", "Name of the machine (optional; derived from --host if omitted)")
	cmd.Flags().StringVar(&handler.host, "host", "", "Host and optionally port of the machine (e.g. 10.0.0.5 or 10.0.0.5:2222)")
	cmd.Flags().StringVar(&handler.hostSSHUsername, "ssh-username", "", "SSH username for connecting to the machine")
	cmd.Flags().StringVar(&handler.hostSSHPrivateKey, "ssh-private-key", "", "Path to SSH private key file (required if no bastion flags are set)")
	cmd.Flags().StringVar(&handler.sshSecretName, "ssh-secret-name", "", "Name of the Kubernetes secret for SSH credentials (defaults to ssh-$site)")
	cmd.Flags().StringVar(&handler.bastionHost, "bastion-host", "", "Host and optionally port of the bastion (e.g. 5.6.7.8 or 5.6.7.8:2222)")
	cmd.Flags().StringVar(&handler.bastionSSHSecretName, "bastion-ssh-secret-name", "", "Name of the Kubernetes secret for bastion SSH credentials (defaults to --ssh-secret-name)")
	cmd.Flags().StringVar(&handler.bastionSSHUsername, "bastion-ssh-username", "", "SSH username for the bastion (defaults to --ssh-username)")
	cmd.Flags().StringVar(&handler.bastionSSHPrivateKey, "bastion-ssh-private-key", "", "Path to SSH private key file for the bastion (defaults to --ssh-private-key)")
	cmd.Flags().StringVar(&handler.kubeconfigPath, "kubeconfig", "", "Path to kubeconfig file")
	cmd.Flags().StringArrayVar(&handler.nodeLabels, "node-label", nil, "Label in key=value format to pass to kubelet (can be repeated)")

	// Agent binary download overrides. See `kubectl unbounded machine manual-bootstrap --help`
	// for the equivalent flags on the manual bootstrap path.
	cmd.Flags().StringVar(&handler.agentVersion, "agent-version", "",
		"Pin the unbounded-agent release tag to download on the host (default: latest GitHub release)")
	cmd.Flags().StringVar(&handler.agentBaseURL, "agent-base-url", "",
		"Base URL for unbounded-agent release downloads (default: https://github.com/Azure/unbounded/releases). Use this to self-host or mirror release assets")
	cmd.Flags().StringVar(&handler.agentURL, "agent-url", "",
		"Fully qualified download URL for the unbounded-agent tarball (overrides --agent-version and --agent-base-url)")

	// Rootfs binary download overrides.
	cmd.Flags().StringVar(&handler.kubernetesBaseURL, "kubernetes-base-url", "",
		"Base URL for kubelet/kubectl/kube-proxy downloads (default: https://dl.k8s.io). Mirrors must preserve the <base>/v<ver>/bin/linux/<arch>/ layout")
	cmd.Flags().StringVar(&handler.kubernetesURL, "kubernetes-url", "",
		"Full URL template for kubernetes binary downloads (fmt placeholders: version, arch, binary)")
	cmd.Flags().StringVar(&handler.kubernetesVersionOverride, "kubernetes-binary-version", "",
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

	if err := cmd.MarkFlagRequired("host"); err != nil {
		panic(err)
	}

	if err := cmd.MarkFlagRequired("ssh-username"); err != nil {
		panic(err)
	}

	return cmd
}
