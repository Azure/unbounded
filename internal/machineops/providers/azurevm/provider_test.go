// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azurevm

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1alpha1 "github.com/Azure/unbounded/api/infrastructure/v1alpha1"
	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineops"
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

func TestProviderExecute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		operation unboundedv1alpha3.OperationKind
		wantCalls []string
	}{
		{name: "hard reboot", operation: unboundedv1alpha3.OperationHostReboot, wantCalls: []string{"restart:rg/vm1"}},
		{name: "power off", operation: unboundedv1alpha3.OperationHostPowerOff, wantCalls: []string{"powerOff:rg/vm1"}},
		{name: "power on", operation: unboundedv1alpha3.OperationHostPowerOn, wantCalls: []string{"start:rg/vm1"}},
		{name: "replace", operation: unboundedv1alpha3.OperationHostReplace, wantCalls: []string{"replace:rg/vm1:cloud-init:"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &recordingAzureVMClient{}
			provider := &Provider{
				NewClient: func(subscriptionID string) (azureVMClient, error) {
					require.Equal(t, "sub", subscriptionID)
					return client, nil
				},
			}

			_, err := provider.Execute(context.Background(), machineops.OperationRequest{ProviderID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1", Operation: tt.operation, ReplaceUserData: "cloud-init"})

			require.NoError(t, err)
			require.Equal(t, tt.wantCalls, client.calls)
		})
	}
}

func TestProviderExecutePassesAuthToClientFactory(t *testing.T) {
	t.Parallel()

	auth := &machineops.OperationAuth{
		Mode: unboundedv1alpha3.MachineOperationCredentialAuthExternalPlugin,
		SecretData: map[string]string{
			"tenantID":     "tenant",
			"clientID":     "client",
			"clientSecret": "secret",
		},
	}
	client := &recordingAzureVMClient{}
	provider := &Provider{
		NewClientWithAuth: func(subscriptionID string, gotAuth *machineops.OperationAuth) (azureVMClient, error) {
			require.Equal(t, "sub", subscriptionID)
			require.Equal(t, auth, gotAuth)

			return client, nil
		},
	}

	_, err := provider.Execute(context.Background(), machineops.OperationRequest{
		ProviderID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
		Operation:  unboundedv1alpha3.OperationHostPowerOn,
		Auth:       auth,
	})

	require.NoError(t, err)
	require.Equal(t, []string{"start:rg/vm1"}, client.calls)
}

func TestProviderExecuteUsesAzureMachineProviderRef(t *testing.T) {
	t.Parallel()

	machineUID := types.UID("machine-uid")
	controller := true
	azureMachine := &infrastructurev1alpha1.AzureMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "azure-machine-1",
			UID:        "azure-machine-uid",
			Generation: 3,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: unboundedv1alpha3.GroupVersion.String(),
				Kind:       "Machine",
				Name:       "machine-1",
				UID:        machineUID,
				Controller: &controller,
			}},
		},
		Spec: infrastructurev1alpha1.AzureMachineSpec{
			ResourceID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
		},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, infrastructurev1alpha1.AddToScheme(scheme))
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(azureMachine).Build()
	azureClient := &recordingAzureVMClient{}
	provider := &Provider{
		KubeClient: kubeClient,
		NewClient: func(subscriptionID string) (azureVMClient, error) {
			require.Equal(t, "sub", subscriptionID)

			return azureClient, nil
		},
	}

	_, err := provider.Execute(context.Background(), machineops.OperationRequest{
		MachineName: "machine-1",
		MachineUID:  machineUID,
		ProviderRef: &unboundedv1alpha3.ProviderMachineSnapshot{
			APIGroup:   infrastructurev1alpha1.GroupVersion.Group,
			Kind:       infrastructurev1alpha1.AzureMachineKind,
			Name:       azureMachine.Name,
			UID:        azureMachine.UID,
			Generation: azureMachine.Generation,
		},
		ProviderID: "azure:///subscriptions/wrong/resourceGroups/wrong/providers/Microsoft.Compute/virtualMachines/wrong",
		Operation:  unboundedv1alpha3.OperationHostPowerOn,
	})

	require.NoError(t, err)
	require.Equal(t, []string{"start:rg/vm1"}, azureClient.calls)
}

func TestProviderExecuteRejectsChangedAzureMachine(t *testing.T) {
	t.Parallel()

	azureMachine := &infrastructurev1alpha1.AzureMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "azure-machine-1", UID: "current-uid", Generation: 4},
		Spec: infrastructurev1alpha1.AzureMachineSpec{
			ResourceID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, infrastructurev1alpha1.AddToScheme(scheme))
	provider := &Provider{KubeClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(azureMachine).Build()}

	tests := []struct {
		name       string
		uid        types.UID
		generation int64
		wantErr    string
	}{
		{name: "UID changed", uid: "old-uid", generation: azureMachine.Generation, wantErr: "UID changed"},
		{name: "generation changed", uid: azureMachine.UID, generation: 3, wantErr: "generation changed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := provider.Execute(context.Background(), machineops.OperationRequest{
				ProviderRef: &unboundedv1alpha3.ProviderMachineSnapshot{
					APIGroup:   infrastructurev1alpha1.GroupVersion.Group,
					Kind:       infrastructurev1alpha1.AzureMachineKind,
					Name:       azureMachine.Name,
					UID:        tt.uid,
					Generation: tt.generation,
				},
				Operation: unboundedv1alpha3.OperationHostPowerOn,
			})
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
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

			_, err := newAzureVMClient("sub", tt.auth)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestProviderExecuteHostReplaceRequiresUserData(t *testing.T) {
	t.Parallel()

	provider := &Provider{NewClient: func(subscriptionID string) (azureVMClient, error) {
		require.Equal(t, "sub", subscriptionID)
		return &recordingAzureVMClient{}, nil
	}}

	_, err := provider.Execute(context.Background(), machineops.OperationRequest{ProviderID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1", Operation: unboundedv1alpha3.OperationHostReplace})
	require.Error(t, err)
	require.Contains(t, err.Error(), "replacement user data is required")
}

func TestProviderRegistrationDoesNotDeclareHostReplaceReplaySafe(t *testing.T) {
	t.Parallel()

	provider, err := (&Provider{}).Registration()
	require.NoError(t, err)

	operation, ok := provider.Operation(unboundedv1alpha3.OperationHostReplace)
	require.True(t, ok)
	require.False(t, operation.ReplaySafe())

	groupKind, ok := provider.ProviderMachineKind()
	require.True(t, ok)
	require.Equal(t, infrastructurev1alpha1.GroupVersion.WithKind(infrastructurev1alpha1.AzureMachineKind).GroupKind(), groupKind)
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
		"#cloud-config\n",
		"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/galleries/gallery/images/image/versions/2.0.0",
		123,
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
			OSProfile:         &armcompute.OSProfile{RequireGuestProvisionSignal: toPtr(true)},
			StorageProfile: &armcompute.StorageProfile{OSDisk: &armcompute.OSDisk{
				Name:        toPtr("old-osdisk"),
				ManagedDisk: &armcompute.ManagedDiskParameters{},
			}},
		},
	}

	replacement, err := prepareReplacementVM(vm, "#cloud-config\n", "", 123)
	require.NoError(t, err)

	require.Nil(t, replacement.ID)
	require.Nil(t, replacement.Name)
	require.Equal(t, &armcompute.UserAssignedIdentitiesValue{}, replacement.Identity.UserAssignedIdentities["/subscriptions/sub/resourceGroups/rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/id1"])
	require.Nil(t, replacement.Properties.ProvisioningState)

	wantCustomData := base64.StdEncoding.EncodeToString([]byte("#cloud-config\n"))
	require.Equal(t, wantCustomData, *replacement.Properties.UserData)
	require.Equal(t, wantCustomData, *replacement.Properties.OSProfile.CustomData)
	require.Nil(t, replacement.Properties.OSProfile.RequireGuestProvisionSignal)
	require.Equal(t, armcompute.DiskCreateOptionTypesFromImage, *replacement.Properties.StorageProfile.OSDisk.CreateOption)
	require.NotNil(t, replacement.Properties.StorageProfile.OSDisk.ManagedDisk)
	require.Equal(t, "vm1-osdisk-123", *replacement.Properties.StorageProfile.OSDisk.Name)
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

type recordingAzureVMClient struct {
	calls []string
	err   error
}

func (c *recordingAzureVMClient) Restart(_ context.Context, resourceGroupName, vmName string) error {
	c.calls = append(c.calls, fmt.Sprintf("restart:%s/%s", resourceGroupName, vmName))
	return c.err
}

func (c *recordingAzureVMClient) PowerOff(_ context.Context, resourceGroupName, vmName string) error {
	c.calls = append(c.calls, fmt.Sprintf("powerOff:%s/%s", resourceGroupName, vmName))
	return c.err
}

func (c *recordingAzureVMClient) Start(_ context.Context, resourceGroupName, vmName string) error {
	c.calls = append(c.calls, fmt.Sprintf("start:%s/%s", resourceGroupName, vmName))
	return c.err
}

func (c *recordingAzureVMClient) Replace(_ context.Context, resourceGroupName, vmName, userData, hostImage string) error {
	c.calls = append(c.calls, fmt.Sprintf("replace:%s/%s:%s:%s", resourceGroupName, vmName, userData, hostImage))
	return c.err
}

func toPtr[T any](value T) *T { return &value }
