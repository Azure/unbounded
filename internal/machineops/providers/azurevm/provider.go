// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineops"
	publicmachineops "github.com/Azure/unbounded/pkg/machineops"
)

const azureCustomDataMaxBytes = 65535

type azureVMClient interface {
	Start(ctx context.Context, resourceGroupName, vmName string) error
	PowerOff(ctx context.Context, resourceGroupName, vmName string) error
	Restart(ctx context.Context, resourceGroupName, vmName string) error
	Replace(ctx context.Context, resourceGroupName, vmName, userData, hostImage string) error
}

type azureVMClientFactory func(subscriptionID string) (azureVMClient, error)

type azureVMClientFactoryWithAuth func(subscriptionID string, auth *machineops.OperationAuth) (azureVMClient, error)

type azureVMOperation func(context.Context, azureVMClient, azureVMResourceRef) error

var azureVMOperations = map[unboundedv1alpha3.OperationKind]azureVMOperation{
	unboundedv1alpha3.OperationHostReboot: func(ctx context.Context, client azureVMClient, ref azureVMResourceRef) error {
		return client.Restart(ctx, ref.ResourceGroup, ref.VMName)
	},
	unboundedv1alpha3.OperationHostPowerOff: func(ctx context.Context, client azureVMClient, ref azureVMResourceRef) error {
		return client.PowerOff(ctx, ref.ResourceGroup, ref.VMName)
	},
	unboundedv1alpha3.OperationHostPowerOn: func(ctx context.Context, client azureVMClient, ref azureVMResourceRef) error {
		return client.Start(ctx, ref.ResourceGroup, ref.VMName)
	},
	unboundedv1alpha3.OperationHostReplace: func(ctx context.Context, client azureVMClient, ref azureVMResourceRef) error {
		if ref.ReplaceUserData == "" {
			return fmt.Errorf("replacement user data is required for Azure VM HostReplace")
		}

		return client.Replace(ctx, ref.ResourceGroup, ref.VMName, ref.ReplaceUserData, ref.HostImage)
	},
}

// Provider executes operations against Azure virtual machines.
type Provider struct {
	NewClient         azureVMClientFactory
	NewClientWithAuth azureVMClientFactoryWithAuth
}

func (p *Provider) Name() string {
	return unboundedv1alpha3.ExternalProviderAzureVM
}

// Registration returns this Azure adapter's MachineOperation lifecycle
// registration.
func (p *Provider) Registration() (*publicmachineops.Provider, error) {
	options := make([]publicmachineops.ProviderOption, 0, len(azureVMOperations))

	for kind := range azureVMOperations {
		operationOptions := []publicmachineops.OperationOption(nil)
		if kind == unboundedv1alpha3.OperationHostReplace {
			operationOptions = append(
				operationOptions,
				publicmachineops.RequiresReplaceUserData(),
			)
		}

		options = append(options, publicmachineops.WithImmediateOperation(kind, p.Execute, operationOptions...))
	}

	return publicmachineops.NewProvider(p.Name(), options...)
}

func (p *Provider) Execute(ctx context.Context, request machineops.OperationRequest) (machineops.OperationResult, error) {
	operation, ok := azureVMOperations[request.Operation]
	if !ok {
		return machineops.OperationResult{}, fmt.Errorf("unsupported Azure VM operation %q", request.Operation)
	}

	ref, err := parseAzureVMProviderID(request.ProviderID)
	if err != nil {
		return machineops.OperationResult{}, err
	}

	ref.ReplaceUserData = request.ReplaceUserData
	ref.HostImage = request.HostImage

	client, err := p.client(ref.SubscriptionID, request.Auth)
	if err != nil {
		return machineops.OperationResult{}, fmt.Errorf("create Azure VM client: %w", err)
	}

	return machineops.OperationResult{}, operation(ctx, client, ref)
}

func (p *Provider) client(subscriptionID string, auth *machineops.OperationAuth) (azureVMClient, error) {
	if p.NewClientWithAuth != nil {
		return p.NewClientWithAuth(subscriptionID, auth)
	}

	if p.NewClient != nil {
		return p.NewClient(subscriptionID)
	}

	return newAzureVMClient(subscriptionID, auth)
}

type azureVMResourceRef struct {
	SubscriptionID  string
	ResourceGroup   string
	VMName          string
	ReplaceUserData string
	HostImage       string
}

func parseAzureVMProviderID(providerID string) (azureVMResourceRef, error) {
	providerID = strings.TrimSpace(providerID)
	providerID = strings.TrimPrefix(providerID, "azure://")
	providerID = strings.TrimPrefix(providerID, "azure:")
	providerID = strings.Trim(providerID, "/")

	if providerID == "" {
		return azureVMResourceRef{}, fmt.Errorf("azure VM providerID is required")
	}

	parts := strings.Split(providerID, "/")
	if len(parts) != 8 {
		return azureVMResourceRef{}, fmt.Errorf("azure VM providerID must be azure:///subscriptions/{subscription}/resourceGroups/{resourceGroup}/providers/Microsoft.Compute/virtualMachines/{name}")
	}

	if !strings.EqualFold(parts[0], "subscriptions") ||
		!strings.EqualFold(parts[2], "resourceGroups") ||
		!strings.EqualFold(parts[4], "providers") ||
		!strings.EqualFold(parts[5], "Microsoft.Compute") ||
		!strings.EqualFold(parts[6], "virtualMachines") {
		return azureVMResourceRef{}, fmt.Errorf("azure VM providerID must identify a Microsoft.Compute/virtualMachines resource")
	}

	ref := azureVMResourceRef{
		SubscriptionID: parts[1],
		ResourceGroup:  parts[3],
		VMName:         parts[7],
	}

	if ref.SubscriptionID == "" || ref.ResourceGroup == "" || ref.VMName == "" {
		return azureVMResourceRef{}, fmt.Errorf("azure VM providerID is missing subscription, resource group, or VM name")
	}

	return ref, nil
}

// NormalizeResourceID returns the canonical Azure Resource Manager ID for a
// raw ARM ID or Kubernetes-style Azure provider ID.
func NormalizeResourceID(providerID string) (string, error) {
	ref, err := parseAzureVMProviderID(providerID)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(
		"/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/virtualMachines/%s",
		ref.SubscriptionID,
		ref.ResourceGroup,
		ref.VMName,
	), nil
}

func newDefaultAzureVMClient(subscriptionID string) (azureVMClient, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create default Azure credential: %w", err)
	}

	return newAzureVMClientWithCredential(subscriptionID, cred)
}

func newAzureVMClient(subscriptionID string, auth *machineops.OperationAuth) (azureVMClient, error) {
	if auth == nil || auth.Mode == "" || auth.Mode == unboundedv1alpha3.MachineOperationCredentialAuthWorkloadIdentity {
		return newDefaultAzureVMClient(subscriptionID)
	}

	switch auth.Mode {
	case unboundedv1alpha3.MachineOperationCredentialAuthExternalPlugin:
		tenantID, err := auth.RequiredSecretValue("tenantID")
		if err != nil {
			return nil, fmt.Errorf("read Azure external plugin tenantID: %w", err)
		}

		clientID, err := auth.RequiredSecretValue("clientID")
		if err != nil {
			return nil, fmt.Errorf("read Azure external plugin clientID: %w", err)
		}

		clientSecret, err := auth.RequiredSecretValue("clientSecret")
		if err != nil {
			return nil, fmt.Errorf("read Azure external plugin clientSecret: %w", err)
		}

		cred, err := azidentity.NewClientSecretCredential(tenantID, clientID, clientSecret, nil)
		if err != nil {
			return nil, fmt.Errorf("create Azure service principal credential: %w", err)
		}

		return newAzureVMClientWithCredential(subscriptionID, cred)
	default:
		return nil, fmt.Errorf("unsupported Azure VM auth mode %q", auth.Mode)
	}
}

func newAzureVMClientWithCredential(subscriptionID string, cred azcore.TokenCredential) (azureVMClient, error) {
	client, err := armcompute.NewVirtualMachinesClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("create virtual machines client: %w", err)
	}

	return &armAzureVMClient{client: client}, nil
}

type armAzureVMClient struct {
	client *armcompute.VirtualMachinesClient
}

func (c *armAzureVMClient) Start(ctx context.Context, resourceGroupName, vmName string) error {
	poller, err := c.client.BeginStart(ctx, resourceGroupName, vmName, nil)
	if err != nil {
		return fmt.Errorf("start VM %s/%s: %w", resourceGroupName, vmName, err)
	}

	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("wait for VM %s/%s start: %w", resourceGroupName, vmName, err)
	}

	return nil
}

func (c *armAzureVMClient) PowerOff(ctx context.Context, resourceGroupName, vmName string) error {
	poller, err := c.client.BeginPowerOff(ctx, resourceGroupName, vmName, nil)
	if err != nil {
		return fmt.Errorf("power off VM %s/%s: %w", resourceGroupName, vmName, err)
	}

	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("wait for VM %s/%s power off: %w", resourceGroupName, vmName, err)
	}

	return nil
}

func (c *armAzureVMClient) Restart(ctx context.Context, resourceGroupName, vmName string) error {
	poller, err := c.client.BeginRestart(ctx, resourceGroupName, vmName, nil)
	if err != nil {
		return fmt.Errorf("restart VM %s/%s: %w", resourceGroupName, vmName, err)
	}

	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("wait for VM %s/%s restart: %w", resourceGroupName, vmName, err)
	}

	return nil
}

func (c *armAzureVMClient) Replace(ctx context.Context, resourceGroupName, vmName, userData, hostImage string) error {
	if err := validateAzureCustomData(userData); err != nil {
		return fmt.Errorf("validate Azure replacement custom data: %w", err)
	}

	vm, err := c.client.Get(ctx, resourceGroupName, vmName, nil)
	if err != nil && !isAzureNotFound(err) {
		return fmt.Errorf("get VM %s/%s: %w", resourceGroupName, vmName, err)
	}

	if isAzureNotFound(err) {
		return fmt.Errorf("get VM %s/%s: not found; replacement cannot recover without the original VM model", resourceGroupName, vmName)
	}

	source := vm.VirtualMachine
	deleteOptionPayload := prepareVMForReplacementDelete(source)

	replacement, err := prepareReplacementVM(source, userData, hostImage, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("prepare replacement VM %s/%s: %w", resourceGroupName, vmName, err)
	}

	updatePoller, err := c.client.BeginCreateOrUpdate(ctx, resourceGroupName, vmName, deleteOptionPayload, nil)
	if err != nil {
		return fmt.Errorf("update VM %s/%s delete options before replacement: %w", resourceGroupName, vmName, err)
	}

	if _, err := updatePoller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("wait for VM %s/%s delete option update before replacement: %w", resourceGroupName, vmName, err)
	}

	deletePoller, err := c.client.BeginDelete(ctx, resourceGroupName, vmName, &armcompute.VirtualMachinesClientBeginDeleteOptions{ForceDeletion: to.Ptr(false)})
	if err != nil {
		return fmt.Errorf("delete VM %s/%s before replacement: %w", resourceGroupName, vmName, err)
	}

	if _, err := deletePoller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("wait for VM %s/%s delete before replacement: %w", resourceGroupName, vmName, err)
	}

	createPoller, err := c.client.BeginCreateOrUpdate(ctx, resourceGroupName, vmName, replacement, nil)
	if err != nil {
		return fmt.Errorf("create replacement VM %s/%s: %w", resourceGroupName, vmName, err)
	}

	if _, err := createPoller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("wait for replacement VM %s/%s create: %w", resourceGroupName, vmName, err)
	}

	return nil
}

func prepareVMForReplacementDelete(vm armcompute.VirtualMachine) armcompute.VirtualMachine {
	// Azure SDK GET responses include read-only fields and child resources that
	// are invalid in a VM update payload. Clone the nested fields changed below
	// so preparing the independent replacement payload cannot alter this update.
	vm = cloneVMForReplacementMutation(vm)
	vm.ID = nil
	vm.Name = nil
	vm.Type = nil
	vm.Etag = nil
	vm.ManagedBy = nil
	vm.Resources = nil

	if vm.Properties == nil {
		return vm
	}

	if vm.Properties.NetworkProfile != nil {
		for _, nic := range vm.Properties.NetworkProfile.NetworkInterfaces {
			if nic == nil {
				continue
			}

			if nic.Properties == nil {
				nic.Properties = &armcompute.NetworkInterfaceReferenceProperties{}
			}

			nic.Properties.DeleteOption = to.Ptr(armcompute.DeleteOptionsDetach)
		}
	}

	if vm.Properties.StorageProfile != nil {
		if vm.Properties.StorageProfile.OSDisk != nil {
			vm.Properties.StorageProfile.OSDisk.DeleteOption = to.Ptr(armcompute.DiskDeleteOptionTypesDelete)
		}

		for _, disk := range vm.Properties.StorageProfile.DataDisks {
			disk.DeleteOption = to.Ptr(armcompute.DiskDeleteOptionTypesDetach)
		}
	}

	return vm
}

func prepareReplacementVM(vm armcompute.VirtualMachine, userData, hostImage string, diskNameSuffix int64) (armcompute.VirtualMachine, error) {
	// Clone the nested fields changed below so this create payload remains
	// independent from the pre-delete update payload built from the same source.
	vm = cloneVMForReplacementMutation(vm)

	imageReference, err := parseAzureImageReference(hostImage)
	if err != nil {
		return armcompute.VirtualMachine{}, err
	}

	if imageReference != nil && (vm.Properties == nil || vm.Properties.StorageProfile == nil || vm.Properties.StorageProfile.OSDisk == nil) {
		return armcompute.VirtualMachine{}, fmt.Errorf("existing VM has no OS disk storage profile to replace the host image")
	}

	vmName := toValue(vm.Name, "replacement")
	vm.ID = nil
	vm.Name = nil
	vm.Type = nil
	vm.Etag = nil
	vm.ManagedBy = nil

	vm.Resources = nil
	if vm.Identity != nil {
		for id := range vm.Identity.UserAssignedIdentities {
			vm.Identity.UserAssignedIdentities[id] = &armcompute.UserAssignedIdentitiesValue{}
		}
	}

	if vm.Properties != nil {
		vm.Properties.ProvisioningState = nil
		vm.Properties.TimeCreated = nil
		vm.Properties.VMID = nil
		vm.Properties.InstanceView = nil
		encodedUserData := base64.StdEncoding.EncodeToString([]byte(userData))

		vm.Properties.UserData = &encodedUserData
		if vm.Properties.OSProfile != nil {
			vm.Properties.OSProfile.CustomData = &encodedUserData
			vm.Properties.OSProfile.RequireGuestProvisionSignal = nil
		}

		if vm.Properties.StorageProfile != nil && vm.Properties.StorageProfile.OSDisk != nil {
			if imageReference != nil {
				vm.Properties.StorageProfile.ImageReference = imageReference
			}

			vm.Properties.StorageProfile.OSDisk.CreateOption = to.Ptr(armcompute.DiskCreateOptionTypesFromImage)
			vm.Properties.StorageProfile.OSDisk.Vhd = nil

			if vm.Properties.StorageProfile.OSDisk.ManagedDisk != nil {
				vm.Properties.StorageProfile.OSDisk.ManagedDisk.ID = nil
			}

			vm.Properties.StorageProfile.OSDisk.Name = to.Ptr(fmt.Sprintf("%s-osdisk-%d", vmName, diskNameSuffix))
			for _, disk := range vm.Properties.StorageProfile.DataDisks {
				disk.DiskIOPSReadWrite = nil
				disk.DiskMBpsReadWrite = nil
				disk.ToBeDetached = nil
			}
		}
	}

	return vm, nil
}

func cloneVMForReplacementMutation(vm armcompute.VirtualMachine) armcompute.VirtualMachine {
	cloned := vm

	if vm.Identity != nil {
		identity := *vm.Identity
		cloned.Identity = &identity

		if vm.Identity.UserAssignedIdentities != nil {
			identity.UserAssignedIdentities = make(
				map[string]*armcompute.UserAssignedIdentitiesValue,
				len(vm.Identity.UserAssignedIdentities),
			)
			for id, value := range vm.Identity.UserAssignedIdentities {
				identity.UserAssignedIdentities[id] = value
			}
		}
	}

	if vm.Properties == nil {
		return cloned
	}

	properties := *vm.Properties
	cloned.Properties = &properties

	if vm.Properties.OSProfile != nil {
		osProfile := *vm.Properties.OSProfile
		properties.OSProfile = &osProfile
	}

	if vm.Properties.NetworkProfile != nil {
		networkProfile := *vm.Properties.NetworkProfile
		properties.NetworkProfile = &networkProfile

		if vm.Properties.NetworkProfile.NetworkInterfaces != nil {
			networkProfile.NetworkInterfaces = make(
				[]*armcompute.NetworkInterfaceReference,
				len(vm.Properties.NetworkProfile.NetworkInterfaces),
			)
			for i, reference := range vm.Properties.NetworkProfile.NetworkInterfaces {
				if reference == nil {
					continue
				}

				clonedReference := *reference
				networkProfile.NetworkInterfaces[i] = &clonedReference

				if reference.Properties != nil {
					referenceProperties := *reference.Properties
					clonedReference.Properties = &referenceProperties
				}
			}
		}
	}

	if vm.Properties.StorageProfile != nil {
		storageProfile := *vm.Properties.StorageProfile
		properties.StorageProfile = &storageProfile

		if vm.Properties.StorageProfile.OSDisk != nil {
			osDisk := *vm.Properties.StorageProfile.OSDisk
			storageProfile.OSDisk = &osDisk

			if vm.Properties.StorageProfile.OSDisk.ManagedDisk != nil {
				managedDisk := *vm.Properties.StorageProfile.OSDisk.ManagedDisk
				osDisk.ManagedDisk = &managedDisk
			}
		}

		if vm.Properties.StorageProfile.DataDisks != nil {
			storageProfile.DataDisks = make(
				[]*armcompute.DataDisk,
				len(vm.Properties.StorageProfile.DataDisks),
			)
			for i, dataDisk := range vm.Properties.StorageProfile.DataDisks {
				if dataDisk == nil {
					continue
				}

				clonedDataDisk := *dataDisk
				storageProfile.DataDisks[i] = &clonedDataDisk
			}
		}
	}

	return cloned
}

func parseAzureImageReference(hostImage string) (*armcompute.ImageReference, error) {
	hostImage = strings.TrimSpace(hostImage)
	if hostImage == "" {
		return nil, nil
	}

	if strings.HasPrefix(hostImage, "/") {
		lowerImage := strings.ToLower(strings.TrimRight(hostImage, "/"))
		if !strings.HasPrefix(lowerImage, "/subscriptions/") ||
			!strings.Contains(lowerImage, "/resourcegroups/") ||
			!strings.Contains(lowerImage, "/providers/microsoft.compute/") ||
			!strings.Contains(lowerImage, "/images/") {
			return nil, fmt.Errorf("host image Azure resource ID must identify a Microsoft.Compute image or gallery image")
		}

		return &armcompute.ImageReference{ID: to.Ptr(hostImage)}, nil
	}

	parts := strings.Split(hostImage, ":")
	if len(parts) != 4 {
		return nil, fmt.Errorf("host image must be an Azure resource ID or publisher:offer:sku:version reference")
	}

	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
		if parts[i] == "" {
			return nil, fmt.Errorf("host image publisher, offer, SKU, and version must be non-empty")
		}
	}

	return &armcompute.ImageReference{
		Publisher: to.Ptr(parts[0]),
		Offer:     to.Ptr(parts[1]),
		SKU:       to.Ptr(parts[2]),
		Version:   to.Ptr(parts[3]),
	}, nil
}

func validateAzureCustomData(userData string) error {
	if len([]byte(userData)) > azureCustomDataMaxBytes {
		return fmt.Errorf("replacement cloud-init is %d bytes, exceeding Azure customData limit of %d bytes", len([]byte(userData)), azureCustomDataMaxBytes)
	}

	return nil
}

func isAzureNotFound(err error) bool {
	var responseErr *azcore.ResponseError
	return err != nil && errors.As(err, &responseErr) && responseErr.StatusCode == http.StatusNotFound
}

func toValue(value *string, fallback string) string {
	if value == nil || *value == "" {
		return fallback
	}

	return *value
}
