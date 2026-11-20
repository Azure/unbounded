// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
)

const (
	hostSubscription  = "sub"
	hostResourceGroup = "rg"
	hostVMName        = "vm1"
	hostOperationUID  = types.UID("0b5c7d2e-1f3a-4b6c-8d9e-0a1b2c3d4e5f")
	hostUserData      = "#cloud-config\nbootstrap-token: abcdef.0123456789abcdef\n"
)

// fakeVMClient is one Azure VM whose long-running operations settle as time
// passes. Time passes only when the test advances it, so a test controls
// exactly what each observation sees.
type fakeVMClient struct {
	vm *armcompute.VirtualMachine
	// power is the VM's power state, and statusTime when Azure completed
	// its latest operation.
	power      string
	statusTime time.Time
	failCode   string
	clock      time.Time

	// pending settles the operation in progress after settleTicks ticks.
	pending     func()
	ticksLeft   int
	settleTicks int

	getErrs    []error
	createErrs []error
	updateErrs []error
	deleteErrs []error
	actionErrs []error

	calls   []string
	updates []armcompute.VirtualMachine
	creates []armcompute.VirtualMachine
	gets    int
	// vanishAfterGets, when positive, makes the VM disappear after that many
	// more reads.
	vanishAfterGets int
	// loseResponse names a call ("update", "delete" or "create") that Azure
	// accepts once while the client gets an error instead of the answer.
	loseResponse string
	// changes counts the VM's state changes; its etag is derived from it.
	changes int
}

func (c *fakeVMClient) currentEtag() string {
	return fmt.Sprintf("etag-%d", c.changes)
}

func newFakeVMClient(vm *armcompute.VirtualMachine) *fakeVMClient {
	return &fakeVMClient{
		vm:          vm,
		power:       powerStateRunning,
		statusTime:  testNow.Add(-time.Hour),
		clock:       testNow,
		settleTicks: 2,
	}
}

// tick lets time pass: the operation in progress settles after settleTicks
// ticks.
func (c *fakeVMClient) tick() {
	c.clock = c.clock.Add(pollInterval)

	if c.pending == nil {
		return
	}

	c.ticksLeft--
	if c.ticksLeft > 0 {
		return
	}

	settle := c.pending
	c.pending = nil

	settle()
}

// settle finishes the operation in progress at once.
func (c *fakeVMClient) settle() {
	for c.pending != nil {
		c.tick()
	}
}

func (c *fakeVMClient) start(state, power string, settle func()) {
	c.vm.Properties.ProvisioningState = new(state)
	if power != "" {
		c.power = power
	}

	c.changes++
	c.ticksLeft = c.settleTicks
	c.pending = func() {
		c.statusTime = c.clock
		c.changes++

		settle()
	}
}

var errResponseLost = errors.New("connection reset before the response arrived")

// lost reports, once, that the response to the accepted call is lost.
func (c *fakeVMClient) lost(call string) error {
	if c.loseResponse != call {
		return nil
	}

	c.loseResponse = ""

	return errResponseLost
}

func popErr(errs *[]error) error {
	if len(*errs) == 0 {
		return nil
	}

	err := (*errs)[0]
	*errs = (*errs)[1:]

	return err
}

func (c *fakeVMClient) GetVM(_ context.Context, resourceGroup, name string, instanceView bool) (*armcompute.VirtualMachine, error) {
	c.gets++

	if err := popErr(&c.getErrs); err != nil {
		return nil, err
	}

	if !strings.EqualFold(resourceGroup, hostResourceGroup) || name != hostVMName || c.vm == nil {
		return nil, nil
	}

	if c.vanishAfterGets > 0 {
		c.vanishAfterGets--
		if c.vanishAfterGets == 0 {
			defer func() { c.vm = nil }()
		}
	}

	vm := copyVM(c.vm)

	// A test asks for a VM without an etag through this tag.
	if tagValue(vm.Tags, "test-no-etag") == "" {
		vm.Etag = new(c.currentEtag())
	}

	if instanceView {
		state := strings.ToLower(provisioningState(vm))
		if c.failCode != "" {
			state = "failed/" + c.failCode
		}

		vm.Properties.InstanceView = &armcompute.VirtualMachineInstanceView{
			Statuses: []*armcompute.InstanceViewStatus{
				{Code: new("ProvisioningState/" + state), Time: new(c.statusTime), Message: new("status message")},
				{Code: new("PowerState/" + c.power)},
			},
		}
	}

	return vm, nil
}

// UpdateVM is the conditional PUT of an existing VM: Azure refuses it when
// the VM is gone or no longer has the etag.
func (c *fakeVMClient) UpdateVM(_ context.Context, _, name string, vm armcompute.VirtualMachine, etag string) error {
	c.calls = append(c.calls, "update:"+name)

	if err := popErr(&c.updateErrs); err != nil {
		return err
	}

	if c.vm == nil || etag != c.currentEtag() {
		return azureResponseError(http.StatusPreconditionFailed, "PreconditionFailed")
	}

	c.updates = append(c.updates, *copyVM(&vm))

	updated := copyVM(&vm)
	updated.ID = c.vm.ID
	updated.Name = c.vm.Name
	updated.Properties.VMID = c.vm.Properties.VMID
	c.vm = updated
	c.start("Updating", "", func() { c.vm.Properties.ProvisioningState = new("Succeeded") })

	return c.lost("update")
}

// CreateVM is the conditional PUT of a new VM: Azure refuses it when a VM
// has the name.
func (c *fakeVMClient) CreateVM(_ context.Context, _, name string, vm armcompute.VirtualMachine) error {
	c.calls = append(c.calls, "create:"+name)

	if err := popErr(&c.createErrs); err != nil {
		return err
	}

	if c.vm != nil {
		return azureResponseError(http.StatusPreconditionFailed, "PreconditionFailed")
	}

	c.creates = append(c.creates, *copyVM(&vm))

	created := copyVM(&vm)
	created.ID = new(vmResourceID(hostSubscription, hostResourceGroup, name))
	created.Name = new(name)
	created.Properties.VMID = new(fmt.Sprintf("vm-guid-%d", len(c.creates)+1))
	created.Properties.OSProfile.CustomData = nil
	c.vm = created
	c.start("Creating", powerStateStarting, func() {
		c.vm.Properties.ProvisioningState = new("Succeeded")
		c.power = powerStateRunning
	})

	return c.lost("create")
}

func (c *fakeVMClient) DeleteVM(_ context.Context, _, name string) error {
	c.calls = append(c.calls, "delete:"+name)

	if err := popErr(&c.deleteErrs); err != nil {
		return err
	}

	c.start("Deleting", "", func() { c.vm = nil })

	return c.lost("delete")
}

func (c *fakeVMClient) action(name, call, transitional, final string) error {
	c.calls = append(c.calls, call+":"+name)

	if err := popErr(&c.actionErrs); err != nil {
		return err
	}

	c.start("Updating", transitional, func() {
		c.vm.Properties.ProvisioningState = new("Succeeded")
		c.power = final
	})

	return nil
}

func (c *fakeVMClient) StartVM(_ context.Context, _, name string) error {
	return c.action(name, "start", powerStateStarting, powerStateRunning)
}

func (c *fakeVMClient) PowerOffVM(_ context.Context, _, name string) error {
	return c.action(name, "powerOff", powerStateStopping, powerStateStopped)
}

func (c *fakeVMClient) RestartVM(_ context.Context, _, name string) error {
	return c.action(name, "restart", powerStateStarting, powerStateRunning)
}

func (c *fakeVMClient) count(call string) int {
	count := 0

	for _, recorded := range c.calls {
		if strings.HasPrefix(recorded, call+":") {
			count++
		}
	}

	return count
}

// copyVM returns an independent copy, as every Azure read and write does.
func copyVM(vm *armcompute.VirtualMachine) *armcompute.VirtualMachine {
	raw, err := json.Marshal(vm)
	if err != nil {
		panic(err)
	}

	var copied armcompute.VirtualMachine
	if err := json.Unmarshal(raw, &copied); err != nil {
		panic(err)
	}

	return &copied
}

// newHostProvider returns a Provider that holds nothing but the fake client
// and its clock. Tests make a new one for every call, as a controller that
// restarted between the calls would.
func newHostProvider(client *fakeVMClient) *Provider {
	return &Provider{
		NewVMClient: func(subscriptionID string, _ *machineops.OperationAuth) (azureVMClient, error) {
			if subscriptionID != hostSubscription {
				return nil, fmt.Errorf("unexpected subscription %q", subscriptionID)
			}

			return client, nil
		},
		Now: func() time.Time {
			return client.clock
		},
	}
}

func hostVMID() string {
	return vmResourceID(hostSubscription, hostResourceGroup, hostVMName)
}

func hostRequest(kind unboundedv1alpha3.OperationKind) machineops.OperationRequest {
	request := machineops.OperationRequest{
		MachineName:  "machine-1",
		OperationUID: hostOperationUID,
		ProviderID:   "azure://" + hostVMID(),
		Operation:    kind,
	}

	if kind == unboundedv1alpha3.OperationHostReplace {
		request.ReplaceUserData = hostUserData
	}

	return request
}

func hostDiskID(name string) string {
	return "/subscriptions/" + hostSubscription + "/resourceGroups/" + hostResourceGroup + "/providers/Microsoft.Compute/disks/" + name
}

func hostNICID(name string) string {
	return "/subscriptions/" + hostSubscription + "/resourceGroups/" + hostResourceGroup + "/providers/Microsoft.Network/networkInterfaces/" + name
}

const hostIdentityID = "/subscriptions/" + hostSubscription + "/resourceGroups/rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/id1"

// replaceableVM is a Linux VM with SSH-only access, a primary NIC deleted
// with the VM, a data disk created empty with the VM and deleted with it, and
// an attached data disk that outlives it.
func replaceableVM() *armcompute.VirtualMachine {
	return &armcompute.VirtualMachine{
		ID:       new(hostVMID()),
		Name:     new(hostVMName),
		Location: new("eastus2"),
		Tags: map[string]*string{
			"env": new("prod"),
		},
		Identity: &armcompute.VirtualMachineIdentity{
			Type: new(armcompute.ResourceIdentityTypeUserAssigned),
			UserAssignedIdentities: map[string]*armcompute.UserAssignedIdentitiesValue{
				hostIdentityID: {ClientID: new("client-id"), PrincipalID: new("principal-id")},
			},
		},
		Properties: &armcompute.VirtualMachineProperties{
			VMID:              new("vm-guid-1"),
			ProvisioningState: new("Succeeded"),
			HardwareProfile: &armcompute.HardwareProfile{
				VMSize: new(armcompute.VirtualMachineSizeTypes("Standard_D4s_v3")),
			},
			OSProfile: &armcompute.OSProfile{
				ComputerName:  new(hostVMName),
				AdminUsername: new("azureuser"),
				LinuxConfiguration: &armcompute.LinuxConfiguration{
					DisablePasswordAuthentication: new(true),
					SSH: &armcompute.SSHConfiguration{PublicKeys: []*armcompute.SSHPublicKey{{
						Path:    new("/home/azureuser/.ssh/authorized_keys"),
						KeyData: new("ssh-ed25519 AAAA test"),
					}}},
				},
			},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: &armcompute.ImageReference{
					Publisher: new("Canonical"),
					Offer:     new("ubuntu-24_04-lts"),
					SKU:       new("server"),
					Version:   new("latest"),
				},
				OSDisk: &armcompute.OSDisk{
					Name:         new("vm1-osdisk"),
					CreateOption: new(armcompute.DiskCreateOptionTypesFromImage),
					OSType:       new(armcompute.OperatingSystemTypesLinux),
					ManagedDisk: &armcompute.ManagedDiskParameters{
						ID:                 new(hostDiskID("vm1-osdisk")),
						StorageAccountType: new(armcompute.StorageAccountTypesPremiumLRS),
					},
				},
				DataDisks: []*armcompute.DataDisk{
					{
						Lun:          new(int32(0)),
						Name:         new("vm1-data0"),
						CreateOption: new(armcompute.DiskCreateOptionTypesEmpty),
						DiskSizeGB:   new(int32(128)),
						Caching:      new(armcompute.CachingTypesReadOnly),
						DeleteOption: new(armcompute.DiskDeleteOptionTypesDelete),
						ManagedDisk: &armcompute.ManagedDiskParameters{
							ID:                 new(hostDiskID("vm1-data0")),
							StorageAccountType: new(armcompute.StorageAccountTypesPremiumLRS),
						},
					},
					{
						Lun:          new(int32(3)),
						Name:         new("shared-data"),
						CreateOption: new(armcompute.DiskCreateOptionTypesAttach),
						ManagedDisk: &armcompute.ManagedDiskParameters{
							ID: new(hostDiskID("shared-data")),
						},
					},
				},
			},
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{
					ID: new(hostNICID("vm1-nic")),
					Properties: &armcompute.NetworkInterfaceReferenceProperties{
						Primary:      new(true),
						DeleteOption: new(armcompute.DeleteOptionsDelete),
					},
				}},
			},
		},
	}
}

func requirePermanentReason(t *testing.T, err error, reason string) {
	t.Helper()

	permanentErr, ok := errors.AsType[*machineops.PermanentError](err)
	require.True(t, ok, "error %v must be permanent", err)
	require.Equal(t, reason, permanentErr.Reason)
}
