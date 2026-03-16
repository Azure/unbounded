package azuredev

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/azsdk"
)

type Inventory struct {
	Machines []Machine
}

type Machine struct {
	Name      string
	IPAddress string
	Port      int
}

type InventoryGetter struct {
	AzureCli          *azsdk.ClientSet
	ResourceGroupName string
	LoadBalancerName  string
	SSHBackendPort    int32
}

func (g *InventoryGetter) Get(ctx context.Context) (*Inventory, error) {
	// For each frontend IP configuration get the public IP address. Then for each backend pool associated with
	// the frontend IP get the associated network interface for the attached VMSS. With that information
	// query the QueryInboundNatRulePortMappingRequest to get the port mapping to the VMSS instance.
	if g.SSHBackendPort == 0 {
		g.SSHBackendPort = 22
	}

	// Get the load balancer
	lbCli := g.AzureCli.NetworkLoadBalancersClientV2

	lb, err := lbCli.Get(ctx, g.ResourceGroupName, g.LoadBalancerName, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get load balancer: %w", err)
	}

	if lb.Properties == nil {
		return nil, fmt.Errorf("load balancer has no properties")
	}

	// Build a map of frontend IP cloudConfig ID -> public IP address
	frontendIPMap := make(map[string]string)

	for _, feConfig := range lb.Properties.FrontendIPConfigurations {
		if feConfig.ID == nil || feConfig.Properties == nil {
			continue
		}

		var publicIPAddress string

		// Get the public IP address if it exists
		if feConfig.Properties.PublicIPAddress != nil && feConfig.Properties.PublicIPAddress.ID != nil {
			pubIPID := *feConfig.Properties.PublicIPAddress.ID

			pubIP, err := g.getPublicIPFromID(ctx, pubIPID)
			if err != nil {
				return nil, fmt.Errorf("failed to get public IP: %w", err)
			}

			if pubIP.Properties != nil && pubIP.Properties.IPAddress != nil {
				publicIPAddress = *pubIP.Properties.IPAddress
			}
		}

		if publicIPAddress != "" {
			frontendIPMap[*feConfig.ID] = publicIPAddress
		}
	}

	// Build a map of inbound NAT rule name -> frontend IP address
	natRuleFrontendMap := make(map[string]string)

	for _, rule := range lb.Properties.InboundNatRules {
		if rule.Name == nil || rule.Properties == nil || rule.Properties.FrontendIPConfiguration == nil {
			continue
		}

		if rule.Properties.FrontendIPConfiguration.ID != nil {
			if ip, ok := frontendIPMap[*rule.Properties.FrontendIPConfiguration.ID]; ok {
				natRuleFrontendMap[*rule.Name] = ip
			}
		}
	}

	// Get port mappings for each backend pool
	inventory := &Inventory{
		Machines: []Machine{},
	}

	backendPoolCli := g.AzureCli.NetworkLoadBalancerBackendAddressPoolsClientV2

	for _, bePool := range lb.Properties.BackendAddressPools {
		if bePool.Name == nil {
			continue
		}

		backendPool, err := backendPoolCli.Get(ctx, g.ResourceGroupName, g.LoadBalancerName, *bePool.Name, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to get backend pool %s: %w", *bePool.Name, err)
		}

		if backendPool.Properties == nil || backendPool.Properties.LoadBalancerBackendAddresses == nil {
			continue
		}

		// For each backend address, query the port mappings
		for _, addr := range backendPool.Properties.LoadBalancerBackendAddresses {
			if addr.Properties == nil || addr.Properties.NetworkInterfaceIPConfiguration == nil {
				continue
			}

			ipConfigID := addr.Properties.NetworkInterfaceIPConfiguration.ID
			if ipConfigID == nil {
				continue
			}

			// Extract VM name from the IP configuration ID
			vmName := extractVMNameFromIPConfigID(*ipConfigID)

			mappingRequest := armnetwork.QueryInboundNatRulePortMappingRequest{
				IPConfiguration: &armnetwork.SubResource{
					ID: ipConfigID,
				},
			}

			p, err := lbCli.BeginListInboundNatRulePortMappings(ctx, g.ResourceGroupName, g.LoadBalancerName, *bePool.Name, mappingRequest, nil)
			if err != nil {
				continue
			}

			res, err := p.PollUntilDone(ctx, nil)
			if err != nil {
				continue
			}

			// Find the SSH port mapping (backend port 22)
			for _, m := range res.InboundNatRulePortMappings {
				if m.BackendPort == nil || m.FrontendPort == nil || m.InboundNatRuleName == nil {
					continue
				}

				// Only include mappings for SSH (backend port 22)
				if *m.BackendPort != g.SSHBackendPort {
					continue
				}

				// Get the frontend IP for this NAT rule
				frontendIP, ok := natRuleFrontendMap[*m.InboundNatRuleName]
				if !ok {
					continue
				}

				inventory.Machines = append(inventory.Machines, Machine{
					Name:      vmName,
					IPAddress: frontendIP,
					Port:      int(*m.FrontendPort),
				})
			}
		}
	}

	return inventory, nil
}

// getPublicIPFromID retrieves a public IP address resource from its full resource ID.
func (g *InventoryGetter) getPublicIPFromID(ctx context.Context, publicIPID string) (*armnetwork.PublicIPAddress, error) {
	// Parse the resource ID to extract resource group and name
	// Format: /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Network/publicIPAddresses/{name}
	parts := strings.Split(publicIPID, "/")
	if len(parts) < 9 {
		return nil, fmt.Errorf("invalid public IP ID format: %s", publicIPID)
	}

	var rgName, pipName string

	for i, part := range parts {
		if strings.EqualFold(part, "resourceGroups") && i+1 < len(parts) {
			rgName = parts[i+1]
		}

		if strings.EqualFold(part, "publicIPAddresses") && i+1 < len(parts) {
			pipName = parts[i+1]
		}
	}

	if rgName == "" || pipName == "" {
		return nil, fmt.Errorf("could not parse resource group or name from public IP ID: %s", publicIPID)
	}

	pubIPCli := g.AzureCli.NetworkPublicIPAddressesClientV2

	resp, err := pubIPCli.Get(ctx, rgName, pipName, nil)
	if err != nil {
		return nil, err
	}

	return &resp.PublicIPAddress, nil
}

// extractVMNameFromIPConfigID extracts the VM name from a VMSS IP configuration ID.
// Format: /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Compute/virtualMachineScaleSets/{vmss}/virtualMachines/{vmIndex}/networkInterfaces/{nic}/ipConfigurations/{ipconfig}.
func extractVMNameFromIPConfigID(ipConfigID string) string {
	parts := strings.Split(ipConfigID, "/")

	var vmssName, vmIndex string

	for i, part := range parts {
		if strings.EqualFold(part, "virtualMachineScaleSets") && i+1 < len(parts) {
			vmssName = parts[i+1]
		}

		if strings.EqualFold(part, "virtualMachines") && i+1 < len(parts) {
			vmIndex = parts[i+1]
		}
	}

	if vmssName != "" && vmIndex != "" {
		return fmt.Sprintf("%s_%s", vmssName, vmIndex)
	}

	return ipConfigID
}
