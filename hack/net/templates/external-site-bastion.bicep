// external-site-bastion.bicep - Provision Azure Bastion for an external site VNet

@description('Site name used as prefix for bastion resources')
param siteName string

@description('Azure region')
param location string = resourceGroup().location

@description('Full resource ID of the site virtual network')
param vnetId string

resource bastionPip 'Microsoft.Network/publicIPAddresses@2024-05-01' = {
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

resource bastion 'Microsoft.Network/bastionHosts@2024-05-01' = {
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
            id: '${vnetId}/subnets/AzureBastionSubnet'
          }
        }
      }
    ]
  }
}

