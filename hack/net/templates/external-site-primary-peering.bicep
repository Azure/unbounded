targetScope = 'resourceGroup'

// external-site-primary-peering.bicep - Create reverse VNet peering on the primary VNet

@description('Primary site VNet name')
param primaryVnetName string

@description('Remote site VNet resource ID')
param remoteVnetId string

@description('External site name used in peering name')
param siteName string

resource primaryVnet 'Microsoft.Network/virtualNetworks@2024-05-01' existing = {
  name: primaryVnetName
}

resource primaryToSitePeering 'Microsoft.Network/virtualNetworks/virtualNetworkPeerings@2024-05-01' = {
  parent: primaryVnet
  name: '${primaryVnetName}-to-${siteName}'
  properties: {
    allowVirtualNetworkAccess: true
    allowForwardedTraffic: true
    allowGatewayTransit: false
    useRemoteGateways: false
    remoteVirtualNetwork: {
      id: remoteVnetId
    }
  }
}

