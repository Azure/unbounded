// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
)

const azureCustomDataMaxBytes = 65535

type azureVMClient interface {
	Start(ctx context.Context, resourceGroupName, vmName string) error
	PowerOff(ctx context.Context, resourceGroupName, vmName string) error
	Restart(ctx context.Context, resourceGroupName, vmName string) error
	Replace(ctx context.Context, resourceGroupName, vmName, userData string) error
}

type azureVMClientFactory func(subscriptionID string) (azureVMClient, error)

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

		return client.Replace(ctx, ref.ResourceGroup, ref.VMName, ref.ReplaceUserData)
	},
}

// Provider executes operations against Azure virtual machines.
type Provider struct {
	NewClient azureVMClientFactory
}

func (p *Provider) Name() string {
	return unboundedv1alpha3.ExternalProviderAzureVM
}

func (p *Provider) Supports(operation unboundedv1alpha3.OperationKind) bool {
	_, ok := azureVMOperations[operation]
	return ok
}

func (p *Provider) Execute(ctx context.Context, request machineops.OperationRequest) error {
	operation, ok := azureVMOperations[request.Operation]
	if !ok {
		return fmt.Errorf("unsupported Azure VM operation %q", request.Operation)
	}

	ref, err := parseAzureVMProviderID(request.ProviderID)
	if err != nil {
		return err
	}

	ref.ReplaceUserData = request.ReplaceUserData

	newClient := p.NewClient
	if newClient == nil {
		newClient = newDefaultAzureVMClient
	}

	client, err := newClient(ref.SubscriptionID)
	if err != nil {
		return fmt.Errorf("create Azure VM client: %w", err)
	}

	return operation(ctx, client, ref)
}

type azureVMResourceRef struct {
	SubscriptionID  string
	ResourceGroup   string
	VMName          string
	ReplaceUserData string
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

func newDefaultAzureVMClient(subscriptionID string) (azureVMClient, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create default Azure credential: %w", err)
	}

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

func (c *armAzureVMClient) Replace(ctx context.Context, resourceGroupName, vmName, userData string) error {
	if err := validateAzureCustomData(userData); err != nil {
		return err
	}

	vm, err := c.client.Get(ctx, resourceGroupName, vmName, nil)
	if err != nil && !isAzureNotFound(err) {
		return fmt.Errorf("get VM %s/%s: %w", resourceGroupName, vmName, err)
	}

	if isAzureNotFound(err) {
		return fmt.Errorf("get VM %s/%s: not found; replacement cannot recover without the original VM model", resourceGroupName, vmName)
	}

	existing := prepareVMForReplacementDelete(vm.VirtualMachine)

	updatePoller, err := c.client.BeginCreateOrUpdate(ctx, resourceGroupName, vmName, existing, nil)
	if err != nil {
		return fmt.Errorf("update VM %s/%s delete options before replacement: %w", resourceGroupName, vmName, err)
	}

	if _, err := updatePoller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("wait for VM %s/%s delete option update before replacement: %w", resourceGroupName, vmName, err)
	}

	replacement := prepareReplacementVM(existing, userData, time.Now().Unix())

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
	// are invalid in a VM update payload. The nested objects are intentionally
	// mutated because the next operation immediately sends this model back to ARM.
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

func prepareReplacementVM(vm armcompute.VirtualMachine, userData string, diskNameSuffix int64) armcompute.VirtualMachine {
	// This mutates the captured VM model into a create payload for the replacement
	// VM while preserving configurable settings from the original resource.
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
			vm.Properties.StorageProfile.OSDisk.CreateOption = to.Ptr(armcompute.DiskCreateOptionTypesFromImage)
			vm.Properties.StorageProfile.OSDisk.Vhd = nil

			vm.Properties.StorageProfile.OSDisk.Name = to.Ptr(fmt.Sprintf("%s-osdisk-%d", vmName, diskNameSuffix))
			for _, disk := range vm.Properties.StorageProfile.DataDisks {
				disk.DiskIOPSReadWrite = nil
				disk.DiskMBpsReadWrite = nil
				disk.ToBeDetached = nil
			}
		}
	}

	return vm
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
