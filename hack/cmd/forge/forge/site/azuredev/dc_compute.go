// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuredev

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"

	"github.com/Azure/unbounded/hack/cmd/forge/forge/azsdk"
	"github.com/Azure/unbounded/hack/cmd/forge/forge/infra"
)

// Default host image for machine pools when the caller does not specify one.
// Ubuntu 24.04 LTS is the only host OS the unbounded agent is known to provision
// end to end on Azure, so it remains the default.
const (
	defaultImagePublisher = "Canonical"
	defaultImageOffer     = "ubuntu-24_04-lts"
	defaultImageSKU       = "server"
	defaultImageVersion   = "latest"
)

// defaultImageURN is the marketplace URN form of the default image. It is used
// where a pool must be pinned to the default regardless of any site-wide
// override.
const defaultImageURN = defaultImagePublisher + ":" + defaultImageOffer + ":" +
	defaultImageSKU + ":" + defaultImageVersion

// defaultImageReference returns the marketplace image used when a pool does not
// pin its own image.
func defaultImageReference() *armcompute.ImageReference {
	return &armcompute.ImageReference{
		Publisher: to.Ptr(defaultImagePublisher),
		Offer:     to.Ptr(defaultImageOffer),
		SKU:       to.Ptr(defaultImageSKU),
		Version:   to.Ptr(defaultImageVersion),
	}
}

// parseImageReference converts a user-supplied image string into an ARM image
// reference. It accepts either a "publisher:offer:sku:version" marketplace URN,
// for example "MicrosoftCBLMariner:azure-linux-3:azure-linux-3-acl:latest", or an
// Azure resource ID identifying a managed image or a Compute Gallery image
// version. An empty string yields a nil reference, meaning "use the default".
func parseImageReference(image string) (*armcompute.ImageReference, error) {
	image = strings.TrimSpace(image)
	if image == "" {
		return nil, nil
	}

	if strings.HasPrefix(image, "/") {
		lowerImage := strings.ToLower(strings.TrimRight(image, "/"))
		if !strings.HasPrefix(lowerImage, "/subscriptions/") ||
			!strings.Contains(lowerImage, "/resourcegroups/") ||
			!strings.Contains(lowerImage, "/providers/microsoft.compute/") ||
			!strings.Contains(lowerImage, "/images/") {
			return nil, fmt.Errorf("image Azure resource ID must identify a Microsoft.Compute image or gallery image")
		}

		return &armcompute.ImageReference{ID: to.Ptr(image)}, nil
	}

	parts := strings.Split(image, ":")
	if len(parts) != 4 {
		return nil, fmt.Errorf("image must be an Azure resource ID or a publisher:offer:sku:version reference")
	}

	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
		if parts[i] == "" {
			return nil, fmt.Errorf("image publisher, offer, SKU, and version must be non-empty")
		}
	}

	return &armcompute.ImageReference{
		Publisher: to.Ptr(parts[0]),
		Offer:     to.Ptr(parts[1]),
		SKU:       to.Ptr(parts[2]),
		Version:   to.Ptr(parts[3]),
	}, nil
}

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
	image                          *armcompute.ImageReference
	subnet                         *armnetwork.Subnet
	loadBalancerBackendAddressPool *armnetwork.BackendAddressPool
	tags                           map[string]*string
}

// buildVMSS assembles the desired scale set for a machine pool. It performs no
// I/O so that the resulting specification can be asserted on directly in tests.
func buildVMSS(cfg machinePoolConfig, location *string) *armcompute.VirtualMachineScaleSet {
	image := cfg.image
	if image == nil {
		image = defaultImageReference()
	}

	desired := &armcompute.VirtualMachineScaleSet{
		Name:     to.Ptr(cfg.name),
		Location: location,
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
					ImageReference: image,
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
		// Azure expects base64, and the two fields are populated together
		// because consumers differ: cloud-init and Ignition read customData
		// from the provisioning agent, while UserData is served from the
		// instance metadata service. Setting only one silently does nothing for
		// half of them.
		encoded := base64.StdEncoding.EncodeToString([]byte(cfg.userData))

		desired.Properties.VirtualMachineProfile.UserData = to.Ptr(encoded)
		desired.Properties.VirtualMachineProfile.OSProfile.CustomData = to.Ptr(encoded)
	}

	if cfg.loadBalancerBackendAddressPool != nil {
		desired.Properties.VirtualMachineProfile.NetworkProfile.NetworkInterfaceConfigurations[0].Properties.IPConfigurations[0].Properties.LoadBalancerBackendAddressPools = []*armcompute.SubResource{
			{
				ID: cfg.loadBalancerBackendAddressPool.ID,
			},
		}
	}

	return desired
}

func (m *datacenterComputeManager) createOrUpdate(ctx context.Context, cfg machinePoolConfig) (*datacenterCompute, error) {
	m.logger.Info("Applying datacenter machine pool", "pool", cfg.name)

	desired := buildVMSS(cfg, m.resourceGroup.Location)

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
