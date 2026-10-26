// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/kube"
	"github.com/Azure/unbounded/internal/unbounded"
)

//go:embed assets/unbounded-net-site/*.yaml
var siteTemplates embed.FS

// siteTemplateDir is the embedded directory holding the Site manifest templates.
const siteTemplateDir = "assets/unbounded-net-site"

// clusterSiteName is the reserved name of the Site for the control-plane
// cluster's own nodes. The token refresher ignores this Site, so site init
// neither enables the token refresher on it nor creates a bootstrap token for
// it.
const clusterSiteName = "cluster"

// siteInitHandler creates one Site and its bootstrap credentials.
// Component installation is reconciled by unbounded-operator from the Site spec.
type siteInitHandler struct {
	// name is the site name and is used to create CNI resources as well as label things like machines and other
	// secondary resources created for the site.
	name string

	// nodeCIDRs are the CIDRs that contain the node IPs of this site.
	nodeCIDRs []string

	// podCIDRs are the CIDRs that pod IPs in this site are allocated from.
	podCIDRs []string

	// manageCniPlugin controls whether unbounded-net manages the CNI plugin
	// for the site. When false, the Site is configured with manageCniPlugin: false
	// so that an existing CNI (e.g. Cilium, Calico) handles intra-site networking.
	// Defaults to true.
	manageCniPlugin bool

	enableMachina  bool
	enableMetalman bool

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

	return h.run(ctx)
}

// run applies the Site and its bootstrap token with the handler's clients.
func (h *siteInitHandler) run(ctx context.Context) error {
	if h.logger == nil {
		h.logger = slog.Default()
	}

	if err := h.checkOperatorPrerequisites(ctx); err != nil {
		return err
	}

	if err := h.ensureUnboundedSite(ctx, h.siteConfig()); err != nil {
		return fmt.Errorf("ensuring unbounded CNI site %s: %w", h.name, err)
	}

	if h.isClusterSite() {
		return nil
	}

	if err := h.ensureBootstrapToken(ctx); err != nil {
		return fmt.Errorf("ensuring bootstrap token for site %s: %w", h.name, err)
	}

	return nil
}

// isClusterSite reports whether the handler targets the reserved cluster Site.
func (h *siteInitHandler) isClusterSite() bool {
	return h.name == clusterSiteName
}

// checkOperatorPrerequisites verifies the cluster is ready to accept the Site
// resources site init is about to create. The unbounded-operator owns CRD
// lifecycle and reconciles Sites, so it must be installed first (via
// `kubectl unbounded install`).
//
// The check uses API discovery to confirm the apiserver actually serves the
// GroupVersionKinds site init applies (derived from the embedded manifests, so
// the check tracks future API versions automatically). Discovery is used rather
// than reading CustomResourceDefinition objects because it does not require
// cluster-scoped CRD read permission and reflects the versions the apiserver
// serves, not merely that a CRD is Established. A type that is not served is a
// hard error; a missing or not-yet-ready operator Deployment is only a warning,
// because the operator reconciles the Site once it becomes ready.
func (h *siteInitHandler) checkOperatorPrerequisites(ctx context.Context) error {
	required, err := requiredSiteAPIs()
	if err != nil {
		return fmt.Errorf("determining required Site API types: %w", err)
	}

	disco := h.kubeCli.Discovery()
	servedKinds := map[schema.GroupVersion]map[string]bool{}

	var missing []string

	for _, gvk := range required {
		gv := gvk.GroupVersion()

		kinds, cached := servedKinds[gv]
		if !cached {
			kinds, err = servedKindsForGroupVersion(disco, gv)
			if err != nil {
				return fmt.Errorf("checking whether the cluster serves %s: %w", gv, err)
			}

			servedKinds[gv] = kinds
		}

		if !kinds[gvk.Kind] {
			missing = append(missing, fmt.Sprintf("%s.%s", gvk.Kind, gv))
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf(
			"unbounded-operator is not installed: the cluster is not serving required API types (%s); run \"kubectl unbounded install\" first to bootstrap the unbounded-operator",
			strings.Join(missing, ", "),
		)
	}

	h.warnIfOperatorNotReady(ctx)

	return nil
}

// servedKindsForGroupVersion returns the set of Kinds the apiserver serves for
// gv. A GroupVersion the apiserver does not serve yields an empty set (not an
// error), so callers can treat "not served" uniformly with "kind absent".
func servedKindsForGroupVersion(disco discovery.DiscoveryInterface, gv schema.GroupVersion) (map[string]bool, error) {
	list, err := disco.ServerResourcesForGroupVersion(gv.String())
	if err != nil {
		if apierrors.IsNotFound(err) {
			return map[string]bool{}, nil
		}

		return nil, err
	}

	kinds := make(map[string]bool, len(list.APIResources))
	for _, resource := range list.APIResources {
		kinds[resource.Kind] = true
	}

	return kinds, nil
}

// requiredSiteAPIs returns the distinct GroupVersionKinds declared by the
// embedded Site manifest templates. Deriving the set from the templates (instead
// of a hard-coded list) keeps the preflight in lockstep with the versions site
// init actually applies, including future API bumps.
func requiredSiteAPIs() ([]schema.GroupVersionKind, error) {
	entries, err := fs.ReadDir(siteTemplates, siteTemplateDir)
	if err != nil {
		return nil, fmt.Errorf("reading site manifest templates: %w", err)
	}

	// Placeholder values so the templates render into valid YAML; only the
	// static apiVersion/kind fields are read, which do not depend on the data.
	data := unboundedSiteConfig{
		SiteName:  "preflight",
		NodeCIDRs: []string{"0.0.0.0/0"},
		PodCIDRs:  []string{"0.0.0.0/0"},
	}

	seen := map[schema.GroupVersionKind]bool{}

	var gvks []schema.GroupVersionKind

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()

		content, err := fs.ReadFile(siteTemplates, siteTemplateDir+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("reading site manifest template %s: %w", name, err)
		}

		tmpl, err := template.New(name).Parse(string(content))
		if err != nil {
			return nil, fmt.Errorf("parsing site manifest template %s: %w", name, err)
		}

		buf := &bytes.Buffer{}
		if err := tmpl.Execute(buf, data); err != nil {
			return nil, fmt.Errorf("rendering site manifest template %s: %w", name, err)
		}

		decoder := utilyaml.NewYAMLOrJSONDecoder(buf, 4096)

		for {
			obj := &unstructured.Unstructured{}

			if err := decoder.Decode(obj); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}

				return nil, fmt.Errorf("decoding site manifest template %s: %w", name, err)
			}

			gvk := obj.GroupVersionKind()
			if gvk.Empty() {
				continue
			}

			if !seen[gvk] {
				seen[gvk] = true

				gvks = append(gvks, gvk)
			}
		}
	}

	return gvks, nil
}

// warnIfOperatorNotReady logs a warning (never an error) when the
// unbounded-operator Deployment is absent or not fully rolled out. The served
// API types checked by checkOperatorPrerequisites are enough to apply Site
// resources; a not-yet-ready operator only delays reconciliation, so site init
// proceeds. Permission errors are downgraded to a debug log so a caller that can
// apply Sites but cannot read Deployments is not blocked or alarmed.
func (h *siteInitHandler) warnIfOperatorNotReady(ctx context.Context) {
	namespace := unbounded.SystemNamespace()

	deploy, err := h.kubeCli.AppsV1().Deployments(namespace).Get(ctx, "unbounded-operator", metav1.GetOptions{})
	if err != nil {
		switch {
		case apierrors.IsNotFound(err):
			h.logger.Warn(
				"unbounded-operator Deployment not found; the Site will not be reconciled until the operator is installed and ready",
				"namespace", namespace,
			)
		case apierrors.IsForbidden(err):
			h.logger.Debug(
				"skipping unbounded-operator readiness check: not permitted to read Deployments",
				"namespace", namespace,
			)
		default:
			h.logger.Debug(
				"could not inspect unbounded-operator Deployment",
				"namespace", namespace,
				"error", err,
			)
		}

		return
	}

	if !deploymentRolloutComplete(deploy) {
		h.logger.Warn(
			"unbounded-operator Deployment is not fully rolled out; the Site will be reconciled once the operator is ready",
			"namespace", namespace,
		)
	}
}

func (h *siteInitHandler) siteConfig() unboundedSiteConfig {
	return unboundedSiteConfig{
		SiteName:             h.name,
		NodeCIDRs:            h.nodeCIDRs,
		PodCIDRs:             h.podCIDRs,
		ManageCniPlugin:      h.manageCniPlugin,
		EnableMachina:        h.enableMachina,
		EnableMetalman:       h.enableMetalman,
		EnableTokenRefresher: !h.isClusterSite(),
		Manifests:            []string{"site.yaml"},
	}
}

func (h *siteInitHandler) validate() error {
	if isEmpty(h.name) {
		return errors.New("site name is required")
	}

	// Node CIDRs of one Site may overlap, but the net controller exits when a
	// network is listed twice. Pod CIDRs may not overlap at all, which the Site
	// webhook also enforces, but it is not running yet when the first Site is
	// created.
	nodeCIDRs, err := parseCIDRList("--node-cidr", h.nodeCIDRs, false)
	if err != nil {
		return err
	}

	podCIDRs, err := parseCIDRList("--pod-cidr", h.podCIDRs, true)
	if err != nil {
		return err
	}

	h.nodeCIDRs = nodeCIDRs
	h.podCIDRs = podCIDRs

	h.kubeconfigPath = getKubeconfigPath(h.kubeconfigPath)

	if !isReadableFile(h.kubeconfigPath) {
		return fmt.Errorf("kubeconfig %q not readable", h.kubeconfigPath)
	}

	return nil
}

type unboundedSiteConfig struct {
	SiteName             string
	NodeCIDRs            []string
	PodCIDRs             []string
	Manifests            []string
	EnableMachina        bool
	EnableMetalman       bool
	EnableTokenRefresher bool
	// ManageCniPlugin controls whether unbounded-net manages the CNI plugin.
	// When false, the template emits manageCniPlugin: false so that an
	// existing CNI (e.g. Cilium, Calico) is left in place.
	ManageCniPlugin bool
}

// ensureUnboundedSite renders the Site manifests for cfg and applies every
// resulting YAML document to the cluster.
func (h *siteInitHandler) ensureUnboundedSite(ctx context.Context, cfg unboundedSiteConfig) error {
	manifests, err := renderSiteManifests(cfg)
	if err != nil {
		return err
	}

	if err := kube.ApplyManifests(ctx, h.logger, h.kubeResourcesCli, fieldManagerID, manifests); err != nil {
		return fmt.Errorf("applying site manifests for %s: %w", cfg.SiteName, err)
	}

	return nil
}

// renderSiteManifests looks up each manifest named in cfg.Manifests in the
// assets/unbounded-net-site embed.FS and renders it as a Go template with cfg
// as the data.
func renderSiteManifests(cfg unboundedSiteConfig) ([]byte, error) {
	buf := &bytes.Buffer{}

	for _, name := range cfg.Manifests {
		path := siteTemplateDir + "/" + name

		content, err := fs.ReadFile(siteTemplates, path)
		if err != nil {
			return nil, fmt.Errorf("reading site manifest template %s: %w", name, err)
		}

		t, err := template.New(name).Parse(string(content))
		if err != nil {
			return nil, fmt.Errorf("parsing site manifest template %s: %w", name, err)
		}

		if err := t.Execute(buf, cfg); err != nil {
			return nil, fmt.Errorf("rendering site manifest template %s: %w", name, err)
		}

		// Ensure each rendered document ends with a newline so YAML
		// document separators (---) in the next template are valid.
		if buf.Len() > 0 && buf.Bytes()[buf.Len()-1] != '\n' {
			buf.WriteByte('\n')
		}
	}

	return buf.Bytes(), nil
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
		Long: `Create or update one Site. For any Site other than "cluster", also create a
bootstrap token for joining machines to it, unless the Site already has a
valid one. The cluster Site never gets a token, because the token refresher
ignores it.

Run it once per Site: once with the reserved name "cluster" for the
control-plane cluster's own nodes, then once for each remote site.

site init does not create gateway resources. Connect Sites to gateway nodes
with "kubectl unbounded net create gatewaypool" and
"kubectl unbounded net create sitegatewaypoolassignment".`,
		Example: `  # The control-plane cluster's own nodes
  kubectl unbounded site init --name cluster \
    --node-cidr 10.224.0.0/16 \
    --pod-cidr 10.244.0.0/16

  # A remote site with two node and two pod CIDR ranges
  kubectl unbounded site init --name my-edge-site \
    --node-cidr 10.100.0.0/24,10.110.0.0/24 \
    --pod-cidr 10.101.0.0/24,10.111.0.0/24`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return handler.execute(cmd.Context())
		},
	}

	cmd.Flags().StringVar(&handler.kubeconfigPath, "kubeconfig", "", "Path to kubeconfig file")
	cmd.Flags().StringVar(&handler.name, "name", "", `The name of the Site; use "cluster" for the control-plane cluster's own nodes`)
	cmd.Flags().StringSliceVar(&handler.nodeCIDRs, "node-cidr", nil, "Comma-separated list of CIDRs that contain the node IPs of the Site")
	cmd.Flags().StringSliceVar(&handler.podCIDRs, "pod-cidr", nil, "Comma-separated list of CIDRs to allocate the Site's pod IPs from")
	cmd.Flags().BoolVar(&handler.manageCniPlugin, "manage-cni-plugin", true, "Whether unbounded-net manages the CNI plugin; set to false when the cluster already has a CNI (e.g. Cilium, Calico)")
	cmd.Flags().BoolVar(&handler.enableMachina, "enable-machina", true, "Enable machina for the Site; machina runs cluster-wide when any Site enables it")
	cmd.Flags().BoolVar(&handler.enableMetalman, "enable-metalman", false, "Enable metalman for the Site")

	if err := cmd.MarkFlagRequired("name"); err != nil {
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
