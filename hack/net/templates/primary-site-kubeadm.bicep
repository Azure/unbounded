// primary-site-kubeadm.bicep - Deploy a kubeadm-based primary site with identity, networking, and VMSS pools
//
// Orchestrates deployment of:
//   1. User-assigned managed identity (kubelet identity)
//   2. Networking (VNet, subnets, NSG, outbound LB, public IPs) via external-site-networking.bicep
//   3. API server public IP and load balancer for the control plane
//   4. Optional Bastion via external-site-bastion.bicep
//   5. VMSS pools (control plane, gateway, worker) via external-site-vmss.bicep
//
// Each pool object must contain: name, sku, instanceCount, computerNamePrefix, enablePublicIPPerVM

// --- Site parameters ---

@description('Site name used as prefix for all resources')
param siteName string

@description('Azure region')
param location string = resourceGroup().location

// --- Networking parameters ---

@description('IPv4 address range for the virtual network (CIDR notation)')
param ipv4Range string

@description('IPv6 address range for the virtual network. Set to "generate" for deterministic ULA /48, or empty to disable.')
param ipv6Range string = 'generate'

// Generate a deterministic ULA IPv6 /48 from deployment context
var ipv6Guid = guid(subscription().subscriptionId, resourceGroup().name, location, siteName)
var ipv6Suffix = substring(ipv6Guid, 26, 10)
var defaultIpv6Range = 'fd${substring(ipv6Suffix, 0, 2)}:${substring(ipv6Suffix, 2, 4)}:${substring(ipv6Suffix, 6, 4)}::/48'
var effectiveIpv6Range = ipv6Range == 'generate' ? defaultIpv6Range : ipv6Range

@description('Deploy Azure Bastion Standard into the VNet')
param enableBastion bool = true

@description('Allocated outbound ports per VM on the load balancer')
param portsPerVM int = 1024

@description('Number of outbound IPv4 public IPs')
param outboundIpCount int = 1

// --- Pool definitions ---

@description('Array of VMSS pool objects. Each must have: name (string), sku (string), instanceCount (int), computerNamePrefix (string), enablePublicIPPerVM (bool). Optional: allowedHostPorts (string), allowedHostPortsPriority (int), isControlPlane (bool)')
param pools array

@description('Object keyed by pool name, where each value is the base64-encoded custom data string for that pool')
@secure()
param poolCustomDatas object

// --- Shared VMSS parameters ---

@description('Admin username')
param adminUsername string = 'azureuser'

@description('Admin password (empty if not using password auth)')
@secure()
param adminPassword string = ''

@description('SSH public key (empty if not using SSH auth)')
param sshPublicKey string = ''

// --- Managed Identity ---

var networkContributorRoleId = '4d97b98b-1d4f-4787-a291-c67834d212e7'

resource clusterIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: '${siteName}-identity'
  location: location
}

// --- API Server Load Balancer ---

resource apiServerPip 'Microsoft.Network/publicIPAddresses@2024-05-01' = {
  name: '${siteName}-apiserver-pip'
  location: location
  sku: { name: 'Standard' }
  properties: {
    publicIPAllocationMethod: 'Static'
    publicIPAddressVersion: 'IPv4'
  }
}

var apiLbName = '${siteName}-apiserver-lb'

resource apiLb 'Microsoft.Network/loadBalancers@2024-05-01' = {
  name: apiLbName
  location: location
  sku: { name: 'Standard' }
  properties: {
    frontendIPConfigurations: [
      {
        name: 'apiserver-frontend'
        properties: {
          publicIPAddress: { id: apiServerPip.id }
        }
      }
    ]
    backendAddressPools: [
      { name: '${siteName}-control-plane' }
    ]
    probes: [
      {
        name: 'apiserver-health'
        properties: {
          protocol: 'Https'
          port: 6443
          requestPath: '/healthz'
          intervalInSeconds: 5
          numberOfProbes: 2
          probeThreshold: 2
        }
      }
    ]
    loadBalancingRules: [
      {
        name: 'apiserver-rule'
        properties: {
          frontendIPConfiguration: {
            id: resourceId('Microsoft.Network/loadBalancers/frontendIPConfigurations', apiLbName, 'apiserver-frontend')
          }
          backendAddressPool: {
            id: resourceId('Microsoft.Network/loadBalancers/backendAddressPools', apiLbName, '${siteName}-control-plane')
          }
          probe: {
            id: resourceId('Microsoft.Network/loadBalancers/probes', apiLbName, 'apiserver-health')
          }
          protocol: 'Tcp'
          frontendPort: 6443
          backendPort: 6443
          enableFloatingIP: false
          idleTimeoutInMinutes: 30
          enableTcpReset: true
          disableOutboundSnat: true
        }
      }
    ]
    // Outbound rule so control plane VMs can reach the internet (download binaries)
    outboundRules: [
      {
        name: 'control-plane-outbound'
        properties: {
          frontendIPConfigurations: [
            {
              id: resourceId('Microsoft.Network/loadBalancers/frontendIPConfigurations', apiLbName, 'apiserver-frontend')
            }
          ]
          backendAddressPool: {
            id: resourceId('Microsoft.Network/loadBalancers/backendAddressPools', apiLbName, '${siteName}-control-plane')
          }
          protocol: 'All'
          allocatedOutboundPorts: portsPerVM
        }
      }
    ]
  }
}

// --- Networking (VNet, subnets, NSG, outbound LB) ---

module networking 'external-site-networking.bicep' = {
  name: '${siteName}-networking'
  params: {
    siteName: siteName
    location: location
    ipv4Range: ipv4Range
    ipv6Range: effectiveIpv6Range
    enableBastion: enableBastion
    portsPerVM: portsPerVM
    outboundIpCount: outboundIpCount
    identityPrincipalId: clusterIdentity.properties.principalId
  }
}

// RBAC: Network Contributor on the API server public IP
resource apiPipRoleAssignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(apiServerPip.id, clusterIdentity.id, networkContributorRoleId)
  scope: apiServerPip
  properties: {
    principalId: clusterIdentity.properties.principalId
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', networkContributorRoleId)
    principalType: 'ServicePrincipal'
  }
}

// --- Bastion ---

module bastion 'external-site-bastion.bicep' = if (enableBastion) {
  name: '${siteName}-bastion'
  params: {
    siteName: siteName
    location: location
    vnetId: networking.outputs.vnetId
  }
}

// --- VMSS pools ---

// Control plane pools join the API server LB backend pool; others join the outbound LB
module vmssPool 'external-site-vmss.bicep' = [
  for pool in pools: {
    name: '${siteName}-${pool.name}'
    params: {
      vmssName: '${siteName}-${pool.name}'
      location: location
      vmSku: pool.sku
      instanceCount: pool.instanceCount
      subnetId: networking.outputs.defaultSubnetId
      identityId: clusterIdentity.id
      nsgId: networking.outputs.nsgId
      customDataBase64: poolCustomDatas[pool.name]
      computerNamePrefix: pool.computerNamePrefix
      adminUsername: adminUsername
      adminPassword: adminPassword
      sshPublicKey: sshPublicKey
      enablePublicIPPerVM: pool.enablePublicIPPerVM
      useIPv6: networking.outputs.useIPv6
      // Control plane pools get the API server LB; non-public-IP pools get outbound LB
      loadBalancerBackendPoolIdv4: (pool.?isControlPlane ?? false) ? apiLb.properties.backendAddressPools[0].id : (pool.enablePublicIPPerVM ? '' : networking.outputs.backendPoolIdv4)
      loadBalancerBackendPoolIdv6: (pool.?isControlPlane ?? false) ? '' : (pool.enablePublicIPPerVM ? '' : networking.outputs.backendPoolIdv6)
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
output identityId string = clusterIdentity.id
output identityPrincipalId string = clusterIdentity.properties.principalId
output identityClientId string = clusterIdentity.properties.clientId
output apiServerPublicIP string = apiServerPip.properties.ipAddress
output apiServerLbName string = apiLb.name
output outboundLbName string = networking.outputs.lbName
output vmssNames array = [for (pool, i) in pools: vmssPool[i].outputs.vmssName]
