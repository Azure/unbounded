// primary-site.bicep - Deploy a primary AKS site with identity, networking, bastion, and AKS

@description('Site name used as prefix for resources')
param siteName string

@description('AKS cluster name')
param clusterName string

@description('AKS DNS prefix')
param dnsPrefix string

@description('Azure region')
param location string = resourceGroup().location

@description('Kubernetes version (empty for latest default)')
param kubernetesVersion string = ''

@description('IPv4 range for the virtual network')
param ipv4Range string

@description('IPv6 range for the virtual network. Set to "generate" for deterministic ULA /48, or empty to disable.')
param ipv6Range string = 'generate'

@description('AKS pod IPv4 CIDR (empty to use provider default)')
param podIpv4Cidr string = ''

@description('AKS pod IPv6 CIDR (empty to disable dual-stack pod CIDR)')
param podIpv6Cidr string = ''

@description('AKS service IPv4 CIDR (empty to use provider default)')
param serviceIpv4Cidr string = ''

@description('AKS service IPv6 CIDR (empty to disable dual-stack service CIDR)')
param serviceIpv6Cidr string = ''

@description('Deploy Azure Bastion Standard')
param enableBastion bool = true

@description('Deploy Azure Managed Prometheus and Grafana for cluster monitoring')
param enableMonitoring bool = false

@description('Entra ID object ID to grant Grafana Admin role (empty to skip)')
param grafanaAdminObjectId string = ''

@description('SSH public key for AKS Linux profile')
@secure()
param sshPublicKey string

@description('Admin username for AKS Linux profile')
param adminUsername string = 'azureuser'

@description('AKS network plugin. Set to "none" to omit and use AKS default.')
param networkPlugin string = 'none'

@description('AKS network plugin mode. Set to "none" to omit.')
param networkPluginMode string = 'none'

@description('System pool VM size')
param systemPoolVmSize string = 'Standard_D2ads_v5'

@description('Gateway pool VM size')
param gatewayPoolVmSize string = 'Standard_D2ads_v5'

@description('User pool VM size')
param userPoolVmSize string = 'Standard_D2ads_v5'

@description('System pool node count')
param systemPoolCount int = 2

@description('Gateway pool node count')
param gatewayPoolCount int = 2

@description('User pool node count')
param userPoolCount int = 2

@description('Number of managed outbound public IP addresses for the load balancer (0 to use existing outbound PIPs)')
param managedOutboundIPCount int = 0

@description('Number of allocated outbound SNAT ports per node on the load balancer')
param allocatedOutboundPorts int = 1024

var ipv6Guid = guid(subscription().subscriptionId, resourceGroup().name, location, siteName)
var ipv6Suffix = substring(ipv6Guid, 26, 10)
var defaultIpv6Range = 'fd${substring(ipv6Suffix, 0, 2)}:${substring(ipv6Suffix, 2, 4)}:${substring(ipv6Suffix, 6, 4)}::/48'
var effectiveIpv6Range = ipv6Range == 'generate' ? defaultIpv6Range : ipv6Range
var useIPv6 = !empty(effectiveIpv6Range)

var ipv4PrefixLength = int(split(ipv4Range, '/')[1])
var defaultSubnetV4 = cidrSubnet(ipv4Range, ipv4PrefixLength + 1, 1)
var bastionSubnetV4 = cidrSubnet(ipv4Range, 26, 0)
var defaultSubnetV6 = useIPv6 ? cidrSubnet(effectiveIpv6Range, 64, 0) : ''
var defaultSubnetPrefixes = useIPv6 ? [defaultSubnetV4, defaultSubnetV6] : [defaultSubnetV4]
var vnetAddressPrefixes = useIPv6 ? [ipv4Range, effectiveIpv6Range] : [ipv4Range]
var outboundPublicIPs = aksUsesIPv6 ? [{ id: outboundPip4.id }, { id: outboundPip6!.id }] : [{ id: outboundPip4.id }]
var networkContributorRoleId = '4d97b98b-1d4f-4787-a291-c67834d212e7'
var monitoringReaderRoleId = '43d0d8ad-25c7-4714-9337-8ba259a9fe05'
var monitoringDataReaderRoleId = 'b0d8363b-8ddd-447d-831f-62ca05bff136'
var grafanaAdminRoleId = '22926164-76b3-42b3-bc55-97df8dab3e41'

var networkPluginParams = networkPlugin == 'none' ? { networkPlugin: 'none' } : { networkPlugin: networkPlugin }
var networkPluginModeParams = networkPluginMode == 'none' ? { networkPluginMode: 'none' } : { networkPluginMode: networkPluginMode }
var podCidrs = concat(empty(podIpv4Cidr) ? [] : [podIpv4Cidr], empty(podIpv6Cidr) ? [] : [podIpv6Cidr])
var serviceCidrs = concat(empty(serviceIpv4Cidr) ? [] : [serviceIpv4Cidr], empty(serviceIpv6Cidr) ? [] : [serviceIpv6Cidr])
var dnsServiceIP = !empty(serviceIpv4Cidr) ? cidrHost(serviceIpv4Cidr, 9) : !empty(serviceIpv6Cidr) ? cidrHost(serviceIpv6Cidr, 9) : ''
var aksPodCIDRParams = length(podCidrs) == 0 ? {} : length(podCidrs) == 1 ? { podCidr: podCidrs[0] } : { podCidrs: podCidrs }
var aksServiceCIDRParams = length(serviceCidrs) == 0 ? {} : length(serviceCidrs) == 1 ? { serviceCidr: serviceCidrs[0] } : { serviceCidrs: serviceCidrs }
var aksDNSServiceIPParams = empty(dnsServiceIP) ? {} : { dnsServiceIP: dnsServiceIP }
var aksIPFamiliesParams = (length(podCidrs) > 1 || length(serviceCidrs) > 1) ? { ipFamilies: [ 'IPv4', 'IPv6' ] } : {}
var aksUsesIPv6 = !empty(podIpv6Cidr) || !empty(serviceIpv6Cidr)

resource clusterIdentity 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: '${siteName}-identity'
  location: location
}

resource outboundPip4 'Microsoft.Network/publicIPAddresses@2024-05-01' = {
  name: '${siteName}-aks-outbound-pip4'
  location: location
  sku: {
    name: 'Standard'
  }
  properties: {
    publicIPAllocationMethod: 'Static'
    publicIPAddressVersion: 'IPv4'
  }
}

resource outboundPip6 'Microsoft.Network/publicIPAddresses@2024-05-01' = if (useIPv6) {
  name: '${siteName}-aks-outbound-pip6'
  location: location
  sku: {
    name: 'Standard'
  }
  properties: {
    publicIPAllocationMethod: 'Static'
    publicIPAddressVersion: 'IPv6'
  }
}

resource bastionPip 'Microsoft.Network/publicIPAddresses@2024-05-01' = if (enableBastion) {
  name: '${siteName}-bastion-pip4'
  location: location
  sku: {
    name: 'Standard'
  }
  properties: {
    publicIPAllocationMethod: 'Static'
    publicIPAddressVersion: 'IPv4'
  }
}

resource vnet 'Microsoft.Network/virtualNetworks@2024-05-01' = {
  name: '${siteName}-vnet'
  location: location
  properties: {
    addressSpace: {
      addressPrefixes: vnetAddressPrefixes
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

resource vnetRoleAssignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(vnet.id, clusterIdentity.id, networkContributorRoleId)
  scope: vnet
  properties: {
    principalId: clusterIdentity.properties.principalId
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', networkContributorRoleId)
    principalType: 'ServicePrincipal'
  }
}

resource outboundPip4RoleAssignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(outboundPip4.id, clusterIdentity.id, networkContributorRoleId)
  scope: outboundPip4
  properties: {
    principalId: clusterIdentity.properties.principalId
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', networkContributorRoleId)
    principalType: 'ServicePrincipal'
  }
}

resource outboundPip6RoleAssignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (useIPv6) {
  name: guid(outboundPip6!.id, clusterIdentity.id, networkContributorRoleId)
  scope: outboundPip6!
  properties: {
    principalId: clusterIdentity.properties.principalId
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', networkContributorRoleId)
    principalType: 'ServicePrincipal'
  }
}

resource bastion 'Microsoft.Network/bastionHosts@2024-05-01' = if (enableBastion) {
  name: '${siteName}-bastion'
  location: location
  sku: {
    name: 'Standard'
  }
  properties: {
    enableTunneling: true
    enableIpConnect: true
    ipConfigurations: [
      {
        name: 'bastionIpConfig'
        properties: {
          publicIPAddress: {
            id: bastionPip.id
          }
          subnet: {
            id: '${vnet.id}/subnets/AzureBastionSubnet'
          }
        }
      }
    ]
  }
}

// --- Monitoring: Azure Managed Prometheus + Grafana ---

resource monitorWorkspace 'Microsoft.Monitor/accounts@2023-04-03' = if (enableMonitoring) {
  name: '${siteName}-monitor'
  location: location
}

resource dataCollectionEndpoint 'Microsoft.Insights/dataCollectionEndpoints@2022-06-01' = if (enableMonitoring) {
  name: '${siteName}-aks-prom-dce'
  location: location
  kind: 'Linux'
  properties: {}
}

resource dataCollectionRule 'Microsoft.Insights/dataCollectionRules@2022-06-01' = if (enableMonitoring) {
  name: '${siteName}-aks-prom-dcr'
  location: location
  properties: {
    dataCollectionEndpointId: dataCollectionEndpoint.id
    dataSources: {
      prometheusForwarder: [
        {
          name: 'PrometheusDataSource'
          streams: [
            'Microsoft-PrometheusMetrics'
          ]
          labelIncludeFilter: {}
        }
      ]
    }
    destinations: {
      monitoringAccounts: [
        {
          accountResourceId: monitorWorkspace.id
          name: 'MonitoringAccount1'
        }
      ]
    }
    dataFlows: [
      {
        streams: [
          'Microsoft-PrometheusMetrics'
        ]
        destinations: [
          'MonitoringAccount1'
        ]
      }
    ]
  }
}

resource grafana 'Microsoft.Dashboard/grafana@2023-09-01' = if (enableMonitoring) {
  name: '${siteName}-grafana'
  location: location
  sku: {
    name: 'Standard'
  }
  identity: {
    type: 'SystemAssigned'
  }
  properties: {
    publicNetworkAccess: 'Enabled'
    grafanaIntegrations: {
      azureMonitorWorkspaceIntegrations: [
        {
          azureMonitorWorkspaceResourceId: monitorWorkspace.id
        }
      ]
    }
  }
}

resource grafanaMonitoringReaderRole 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (enableMonitoring) {
  name: guid(grafana!.id, monitorWorkspace.id, monitoringReaderRoleId)
  scope: monitorWorkspace
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', monitoringReaderRoleId)
    principalId: grafana!.identity.principalId
    principalType: 'ServicePrincipal'
  }
}

resource grafanaMonitoringDataReaderRole 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (enableMonitoring) {
  name: guid(grafana!.id, monitorWorkspace.id, monitoringDataReaderRoleId)
  scope: monitorWorkspace
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', monitoringDataReaderRoleId)
    principalId: grafana!.identity.principalId
    principalType: 'ServicePrincipal'
  }
}

resource grafanaAdminRoleAssignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = if (enableMonitoring && !empty(grafanaAdminObjectId)) {
  name: guid(grafana!.id, grafanaAdminObjectId, grafanaAdminRoleId)
  scope: grafana!
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', grafanaAdminRoleId)
    principalId: grafanaAdminObjectId
    principalType: 'User'
  }
}

resource aks 'Microsoft.ContainerService/managedClusters@2024-10-01' = {
  name: clusterName
  location: location
  sku: {
    name: 'Base'
    tier: 'Premium'
  }
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${clusterIdentity.id}': {}
    }
  }
  properties: {
    dnsPrefix: dnsPrefix
    kubernetesVersion: empty(kubernetesVersion) ? null : kubernetesVersion
    linuxProfile: {
      adminUsername: adminUsername
      ssh: {
        publicKeys: [
          {
            keyData: sshPublicKey
          }
        ]
      }
    }
    agentPoolProfiles: [
      {
        name: 's1system1'
        mode: 'System'
        count: systemPoolCount
        vmSize: systemPoolVmSize
        osType: 'Linux'
        type: 'VirtualMachineScaleSets'
        vnetSubnetID: '${vnet.id}/subnets/default'
      }
      {
        name: 's1extgw1'
        mode: 'User'
        count: gatewayPoolCount
        vmSize: gatewayPoolVmSize
        osType: 'Linux'
        type: 'VirtualMachineScaleSets'
        vnetSubnetID: '${vnet.id}/subnets/default'
        enableNodePublicIP: true
        networkProfile: {
          allowedHostPorts: [
            {
              portStart: 51820
              portEnd: 51999
              protocol: 'UDP'
            }
          ]
        }
      }
      {
        name: 's1user1'
        mode: 'User'
        count: userPoolCount
        vmSize: userPoolVmSize
        osType: 'Linux'
        type: 'VirtualMachineScaleSets'
        vnetSubnetID: '${vnet.id}/subnets/default'
      }
    ]
    networkProfile: union({
      outboundType: 'loadBalancer'
      loadBalancerSku: 'standard'
      loadBalancerProfile: union({
        allocatedOutboundPorts: allocatedOutboundPorts
      }, managedOutboundIPCount > 0 ? {
        managedOutboundIPs: union({
          count: managedOutboundIPCount
        }, aksUsesIPv6 ? {
          countIPv6: managedOutboundIPCount
        } : {})
      } : {
        outboundIPs: {
          publicIPs: outboundPublicIPs
        }
      })
    }, networkPluginParams, networkPluginModeParams, aksPodCIDRParams, aksServiceCIDRParams, aksDNSServiceIPParams, aksIPFamiliesParams)
    azureMonitorProfile: enableMonitoring ? {
      metrics: {
        enabled: true
      }
    } : null
  }
}

resource dataCollectionRuleAssociation 'Microsoft.Insights/dataCollectionRuleAssociations@2022-06-01' = if (enableMonitoring) {
  name: '${siteName}-aks-prom'
  scope: aks
  properties: {
    dataCollectionRuleId: dataCollectionRule.id
    description: 'Association of data collection rule for Managed Prometheus metrics collection.'
  }
}

output identityId string = clusterIdentity.id
output vnetId string = vnet.id
output outboundPipV4 string = outboundPip4.properties.ipAddress
output outboundPipV6 string = useIPv6 ? outboundPip6!.properties.ipAddress : ''
output aksId string = aks.id
output grafanaUrl string = enableMonitoring ? grafana!.properties.endpoint : ''
output monitorWorkspaceId string = enableMonitoring ? monitorWorkspace!.id : ''
