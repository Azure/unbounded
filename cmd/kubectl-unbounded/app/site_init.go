package app

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"text/template"

	"github.com/project-unbounded/unbounded-kube/cmd/machina/machina/controller"
	"github.com/project-unbounded/unbounded-kube/internal/kube"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed assets/unbounded-net-site/*.yaml
var siteTemplates embed.FS

//go:embed assets/unbounded-cni-site/*.yaml
var siteTemplatesPrototype embed.FS

//go:embed assets/flexagent-temp/*.yaml
var flexAgentTemplates embed.FS

const (
	unboundedCNIRelease = "https://github.com/project-unbounded/unbounded-net/releases/download/v1.0.1/unbounded-net-manifests-v1.0.1.tar.gz"
)

// usePrototypeCNI returns true when UB_PROTOTYPE_UNBOUNDED_CNI=1 is set,
// switching the plugin to use the older unbounded-cni v0.7.x assets and
// namespace instead of unbounded-net v1.x.
func usePrototypeCNI() bool {
	return os.Getenv("UB_PROTOTYPE_UNBOUNDED_CNI") == "1"
}

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

	// clusterServiceCIDR is the Service CIDR of the cluster (e.g. "10.0.0.0/16").
	// When not provided it is derived from the kube-dns Service ClusterIP.
	clusterServiceCIDR string

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
		SiteName:  "cluster",
		NodeCIDRs: []string{h.clusterNodeCIDR},
		PodCIDRs:  []string{h.clusterPodCIDR},
		Manifests: []string{
			"gatewaypool.yaml",
			"site.yaml",
			"sitegatewaypoolassignment.yaml",
		},
	}); err != nil {
		return fmt.Errorf("ensuring unbounded CNI site %s: %w", h.name, err)
	}

	if err := h.ensureUnboundedSite(ctx, unboundedSiteConfig{
		SiteName:  h.name,
		NodeCIDRs: []string{h.nodeCIDR},
		PodCIDRs:  []string{h.podCIDR},
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

	// TEMPORARY!!!
	// THIS IS ONLY NEEDED UNTIL THE FLEX AGENT NODE IS DEPRECATED IN FAVOR OF THE NEW AGENT. IT DOES
	// A BUNCH OF KUBEADM STUFF WE DO NOT NEED.
	if err := h.ensureFlexAgentConfig(ctx); err != nil {
		return fmt.Errorf("ensuring flex agent config for site %s: %w", h.name, err)
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

	if !isHTTPSURL(h.cniManifests) && !isDirectoryOrFile(h.cniManifests) {
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
}

func (h *siteInitHandler) ensureMachinaIsRunning(ctx context.Context) error {
	machinaCfg := controller.DefaultConfig()
	machinaCfg.APIServerEndpoint = h.kubeConfig.Host

	ao := metav1.ApplyOptions{
		FieldManager: fieldManagerID,
	}

	// Ensure the machina-system namespace exists before applying the
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

	s := v1.ConfigMap("machina-config", "machina-system").
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

	if usePrototypeCNI() {
		templateFS = siteTemplatesPrototype
		templateDir = "assets/unbounded-cni-site/"
	}

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

// flexAgentTemplateData holds the values injected into the flexagent-temp
// manifest templates that set up kubeadm-compatible RBAC and ConfigMaps.
type flexAgentTemplateData struct {
	// CertificateAuthorityData is the base64-encoded cluster CA certificate.
	CertificateAuthorityData string

	// Server is the HTTPS URL of the Kubernetes API server.
	Server string

	// KubernetesVersion is the cluster's Kubernetes version (e.g. "v1.34.3").
	KubernetesVersion string

	// ServiceSubnet is the cluster Service CIDR (e.g. "10.0.0.0/16").
	ServiceSubnet string
}

// ensureFlexAgentConfig renders and applies the embedded flexagent-temp
// manifest templates. These create the RBAC rules and ConfigMaps that
// kubeadm expects on a control plane so that remote worker nodes can
// perform a bootstrap-token based kubeadm join against a managed cluster
// (e.g. AKS) that was not bootstrapped by kubeadm.
func (h *siteInitHandler) ensureFlexAgentConfig(ctx context.Context) error {
	h.logger.Info("Ensuring flex agent kubeadm config")

	// CA certificate from kube-root-ca.crt ConfigMap in kube-public.
	cm, err := h.kubeCli.CoreV1().ConfigMaps(metav1.NamespacePublic).Get(ctx, "kube-root-ca.crt", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get kube-root-ca.crt ConfigMap: %w", err)
	}

	caCert, ok := cm.Data["ca.crt"]
	if !ok {
		return fmt.Errorf("ca.crt key not found in kube-root-ca.crt ConfigMap")
	}

	caCertBase64 := base64.StdEncoding.EncodeToString([]byte(caCert))

	// Kubernetes version from the API server.
	sv, err := h.kubeCli.Discovery().ServerVersion()
	if err != nil {
		return fmt.Errorf("get server version: %w", err)
	}

	// Service CIDR — use the flag value or derive from kube-dns.
	serviceCIDR := h.clusterServiceCIDR
	if serviceCIDR == "" {
		derived, err := deriveServiceCIDR(ctx, h.kubeCli)
		if err != nil {
			return fmt.Errorf("derive service CIDR from kube-dns: %w", err)
		}

		serviceCIDR = derived
	}

	data := flexAgentTemplateData{
		CertificateAuthorityData: caCertBase64,
		Server:                   h.kubeConfig.Host,
		KubernetesVersion:        sv.GitVersion,
		ServiceSubnet:            serviceCIDR,
	}

	h.logger.Info("Flex agent template data resolved",
		"server", data.Server,
		"kubernetesVersion", data.KubernetesVersion,
		"serviceSubnet", data.ServiceSubnet,
		"caCertBase64Length", len(data.CertificateAuthorityData),
	)

	buf := &bytes.Buffer{}

	entries, err := fs.ReadDir(flexAgentTemplates, "assets/flexagent-temp")
	if err != nil {
		return fmt.Errorf("reading flexagent-temp directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		path := "assets/flexagent-temp/" + entry.Name()

		content, err := fs.ReadFile(flexAgentTemplates, path)
		if err != nil {
			return fmt.Errorf("reading flex agent template %s: %w", entry.Name(), err)
		}

		t, err := template.New(entry.Name()).Parse(string(content))
		if err != nil {
			return fmt.Errorf("parsing flex agent template %s: %w", entry.Name(), err)
		}

		if err := t.Execute(buf, data); err != nil {
			return fmt.Errorf("rendering flex agent template %s: %w", entry.Name(), err)
		}

		if buf.Len() > 0 && buf.Bytes()[buf.Len()-1] != '\n' {
			buf.WriteByte('\n')
		}
	}

	if err := kube.ApplyManifests(ctx, h.logger, h.kubeResourcesCli, fieldManagerID, buf.Bytes()); err != nil {
		return fmt.Errorf("applying flex agent manifests: %w", err)
	}

	h.logger.Info("Flex agent kubeadm config applied")

	return nil
}

// deriveServiceCIDR infers the cluster Service CIDR from the kube-dns
// Service ClusterIP. It masks the IP to a /16 network. For example,
// if kube-dns has ClusterIP 10.0.0.10, the result is "10.0.0.0/16".
func deriveServiceCIDR(ctx context.Context, kubeCli kubernetes.Interface) (string, error) {
	svc, err := kubeCli.CoreV1().Services(metav1.NamespaceSystem).Get(ctx, "kube-dns", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get kube-dns Service: %w", err)
	}

	if svc.Spec.ClusterIP == "" {
		return "", fmt.Errorf("kube-dns Service has no ClusterIP")
	}

	ip := net.ParseIP(svc.Spec.ClusterIP)
	if ip == nil {
		return "", fmt.Errorf("invalid kube-dns ClusterIP: %s", svc.Spec.ClusterIP)
	}

	ip = ip.To4()
	if ip == nil {
		return "", fmt.Errorf("kube-dns ClusterIP %s is not IPv4", svc.Spec.ClusterIP)
	}

	// Apply a /16 mask to get the network address.
	mask := net.CIDRMask(16, 32)
	network := ip.Mask(mask)

	return fmt.Sprintf("%s/16", network.String()), nil
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

	cniDefault := unboundedCNIRelease
	if usePrototypeCNI() {
		cniDefault = ""
	}

	cmd.Flags().StringVar(&handler.cniManifests, "cni-manifests", cniDefault, "Path or https URL to CNI plugin manifests")
	cmd.Flags().StringVar(&handler.machinaManifests, "machina-manifests", "", "Path or https URL to Machina manifests (uses embedded manifests if omitted)")
	cmd.Flags().StringVar(&handler.kubeconfigPath, "kubeconfig", "", "Path to kubeconfig file")
	cmd.Flags().StringVar(&handler.name, "name", "", "The name of the site")
	cmd.Flags().StringVar(&handler.clusterNodeCIDR, "cluster-node-cidr", "", "The cluster node cidr")
	cmd.Flags().StringVar(&handler.clusterPodCIDR, "cluster-pod-cidr", "", "The cluster pod cidr")
	cmd.Flags().StringVar(&handler.nodeCIDR, "node-cidr", "", "The node CIDR")
	cmd.Flags().StringVar(&handler.podCIDR, "pod-cidr", "", "The pod CIDR")
	cmd.Flags().StringVar(&handler.clusterServiceCIDR, "cluster-service-cidr", "", "The Service CIDR of the cluster (e.g. 10.0.0.0/16); derived from kube-dns if omitted")

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
