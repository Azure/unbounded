// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azurevm

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineops"
)

type azureVMClient interface {
	Start(ctx context.Context, resourceGroupName, vmName string) error
	PowerOff(ctx context.Context, resourceGroupName, vmName string) error
	Restart(ctx context.Context, resourceGroupName, vmName string) error
}

type azureVMClientFactory func(subscriptionID string) (azureVMClient, error)
type azureVMOperation func(context.Context, azureVMClient, azureVMResourceRef) error

var azureVMOperations = map[unboundedv1alpha3.OperationKind]azureVMOperation{
	unboundedv1alpha3.OperationHardReboot: func(ctx context.Context, client azureVMClient, ref azureVMResourceRef) error {
		return client.Restart(ctx, ref.ResourceGroup, ref.VMName)
	},
	unboundedv1alpha3.OperationPowerOff: func(ctx context.Context, client azureVMClient, ref azureVMResourceRef) error {
		return client.PowerOff(ctx, ref.ResourceGroup, ref.VMName)
	},
	unboundedv1alpha3.OperationPowerOn: func(ctx context.Context, client azureVMClient, ref azureVMResourceRef) error {
		return client.Start(ctx, ref.ResourceGroup, ref.VMName)
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
	SubscriptionID string
	ResourceGroup  string
	VMName         string
}

func parseAzureVMProviderID(providerID string) (azureVMResourceRef, error) {
	providerID = strings.TrimSpace(providerID)
	providerID = strings.TrimPrefix(providerID, "azure://")
	providerID = strings.TrimPrefix(providerID, "azure:")
	providerID = strings.Trim(providerID, "/")

	if providerID == "" {
		return azureVMResourceRef{}, fmt.Errorf("Azure VM providerID is required")
	}

	parts := strings.Split(providerID, "/")
	if len(parts) != 8 {
		return azureVMResourceRef{}, fmt.Errorf("Azure VM providerID must be azure:///subscriptions/{subscription}/resourceGroups/{resourceGroup}/providers/Microsoft.Compute/virtualMachines/{name}")
	}

	if !strings.EqualFold(parts[0], "subscriptions") ||
		!strings.EqualFold(parts[2], "resourceGroups") ||
		!strings.EqualFold(parts[4], "providers") ||
		!strings.EqualFold(parts[5], "Microsoft.Compute") ||
		!strings.EqualFold(parts[6], "virtualMachines") {
		return azureVMResourceRef{}, fmt.Errorf("Azure VM providerID must identify a Microsoft.Compute/virtualMachines resource")
	}

	ref := azureVMResourceRef{
		SubscriptionID: parts[1],
		ResourceGroup:  parts[3],
		VMName:         parts[7],
	}

	if ref.SubscriptionID == "" || ref.ResourceGroup == "" || ref.VMName == "" {
		return azureVMResourceRef{}, fmt.Errorf("Azure VM providerID is missing subscription, resource group, or VM name")
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
