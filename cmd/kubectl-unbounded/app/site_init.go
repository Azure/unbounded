// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"text/template"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/kube"
)

//go:embed assets/unbounded-net-site/*.yaml
var siteTemplates embed.FS

// siteInitHandler creates declarative Site configuration and bootstrap credentials.
// Component installation is reconciled by unbounded-operator from the Site spec.
type siteInitHandler struct {
	// name is the site name and is used to create CNI resources as well as label things like machines and other
	// secondary resources created for the site.
	name string

	// clusterNodeCIDR is the CIDR range that the cluster is configured to use for node IPs.
	clusterNodeCIDR string

	// clusterPodCIDR is the CIDR range that the cluster is configured to use for pod IPs.
	clusterPodCIDR string

	// nodeCIDR is the CIDR to use for node IPs in this site.
	nodeCIDR string

	// podCIDR is the CIDR to use for pod IPs in this site.
	podCIDR string

	// manageCniPlugin controls whether unbounded-net manages the CNI plugin
	// for the site. When false, the Site is configured with manageCniPlugin: false
	// so that an existing CNI (e.g. Cilium, Calico) handles intra-site networking.
	// Defaults to true.
	manageCniPlugin bool

	enableNet              bool
	enableMachina          bool
	enableMetalman         bool
	enableUnboundedStorage bool
	skipInstall            bool
	installTimeout         time.Duration

	// kubeCli is the kubernetes client interface.
	kubeCli kubernetes.Interface

	// kubeconfigPath is the path to the kubeconfig file to use for connecting to the cluster.
	kubeconfigPath string

	// kubeResourcesCli is the controller-runtime client used for server-side apply of manifests.
	kubeResourcesCli client.Client
	kubeConfig       *rest.Config

	logger *slog.Logger
}

func (h *siteInitHandler) execute(ctx context.Context) error {
	if h.logger == nil {
		h.logger = slog.Default()
	}

	if err := h.validate(); err != nil {
		return fmt.Errorf("validating input for site initialization %s: %w", h.name, err)
	}

	kubeCli, kubeConfig, err := kube.ClientAndConfigFromFile(h.kubeconfigPath)
	if err != nil {
		return fmt.Errorf("creating Kubernetes client for site initialization %s: %w", h.name, err)
	}

	h.kubeCli = kubeCli

	kubeResourcesCli, err := client.New(kubeConfig, client.Options{})
	if err != nil {
		return fmt.Errorf("creating controller-runtime client for site initialization %s: %w", h.name, err)
	}

	h.kubeResourcesCli = kubeResourcesCli
	h.kubeConfig = kubeConfig

	if !h.skipInstall {
		installer := installHandler{
			kubeconfigPath:   h.kubeconfigPath,
			namespace:        machinaNamespace,
			netNamespace:     netNamespace,
			wait:             true,
			timeout:          h.installTimeout,
			kubeCli:          kubeCli,
			kubeResourcesCli: kubeResourcesCli,
			restConfig:       kubeConfig,
			logger:           h.logger,
		}
		if err := installer.execute(ctx); err != nil {
			return fmt.Errorf("bootstrapping unbounded-operator: %w", err)
		}
	}

	if err := h.ensureUnboundedSite(ctx, h.clusterSiteConfig()); err != nil {
		return fmt.Errorf("ensuring unbounded CNI site %s: %w", h.name, err)
	}

	if err := h.ensureUnboundedSite(ctx, h.remoteSiteConfig()); err != nil {
		return fmt.Errorf("ensuring unbounded CNI site %s: %w", h.name, err)
	}

	if err := h.ensureBootstrapToken(ctx); err != nil {
		return fmt.Errorf("ensuring bootstrap token for site %s: %w", h.name, err)
	}

	return nil
}

func (h *siteInitHandler) clusterSiteConfig() unboundedSiteConfig {
	return unboundedSiteConfig{
		SiteName:               "cluster",
		NodeCIDRs:              []string{h.clusterNodeCIDR},
		PodCIDRs:               []string{h.clusterPodCIDR},
		ManageCniPlugin:        h.manageCniPlugin,
		EnableNet:              h.enableNet,
		EnableMachina:          h.enableMachina,
		EnableUnboundedStorage: h.enableUnboundedStorage,
		Manifests: []string{
			"gatewaypool.yaml",
			"site.yaml",
			"sitegatewaypoolassignment.yaml",
		},
	}
}

func (h *siteInitHandler) remoteSiteConfig() unboundedSiteConfig {
	return unboundedSiteConfig{
		SiteName:        h.name,
		NodeCIDRs:       []string{h.nodeCIDR},
		PodCIDRs:        []string{h.podCIDR},
		ManageCniPlugin: h.manageCniPlugin,
		EnableMetalman:  h.enableMetalman,
		Manifests: []string{
			"site.yaml",
			"sitegatewaypoolassignment.yaml",
		},
	}
}

func (h *siteInitHandler) validate() error {
	if isEmpty(h.name) {
		return errors.New("site name is required")
	}

	// cluster CIDR validations

	if isEmpty(h.clusterNodeCIDR) {
		return errors.New("cluster node CIDR is required")
	}

	if !isValidIPv4CIDR(h.clusterNodeCIDR) {
		return errors.New("cluster pod CIDR is invalid")
	}

	if isEmpty(h.clusterPodCIDR) {
		return errors.New("cluster node CIDR is required")
	}

	if !isValidIPv4CIDR(h.clusterPodCIDR) {
		return errors.New("cluster pod CIDR is invalid")
	}

	// site CIDR validations

	if isEmpty(h.nodeCIDR) {
		return errors.New("node CIDR is required")
	}

	if !isValidIPv4CIDR(h.nodeCIDR) {
		return errors.New("node CIDR is invalid")
	}

	if isEmpty(h.podCIDR) {
		return errors.New("pod CIDR is required")
	}

	if !isValidIPv4CIDR(h.podCIDR) {
		return errors.New("pod CIDR is invalid")
	}

	h.kubeconfigPath = getKubeconfigPath(h.kubeconfigPath)

	if !isReadableFile(h.kubeconfigPath) {
		return fmt.Errorf("kubeconfig %q not readable", h.kubeconfigPath)
	}

	return nil
}

type unboundedSiteConfig struct {
	SiteName               string
	NodeCIDRs              []string
	PodCIDRs               []string
	Manifests              []string
	EnableNet              bool
	EnableMachina          bool
	EnableMetalman         bool
	EnableUnboundedStorage bool
	// ManageCniPlugin controls whether unbounded-net manages the CNI plugin.
	// When false, the template emits manageCniPlugin: false so that an
	// existing CNI (e.g. Cilium, Calico) is left in place.
	ManageCniPlugin bool
}

// ensureUnboundedSite sets up the main gateway and the cluster site that encompasses any nodes attached to the
// main cluster. For each manifest file name in cfg.Manifests it looks up the file from the
// assets/unbounded-net-site embed.FS, renders it as a Go template with cfg as the data, and applies
// all the resulting YAML documents to the cluster.
func (h *siteInitHandler) ensureUnboundedSite(ctx context.Context, cfg unboundedSiteConfig) error {
	buf := &bytes.Buffer{}

	templateFS := siteTemplates
	templateDir := "assets/unbounded-net-site/"

	for _, name := range cfg.Manifests {
		path := templateDir + name

		content, err := fs.ReadFile(templateFS, path)
		if err != nil {
			return fmt.Errorf("reading site manifest template %s: %w", name, err)
		}

		t, err := template.New(name).Parse(string(content))
		if err != nil {
			return fmt.Errorf("parsing site manifest template %s: %w", name, err)
		}

		if err := t.Execute(buf, cfg); err != nil {
			return fmt.Errorf("rendering site manifest template %s: %w", name, err)
		}

		// Ensure each rendered document ends with a newline so YAML
		// document separators (---) in the next template are valid.
		if buf.Len() > 0 && buf.Bytes()[buf.Len()-1] != '\n' {
			buf.WriteByte('\n')
		}
	}

	if err := kube.ApplyManifests(ctx, h.logger, h.kubeResourcesCli, fieldManagerID, buf.Bytes()); err != nil {
		return fmt.Errorf("applying site manifests for %s: %w", cfg.SiteName, err)
	}

	return nil
}

func (h *siteInitHandler) ensureBootstrapToken(ctx context.Context) error {
	tok, err := kube.GetBootstrapTokenForSite(ctx, h.kubeCli, h.name)
	if err != nil && !errors.Is(err, kube.ErrBootstrapTokenNotFound) {
		return fmt.Errorf("getting bootstrap token for %s: %w", h.name, err)
	}

	if tok == nil {
		tok, err := kube.NewBootstrapToken()
		if err != nil {
			return fmt.Errorf("generating bootstrap token for %s: %w", h.name, err)
		}

		tok.WithLabel("unbounded-cloud.io/site", h.name)

		if err := kube.ApplyBootstrapToken(ctx, h.kubeCli, fieldManagerID, tok); err != nil {
			return fmt.Errorf("applying bootstrap token for %s: %w", h.name, err)
		}
	}

	return nil
}

func siteInitCommand() *cobra.Command {
	handler := siteInitHandler{}

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a new unbounded-kube site",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return handler.execute(cmd.Context())
		},
	}

	cmd.Flags().StringVar(&handler.kubeconfigPath, "kubeconfig", "", "Path to kubeconfig file")
	cmd.Flags().StringVar(&handler.name, "name", "", "The name of the site")
	cmd.Flags().StringVar(&handler.clusterNodeCIDR, "cluster-node-cidr", "", "The cluster node cidr")
	cmd.Flags().StringVar(&handler.clusterPodCIDR, "cluster-pod-cidr", "", "The cluster pod cidr")
	cmd.Flags().StringVar(&handler.nodeCIDR, "node-cidr", "", "The node CIDR")
	cmd.Flags().StringVar(&handler.podCIDR, "pod-cidr", "", "The pod CIDR")
	cmd.Flags().BoolVar(&handler.manageCniPlugin, "manage-cni-plugin", true, "Whether unbounded-net manages the CNI plugin; set to false when the cluster already has a CNI (e.g. Cilium, Calico)")
	cmd.Flags().BoolVar(&handler.enableNet, "enable-net", true, "Enable unbounded-net for the Site")
	cmd.Flags().BoolVar(&handler.enableMachina, "enable-machina", true, "Enable machina for the Site")
	cmd.Flags().BoolVar(&handler.enableMetalman, "enable-metalman", false, "Enable metalman for the Site")
	cmd.Flags().BoolVar(&handler.enableUnboundedStorage, "enable-unbounded-storage", false, "Enable unbounded-storage for the Site")
	cmd.Flags().BoolVar(&handler.skipInstall, "skip-install", false, "Skip bootstrapping CRDs and unbounded-operator before creating site resources")
	cmd.Flags().DurationVar(&handler.installTimeout, "install-timeout", defaultInstallTimeout, "Timeout while waiting for unbounded-operator bootstrap")

	if err := cmd.MarkFlagRequired("name"); err != nil {
		panic(err)
	}

	if err := cmd.MarkFlagRequired("cluster-node-cidr"); err != nil {
		panic(err)
	}

	if err := cmd.MarkFlagRequired("cluster-pod-cidr"); err != nil {
		panic(err)
	}

	if err := cmd.MarkFlagRequired("node-cidr"); err != nil {
		panic(err)
	}

	if err := cmd.MarkFlagRequired("pod-cidr"); err != nil {
		panic(err)
	}

	return cmd
}
