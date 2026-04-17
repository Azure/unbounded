// external-site.bicep - Deploy a complete external site with networking and VMSS pools
//
// Orchestrates deployment of:
//   1. Networking (VNet, subnets, LB, public IPs) via external-site-networking.bicep
//   2. Optional Bastion via external-site-bastion.bicep
//   3. VMSS pools defined in the pools array via external-site-vmss.bicep
//
// Each pool object must contain: name, sku, instanceCount, computerNamePrefix, enablePublicIPPerVM

// --- Site parameters ---

@description('Site name used as prefix for all resources')
param siteName string

@description('Azure region')
param location string = resourceGroup().location

// --- Networking parameters ---

@description('IPv4 address range for the virtual network (CIDR notation). Must not overlap with any other VNets used by or peered with the cluster or other sites!')
param ipv4Range string

@description('IPv6 address range for the virtual network (CIDR notation). Set to "generate" to auto-create a deterministic ULA /48, or empty string to disable IPv6.')
param ipv6Range string = 'generate'

// Generate a deterministic ULA IPv6 /48 from deployment context
var ipv6Guid = guid(subscription().subscriptionId, resourceGroup().name, location, siteName)
var ipv6Suffix = substring(ipv6Guid, 26, 10)
var defaultIpv6Range = 'fd${substring(ipv6Suffix, 0, 2)}:${substring(ipv6Suffix, 2, 4)}:${substring(ipv6Suffix, 6, 4)}::/48'
var effectiveIpv6Range = ipv6Range == 'generate' ? defaultIpv6Range : ipv6Range

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

// --- Pool definitions ---

@description('Array of VMSS pool objects. Each must have: name (string), sku (string), instanceCount (int), computerNamePrefix (string), enablePublicIPPerVM (bool). Optional: allowedHostPorts (string, e.g. "51820-52999"), allowedHostPortsPriority (int, 200-399)')
param pools array

@description('Object keyed by pool name, where each value is the base64-encoded custom data string for that pool. Contains bootstrap tokens so must be secure.')
@secure()
param poolCustomDatas object

// --- Shared VMSS parameters ---

@description('Full resource ID of the user-assigned managed identity (kubelet identity)')
param identityId string

@description('Principal (object) ID of the kubelet managed identity, used for role assignment naming')
param identityPrincipalId string = ''

@description('Admin username')
param adminUsername string = 'azureuser'

@description('Admin password (empty if not using password auth)')
@secure()
param adminPassword string = ''

@description('SSH public key (empty if not using SSH auth)')
param sshPublicKey string = ''

// --- Networking ---

module networking 'external-site-networking.bicep' = {
  name: '${siteName}-networking'
  params: {
    siteName: siteName
    location: location
    ipv4Range: ipv4Range
    ipv6Range: effectiveIpv6Range
    enableBastion: enableBastion
    peerPrimaryVnet: peerPrimaryVnet
    primaryVnetName: primaryVnetName
    primaryVnetResourceGroup: primaryVnetResourceGroup
    primaryVnetSubscriptionId: primaryVnetSubscriptionId
    portsPerVM: portsPerVM
    outboundIpCount: outboundIpCount
    identityPrincipalId: identityPrincipalId
  }
}

var effectivePrimarySubId = !empty(primaryVnetSubscriptionId) ? primaryVnetSubscriptionId : subscription().subscriptionId

module primaryPeering 'external-site-primary-peering.bicep' = if (peerPrimaryVnet && !empty(primaryVnetName) && !empty(primaryVnetResourceGroup)) {
  name: '${siteName}-primary-peering'
  scope: resourceGroup(effectivePrimarySubId, primaryVnetResourceGroup)
  params: {
    primaryVnetName: primaryVnetName
    remoteVnetId: networking.outputs.vnetId
    siteName: siteName
  }
}

module bastion 'external-site-bastion.bicep' = if (enableBastion) {
  name: '${siteName}-bastion'
  params: {
    siteName: siteName
    location: location
    vnetId: networking.outputs.vnetId
  }
}

// --- VMSS pools ---

module vmssPool 'external-site-vmss.bicep' = [
  for pool in pools: {
    name: '${siteName}-${pool.name}'
    params: {
      vmssName: '${siteName}-${pool.name}'
      location: location
      vmSku: pool.sku
      instanceCount: pool.instanceCount
      subnetId: networking.outputs.defaultSubnetId
      identityId: identityId
      nsgId: networking.outputs.nsgId
      customDataBase64: poolCustomDatas[pool.name]
      computerNamePrefix: pool.computerNamePrefix
      adminUsername: adminUsername
      adminPassword: adminPassword
      sshPublicKey: sshPublicKey
      enablePublicIPPerVM: pool.enablePublicIPPerVM
      useIPv6: networking.outputs.useIPv6
      loadBalancerBackendPoolIdv4: pool.enablePublicIPPerVM ? '' : networking.outputs.backendPoolIdv4
      loadBalancerBackendPoolIdv6: pool.enablePublicIPPerVM ? '' : networking.outputs.backendPoolIdv6
      allowedHostPorts: pool.?allowedHostPorts ?? ''
      allowedHostPortsPriority: pool.?allowedHostPortsPriority ?? 200
    }
  }
]

// --- Outputs ---

output vnetId string = networking.outputs.vnetId
output vnetName string = networking.outputs.vnetName
output defaultSubnetId string = networking.outputs.defaultSubnetId
output nsgId string = networking.outputs.nsgId
output nsgName string = networking.outputs.nsgName
output lbName string = networking.outputs.lbName
output backendPoolIdv4 string = networking.outputs.backendPoolIdv4
output backendPoolIdv6 string = networking.outputs.backendPoolIdv6
output outboundPipV4Address string = networking.outputs.outboundPipV4Address
output vmssNames array = [for (pool, i) in pools: vmssPool[i].outputs.vmssName]
