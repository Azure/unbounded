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

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded-kube/cmd/machina/machina/controller"
	"github.com/Azure/unbounded-kube/internal/kube"
)

//go:embed assets/unbounded-net-site/*.yaml
var siteTemplates embed.FS

// siteInitHandler is responsible for handling initial unbounded-kube bootstrap and also ensuring a site
// is ready for machines to be added to it. The handler performs the following duties:
//
//  1. Validate inputs and runtime environment.
//     - Check if parameters themselves are valid (e.g. CIDRs, site name, etc.).
//     - Check if kubectl is available and can connect to the cluster.
//     - Check if the cluster has at least one node with a label unbounded-kube.io/unbounded-net-gateway=true. If not,
//     the handler will exit and prompt the user to label at least one node with that label.
//
//  2. Install the unbounded-net plugin.
//     - Download the CNI plugin release OR use a local tarball/directory of manifests provided by the user.
//     - Verify the unbounded-net controller is up and running.
//
// 3. Install a site specific manifest for the unbounded-net.
// 4. Install and configure machina-controller.
// 5. Create a bootstrap token for the site if one does not already exist.
// 6. Verify machina-controller is up and running.
// 7. Print out Site Initialized message and show how to access unbounded-net UI and register a machine.
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

	// cniManifests is a path to either a directory or archive containing CNI manifests to apply
	// to the cluster for this site. This is temporarily required to support installing the
	// unbounded CNI until we have public downloadable releases coming from that repository.
	cniManifests string

	// machinaManifests is a path to either a directory or archive containing machina manifests to apply
	// to the cluster for this site. This is temporarily required to support installing the
	// machina controller until we have public downloadable releases coming from that repository.
	machinaManifests string

	// manageCniPlugin controls whether unbounded-net manages the CNI plugin
	// for the site. When false, the Site is configured with manageCniPlugin: false
	// so that an existing CNI (e.g. Cilium, Calico) handles intra-site networking.
	// Defaults to true.
	manageCniPlugin bool

	// kubeCli is the kubernetes client interface.
	kubeCli kubernetes.Interface

	kubeConfig *rest.Config

	// kubeconfigPath is the path to the kubeconfig file to use for connecting to the cluster.
	kubeconfigPath string

	// kubeResourcesCli is the controller-runtime client used for server-side apply of manifests.
	kubeResourcesCli client.Client

	// kubectl is function that creates a kubectl command pointed to the correct KUBECONFIG for the cluster.
	kubectl kube.KubectlFunc

	installUnboundedCNI *installUnboundedCNI

	installMachina *installMachina

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
	h.kubeConfig = kubeConfig
	h.kubectl = kube.Kubectl(nil, h.kubeconfigPath)

	kubeResourcesCli, err := client.New(kubeConfig, client.Options{})
	if err != nil {
		return fmt.Errorf("creating controller-runtime client for site initialization %s: %w", h.name, err)
	}

	h.kubeResourcesCli = kubeResourcesCli

	if h.installUnboundedCNI == nil {
		h.installUnboundedCNI = newInstallUnboundedCNI(
			h.cniManifests,
			nil,
			h.logger,
			h.kubeResourcesCli,
			h.kubeCli,
		)
	}

	if h.installMachina == nil {
		h.installMachina = newInstallMachina(h.machinaManifests, nil, h.logger, h.kubeResourcesCli, h.kubeCli)
	}

	if err := h.ensureUnboundedCNI(ctx); err != nil {
		return fmt.Errorf("ensuring unbounded CNI for site %s: %w", h.name, err)
	}

	if err := h.ensureUnboundedSite(ctx, unboundedSiteConfig{
		SiteName:        "cluster",
		NodeCIDRs:       []string{h.clusterNodeCIDR},
		PodCIDRs:        []string{h.clusterPodCIDR},
		ManageCniPlugin: h.manageCniPlugin,
		Manifests: []string{
			"gatewaypool.yaml",
			"site.yaml",
			"sitegatewaypoolassignment.yaml",
		},
	}); err != nil {
		return fmt.Errorf("ensuring unbounded CNI site %s: %w", h.name, err)
	}

	if err := h.ensureUnboundedSite(ctx, unboundedSiteConfig{
		SiteName:        h.name,
		NodeCIDRs:       []string{h.nodeCIDR},
		PodCIDRs:        []string{h.podCIDR},
		ManageCniPlugin: h.manageCniPlugin,
		Manifests: []string{
			"site.yaml",
			"sitegatewaypoolassignment.yaml",
		},
	}); err != nil {
		return fmt.Errorf("ensuring unbounded CNI site %s: %w", h.name, err)
	}

	if err := h.ensureBootstrapToken(ctx); err != nil {
		return fmt.Errorf("ensuring bootstrap token for site %s: %w", h.name, err)
	}

	if err := h.ensureMachinaIsRunning(ctx); err != nil {
		return fmt.Errorf("installing machina controller for site %s: %w", h.name, err)
	}

	return nil
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

	if h.cniManifests != "" && !isHTTPSURL(h.cniManifests) && !isDirectoryOrFile(h.cniManifests) {
		return errors.New("CNI manifests path is invalid")
	}

	if err := kube.CheckKubectlAvailable(); err != nil {
		return fmt.Errorf("kubectl not available: %w", err)
	}

	h.kubeconfigPath = getKubeconfigPath(h.kubeconfigPath)

	if !isReadableFile(h.kubeconfigPath) {
		return fmt.Errorf("kubeconfig %q not readable", h.kubeconfigPath)
	}

	return nil
}

func (h *siteInitHandler) checkUnboundedCNIGatewayNode(ctx context.Context) (bool, error) {
	opts := metav1.ListOptions{
		LabelSelector: "unbounded-kube.io/unbounded-net-gateway=true",
	}

	nodes, err := h.kubeCli.CoreV1().Nodes().List(ctx, opts)
	if err != nil {
		return false, fmt.Errorf("listing nodes with unbounded CNI gateway label: %w", err)
	}

	return len(nodes.Items) > 0, nil
}

func (h *siteInitHandler) ensureUnboundedCNI(ctx context.Context) error {
	hasGatewayAssignableNode, err := h.checkUnboundedCNIGatewayNode(ctx)
	if err != nil {
		return fmt.Errorf("checking for unbounded CNI gateway node: %w", err)
	}

	if !hasGatewayAssignableNode {
		return fmt.Errorf(
			"no nodes with label unbounded-kube.io/unbounded-net-gateway=true found; please label at least one node with that label before initializing the site",
		)
	}

	if h.cniManifests == "" {
		h.logger.Info("Using embedded unbounded-net manifests")
	}

	if err := h.installUnboundedCNI.run(ctx); err != nil {
		return fmt.Errorf("installing unbounded CNI: %w", err)
	}

	return nil
}

type unboundedSiteConfig struct {
	SiteName  string
	NodeCIDRs []string
	PodCIDRs  []string
	Manifests []string
	// ManageCniPlugin controls whether unbounded-net manages the CNI plugin.
	// When false, the template emits manageCniPlugin: false so that an
	// existing CNI (e.g. Cilium, Calico) is left in place.
	ManageCniPlugin bool
}

func (h *siteInitHandler) ensureMachinaIsRunning(ctx context.Context) error {
	machinaCfg := controller.DefaultConfig()
	machinaCfg.APIServerEndpoint = h.kubeConfig.Host

	ao := metav1.ApplyOptions{
		FieldManager: fieldManagerID,
	}

	// Ensure the unbounded-kube namespace exists before applying the
	// ConfigMap — the namespace manifest is part of the installer bundle
	// but we need it earlier.
	nsApply := v1.Namespace(machinaNamespace)
	if _, err := h.kubeCli.CoreV1().Namespaces().Apply(ctx, nsApply, ao); err != nil {
		return fmt.Errorf("ensuring namespace %s: %w", machinaNamespace, err)
	}

	b, err := yaml.Marshal(machinaCfg)
	if err != nil {
		return fmt.Errorf("marshaling machina controller config: %w", err)
	}

	s := v1.ConfigMap("machina-config", "unbounded-kube").
		WithData(map[string]string{
			"config.yaml": string(b),
		})

	if err := kube.ApplyConfigMap(ctx, h.kubeCli, s, ao); err != nil {
		return fmt.Errorf("applying machina config: %w", err)
	}

	if h.machinaManifests == "" {
		h.logger.Info("Using embedded machina manifests")
	}

	h.installMachina.skipPaths = []string{"03-config.yaml"}

	return h.installMachina.run(ctx)
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

		tok.WithLabel("unbounded-kube.io/site", h.name)

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

	cmd.Flags().StringVar(&handler.cniManifests, "cni-manifests", "", "Path or https URL to CNI plugin manifests (uses embedded manifests if omitted)")
	cmd.Flags().StringVar(&handler.machinaManifests, "machina-manifests", "", "Path or https URL to Machina manifests (uses embedded manifests if omitted)")
	cmd.Flags().StringVar(&handler.kubeconfigPath, "kubeconfig", "", "Path to kubeconfig file")
	cmd.Flags().StringVar(&handler.name, "name", "", "The name of the site")
	cmd.Flags().StringVar(&handler.clusterNodeCIDR, "cluster-node-cidr", "", "The cluster node cidr")
	cmd.Flags().StringVar(&handler.clusterPodCIDR, "cluster-pod-cidr", "", "The cluster pod cidr")
	cmd.Flags().StringVar(&handler.nodeCIDR, "node-cidr", "", "The node CIDR")
	cmd.Flags().StringVar(&handler.podCIDR, "pod-cidr", "", "The pod CIDR")
	cmd.Flags().BoolVar(&handler.manageCniPlugin, "manage-cni-plugin", true, "Whether unbounded-net manages the CNI plugin; set to false when the cluster already has a CNI (e.g. Cilium, Calico)")

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
