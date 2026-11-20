// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/stretchr/testify/require"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
)

func TestParseAzureVMProviderID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		providerID string
		want       azureVMResourceRef
		wantErr    string
	}{
		{
			name:       "raw ARM ID",
			providerID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
			want: azureVMResourceRef{
				SubscriptionID: "sub",
				ResourceGroup:  "rg",
				VMName:         "vm1",
			},
		},
		{
			name:       "azure provider ID",
			providerID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
			want: azureVMResourceRef{
				SubscriptionID: "sub",
				ResourceGroup:  "rg",
				VMName:         "vm1",
			},
		},
		{
			name:       "wrong provider",
			providerID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/ip1",
			wantErr:    "Microsoft.Compute/virtualMachines",
		},
		{
			name:    "empty",
			wantErr: "required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseAzureVMProviderID(tt.providerID)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestNormalizeResourceID(t *testing.T) {
	t.Parallel()

	resourceID, err := NormalizeResourceID("azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1")
	require.NoError(t, err)
	require.Equal(t, "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1", resourceID)
}

// TestBeginThroughTheRegistration drives each host operation's Begin through the
// registration. A power operation's Begin sends its one Azure request and
// returns without waiting for it; HostReplace's Begin only records the VM
// model and leaves every request to Poll.
func TestBeginThroughTheRegistration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		operation unboundedv1alpha3.OperationKind
		power     string
		wantCalls []string
	}{
		{name: "out-of-band reboot", operation: unboundedv1alpha3.OperationHostHardReboot, power: powerStateRunning, wantCalls: []string{"restart:vm1"}},
		{name: "power off", operation: unboundedv1alpha3.OperationHostPowerOff, power: powerStateRunning, wantCalls: []string{"powerOff:vm1"}},
		{name: "power on", operation: unboundedv1alpha3.OperationHostPowerOn, power: powerStateStopped, wantCalls: []string{"start:vm1"}},
		{name: "replace", operation: unboundedv1alpha3.OperationHostReplace, power: powerStateRunning, wantCalls: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := newFakeVMClient(replaceableVM())
			client.power = tt.power

			registration, err := newHostProvider(client).Registration()
			require.NoError(t, err)

			operation, ok := registration.Operation(tt.operation)
			require.True(t, ok)

			begin, err := operation.Begin(t.Context(), hostRequest(tt.operation))

			require.NoError(t, err)
			require.Equal(t, hostVMID(), begin.Operation.OperationID)
			require.Equal(t, tt.wantCalls, client.calls)
		})
	}
}

func TestBeginPassesAuthToClientFactory(t *testing.T) {
	t.Parallel()

	auth := &machineops.OperationAuth{
		Mode: unboundedv1alpha3.MachineOperationCredentialAuthExternalPlugin,
		SecretData: map[string]string{
			"tenantID":     "tenant",
			"clientID":     "client",
			"clientSecret": "secret",
		},
	}
	client := newFakeVMClient(replaceableVM())
	client.power = powerStateStopped
	provider := &Provider{
		NewVMClient: func(subscriptionID string, gotAuth *machineops.OperationAuth) (azureVMClient, error) {
			require.Equal(t, "sub", subscriptionID)
			require.Equal(t, auth, gotAuth)

			return client, nil
		},
	}

	request := hostRequest(unboundedv1alpha3.OperationHostPowerOn)
	request.Auth = auth

	_, err := provider.BeginPowerOperation(t.Context(), request)

	require.NoError(t, err)
	require.Equal(t, []string{"start:vm1"}, client.calls)
}

func TestNewAzureVMClientValidatesAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		auth    *machineops.OperationAuth
		wantErr string
	}{
		{
			name: "service principal missing client secret",
			auth: &machineops.OperationAuth{
				Mode: unboundedv1alpha3.MachineOperationCredentialAuthExternalPlugin,
				SecretData: map[string]string{
					"tenantID": "tenant",
					"clientID": "client",
				},
			},
			wantErr: "clientSecret",
		},
		{
			name: "unsupported auth mode",
			auth: &machineops.OperationAuth{
				Mode: unboundedv1alpha3.MachineOperationCredentialAuthMode("unsupported"),
			},
			wantErr: "unsupported Azure VM auth mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := (&Provider{}).client("sub", tt.auth)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestBeginReplaceRequiresUserData(t *testing.T) {
	t.Parallel()

	client := newFakeVMClient(replaceableVM())
	request := hostRequest(unboundedv1alpha3.OperationHostReplace)
	request.ReplaceUserData = ""

	_, err := newHostProvider(client).BeginReplace(t.Context(), request)
	require.Error(t, err)
	require.Contains(t, err.Error(), "replacement user data is required")
	requirePermanentReason(t, err, string(unboundedv1alpha3.FailureInvalidRequest))
	require.Empty(t, client.calls)
}

func TestPrepareReplacementVMChangesHostImage(t *testing.T) {
	t.Parallel()

	vm := armcompute.VirtualMachine{
		Name: toPtr("vm1"),
		Properties: &armcompute.VirtualMachineProperties{
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: &armcompute.ImageReference{
					Publisher: toPtr("old-publisher"),
					Offer:     toPtr("old-offer"),
					SKU:       toPtr("old-sku"),
					Version:   toPtr("old-version"),
				},
				OSDisk: &armcompute.OSDisk{},
			},
		},
	}

	replacement, err := prepareReplacementVM(
		vm,
		replaceRestore{},
		"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/galleries/gallery/images/image/versions/2.0.0",
		hostOperationUID,
	)
	require.NoError(t, err)
	require.Equal(t, "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/galleries/gallery/images/image/versions/2.0.0", *replacement.Properties.StorageProfile.ImageReference.ID)
	require.Nil(t, replacement.Properties.StorageProfile.ImageReference.Publisher)
}

func TestParseAzureImageReference(t *testing.T) {
	t.Parallel()

	marketplace, err := parseAzureImageReference("Canonical:0001-com-ubuntu-server-jammy:22_04-lts:latest")
	require.NoError(t, err)
	require.Equal(t, "Canonical", *marketplace.Publisher)
	require.Equal(t, "latest", *marketplace.Version)

	_, err = parseAzureImageReference("not-an-image")
	require.ErrorContains(t, err, "Azure resource ID or publisher:offer:sku:version")

	_, err = parseAzureImageReference("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/not-an-image")
	require.ErrorContains(t, err, "Microsoft.Compute image or gallery image")
}

func TestPrepareReplacementVM(t *testing.T) {
	t.Parallel()

	vm := armcompute.VirtualMachine{
		Name: toPtr("vm1"),
		ID:   toPtr("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1"),
		Identity: &armcompute.VirtualMachineIdentity{UserAssignedIdentities: map[string]*armcompute.UserAssignedIdentitiesValue{
			"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/id1": {ClientID: toPtr("client-id")},
		}},
		Properties: &armcompute.VirtualMachineProperties{
			ProvisioningState: toPtr("Succeeded"),
			// A VM replaced by an earlier version of this adapter carries an
			// old bootstrap token in userData.
			UserData:  toPtr("b2xkLXRva2Vu"),
			OSProfile: &armcompute.OSProfile{RequireGuestProvisionSignal: toPtr(true)},
			StorageProfile: &armcompute.StorageProfile{OSDisk: &armcompute.OSDisk{
				Name: toPtr("old-osdisk"),
				ManagedDisk: &armcompute.ManagedDiskParameters{
					ID:                 toPtr("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/disks/old-osdisk"),
					StorageAccountType: toPtr(armcompute.StorageAccountTypesPremiumLRS),
				},
			}},
		},
	}

	replacement, err := prepareReplacementVM(vm, replaceRestore{}, "", hostOperationUID)
	require.NoError(t, err)

	require.Nil(t, replacement.ID)
	require.Nil(t, replacement.Name)
	require.Equal(t, &armcompute.UserAssignedIdentitiesValue{}, replacement.Identity.UserAssignedIdentities["/subscriptions/sub/resourceGroups/rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/id1"])
	require.Nil(t, replacement.Properties.ProvisioningState)

	// The model carries no cloud-init, and never the userData of the VM it
	// replaces, which Azure keeps readable for the VM's life.
	require.Nil(t, replacement.Properties.UserData)
	require.Nil(t, replacement.Properties.OSProfile.CustomData)
	require.Nil(t, replacement.Properties.OSProfile.RequireGuestProvisionSignal)
	require.Equal(t, armcompute.DiskCreateOptionTypesFromImage, *replacement.Properties.StorageProfile.OSDisk.CreateOption)
	require.NotNil(t, replacement.Properties.StorageProfile.OSDisk.ManagedDisk)
	require.Nil(t, replacement.Properties.StorageProfile.OSDisk.ManagedDisk.ID)
	require.Equal(t, armcompute.StorageAccountTypesPremiumLRS, *replacement.Properties.StorageProfile.OSDisk.ManagedDisk.StorageAccountType)
	require.Equal(t, "vm1-osdisk-0b5c7d2e", *replacement.Properties.StorageProfile.OSDisk.Name)
	require.NotNil(t, vm.Properties.StorageProfile.OSDisk.ManagedDisk.ID)
}

func TestValidateAzureCustomData(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateAzureCustomData(strings.Repeat("a", azureCustomDataMaxBytes)))
	err := validateAzureCustomData(strings.Repeat("a", azureCustomDataMaxBytes+1))
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeding Azure customData limit")
}

func TestPrepareVMForReplacementDelete(t *testing.T) {
	t.Parallel()

	vm := armcompute.VirtualMachine{Properties: &armcompute.VirtualMachineProperties{
		NetworkProfile: &armcompute.NetworkProfile{NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{}}},
		StorageProfile: &armcompute.StorageProfile{
			OSDisk:    &armcompute.OSDisk{},
			DataDisks: []*armcompute.DataDisk{{}},
		},
	}, Resources: []*armcompute.VirtualMachineExtension{{Name: toPtr("old-extension")}}}

	updated := prepareVMForReplacementDelete(vm)

	require.Nil(t, updated.Resources)
	require.Equal(t, armcompute.DeleteOptionsDetach, *updated.Properties.NetworkProfile.NetworkInterfaces[0].Properties.DeleteOption)
	require.Equal(t, armcompute.DiskDeleteOptionTypesDelete, *updated.Properties.StorageProfile.OSDisk.DeleteOption)
	require.Equal(t, armcompute.DiskDeleteOptionTypesDetach, *updated.Properties.StorageProfile.DataDisks[0].DeleteOption)
}

func TestPreparingReplacementDoesNotMutateDeleteOptionPayload(t *testing.T) {
	t.Parallel()

	source := armcompute.VirtualMachine{
		Name: toPtr("vm1"),
		Properties: &armcompute.VirtualMachineProperties{
			StorageProfile: &armcompute.StorageProfile{
				OSDisk: &armcompute.OSDisk{Name: toPtr("existing-osdisk")},
			},
		},
	}

	deleteOptionPayload := prepareVMForReplacementDelete(source)
	_, err := prepareReplacementVM(source, replaceRestore{}, "", hostOperationUID)
	require.NoError(t, err)
	require.Equal(t, "existing-osdisk", *deleteOptionPayload.Properties.StorageProfile.OSDisk.Name)
}

func toPtr[T any](value T) *T { return &value }
