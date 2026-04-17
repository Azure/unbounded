// external-vmss.bicep - Deploy an external VMSS pool for an AKS cluster
//
// This template creates a Uniform VMSS with Ubuntu 24.04, assigns a managed
// identity (the cluster's kubelet identity), and configures it with custom data
// that bootstraps the nodes into the Kubernetes cluster.

@description('Name of the VMSS')
param vmssName string

@description('Azure region for the VMSS (defaults to resource group location)')
param location string = resourceGroup().location

@description('VM SKU')
param vmSku string = 'Standard_D2ads_v6'

@description('Number of VM instances')
param instanceCount int = 2

@description('Full resource ID of the subnet to attach the VMSS to')
param subnetId string

@description('Full resource ID of the user-assigned managed identity (kubelet identity)')
param identityId string

@description('Full resource ID of the NSG to attach to the NIC (empty string to skip)')
param nsgId string = ''

@description('Base64-encoded custom data (cloud-init) for bootstrapping')
param customDataBase64 string

@description('Computer name prefix for VMSS instances')
param computerNamePrefix string

@description('Admin username')
param adminUsername string = 'azureuser'

@description('Admin password (empty string if not using password auth)')
@secure()
param adminPassword string = ''

@description('SSH public key (empty string if not using SSH auth)')
param sshPublicKey string = ''

@description('Enable instance-level public IP on each VM')
param enablePublicIPPerVM bool = false

@description('Enable IPv6 connectivity and backend pool membership (requires IPv6 subnet and LB backend pool)')
param useIPv6 bool = false

@description('Full resource ID of a load balancer backend pool to join (empty to skip)')
param loadBalancerBackendPoolIdv4 string = ''

@description('Full resource ID of a load balancer backend pool to join (empty to skip)')
param loadBalancerBackendPoolIdv6 string = ''

@description('Allowed host ports/protocol for inbound traffic, e.g. "51820-51999/udp". When set, creates an ASG for this VMSS and an inbound NSG allow rule on the site NSG. Empty to skip.')
param allowedHostPorts string = ''

@description('NSG rule priority for the allowed host ports rule (must be unique within the NSG, range 200-399)')
param allowedHostPortsPriority int = 200

@description('OS disk size in GB')
param osDiskSizeGB int = 128

// Determine OS profile based on provided credentials
var usePassword = !empty(adminPassword)
var useSSH = !empty(sshPublicKey)

var sshConfiguration = useSSH ? {
  publicKeys: [
    {
      path: '/home/${adminUsername}/.ssh/authorized_keys'
      keyData: sshPublicKey
    }
  ]
} : null

var linuxConfiguration = {
  disablePasswordAuthentication: !usePassword
  ssh: useSSH ? sshConfiguration : null
}

var nsgConfig = !empty(nsgId) ? {
  id: nsgId
} : null

var backendPoolConfigv4 = !empty(loadBalancerBackendPoolIdv4) ? [
  {
    id: loadBalancerBackendPoolIdv4
  }
] : []
var backendPoolConfigv6 = !empty(loadBalancerBackendPoolIdv6) ? [
  {
    id: loadBalancerBackendPoolIdv6
  }
] : []

// --- Application Security Group (for gateway pools with allowed host ports) ---

var useAllowedHostPorts = !empty(allowedHostPorts) && !empty(nsgId)
var allowedHostPortsParts = split(allowedHostPorts, '/')
var allowedHostPortsRange = allowedHostPortsParts[0]
var allowedHostPortsProtocolRaw = length(allowedHostPortsParts) > 1 ? toLower(allowedHostPortsParts[1]) : 'udp'
var allowedHostPortsProtocol = allowedHostPortsProtocolRaw == 'tcp' ? 'Tcp' : allowedHostPortsProtocolRaw == 'udp' ? 'Udp' : '*'

resource asg 'Microsoft.Network/applicationSecurityGroups@2024-05-01' = if (useAllowedHostPorts) {
  name: '${vmssName}-asg'
  location: location
}

// Reference the site NSG to create rules in it
resource siteNsg 'Microsoft.Network/networkSecurityGroups@2024-05-01' existing = if (useAllowedHostPorts) {
  name: last(split(nsgId, '/'))
}

// NSG rule to allow inbound traffic on the specified host ports/protocol to this ASG
resource nsgRule 'Microsoft.Network/networkSecurityGroups/securityRules@2024-05-01' = if (useAllowedHostPorts) {
  parent: siteNsg
  name: 'Allow-${vmssName}-${allowedHostPortsProtocolRaw}-${allowedHostPortsRange}'
  properties: {
    priority: allowedHostPortsPriority
    direction: 'Inbound'
    access: 'Allow'
    protocol: allowedHostPortsProtocol
    sourceAddressPrefix: '*'
    sourcePortRange: '*'
    destinationApplicationSecurityGroups: [
      {
        id: asg.id
      }
    ]
    destinationPortRange: allowedHostPortsRange
  }
}

var asgConfig = useAllowedHostPorts ? [
  {
    id: asg.id
  }
] : []

resource vmss 'Microsoft.Compute/virtualMachineScaleSets@2024-07-01' = {
  name: vmssName
  location: location
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${identityId}': {}
    }
  }
  sku: {
    name: vmSku
    tier: 'Standard'
    capacity: instanceCount
  }
  properties: {
    singlePlacementGroup: false
    overprovision: false
    upgradePolicy: {
      mode: 'Manual'
    }
    virtualMachineProfile: {
      storageProfile: {
        imageReference: {
          publisher: 'Canonical'
          offer: 'ubuntu-24_04-lts'
          sku: 'server'
          version: 'latest'
        }
        osDisk: {
          createOption: 'FromImage'
          caching: 'ReadWrite'
          diskSizeGB: osDiskSizeGB
          managedDisk: {
            storageAccountType: 'Premium_LRS'
          }
        }
      }
      osProfile: {
        computerNamePrefix: computerNamePrefix
        adminUsername: adminUsername
        adminPassword: usePassword ? adminPassword : null
        customData: customDataBase64
        linuxConfiguration: linuxConfiguration
      }
      networkProfile: {
        networkInterfaceConfigurations: [
          {
            name: '${vmssName}-nic'
            properties: {
              primary: true
              networkSecurityGroup: nsgConfig
              ipConfigurations: union([
                {
                  name: '${vmssName}-ipconfig4'
                  properties: {
                    primary: true
                    subnet: {
                      id: subnetId
                    }
                    privateIPAddressVersion: 'IPv4'
                    loadBalancerBackendAddressPools: enablePublicIPPerVM ? [] : backendPoolConfigv4
                    applicationSecurityGroups: asgConfig
                    publicIPAddressConfiguration: enablePublicIPPerVM ? {
                      name: '${vmssName}-pip4'
                      properties: {
                        idleTimeoutInMinutes: 4
                        publicIPAddressVersion: 'IPv4'
                      }
                    } : null
                  }
                }
              ], useIPv6 ? [{
                  name: '${vmssName}-ipconfig6'
                  properties: {
                    primary: false
                    subnet: {
                      id: subnetId
                    }
                    privateIPAddressVersion: 'IPv6'
                    loadBalancerBackendAddressPools: enablePublicIPPerVM ? [] : backendPoolConfigv6
                    applicationSecurityGroups: asgConfig
                    publicIPAddressConfiguration: enablePublicIPPerVM ? {
                      name: '${vmssName}-pip6'
                      properties: {
                        idleTimeoutInMinutes: 4
                        publicIPAddressVersion: 'IPv6'
                      }
                    } : null
                  }
                }] : [])
            }
          }
        ]
      }
      diagnosticsProfile: {
        bootDiagnostics: {
          enabled: true
        }
      }
    }
  }
}

output vmssId string = vmss.id
output vmssName string = vmss.name
output asgId string = useAllowedHostPorts ? asg.id : ''
