package cluster

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v4"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/infra"
)

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
			"unbounded-kube.io/unbounded-net-gateway": "true",
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
