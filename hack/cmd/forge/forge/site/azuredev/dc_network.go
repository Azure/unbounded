// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuredev

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"

	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/azsdk"
	"github.com/Azure/unbounded-kube/hack/cmd/forge/forge/infra"
)

const (
	datacenterVirtualNetworkName = "main"
)

type datacenterNetworkManager struct {
	azureCli               *azsdk.ClientSet
	resourceGroup          *armresources.ResourceGroup
	logger                 *slog.Logger
	mainVirtualNetworkCIDR string
	clusterResourceGroup   *armresources.ResourceGroup
	clusterVirtualNetwork  *armnetwork.VirtualNetwork
	peeringWithCluster     bool
}
type datacenterNetwork struct {
	routeTable           *armnetwork.RouteTable
	networkSecurityGroup *armnetwork.SecurityGroup
	virtualNetwork       *armnetwork.VirtualNetwork
}

func (m *datacenterNetworkManager) CreateOrUpdate(ctx context.Context) (*datacenterNetwork, error) {
	m.logger.Info("Applying datacenter route table")

	rt, err := m.createOrUpdateRouteTable(ctx)
	if err != nil {
		return nil, fmt.Errorf("create or update route table: %w", err)
	}

	m.logger.Info("Applying datacenter network security group")

	nsg, err := m.createOrUpdateNetworkSecurityGroup(ctx)
	if err != nil {
		return nil, fmt.Errorf("create or update network security group: %w", err)
	}

	m.logger.Info("Applying datacenter virtual network")

	vnet, err := m.createVirtualNetwork(ctx, rt, nsg)
	if err != nil {
		return nil, fmt.Errorf("create or update virtual network: %w", err)
	}

	if m.peeringWithCluster {
		m.logger.Info("Applying datacenter virtual network peering relationship with cluster")

		if err := m.createPeeringWithCluster(ctx, *m.resourceGroup.Name, m.clusterResourceGroup, m.clusterVirtualNetwork, vnet); err != nil {
			return nil, fmt.Errorf("create or update peering with cluster: %w", err)
		}
	}

	return &datacenterNetwork{
		routeTable:           rt,
		networkSecurityGroup: nsg,
		virtualNetwork:       vnet,
	}, nil
}

func (m *datacenterNetworkManager) GetVirtualNetwork(ctx context.Context) (*armnetwork.VirtualNetwork, error) {
	vnm := infra.VirtualNetworkManager{
		Client: m.azureCli.NetworkVirtualNetworksClientV2,
		Logger: m.logger,
	}

	vnet, err := vnm.Get(ctx, *m.resourceGroup.Name, datacenterVirtualNetworkName)
	if err != nil {
		return nil, err
	}

	return vnet, nil
}

func (m *datacenterNetworkManager) createOrUpdateRouteTable(ctx context.Context) (*armnetwork.RouteTable, error) {
	desired := armnetwork.RouteTable{
		Name:     to.Ptr("main"),
		Location: m.resourceGroup.Location,
		Properties: &armnetwork.RouteTablePropertiesFormat{
			DisableBgpRoutePropagation: to.Ptr(false),
		},
	}

	rtm := infra.RouteTableManager{
		Client: m.azureCli.NetworkRouteTablesClientV2,
		Logger: m.logger,
	}

	applied, err := rtm.CreateOrUpdate(ctx, *m.resourceGroup.Name, desired)
	if err != nil {
		return nil, err
	}

	return applied, nil
}

func (m *datacenterNetworkManager) createOrUpdateNetworkSecurityGroup(ctx context.Context) (*armnetwork.SecurityGroup, error) {
	desired := armnetwork.SecurityGroup{
		Name:     to.Ptr("main"),
		Location: m.resourceGroup.Location,
		Tags:     map[string]*string{},
		Properties: &armnetwork.SecurityGroupPropertiesFormat{
			SecurityRules: []*armnetwork.SecurityRule{
				{
					Name: to.Ptr("AllowSSHFromCorpNetPublic"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Priority:                 to.Ptr(int32(150)),
						Access:                   to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						Protocol:                 to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
						SourcePortRange:          to.Ptr("*"),
						DestinationPortRanges:    to.SliceOfPtrs("22"),
						SourceAddressPrefix:      to.Ptr("CorpNetPublic"),
						DestinationAddressPrefix: to.Ptr("*"),
					},
				},

				// This rule is required for SSH to nodes for provisioning.
				{
					Name: to.Ptr("AllowSSHFromAzureCloud"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Priority:                 to.Ptr(int32(151)),
						Access:                   to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						Protocol:                 to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						SourcePortRange:          to.Ptr("*"),
						DestinationPortRanges:    to.SliceOfPtrs("22"),
						SourceAddressPrefix:      to.Ptr("AzureCloud"),
						DestinationAddressPrefix: to.Ptr("*"),
					},
				},

				// This rule is required for the kube-apiserver to establish connections to the kubelet on the gateway
				// nodes. It works in conjunction with the peering relationship setup between the datacenter and the
				// cluster which allows cross virtual network traffic from the cluster to the datacenter.
				{
					Name: to.Ptr("AllowKubeletFromAzureCloud"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Priority:                 to.Ptr(int32(152)),
						Access:                   to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						Protocol:                 to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
						SourcePortRange:          to.Ptr("*"),
						DestinationPortRanges:    to.SliceOfPtrs("10250"),
						SourceAddressPrefix:      to.Ptr("AzureCloud"),
						DestinationAddressPrefix: to.Ptr("*"),
					},
				},
			},
		},
	}

	nsgm := infra.NetworkSecurityGroupManager{
		Client: m.azureCli.NetworkSecurityGroupsClientV2,
		Logger: m.logger,
	}

	applied, err := nsgm.CreateOrUpdate(ctx, *m.resourceGroup.Name, desired)
	if err != nil {
		return nil, err
	}

	return applied, nil
}

func (m *datacenterNetworkManager) createVirtualNetwork(
	ctx context.Context,
	rt *armnetwork.RouteTable,
	nsg *armnetwork.SecurityGroup,
) (*armnetwork.VirtualNetwork, error) {
	if rt == nil {
		return nil, fmt.Errorf("route table is nil")
	}

	if nsg == nil {
		return nil, fmt.Errorf("network security group is nil")
	}

	desired := armnetwork.VirtualNetwork{
		Name:     to.Ptr(datacenterVirtualNetworkName),
		Location: m.resourceGroup.Location,
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{
				AddressPrefixes: []*string{to.Ptr(m.mainVirtualNetworkCIDR)},
			},
			EnableDdosProtection:        to.Ptr(false),
			PrivateEndpointVNetPolicies: to.Ptr(armnetwork.PrivateEndpointVNetPoliciesDisabled),
			Subnets: []*armnetwork.Subnet{
				{
					Name: to.Ptr("main"),
					Properties: &armnetwork.SubnetPropertiesFormat{
						AddressPrefix:         to.Ptr(m.mainVirtualNetworkCIDR),
						DefaultOutboundAccess: to.Ptr(false),
						RouteTable: &armnetwork.RouteTable{
							ID: rt.ID,
						},
						NetworkSecurityGroup: &armnetwork.SecurityGroup{
							ID: nsg.ID,
						},
					},
				},
			},
		},
	}

	vnm := infra.VirtualNetworkManager{
		Client: m.azureCli.NetworkVirtualNetworksClientV2,
		Logger: m.logger,
	}

	applied, err := vnm.CreateOrUpdate(ctx, *m.resourceGroup.Name, desired)
	if err != nil {
		return nil, err
	}

	return applied, nil
}

func (m *datacenterNetworkManager) createPeeringWithCluster(
	ctx context.Context,
	datacenterName string,
	clusterNodesResourceGroup *armresources.ResourceGroup,
	clusterVirtualNetwork,
	datacenterVirtualNetwork *armnetwork.VirtualNetwork,
) error {
	if clusterVirtualNetwork == nil {
		return fmt.Errorf("cluster virtual network is nil")
	}

	vnpm := infra.VirtualNetworkPeeringsManager{
		Client: m.azureCli.NetworkVirtualNetworkPeeringsClientV2,
		Logger: m.logger,
	}

	desiredClusterToDC := armnetwork.VirtualNetworkPeering{
		Name: to.Ptr(datacenterName),
		Properties: &armnetwork.VirtualNetworkPeeringPropertiesFormat{
			RemoteVirtualNetwork: &armnetwork.SubResource{
				ID: datacenterVirtualNetwork.ID,
			},
			AllowVirtualNetworkAccess: to.Ptr(true),
		},
	}

	if _, err := vnpm.CreateOrUpdate(ctx, *clusterNodesResourceGroup.Name, *clusterVirtualNetwork.Name, desiredClusterToDC); err != nil {
		return fmt.Errorf("create or update cluster to datacenter virtual network peering: %w", err)
	}

	desiredDCToCluster := armnetwork.VirtualNetworkPeering{
		Name: to.Ptr("cluster"),
		Properties: &armnetwork.VirtualNetworkPeeringPropertiesFormat{
			RemoteVirtualNetwork: &armnetwork.SubResource{
				ID: clusterVirtualNetwork.ID,
			},
			AllowVirtualNetworkAccess: to.Ptr(true),
		},
	}

	if _, err := vnpm.CreateOrUpdate(ctx, *m.resourceGroup.Name, *datacenterVirtualNetwork.Name, desiredDCToCluster); err != nil {
		return fmt.Errorf("create or update datacenter to cluster virtual network peering: %w", err)
	}

	return nil
}
