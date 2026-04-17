// external-site-networking.bicep - Provision networking infrastructure for an external site
//
// Creates a VNet with a default subnet and optional AzureBastionSubnet, an outbound
// load balancer with public IPs (dual-stack if IPv6 range is provided), and NSG/RBAC.

@description('Site name used as prefix for all resources')
param siteName string

@description('IPv4 address range for the virtual network (CIDR notation)')
param ipv4Range string

@description('IPv6 address range for the virtual network (CIDR notation, empty to disable IPv6)')
param ipv6Range string = ''

@description('Deploy Azure Bastion Standard into the VNet')
param enableBastion bool = true

@description('Create bidirectional peering with the primary site VNet')
param peerPrimaryVnet bool = false

@description('Primary site VNet name used when peerPrimaryVnet is true')
param primaryVnetName string = ''

@description('Primary site VNet resource group used when peerPrimaryVnet is true')
param primaryVnetResourceGroup string = ''

@description('Subscription ID of the primary site VNet, when it lives in a different subscription. Empty means same subscription.')
param primaryVnetSubscriptionId string = ''

@description('Allocated outbound ports per VM on the load balancer')
param portsPerVM int = 1024

@description('Number of outbound IPv4 public IPs')
param outboundIpCount int = 1

@description('Principal (object) ID of the kubelet managed identity, used for role assignment naming')
param identityPrincipalId string = ''

@description('Azure region')
param location string = resourceGroup().location

// --- Derived variables ---

var useIPv6 = !empty(ipv6Range)
var enablePrimaryVnetPeering = peerPrimaryVnet && !empty(primaryVnetName) && !empty(primaryVnetResourceGroup)
var effectivePrimarySub = !empty(primaryVnetSubscriptionId) ? primaryVnetSubscriptionId : subscription().subscriptionId
var primaryVnetId = enablePrimaryVnetPeering ? resourceId(effectivePrimarySub, primaryVnetResourceGroup, 'Microsoft.Network/virtualNetworks', primaryVnetName) : ''

// Subnet calculations:
//   AzureBastionSubnet: first /26 of the IPv4 range
//   default subnet:     second half of IPv4 range; first /64 of IPv6 range (if enabled)
var ipv4PrefixLength = int(split(ipv4Range, '/')[1])
var defaultSubnetV4 = cidrSubnet(ipv4Range, ipv4PrefixLength + 1, 1)
var bastionSubnetV4 = cidrSubnet(ipv4Range, 26, 0)

var defaultSubnetV6 = useIPv6 ? cidrSubnet(ipv6Range, 64, 0) : ''

var vnetAddressSpace = useIPv6 ? [ipv4Range, ipv6Range] : [ipv4Range]
var defaultSubnetPrefixes = useIPv6 ? [defaultSubnetV4, defaultSubnetV6] : [defaultSubnetV4]

var lbName = '${siteName}-lb'
var backendPoolName = '${siteName}-outbound'

var outboundV4Frontends = [for i in range(0, outboundIpCount): {
  name: i == 0 ? 'outbound-v4' : 'outbound-v4-${i}'
  properties: {
    publicIPAddress: { id: outboundPips[i].id }
  }
}]

var outboundV4FrontendRefs = [for i in range(0, outboundIpCount): {
  id: resourceId(
    'Microsoft.Network/loadBalancers/frontendIPConfigurations',
    lbName,
    i == 0 ? 'outbound-v4' : 'outbound-v4-${i}'
  )
}]

// --- Public IPs ---

resource outboundPips 'Microsoft.Network/publicIPAddresses@2024-05-01' = [for i in range(0, outboundIpCount): {
  name: i == 0 ? '${siteName}-outbound-pip4' : '${siteName}-outbound-pip4-${i}'
  location: location
  sku: { name: 'Standard' }
  properties: {
    publicIPAllocationMethod: 'Static'
    publicIPAddressVersion: 'IPv4'
  }
}]

resource pip6 'Microsoft.Network/publicIPAddresses@2024-05-01' = if (useIPv6) {
  name: '${siteName}-outbound-pip6'
  location: location
  sku: { name: 'Standard' }
  properties: {
    publicIPAllocationMethod: 'Static'
    publicIPAddressVersion: 'IPv6'
  }
}

// --- Virtual Network ---

resource vnet 'Microsoft.Network/virtualNetworks@2024-05-01' = {
  name: '${siteName}-vnet'
  location: location
  properties: {
    addressSpace: {
      addressPrefixes: vnetAddressSpace
    }
    subnets: union(
      enableBastion
        ? [
            {
              name: 'AzureBastionSubnet'
              properties: {
                addressPrefix: bastionSubnetV4
              }
            }
          ]
        : [],
      [
        {
          name: 'default'
          properties: {
            addressPrefixes: defaultSubnetPrefixes
          }
        }
      ]
    )
  }
}

resource siteToPrimaryPeering 'Microsoft.Network/virtualNetworks/virtualNetworkPeerings@2024-05-01' = if (enablePrimaryVnetPeering) {
  parent: vnet
  name: '${siteName}-to-${primaryVnetName}'
  properties: {
    allowVirtualNetworkAccess: true
    allowForwardedTraffic: true
    allowGatewayTransit: false
    useRemoteGateways: false
    remoteVirtualNetwork: {
      id: primaryVnetId
    }
  }
}

// --- Load Balancer ---

resource lb 'Microsoft.Network/loadBalancers@2024-05-01' = {
  name: lbName
  location: location
  sku: { name: 'Standard' }
  properties: {
    frontendIPConfigurations: union(
      outboundV4Frontends,
      useIPv6
        ? [
            {
              name: 'outbound-v6'
              properties: {
                publicIPAddress: { id: pip6.id }
              }
            }
          ]
        : []
    )
    backendAddressPools: union([{ name: '${backendPoolName}-v4' }],
      useIPv6 ? [{ name: '${backendPoolName}-v6' }] : [])
    outboundRules: union(
      [
        {
          name: 'outbound-v4'
          properties: {
            frontendIPConfigurations: outboundV4FrontendRefs
            backendAddressPool: {
              id: resourceId(
                'Microsoft.Network/loadBalancers/backendAddressPools',
                lbName,
                '${backendPoolName}-v4'
              )
            }
            protocol: 'All'
            allocatedOutboundPorts: portsPerVM
          }
        }
      ],
      useIPv6
        ? [
            {
              name: 'outbound-v6'
              properties: {
                frontendIPConfigurations: [
                  {
                    id: resourceId(
                      'Microsoft.Network/loadBalancers/frontendIPConfigurations',
                      lbName,
                      'outbound-v6'
                    )
                  }
                ]
                backendAddressPool: {
                  id: resourceId(
                    'Microsoft.Network/loadBalancers/backendAddressPools',
                    lbName,
                    '${backendPoolName}-v6'
                  )
                }
                protocol: 'All'
                allocatedOutboundPorts: portsPerVM
              }
            }
          ]
        : []
    )
  }
}

// --- Network Security Group ---

resource nsg 'Microsoft.Network/networkSecurityGroups@2024-05-01' = {
  name: '${siteName}-nsg'
  location: location
}


// --- RBAC: Network Contributor for kubelet identity on site network resources ---

// Network Contributor built-in role ID
var networkContributorRoleId = '4d97b98b-1d4f-4787-a291-c67834d212e7'

resource vnetRoleAssignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (!empty(identityPrincipalId)) {
  name: guid(vnet.id, identityPrincipalId, networkContributorRoleId)
  scope: vnet
  properties: {
    principalId: identityPrincipalId
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', networkContributorRoleId)
    principalType: 'ServicePrincipal'
  }
}

resource nsgRoleAssignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (!empty(identityPrincipalId)) {
  name: guid(nsg.id, identityPrincipalId, networkContributorRoleId)
  scope: nsg
  properties: {
    principalId: identityPrincipalId
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', networkContributorRoleId)
    principalType: 'ServicePrincipal'
  }
}

resource outboundPipRoleAssignments 'Microsoft.Authorization/roleAssignments@2022-04-01' = [for i in range(0, outboundIpCount): if (!empty(identityPrincipalId)) {
  name: guid(outboundPips[i].id, identityPrincipalId, networkContributorRoleId)
  scope: outboundPips[i]
  properties: {
    principalId: identityPrincipalId
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', networkContributorRoleId)
    principalType: 'ServicePrincipal'
  }
}]

resource outboundPip6RoleAssignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (!empty(identityPrincipalId) && useIPv6) {
  name: guid(pip6.id, identityPrincipalId, networkContributorRoleId)
  scope: pip6
  properties: {
    principalId: identityPrincipalId
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', networkContributorRoleId)
    principalType: 'ServicePrincipal'
  }
}

// --- Outputs ---

output vnetId string = vnet.id
output vnetName string = vnet.name
output defaultSubnetId string = '${vnet.id}/subnets/default'
output nsgId string = nsg.id
output nsgName string = nsg.name
output backendPoolIdv4 string = resourceId(
  'Microsoft.Network/loadBalancers/backendAddressPools',
  lbName,
  '${backendPoolName}-v4'
)
output backendPoolIdv6 string = resourceId(
  'Microsoft.Network/loadBalancers/backendAddressPools',
  lbName,
  '${backendPoolName}-v6'
)
output lbName string = lb.name
output useIPv6 bool = useIPv6
output outboundPipV4Address string = outboundPips[0].properties.ipAddress
output outboundPipV6Address string = useIPv6 ? pip6!.properties.ipAddress : ''
