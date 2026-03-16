package unboundedcni

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/azsdk"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/kube"
	"k8s.io/client-go/kubernetes"
)

//go:embed assets
var Assets embed.FS

type Connector struct {
	AzureCli              *azsdk.ClientSet
	KubeCli               kubernetes.Interface
	Kubectl               func(context.Context) *exec.Cmd
	Logger                *slog.Logger
	ClusterResourceGroup  *armresources.ResourceGroup
	ClusterVirtualNetwork *armnetwork.VirtualNetwork
	// ExtraParameters is a Driver specific map of extra parameters. Each driver
	// may consume these differently, for example, a driver may decide to read
	// a configuration file with API endpoints from here.
	ExtraParameters          map[string]string
	DataDir                  string
	GatewayPoolName          string
	GatewayPoolType          string
	GatewayPoolAgentPoolName string
	// UnboundedCNIReleaseURL is the URL to the unbounded-cni manifests tarball.
	// Supports https:// and file:// schemes. The version is parsed from the URL
	// for https:// URLs. For file:// URLs, manifests are always extracted fresh.
	UnboundedCNIReleaseURL string
	// ControllerImage overrides the controller container image in unbounded-cni manifests.
	ControllerImage string
	// NodeImage overrides the node container image in unbounded-cni manifests.
	NodeImage string
}

func (c Connector) ProvisionClusterConnectivity(ctx context.Context) error {
	if c.GatewayPoolType == "" {
		c.GatewayPoolType = "External"
	}

	l := c.Logger.With("component", "unbounded-cni")

	manifestDir, err := DownloadAndCache(l, c.UnboundedCNIReleaseURL)
	if err != nil {
		return fmt.Errorf("download unbounded-cni manifests: %w", err)
	}

	if err := PatchConfigMapTenantID(l, manifestDir, c.AzureCli.TenantID); err != nil {
		return fmt.Errorf("patch configmap tenant ID: %w", err)
	}

	if c.ControllerImage != "" {
		if err := OverrideControllerImage(l, manifestDir, c.ControllerImage); err != nil {
			return fmt.Errorf("override controller image: %w", err)
		}
	}

	if c.NodeImage != "" {
		if err := OverrideNodeImage(l, manifestDir, c.NodeImage); err != nil {
			return fmt.Errorf("override node image: %w", err)
		}
	}

	l.Info("Applying unbounded-cni manifests", "path", manifestDir)

	if err := applyUnboundedManifests(ctx, l, c.Kubectl, manifestDir); err != nil {
		return fmt.Errorf("apply unbounded-cni manifests: %w", err)
	}

	l.Info("Applying unbounded-cni main gateway and site manifests")

	if err := os.MkdirAll(c.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	if err := ApplyGatewayPoolManifest(ctx, l, c.Kubectl, c.DataDir, GatewayPoolConfig{
		PoolName:  c.GatewayPoolName,
		AgentPool: c.GatewayPoolAgentPoolName,
		Type:      c.GatewayPoolType,
	}); err != nil {
		return fmt.Errorf("apply cluster (default) gateway pool: %w", err)
	}

	if err := ApplySiteManifest(ctx, l, c.Kubectl, c.DataDir, SiteConfig{
		SiteName:  "cluster",
		NodeCIDRs: clusterSiteCIDRs(c.ClusterVirtualNetwork),
		PodCIDRs:  []string{"100.124.0.0/16"},
	}); err != nil {
		return fmt.Errorf("apply cluster site: %w", err)
	}

	if err := ApplySiteGatewayPoolAssignmentManifest(ctx, l, c.Kubectl, c.DataDir, SiteGatewayPoolAssignment{
		SiteName:        "cluster",
		SiteNames:       []string{"cluster"},
		GatewayPoolName: c.GatewayPoolName,
	}); err != nil {
		return fmt.Errorf("apply cluster site gateway pool assignment: %w", err)
	}

	return nil
}

func applyUnboundedManifests(ctx context.Context, logger *slog.Logger, kubectl func(context.Context) *exec.Cmd, manifestDir string) error {
	logger.Info("Applying unbounded-cni manifests", "path", manifestDir)

	if err := kube.ApplyManifests(ctx, logger, kubectl, filepath.Join(manifestDir, "crds")); err != nil {
		return fmt.Errorf("apply unbounded-cni manifests: %w", err)
	}

	if err := kube.ApplyManifestsInDirectory(ctx, logger, kubectl, manifestDir); err != nil {
		return fmt.Errorf("apply unbounded-cni manifests: %w", err)
	}

	return nil
}

func clusterSiteCIDRs(vnet *armnetwork.VirtualNetwork) []string {
	var res []string

	for _, cidr := range vnet.Properties.AddressSpace.AddressPrefixes {
		res = append(res, *cidr)
	}

	return res
}
