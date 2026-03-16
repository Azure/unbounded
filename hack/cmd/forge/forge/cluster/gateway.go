package cluster

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"math/big"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v4"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/infra"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/kube"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/unboundedcni"
)

type AddGatewayPool struct {
	AgentPoolsClient *armcontainerservice.AgentPoolsClient
	Logger           *slog.Logger
	Kubectl          func(context.Context) *exec.Cmd
	ResourceGroup    string
	SiteName         string
	PoolSKU          string
	PoolNodeCount    int32
	DataDir          string
}

func (m *AddGatewayPool) Do(ctx context.Context) error {
	m.Logger.Info("Adding gateway agent pool", "cluster", m.ResourceGroup, "site", m.SiteName)

	poolProfile := newGatewayAgentPool(m.SiteName, m.PoolSKU, m.PoolNodeCount)

	pool := armcontainerservice.AgentPool{
		Name:       poolProfile.Name,
		Properties: agentPoolProfileToAgentPoolProfileProperties(poolProfile),
	}

	m.Logger.Info("Adding gateway agent pool for site", "site", m.SiteName, "pool", *pool.Name)

	r, err := m.AgentPoolsClient.BeginCreateOrUpdate(ctx, m.ResourceGroup, m.ResourceGroup, *pool.Name, pool, nil)
	if err != nil {
		return fmt.Errorf("begin create or update agent pool: %w", err)
	}

	if _, err := r.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("poll create or update agent pool: %w", err)
	}

	gpc := unboundedcni.GatewayPoolConfig{
		PoolName:  m.SiteName,
		AgentPool: *pool.Name,
		Type:      "External",
	}

	gpcManifest, err := unboundedcni.RenderGatewayPoolManifest(gpc)
	if err != nil {
		return fmt.Errorf("render gateway pool manifest: %w", err)
	}

	manifestPath := filepath.Join(m.DataDir, fmt.Sprintf("gatewaypool-%s.yaml", m.SiteName))

	if err := kube.WriteAndApplyManifest(ctx, m.Logger, m.Kubectl, manifestPath, gpcManifest); err != nil {
		return fmt.Errorf("install gateway pool manifest: %w", err)
	}

	return nil
}

func agentPoolProfileToAgentPoolProfileProperties(poolProfile armcontainerservice.ManagedClusterAgentPoolProfile) *armcontainerservice.ManagedClusterAgentPoolProfileProperties {
	return &armcontainerservice.ManagedClusterAgentPoolProfileProperties{
		Count:              poolProfile.Count,
		EnableNodePublicIP: poolProfile.EnableNodePublicIP,
		Mode:               poolProfile.Mode,
		NetworkProfile:     poolProfile.NetworkProfile,
		NodeLabels:         poolProfile.NodeLabels,
		NodeTaints:         poolProfile.NodeTaints,
		OSType:             poolProfile.OSType,
		OSSKU:              poolProfile.OSSKU,
		VMSize:             poolProfile.VMSize,
	}
}

func newGatewayAgentPool(site, sku string, nodeCount int32) armcontainerservice.ManagedClusterAgentPoolProfile {
	// AKS node pool names are restricted to 1-12 alphanumeric chars. This makes it difficult to use common
	// names for datacenter sites to name the pool. This function takes a site name and spits out a
	// deterministic pool name for a site.
	name := getGatewayPoolName(site)

	return infra.NewManagedClusterAgentPool(name, sku, nodeCount).
		NodePublicIP().
		User().
		WithAllowedPort(armcontainerservice.PortRange{
			PortStart: to.Ptr(int32(51820)),
			PortEnd:   to.Ptr(int32(51899)),
			Protocol:  to.Ptr(armcontainerservice.ProtocolUDP),
		}).
		WithNodeLabels(map[string]string{
			"unbounded.aks.azure.com/gateway":      "true",
			"unbounded.aks.azure.com/site-gateway": site,
		}).
		WithNodeTaints([]string{
			"CriticalAddonsOnly=true:NoSchedule",
		}).
		Build()
}

func getGatewayPoolName(site string) string {
	if site != "main" {
		hash := sha256.Sum256([]byte(strings.ToLower(site)))
		site = new(big.Int).SetBytes(hash[:]).Text(36)[:10]
	}

	return fmt.Sprintf("gw%s", site)
}
