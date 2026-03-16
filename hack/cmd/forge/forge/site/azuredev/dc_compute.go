package azuredev

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/azsdk"
	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/infra"
)

func getSubnetByName(name string, subnets []*armnetwork.Subnet) (*armnetwork.Subnet, error) {
	for _, s := range subnets {
		if *s.Name == name {
			return s, nil
		}
	}

	return nil, fmt.Errorf("subnet with name %q not found", name)
}

type datacenterComputeManager struct {
	azureCli      *azsdk.ClientSet
	resourceGroup *armresources.ResourceGroup
	logger        *slog.Logger
}

type datacenterCompute struct {
	machinePool *armcompute.VirtualMachineScaleSet
}

type machinePoolConfig struct {
	name                           string
	sku                            *armcompute.SKU
	adminUser                      string
	adminSSHPublicKey              []byte
	userData                       string
	subnet                         *armnetwork.Subnet
	loadBalancerBackendAddressPool *armnetwork.BackendAddressPool
	tags                           map[string]*string
}

func (m *datacenterComputeManager) createOrUpdate(ctx context.Context, cfg machinePoolConfig) (*datacenterCompute, error) {
	m.logger.Info("Applying datacenter machine pool", "pool", cfg.name)

	desired := &armcompute.VirtualMachineScaleSet{
		Name:     to.Ptr(cfg.name),
		Location: m.resourceGroup.Location,
		SKU:      cfg.sku,
		Tags:     cfg.tags,
		Properties: &armcompute.VirtualMachineScaleSetProperties{
			Overprovision: to.Ptr(false),
			VirtualMachineProfile: &armcompute.VirtualMachineScaleSetVMProfile{
				DiagnosticsProfile: &armcompute.DiagnosticsProfile{
					BootDiagnostics: &armcompute.BootDiagnostics{
						Enabled: to.Ptr(true),
					},
				},
				OSProfile: &armcompute.VirtualMachineScaleSetOSProfile{
					ComputerNamePrefix: to.Ptr(fmt.Sprintf("%s-", cfg.name)),
					AdminUsername:      to.Ptr(cfg.adminUser),
					LinuxConfiguration: &armcompute.LinuxConfiguration{
						DisablePasswordAuthentication: to.Ptr(true),
						SSH: &armcompute.SSHConfiguration{
							PublicKeys: []*armcompute.SSHPublicKey{
								{
									KeyData: to.Ptr(string(cfg.adminSSHPublicKey)),
									Path:    to.Ptr(filepath.Join("/home", cfg.adminUser, ".ssh/authorized_keys")),
								},
							},
						},
					},
				},
				NetworkProfile: &armcompute.VirtualMachineScaleSetNetworkProfile{
					NetworkInterfaceConfigurations: []*armcompute.VirtualMachineScaleSetNetworkConfiguration{
						{
							Name: to.Ptr("main"),
							Properties: &armcompute.VirtualMachineScaleSetNetworkConfigurationProperties{
								Primary: to.Ptr(true),
								IPConfigurations: []*armcompute.VirtualMachineScaleSetIPConfiguration{
									{
										Name: to.Ptr("ipconfig1"),
										Properties: &armcompute.VirtualMachineScaleSetIPConfigurationProperties{
											Subnet: &armcompute.APIEntityReference{
												ID: cfg.subnet.ID,
											},
										},
									},
								},
							},
						},
					},
				},
				StorageProfile: &armcompute.VirtualMachineScaleSetStorageProfile{
					ImageReference: &armcompute.ImageReference{
						Publisher: to.Ptr("Canonical"),
						Offer:     to.Ptr("0001-com-ubuntu-server-jammy"),
						SKU:       to.Ptr("22_04-lts-gen2"),
						Version:   to.Ptr("latest"),
					},
					OSDisk: &armcompute.VirtualMachineScaleSetOSDisk{
						OSType:       to.Ptr(armcompute.OperatingSystemTypesLinux),
						CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesFromImage),
						DiskSizeGB:   to.Ptr[int32](30),
						Caching:      to.Ptr(armcompute.CachingTypesReadOnly),
						ManagedDisk: &armcompute.VirtualMachineScaleSetManagedDiskParameters{
							StorageAccountType: to.Ptr(armcompute.StorageAccountTypesStandardSSDLRS),
						},
						DiffDiskSettings: &armcompute.DiffDiskSettings{
							Option: to.Ptr(armcompute.DiffDiskOptionsLocal),
						},
					},
				},
			},
			UpgradePolicy: &armcompute.UpgradePolicy{
				Mode: to.Ptr(armcompute.UpgradeModeManual),
			},
		},
	}

	if cfg.userData != "" {
		desired.Properties.VirtualMachineProfile.UserData = to.Ptr(cfg.userData)
	}

	if cfg.loadBalancerBackendAddressPool != nil {
		desired.Properties.VirtualMachineProfile.NetworkProfile.NetworkInterfaceConfigurations[0].Properties.IPConfigurations[0].Properties.LoadBalancerBackendAddressPools = []*armcompute.SubResource{
			{
				ID: cfg.loadBalancerBackendAddressPool.ID,
			},
		}
	}

	vmssMan := infra.VirtualMachineScaleSetManager{
		Client: m.azureCli.ComputeVMScaleSetClientV2,
		Logger: m.logger,
	}

	applied, err := vmssMan.CreateOrUpdate(ctx, *m.resourceGroup.Name, *desired)
	if err != nil {
		return nil, fmt.Errorf("create or update vmss: %w", err)
	}

	return &datacenterCompute{
		machinePool: applied,
	}, nil
}
