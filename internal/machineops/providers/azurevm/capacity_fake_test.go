// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineops"
	publicmachineops "github.com/Azure/unbounded/pkg/machineops"
)

const testGalleryImage = "/subscriptions/" + testSubscription + "/resourceGroups/images/providers/Microsoft.Compute/galleries/workers/images/ubuntu"

const (
	testSubscription  = "00000000-0000-0000-0000-000000000001"
	testResourceGroup = "workers"
	testAllocationUID = types.UID("0123abcd-4567-89ab-cdef-0123456789ab")
	testVMName        = "ub-0123abcd456789abcdef0123456789ab"
	testSubnetID      = "/subscriptions/" + testSubscription + "/resourceGroups/net/providers/Microsoft.Network/virtualNetworks/vnet/subnets/workers"
	testIdentityID    = "/subscriptions/" + testSubscription + "/resourceGroups/main/providers/Microsoft.ManagedIdentity/userAssignedIdentities/worker"
	testImage         = "Canonical:ubuntu-24_04-lts:server:24.04.202610160"
)

var testNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

type fakeCapacityClient struct {
	vms   map[string]*armcompute.VirtualMachine
	nics  map[string]*armnetwork.Interface
	disks map[string]*armcompute.Disk
	skus  map[string]*armcompute.ResourceSKU

	createErr error
	// getErr fails VM reads; childGetErr fails NIC and disk reads.
	getErr      error
	childGetErr error
	deleteErr   error
	skuErr      error
	// deleteAcceptedErr simulates Azure accepting a delete whose response
	// was lost: the resource starts deleting but the caller sees an error.
	deleteAcceptedErr error

	creates     []armcompute.VirtualMachine
	vmDeletes   int
	nicDeletes  int
	diskDeletes int
	skuLists    int
	// reads counts every VM, NIC and disk read or list sent to Azure.
	reads    int
	nicGets  []string
	nicLists int
}

// requireTestResourceGroup fails a child read or delete addressed outside
// the allocation's resource group. ARM resource group names are
// case-insensitive.
func requireTestResourceGroup(resourceGroup string) error {
	if !strings.EqualFold(resourceGroup, testResourceGroup) {
		return fmt.Errorf("unexpected resource group %q", resourceGroup)
	}

	return nil
}

func newFakeCapacityClient() *fakeCapacityClient {
	return &fakeCapacityClient{
		vms:   map[string]*armcompute.VirtualMachine{},
		nics:  map[string]*armnetwork.Interface{},
		disks: map[string]*armcompute.Disk{},
		skus: map[string]*armcompute.ResourceSKU{
			"standard_d4s_v3": testSKU(),
		},
	}
}

func (c *fakeCapacityClient) GetVM(_ context.Context, _, name string, _ bool) (*armcompute.VirtualMachine, error) {
	c.reads++

	if c.getErr != nil {
		return nil, c.getErr
	}

	return c.vms[name], nil
}

func (c *fakeCapacityClient) CreateVM(_ context.Context, _, name string, vm armcompute.VirtualMachine) error {
	if c.createErr != nil {
		return c.createErr
	}

	c.creates = append(c.creates, vm)
	c.vms[name] = createdVM(name, vm)

	// Compound create makes the NIC and OS disk attached to the VM. Azure
	// appends a random suffix to the NIC configuration name; the disk keeps
	// its configured name.
	vmID := vmResourceID(testSubscription, testResourceGroup, name)
	c.nics[suffixedNICName(name)] = testNIC("nic-guid-"+name, vmID)
	c.disks[name+osDiskNameSuffix] = testDisk("disk-guid-"+name, vmID)

	return nil
}

// createdVM is the VM Azure reports after a compound create: it references
// its NIC and OS disk by ARM ID, with the resource group in upper case as
// Azure often returns it.
func createdVM(name string, request armcompute.VirtualMachine) *armcompute.VirtualMachine {
	properties := *request.Properties
	network := *properties.NetworkProfile
	storage := *properties.StorageProfile
	osDisk := *storage.OSDisk
	managedDisk := *osDisk.ManagedDisk

	managedDisk.ID = new(referencedDiskID(name))
	osDisk.ManagedDisk = &managedDisk
	storage.OSDisk = &osDisk

	network.NetworkInterfaces = []*armcompute.NetworkInterfaceReference{
		{
			ID: new(referencedNICID(name)),
			Properties: &armcompute.NetworkInterfaceReferenceProperties{
				Primary:      new(true),
				DeleteOption: new(armcompute.DeleteOptionsDelete),
			},
		},
	}

	properties.NetworkProfile = &network
	properties.StorageProfile = &storage
	properties.ProvisioningState = new("Creating")
	properties.VMID = new("vm-guid-" + name)

	vm := request
	vm.Name = new(name)
	vm.Properties = &properties

	return &vm
}

// testNICSuffix is the suffix Azure appended to the live allocation NIC.
const testNICSuffix = "-03360add"

func suffixedNICName(vmName string) string {
	return vmName + nicNameSuffix + testNICSuffix
}

func referencedNICID(vmName string) string {
	return "/subscriptions/" + testSubscription + "/resourceGroups/" + strings.ToUpper(testResourceGroup) + "/providers/Microsoft.Network/networkInterfaces/" + suffixedNICName(vmName)
}

func referencedDiskID(vmName string) string {
	return "/subscriptions/" + testSubscription + "/resourceGroups/" + strings.ToUpper(testResourceGroup) + "/providers/Microsoft.Compute/disks/" + vmName + osDiskNameSuffix
}

func testNIC(guid, attachedTo string) *armnetwork.Interface {
	nic := &armnetwork.Interface{
		Properties: &armnetwork.InterfacePropertiesFormat{
			ResourceGUID:      new(guid),
			ProvisioningState: new(armnetwork.ProvisioningStateSucceeded),
		},
	}

	if attachedTo != "" {
		nic.Properties.VirtualMachine = &armnetwork.SubResource{
			ID: new(attachedTo),
		}
	}

	return nic
}

func testDisk(uniqueID, managedBy string) *armcompute.Disk {
	disk := &armcompute.Disk{
		Properties: &armcompute.DiskProperties{
			UniqueID:          new(uniqueID),
			ProvisioningState: new("Succeeded"),
		},
	}

	if managedBy != "" {
		disk.ManagedBy = new(managedBy)
	}

	return disk
}

// mutations counts every write sent to Azure.
func (c *fakeCapacityClient) mutations() int {
	return len(c.creates) + c.vmDeletes + c.nicDeletes + c.diskDeletes
}

func (c *fakeCapacityClient) DeleteVM(_ context.Context, _, name string) error {
	if c.deleteErr != nil {
		return c.deleteErr
	}

	c.vmDeletes++
	c.vms[name].Properties.ProvisioningState = new("Deleting")

	return c.deleteAcceptedErr
}

func (c *fakeCapacityClient) GetNIC(_ context.Context, resourceGroup, name string) (*armnetwork.Interface, error) {
	if err := requireTestResourceGroup(resourceGroup); err != nil {
		return nil, err
	}

	c.reads++
	c.nicGets = append(c.nicGets, name)

	if c.childGetErr != nil {
		return nil, c.childGetErr
	}

	return c.nics[name], nil
}

// ListNICNames returns every NIC in the resource group, including ones that
// belong to other VMs, as the Azure list does.
func (c *fakeCapacityClient) ListNICNames(_ context.Context, resourceGroup string) ([]string, error) {
	if err := requireTestResourceGroup(resourceGroup); err != nil {
		return nil, err
	}

	c.reads++
	c.nicLists++

	if c.childGetErr != nil {
		return nil, c.childGetErr
	}

	names := make([]string, 0, len(c.nics))
	for name := range c.nics {
		names = append(names, name)
	}

	slices.Sort(names)

	return names, nil
}

func (c *fakeCapacityClient) DeleteNIC(_ context.Context, resourceGroup, name string) error {
	if err := requireTestResourceGroup(resourceGroup); err != nil {
		return err
	}

	c.nicDeletes++
	c.nics[name].Properties.ProvisioningState = new(armnetwork.ProvisioningStateDeleting)

	return nil
}

func (c *fakeCapacityClient) GetDisk(_ context.Context, resourceGroup, name string) (*armcompute.Disk, error) {
	if err := requireTestResourceGroup(resourceGroup); err != nil {
		return nil, err
	}

	c.reads++

	if c.childGetErr != nil {
		return nil, c.childGetErr
	}

	return c.disks[name], nil
}

func (c *fakeCapacityClient) DeleteDisk(_ context.Context, resourceGroup, name string) error {
	if err := requireTestResourceGroup(resourceGroup); err != nil {
		return err
	}

	c.diskDeletes++
	c.disks[name].Properties.ProvisioningState = new("Deleting")

	return nil
}

func (c *fakeCapacityClient) VMSKUs(_ context.Context, _ string) (map[string]*armcompute.ResourceSKU, error) {
	c.skuLists++

	if c.skuErr != nil {
		return nil, c.skuErr
	}

	return c.skus, nil
}

func testSKU() *armcompute.ResourceSKU {
	return &armcompute.ResourceSKU{
		Name:         new("Standard_D4s_v3"),
		ResourceType: new("virtualMachines"),
		Capabilities: []*armcompute.ResourceSKUCapabilities{
			{
				Name:  new("vCPUs"),
				Value: new("4"),
			},
			{
				Name:  new("MemoryGB"),
				Value: new("16"),
			},
			{
				Name:  new("CpuArchitectureType"),
				Value: new("x64"),
			},
		},
		LocationInfo: []*armcompute.ResourceSKULocationInfo{
			{
				Location: new("eastus2"),
				Zones: []*string{
					new("1"),
					new("2"),
					new("3"),
				},
			},
		},
	}
}

func testConfig() map[string]string {
	return map[string]string{
		ConfigSubscriptionID:           testSubscription,
		ConfigResourceGroup:            testResourceGroup,
		ConfigLocation:                 "eastus2",
		ConfigZone:                     "1",
		ConfigVMSize:                   "Standard_D4s_v3",
		ConfigSubnetID:                 testSubnetID,
		ConfigImage:                    testImage,
		ConfigAdminUsername:            "azureuser",
		ConfigSSHPublicKey:             "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA test",
		ConfigWorkerIdentityResourceID: testIdentityID,
		ConfigKubeReserved:             "cpu=100m,memory=1Gi",
		ConfigPriceCurrency:            "USD",
		ConfigPricePerHour:             "0.192",
		ConfigPriceSource:              "azure-retail-prices-api",
		ConfigPriceObservedAt:          "2026-10-21T00:00:00Z",
	}
}

func newTestProvider(client *fakeCapacityClient) *Provider {
	return &Provider{
		NewCapacityClient: func(string, *machineops.OperationAuth) (capacityClient, error) {
			return client, nil
		},
		Now: func() time.Time {
			return testNow
		},
	}
}

func resolveTestOffering(t *testing.T, provider *Provider) unboundedv1alpha3.MachineClassOffering {
	t.Helper()

	resolution, err := provider.ResolveClass(t.Context(), publicmachineops.ClassRequest{
		ClassName:      "azure",
		ProviderConfig: testProviderConfig(),
	})
	require.NoError(t, err)
	require.Len(t, resolution.Offerings, 1)

	return resolution.Offerings[0]
}

func testOperationRequest(t *testing.T, kind unboundedv1alpha3.OperationKind, offering unboundedv1alpha3.MachineClassOffering) machineops.OperationRequest {
	t.Helper()

	request := machineops.OperationRequest{
		MachineName: testVMName,
		ProviderID:  "azure:///subscriptions/" + testSubscription + "/resourceGroups/" + testResourceGroup + "/providers/Microsoft.Compute/virtualMachines/" + testVMName,
		HostImage:   testImage,
		Operation:   kind,
		Allocation: &publicmachineops.AllocationRequest{
			Name:      "claim-a",
			UID:       testAllocationUID,
			ClassName: "azure",
			HostName:  testVMName,
			Offering:  offering,
		},
	}

	if kind == unboundedv1alpha3.OperationHostAllocate {
		request.Allocation.ProviderConfig = testProviderConfig()
		request.Allocation.UserData = "#cloud-config\n"
	}

	return request
}

func azureResponseError(status int, code string) error {
	return &azcore.ResponseError{
		StatusCode: status,
		ErrorCode:  code,
	}
}

func requireQuantity(t *testing.T, expected string, actual resource.Quantity) {
	t.Helper()

	want := resource.MustParse(expected)
	require.Zero(t, want.Cmp(actual), "expected %s, got %s", expected, actual.String())
}

func testProviderConfig() publicmachineops.ProviderConfig {
	return publicmachineops.ProviderConfig{Data: testConfig()}
}

func requirePermanent(t *testing.T, err error, reason unboundedv1alpha3.MachineAllocationFailureReason) {
	t.Helper()

	var permanentErr *publicmachineops.PermanentError
	require.ErrorAs(t, err, &permanentErr)
	require.Equal(t, string(reason), permanentErr.Reason)
}

func requireTransient(t *testing.T, err error) {
	t.Helper()

	require.Error(t, err)

	var permanentErr *publicmachineops.PermanentError
	require.False(t, errors.As(err, &permanentErr), "error %v must be retryable", err)
}

func requireConflict(t *testing.T, err error) {
	t.Helper()

	requirePermanent(t, err, unboundedv1alpha3.FailureProviderConflict)
}

func testNICName() string {
	return suffixedNICName(testVMName)
}

func testNICID() string {
	return referencedNICID(testVMName)
}

// createdChildNICID is the ID the adapter builds for a NIC it found by name
// rather than through a VM reference.
func createdChildNICID() string {
	return allocationVM{subscriptionID: testSubscription, resourceGroup: testResourceGroup, name: testVMName}.nicID(testNICName())
}

func testDiskName() string {
	return testVMName + osDiskNameSuffix
}

func testDiskID() string {
	return referencedDiskID(testVMName)
}

func testVMID() string {
	return vmResourceID(testSubscription, testResourceGroup, testVMName)
}

// testOwnedResources is the full ownership record of a provisioned
// allocation: the VM, its suffixed NIC and its OS disk.
func testOwnedResources() []unboundedv1alpha3.ProviderOwnedResource {
	return []unboundedv1alpha3.ProviderOwnedResource{
		{
			ID:       testVMID(),
			NativeID: "vm-guid-" + testVMName,
		},
		{
			ID:       testNICID(),
			NativeID: "nic-guid-" + testVMName,
		},
		{
			ID:       testDiskID(),
			NativeID: "disk-guid-" + testVMName,
		},
	}
}

// pollRequestFor returns the request the engine sends with Poll: no provider
// config and no user data.
func pollRequestFor(t *testing.T, request machineops.OperationRequest) machineops.OperationRequest {
	t.Helper()

	poll := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, request.Allocation.Offering)
	poll.Allocation.ProviderConfig = publicmachineops.ProviderConfig{}
	poll.Allocation.UserData = ""

	return poll
}

// failedTestVM is an allocation VM whose provisioning failed with code.
func failedTestVM(code string) *armcompute.VirtualMachine {
	return &armcompute.VirtualMachine{
		Name: new(testVMName),
		Tags: map[string]*string{
			TagAllocationUID: new(string(testAllocationUID)),
		},
		Properties: &armcompute.VirtualMachineProperties{
			VMID:              new("vm-guid-" + testVMName),
			ProvisioningState: new("Failed"),
			InstanceView: &armcompute.VirtualMachineInstanceView{
				Statuses: []*armcompute.InstanceViewStatus{
					{
						Code:    new("ProvisioningState/failed/" + code),
						Message: new("provisioning failed"),
					},
				},
			},
		},
	}
}
