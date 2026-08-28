// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuredev

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
)

const (
	aclImageURN   = "MicrosoftCBLMariner:azure-linux-3:azure-linux-3-acl:latest"
	galleryImgID  = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.Compute/galleries/g1/images/img1/versions/1.0.0"
	managedImgID  = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.Compute/images/img1"
	notComputeID  = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.Network/images/img1"
	noImagesSegID = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.Compute/galleries/g1"
)

func deref(t *testing.T, s *string) string {
	t.Helper()

	if s == nil {
		return "<nil>"
	}

	return *s
}

func TestParseImageReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		image       string
		wantNil     bool
		wantErr     bool
		wantID      string
		wantPublish string
		wantOffer   string
		wantSKU     string
		wantVersion string
	}{
		{
			name:    "empty returns nil reference and no error",
			image:   "",
			wantNil: true,
		},
		{
			name:    "whitespace only returns nil reference and no error",
			image:   "   ",
			wantNil: true,
		},
		{
			name:        "azure container linux urn",
			image:       aclImageURN,
			wantPublish: "MicrosoftCBLMariner",
			wantOffer:   "azure-linux-3",
			wantSKU:     "azure-linux-3-acl",
			wantVersion: "latest",
		},
		{
			name:        "azure container linux arm64 urn",
			image:       "MicrosoftCBLMariner:azure-linux-3:azure-linux-3-arm64-gen2-acl:latest",
			wantPublish: "MicrosoftCBLMariner",
			wantOffer:   "azure-linux-3",
			wantSKU:     "azure-linux-3-arm64-gen2-acl",
			wantVersion: "latest",
		},
		{
			name:        "pinned version urn",
			image:       "MicrosoftCBLMariner:azure-linux-3:azure-linux-3-acl:3.20260809.01",
			wantPublish: "MicrosoftCBLMariner",
			wantOffer:   "azure-linux-3",
			wantSKU:     "azure-linux-3-acl",
			wantVersion: "3.20260809.01",
		},
		{
			name:        "default ubuntu urn round trips",
			image:       defaultImageURN,
			wantPublish: defaultImagePublisher,
			wantOffer:   defaultImageOffer,
			wantSKU:     defaultImageSKU,
			wantVersion: defaultImageVersion,
		},
		{
			name:        "urn segments are trimmed",
			image:       " Canonical : ubuntu-24_04-lts : server : latest ",
			wantPublish: "Canonical",
			wantOffer:   "ubuntu-24_04-lts",
			wantSKU:     "server",
			wantVersion: "latest",
		},
		{
			name:   "gallery image version resource id",
			image:  galleryImgID,
			wantID: galleryImgID,
		},
		{
			name:   "managed image resource id",
			image:  managedImgID,
			wantID: managedImgID,
		},
		{
			name:   "resource id casing is preserved but matched case insensitively",
			image:  "/Subscriptions/00000000-0000-0000-0000-000000000000/ResourceGroups/rg/Providers/Microsoft.Compute/Images/img1",
			wantID: "/Subscriptions/00000000-0000-0000-0000-000000000000/ResourceGroups/rg/Providers/Microsoft.Compute/Images/img1",
		},
		{
			name:    "resource id from another provider is rejected",
			image:   notComputeID,
			wantErr: true,
		},
		{
			name:    "resource id without an images segment is rejected",
			image:   noImagesSegID,
			wantErr: true,
		},
		{
			name:    "three segment urn is rejected",
			image:   "Canonical:ubuntu-24_04-lts:server",
			wantErr: true,
		},
		{
			name:    "five segment urn is rejected",
			image:   "a:b:c:d:e",
			wantErr: true,
		},
		{
			name:    "empty urn segment is rejected",
			image:   "MicrosoftCBLMariner::azure-linux-3-acl:latest",
			wantErr: true,
		},
		{
			name:    "blank urn segment is rejected",
			image:   "MicrosoftCBLMariner:azure-linux-3:   :latest",
			wantErr: true,
		},
		{
			name:    "bare name is rejected",
			image:   "azure-linux-3-acl",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseImageReference(tc.image)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseImageReference(%q) expected error, got %+v", tc.image, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("parseImageReference(%q) unexpected error: %v", tc.image, err)
			}

			if tc.wantNil {
				if got != nil {
					t.Fatalf("parseImageReference(%q) expected nil reference, got %+v", tc.image, got)
				}

				return
			}

			if got == nil {
				t.Fatalf("parseImageReference(%q) returned nil reference", tc.image)
			}

			if tc.wantID != "" {
				if deref(t, got.ID) != tc.wantID {
					t.Errorf("ID = %q, want %q", deref(t, got.ID), tc.wantID)
				}

				// A resource ID reference must not also carry URN fields, otherwise
				// ARM rejects the scale set.
				if got.Publisher != nil {
					t.Errorf("Publisher = %q, want nil for a resource ID reference", deref(t, got.Publisher))
				}

				if got.Offer != nil {
					t.Errorf("Offer = %q, want nil for a resource ID reference", deref(t, got.Offer))
				}

				if got.SKU != nil {
					t.Errorf("SKU = %q, want nil for a resource ID reference", deref(t, got.SKU))
				}

				if got.Version != nil {
					t.Errorf("Version = %q, want nil for a resource ID reference", deref(t, got.Version))
				}

				return
			}

			if deref(t, got.Publisher) != tc.wantPublish {
				t.Errorf("Publisher = %q, want %q", deref(t, got.Publisher), tc.wantPublish)
			}

			if deref(t, got.Offer) != tc.wantOffer {
				t.Errorf("Offer = %q, want %q", deref(t, got.Offer), tc.wantOffer)
			}

			if deref(t, got.SKU) != tc.wantSKU {
				t.Errorf("SKU = %q, want %q", deref(t, got.SKU), tc.wantSKU)
			}

			if deref(t, got.Version) != tc.wantVersion {
				t.Errorf("Version = %q, want %q", deref(t, got.Version), tc.wantVersion)
			}

			if got.ID != nil {
				t.Errorf("ID = %q, want nil for a URN reference", deref(t, got.ID))
			}
		})
	}
}

func testPoolConfig() machinePoolConfig {
	return machinePoolConfig{
		name:              "site1-pool1",
		sku:               &armcompute.SKU{Name: to.Ptr("standard_d2ads_v6"), Tier: to.Ptr("Standard"), Capacity: to.Ptr(int64(2))},
		adminUser:         "kubedev",
		adminSSHPublicKey: []byte("ssh-ed25519 AAAA test"),
		subnet:            &armnetwork.Subnet{ID: to.Ptr("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet/subnets/main")},
	}
}

func storageImage(t *testing.T, vmss *armcompute.VirtualMachineScaleSet) *armcompute.ImageReference {
	t.Helper()

	if vmss.Properties == nil ||
		vmss.Properties.VirtualMachineProfile == nil ||
		vmss.Properties.VirtualMachineProfile.StorageProfile == nil {
		t.Fatal("scale set is missing a storage profile")
	}

	return vmss.Properties.VirtualMachineProfile.StorageProfile.ImageReference
}

func TestBuildVMSSDefaultsToUbuntuWhenImageIsNil(t *testing.T) {
	t.Parallel()

	vmss := buildVMSS(testPoolConfig(), to.Ptr("canadacentral"))

	img := storageImage(t, vmss)
	if img == nil {
		t.Fatal("expected a default image reference, got nil")
	}

	if deref(t, img.Publisher) != "Canonical" {
		t.Errorf("Publisher = %q, want %q", deref(t, img.Publisher), "Canonical")
	}

	if deref(t, img.Offer) != "ubuntu-24_04-lts" {
		t.Errorf("Offer = %q, want %q", deref(t, img.Offer), "ubuntu-24_04-lts")
	}

	if deref(t, img.SKU) != "server" {
		t.Errorf("SKU = %q, want %q", deref(t, img.SKU), "server")
	}

	if deref(t, img.Version) != "latest" {
		t.Errorf("Version = %q, want %q", deref(t, img.Version), "latest")
	}
}

func TestBuildVMSSUsesAzureContainerLinuxImage(t *testing.T) {
	t.Parallel()

	ref, err := parseImageReference(aclImageURN)
	if err != nil {
		t.Fatalf("parseImageReference: %v", err)
	}

	cfg := testPoolConfig()
	cfg.image = ref

	img := storageImage(t, buildVMSS(cfg, to.Ptr("canadacentral")))
	if img == nil {
		t.Fatal("expected an image reference, got nil")
	}

	if deref(t, img.Publisher) != "MicrosoftCBLMariner" {
		t.Errorf("Publisher = %q, want %q", deref(t, img.Publisher), "MicrosoftCBLMariner")
	}

	if deref(t, img.Offer) != "azure-linux-3" {
		t.Errorf("Offer = %q, want %q", deref(t, img.Offer), "azure-linux-3")
	}

	if deref(t, img.SKU) != "azure-linux-3-acl" {
		t.Errorf("SKU = %q, want %q", deref(t, img.SKU), "azure-linux-3-acl")
	}

	if deref(t, img.Version) != "latest" {
		t.Errorf("Version = %q, want %q", deref(t, img.Version), "latest")
	}
}

func TestBuildVMSSUsesGalleryImageResourceID(t *testing.T) {
	t.Parallel()

	ref, err := parseImageReference(galleryImgID)
	if err != nil {
		t.Fatalf("parseImageReference: %v", err)
	}

	cfg := testPoolConfig()
	cfg.image = ref

	img := storageImage(t, buildVMSS(cfg, to.Ptr("canadacentral")))
	if img == nil {
		t.Fatal("expected an image reference, got nil")
	}

	if deref(t, img.ID) != galleryImgID {
		t.Errorf("ID = %q, want %q", deref(t, img.ID), galleryImgID)
	}

	if img.Publisher != nil {
		t.Errorf("Publisher = %q, want nil", deref(t, img.Publisher))
	}
}

func TestBuildVMSSCoreProfile(t *testing.T) {
	t.Parallel()

	cfg := testPoolConfig()
	vmss := buildVMSS(cfg, to.Ptr("canadacentral"))

	if deref(t, vmss.Name) != "site1-pool1" {
		t.Errorf("Name = %q, want %q", deref(t, vmss.Name), "site1-pool1")
	}

	if deref(t, vmss.Location) != "canadacentral" {
		t.Errorf("Location = %q, want %q", deref(t, vmss.Location), "canadacentral")
	}

	profile := vmss.Properties.VirtualMachineProfile

	if got := deref(t, profile.OSProfile.ComputerNamePrefix); got != "site1-pool1-" {
		t.Errorf("ComputerNamePrefix = %q, want %q", got, "site1-pool1-")
	}

	if got := deref(t, profile.OSProfile.AdminUsername); got != "kubedev" {
		t.Errorf("AdminUsername = %q, want %q", got, "kubedev")
	}

	key := profile.OSProfile.LinuxConfiguration.SSH.PublicKeys[0]
	if got := deref(t, key.Path); got != "/home/kubedev/.ssh/authorized_keys" {
		t.Errorf("SSH key path = %q, want %q", got, "/home/kubedev/.ssh/authorized_keys")
	}

	if !*profile.OSProfile.LinuxConfiguration.DisablePasswordAuthentication {
		t.Error("DisablePasswordAuthentication = false, want true")
	}

	osDisk := profile.StorageProfile.OSDisk
	if *osDisk.DiskSizeGB != 30 {
		t.Errorf("DiskSizeGB = %d, want 30", *osDisk.DiskSizeGB)
	}

	// Ephemeral OS disk placement is intentionally left unset. Standard_D2ads_v6
	// rejects an explicit CacheDisk placement, so Azure must pick the default.
	if osDisk.DiffDiskSettings == nil {
		t.Fatal("DiffDiskSettings = nil, want ephemeral OS disk settings")
	}

	if *osDisk.DiffDiskSettings.Option != armcompute.DiffDiskOptionsLocal {
		t.Errorf("DiffDiskSettings.Option = %v, want %v", *osDisk.DiffDiskSettings.Option, armcompute.DiffDiskOptionsLocal)
	}

	if osDisk.DiffDiskSettings.Placement != nil {
		t.Errorf("DiffDiskSettings.Placement = %v, want nil", *osDisk.DiffDiskSettings.Placement)
	}
}

func TestBuildVMSSUserData(t *testing.T) {
	t.Parallel()

	cfg := testPoolConfig()

	if got := buildVMSS(cfg, to.Ptr("canadacentral")).Properties.VirtualMachineProfile.UserData; got != nil {
		t.Errorf("UserData = %q, want nil when unset", deref(t, got))
	}

	cfg.userData = "dXNlcmRhdGE="

	if got := buildVMSS(cfg, to.Ptr("canadacentral")).Properties.VirtualMachineProfile.UserData; deref(t, got) != "dXNlcmRhdGE=" {
		t.Errorf("UserData = %q, want %q", deref(t, got), "dXNlcmRhdGE=")
	}
}

func TestBuildVMSSLoadBalancerBackendPool(t *testing.T) {
	t.Parallel()

	cfg := testPoolConfig()

	ipCfg := func(v *armcompute.VirtualMachineScaleSet) *armcompute.VirtualMachineScaleSetIPConfiguration {
		return v.Properties.VirtualMachineProfile.NetworkProfile.NetworkInterfaceConfigurations[0].Properties.IPConfigurations[0]
	}

	if pools := ipCfg(buildVMSS(cfg, to.Ptr("canadacentral"))).Properties.LoadBalancerBackendAddressPools; pools != nil {
		t.Errorf("LoadBalancerBackendAddressPools = %v, want nil when unset", pools)
	}

	backendID := "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/be"
	cfg.loadBalancerBackendAddressPool = &armnetwork.BackendAddressPool{ID: to.Ptr(backendID)}

	pools := ipCfg(buildVMSS(cfg, to.Ptr("canadacentral"))).Properties.LoadBalancerBackendAddressPools
	if len(pools) != 1 {
		t.Fatalf("len(LoadBalancerBackendAddressPools) = %d, want 1", len(pools))
	}

	if deref(t, pools[0].ID) != backendID {
		t.Errorf("backend pool ID = %q, want %q", deref(t, pools[0].ID), backendID)
	}
}
