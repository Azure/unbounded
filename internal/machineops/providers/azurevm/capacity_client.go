// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineops"
)

const skuCacheTTL = 5 * time.Minute

// capacityClient is the narrow ARM surface capacity allocation uses. Get
// methods return nil without error only when Azure reports that the resource
// or its resource group does not exist (see isAzureNotFound); any other
// failure, including a 404 with another error code, is returned so callers
// never mistake an error for absence.
type capacityClient interface {
	GetVM(ctx context.Context, resourceGroup, name string, instanceView bool) (*armcompute.VirtualMachine, error)
	CreateVM(ctx context.Context, resourceGroup, name string, vm armcompute.VirtualMachine) error
	DeleteVM(ctx context.Context, resourceGroup, name string) error
	GetNIC(ctx context.Context, resourceGroup, name string) (*armnetwork.Interface, error)
	// ListNICNames returns the name of every network interface in the
	// resource group, or none when the resource group does not exist.
	ListNICNames(ctx context.Context, resourceGroup string) ([]string, error)
	DeleteNIC(ctx context.Context, resourceGroup, name string) error
	GetDisk(ctx context.Context, resourceGroup, name string) (*armcompute.Disk, error)
	DeleteDisk(ctx context.Context, resourceGroup, name string) error
	VMSKUs(ctx context.Context, location string) (map[string]*armcompute.ResourceSKU, error)
}

type capacityClientFactory func(subscriptionID string, auth *machineops.OperationAuth) (capacityClient, error)

// credentialCache shares token credentials across reconciles so each
// operation does not perform a fresh token exchange.
type credentialCache struct {
	mu          sync.Mutex
	credentials map[string]azcore.TokenCredential
}

func (c *credentialCache) credential(auth *machineops.OperationAuth) (azcore.TokenCredential, error) {
	key, create, err := credentialSource(auth)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if credential, ok := c.credentials[key]; ok {
		return credential, nil
	}

	credential, err := create()
	if err != nil {
		return nil, err
	}

	if c.credentials == nil {
		c.credentials = map[string]azcore.TokenCredential{}
	}

	c.credentials[key] = credential

	return credential, nil
}

// credentialSource returns a cache key that never contains the secret itself.
func credentialSource(auth *machineops.OperationAuth) (string, func() (azcore.TokenCredential, error), error) {
	if auth == nil || auth.Mode == "" || auth.Mode == unboundedv1alpha3.MachineOperationCredentialAuthWorkloadIdentity {
		return "default", func() (azcore.TokenCredential, error) {
			credential, err := azidentity.NewDefaultAzureCredential(nil)
			if err != nil {
				return nil, fmt.Errorf("create default Azure credential: %w", err)
			}

			return credential, nil
		}, nil
	}

	if auth.Mode != unboundedv1alpha3.MachineOperationCredentialAuthExternalPlugin {
		return "", nil, fmt.Errorf("unsupported Azure VM auth mode %q", auth.Mode)
	}

	tenantID, err := auth.RequiredSecretValue("tenantID")
	if err != nil {
		return "", nil, fmt.Errorf("read Azure external plugin tenantID: %w", err)
	}

	clientID, err := auth.RequiredSecretValue("clientID")
	if err != nil {
		return "", nil, fmt.Errorf("read Azure external plugin clientID: %w", err)
	}

	clientSecret, err := auth.RequiredSecretValue("clientSecret")
	if err != nil {
		return "", nil, fmt.Errorf("read Azure external plugin clientSecret: %w", err)
	}

	digest := sha256.Sum256([]byte(tenantID + "\x00" + clientID + "\x00" + clientSecret))
	key := "secret/" + hex.EncodeToString(digest[:])

	return key, func() (azcore.TokenCredential, error) {
		credential, err := azidentity.NewClientSecretCredential(tenantID, clientID, clientSecret, nil)
		if err != nil {
			return nil, fmt.Errorf("create Azure service principal credential: %w", err)
		}

		return credential, nil
	}, nil
}

type armCapacityClient struct {
	vms   *armcompute.VirtualMachinesClient
	disks *armcompute.DisksClient
	skus  *armcompute.ResourceSKUsClient
	nics  *armnetwork.InterfacesClient
}

func newARMCapacityClient(subscriptionID string, credential azcore.TokenCredential) (*armCapacityClient, error) {
	vms, err := armcompute.NewVirtualMachinesClient(subscriptionID, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("create virtual machines client: %w", err)
	}

	disks, err := armcompute.NewDisksClient(subscriptionID, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("create disks client: %w", err)
	}

	skus, err := armcompute.NewResourceSKUsClient(subscriptionID, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("create resource SKUs client: %w", err)
	}

	nics, err := armnetwork.NewInterfacesClient(subscriptionID, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("create network interfaces client: %w", err)
	}

	return &armCapacityClient{
		vms:   vms,
		disks: disks,
		skus:  skus,
		nics:  nics,
	}, nil
}

func (c *armCapacityClient) GetVM(ctx context.Context, resourceGroup, name string, instanceView bool) (*armcompute.VirtualMachine, error) {
	var options *armcompute.VirtualMachinesClientGetOptions
	if instanceView {
		options = &armcompute.VirtualMachinesClientGetOptions{
			Expand: new(armcompute.InstanceViewTypesInstanceView),
		}
	}

	response, err := c.vms.Get(ctx, resourceGroup, name, options)
	if isAzureNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("get VM %s/%s: %w", resourceGroup, name, err)
	}

	return &response.VirtualMachine, nil
}

// CreateVM sends the PUT and returns once Azure accepts it. Completion is
// observed through GetVM so a controller restart can resume from the
// deterministic resource ID alone.
func (c *armCapacityClient) CreateVM(ctx context.Context, resourceGroup, name string, vm armcompute.VirtualMachine) error {
	if _, err := c.vms.BeginCreateOrUpdate(ctx, resourceGroup, name, vm, nil); err != nil {
		return fmt.Errorf("create VM %s/%s: %w", resourceGroup, name, err)
	}

	return nil
}

func (c *armCapacityClient) DeleteVM(ctx context.Context, resourceGroup, name string) error {
	options := &armcompute.VirtualMachinesClientBeginDeleteOptions{
		ForceDeletion: new(false),
	}

	_, err := c.vms.BeginDelete(ctx, resourceGroup, name, options)
	if err != nil && !isAzureNotFound(err) {
		return fmt.Errorf("delete VM %s/%s: %w", resourceGroup, name, err)
	}

	return nil
}

func (c *armCapacityClient) GetNIC(ctx context.Context, resourceGroup, name string) (*armnetwork.Interface, error) {
	response, err := c.nics.Get(ctx, resourceGroup, name, nil)
	if isAzureNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("get network interface %s/%s: %w", resourceGroup, name, err)
	}

	return &response.Interface, nil
}

func (c *armCapacityClient) ListNICNames(ctx context.Context, resourceGroup string) ([]string, error) {
	pager := c.nics.NewListPager(resourceGroup, nil)

	var names []string

	for pager.More() {
		page, err := pager.NextPage(ctx)
		if isAzureNotFound(err) {
			return nil, nil
		}

		if err != nil {
			return nil, fmt.Errorf("list network interfaces in %s: %w", resourceGroup, err)
		}

		for _, nic := range page.Value {
			if nic != nil && nic.Name != nil {
				names = append(names, *nic.Name)
			}
		}
	}

	return names, nil
}

func (c *armCapacityClient) DeleteNIC(ctx context.Context, resourceGroup, name string) error {
	_, err := c.nics.BeginDelete(ctx, resourceGroup, name, nil)
	if err != nil && !isAzureNotFound(err) {
		return fmt.Errorf("delete network interface %s/%s: %w", resourceGroup, name, err)
	}

	return nil
}

func (c *armCapacityClient) GetDisk(ctx context.Context, resourceGroup, name string) (*armcompute.Disk, error) {
	response, err := c.disks.Get(ctx, resourceGroup, name, nil)
	if isAzureNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("get disk %s/%s: %w", resourceGroup, name, err)
	}

	return &response.Disk, nil
}

func (c *armCapacityClient) DeleteDisk(ctx context.Context, resourceGroup, name string) error {
	_, err := c.disks.BeginDelete(ctx, resourceGroup, name, nil)
	if err != nil && !isAzureNotFound(err) {
		return fmt.Errorf("delete disk %s/%s: %w", resourceGroup, name, err)
	}

	return nil
}

// VMSKUs lists the virtual machine SKUs offered to the subscription in one
// region, keyed by lowercase SKU name.
func (c *armCapacityClient) VMSKUs(ctx context.Context, location string) (map[string]*armcompute.ResourceSKU, error) {
	pager := c.skus.NewListPager(&armcompute.ResourceSKUsClientListOptions{
		Filter: new(fmt.Sprintf("location eq '%s'", location)),
	})

	skus := map[string]*armcompute.ResourceSKU{}

	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list resource SKUs in %s: %w", location, err)
		}

		for _, sku := range page.Value {
			if sku == nil || sku.Name == nil || !strings.EqualFold(toValue(sku.ResourceType, ""), "virtualMachines") {
				continue
			}

			skus[strings.ToLower(*sku.Name)] = sku
		}
	}

	return skus, nil
}

// skuCache bounds Resource SKU listing to one call per subscription and
// region per TTL regardless of how many classes share them.
type skuCache struct {
	mu      sync.Mutex
	entries map[string]skuCacheEntry
}

type skuCacheEntry struct {
	skus       map[string]*armcompute.ResourceSKU
	observedAt time.Time
}

func (c *skuCache) get(
	ctx context.Context,
	client capacityClient,
	subscriptionID string,
	location string,
	now time.Time,
) (skuCacheEntry, error) {
	key := subscriptionID + "/" + location

	c.mu.Lock()
	entry, ok := c.entries[key]
	c.mu.Unlock()

	if ok && now.Sub(entry.observedAt) < skuCacheTTL {
		return entry, nil
	}

	skus, err := client.VMSKUs(ctx, location)
	if err != nil {
		return skuCacheEntry{}, err
	}

	entry = skuCacheEntry{
		skus:       skus,
		observedAt: now,
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil {
		c.entries = map[string]skuCacheEntry{}
	}

	c.entries[key] = entry

	return entry, nil
}

func (p *Provider) capacityClient(subscriptionID string, auth *machineops.OperationAuth) (capacityClient, error) {
	if p.NewCapacityClient != nil {
		return p.NewCapacityClient(subscriptionID, auth)
	}

	credential, err := p.credentials.credential(auth)
	if err != nil {
		return nil, err
	}

	return newARMCapacityClient(subscriptionID, credential)
}
