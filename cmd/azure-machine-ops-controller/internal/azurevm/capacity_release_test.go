// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"github.com/stretchr/testify/require"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
)

func TestReleaseDeletesVMAndConfirmsOwnedResources(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := allocatedVM(t, client, provider)

	begin, err := provider.BeginRelease(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, testVMID(), begin.Operation.OperationID)
	require.Equal(t, 1, client.vmDeletes)

	// A replayed Begin and a poll both observe the delete already issued.
	_, err = provider.BeginRelease(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, machineops.ProviderOperationStateInProgress, pollRelease(t, provider, request).State)
	require.Equal(t, 1, client.vmDeletes)

	// Azure deleted the VM and, with deleteOption Delete, its children.
	delete(client.vms, testVMName)
	delete(client.nics, testNICName())
	delete(client.disks, testDiskName())

	result := pollRelease(t, provider, request)
	require.Equal(t, machineops.ProviderOperationStateSucceeded, result.State)
	require.Nil(t, result.Result.Allocation)
	require.Zero(t, client.nicDeletes)
	require.Zero(t, client.diskDeletes)
}

// Leftovers are deleted only when their provider-native IDs prove they are
// the recorded children, and each delete is sent once.
func TestReleaseDeletesProvenLeftovers(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := allocatedVM(t, client, provider)

	_, err := provider.BeginRelease(t.Context(), request)
	require.NoError(t, err)

	delete(client.vms, testVMName)
	client.nics[testNICName()].Properties.VirtualMachine = nil
	client.disks[testDiskName()].ManagedBy = nil

	result := pollRelease(t, provider, request)
	require.Equal(t, machineops.ProviderOperationStateInProgress, result.State)
	require.Contains(t, result.Message, "network interface "+testNICName())
	require.Equal(t, 1, client.nicDeletes)
	require.Equal(t, 1, client.diskDeletes)
	require.Equal(t, armnetwork.ProvisioningStateDeleting, *client.nics[testNICName()].Properties.ProvisioningState)

	// A replayed Begin and a later poll observe the deletes in progress.
	_, err = provider.BeginRelease(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, machineops.ProviderOperationStateInProgress, pollRelease(t, provider, request).State)
	require.Equal(t, 1, client.nicDeletes)
	require.Equal(t, 1, client.diskDeletes)

	delete(client.nics, testNICName())
	delete(client.disks, testDiskName())

	require.Equal(t, machineops.ProviderOperationStateSucceeded, pollRelease(t, provider, request).State)
}

// A delete Azure accepted but whose response was lost is not reissued: the
// retried Begin observes the VM already deleting.
func TestReleaseLostDeleteAcceptanceSettles(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := allocatedVM(t, client, provider)
	client.deleteAcceptedErr = errors.New("connection reset")

	_, err := provider.BeginRelease(t.Context(), request)
	requireTransient(t, err)

	client.deleteAcceptedErr = nil

	_, err = provider.BeginRelease(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, 1, client.vmDeletes)

	require.Equal(t, machineops.ProviderOperationStateInProgress, pollRelease(t, provider, request).State)
}

// The engine never begins a second HostRelease, so a poll that finds the VM
// present and not deleting sends the delete again instead of failing.
func TestReleasePollReissuesDroppedVMDelete(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := allocatedVM(t, client, provider)

	result := pollRelease(t, provider, request)
	require.Equal(t, machineops.ProviderOperationStateInProgress, result.State)
	require.Equal(t, 1, client.vmDeletes)

	require.Equal(t, machineops.ProviderOperationStateInProgress, pollRelease(t, provider, request).State)
	require.Equal(t, 1, client.vmDeletes)
}

// Azure refusals during release are retried with the same handle, so a
// repaired credential or lock resumes the release.
func TestReleaseAzureErrorsAreRetried(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := allocatedVM(t, client, provider)
	client.deleteErr = azureResponseError(http.StatusForbidden, "AuthorizationFailed")

	_, err := provider.BeginRelease(t.Context(), request)
	requireTransient(t, err)

	_, err = provider.PollRelease(t.Context(), request, machineops.ProviderOperation{OperationID: testVMID()})
	requireTransient(t, err)

	client.deleteErr = nil
	client.getErr = errors.New("timeout")

	_, err = provider.BeginRelease(t.Context(), request)
	requireTransient(t, err)

	_, err = provider.PollRelease(t.Context(), request, machineops.ProviderOperation{OperationID: testVMID()})
	requireTransient(t, err)
	require.Zero(t, client.vmDeletes)
}

// An allocation that recorded nothing created nothing: its release finishes
// without reading or changing Azure, even when a foreign VM and disk carry
// the allocation's names.
func TestReleaseWithNothingRecordedTouchesNothing(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := testOperationRequest(t, unboundedv1alpha3.OperationHostRelease, resolveTestOffering(t, provider))

	client.vms[testVMName] = &armcompute.VirtualMachine{
		Name: new(testVMName),
		Tags: map[string]*string{
			TagAllocationUID: new("ffffffff-ffff-ffff-ffff-ffffffffffff"),
		},
	}
	client.disks[testDiskName()] = testDisk("foreign-disk", "")
	client.getErr = errors.New("must not be read")

	begin, err := provider.BeginRelease(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, testVMID(), begin.Operation.OperationID)

	require.Equal(t, machineops.ProviderOperationStateSucceeded, pollRelease(t, provider, request).State)
	require.Zero(t, client.reads)
	require.Zero(t, client.mutations())
}

func TestReleaseNeverDeletesUnprovenResources(t *testing.T) {
	present := map[string]func(*fakeCapacityClient, *machineops.OperationRequest){
		"VM retagged for another allocation": func(client *fakeCapacityClient, _ *machineops.OperationRequest) {
			client.vms[testVMName].Tags[TagAllocationUID] = new("ffffffff-ffff-ffff-ffff-ffffffffffff")
		},
		"VM recreated under the same name": func(client *fakeCapacityClient, _ *machineops.OperationRequest) {
			client.vms[testVMName].Properties.VMID = new("another-vm-guid")
		},
		"VM never recorded": func(_ *fakeCapacityClient, request *machineops.OperationRequest) {
			request.Allocation.OwnedResources = request.Allocation.OwnedResources[1:]
		},
		"VM references an unrecorded NIC": func(_ *fakeCapacityClient, request *machineops.OperationRequest) {
			owned := request.Allocation.OwnedResources
			request.Allocation.OwnedResources = []unboundedv1alpha3.ProviderOwnedResource{owned[0], owned[2]}
		},
		"VM references a recreated disk": func(client *fakeCapacityClient, _ *machineops.OperationRequest) {
			client.disks[testDiskName()] = testDisk("recreated-disk", testVMID())
		},
	}

	leftovers := map[string]func(*fakeCapacityClient, *machineops.OperationRequest){
		// The VM was recorded, so the disk created under its deterministic
		// name fails closed rather than being reported as released.
		"disk never recorded": func(client *fakeCapacityClient, request *machineops.OperationRequest) {
			request.Allocation.OwnedResources = request.Allocation.OwnedResources[:2]
		},
		"recorded child outside resource group": func(_ *fakeCapacityClient, request *machineops.OperationRequest) {
			request.Allocation.OwnedResources[1].ID = "/subscriptions/" + testSubscription + "/resourceGroups/other/providers/Microsoft.Network/networkInterfaces/" + testNICName()
		},
		"recorded resource of another type": func(_ *fakeCapacityClient, request *machineops.OperationRequest) {
			request.Allocation.OwnedResources = append(request.Allocation.OwnedResources, unboundedv1alpha3.ProviderOwnedResource{
				ID:       "/subscriptions/" + testSubscription + "/resourceGroups/" + testResourceGroup + "/providers/Microsoft.Network/publicIPAddresses/ip",
				NativeID: "ip-guid",
			})
		},
		"NIC recreated after recording": func(client *fakeCapacityClient, _ *machineops.OperationRequest) {
			client.nics[testNICName()] = testNIC("recreated-nic", "")
		},
		"disk recreated after recording": func(client *fakeCapacityClient, _ *machineops.OperationRequest) {
			client.disks[testDiskName()] = testDisk("recreated-disk", "")
		},
		"NIC attached elsewhere": func(client *fakeCapacityClient, _ *machineops.OperationRequest) {
			client.nics[testNICName()].Properties.VirtualMachine = &armnetwork.SubResource{
				ID: new(vmResourceID(testSubscription, testResourceGroup, "other")),
			}
		},
	}

	run := func(t *testing.T, vmGone bool, mutate func(*fakeCapacityClient, *machineops.OperationRequest)) {
		t.Helper()

		client := newFakeCapacityClient()
		provider := newTestProvider(client)
		request := allocatedVM(t, client, provider)

		if vmGone {
			delete(client.vms, testVMName)
			client.nics[testNICName()].Properties.VirtualMachine = nil
			client.disks[testDiskName()].ManagedBy = nil
		}

		mutate(client, &request)

		before := client.mutations()

		_, err := provider.BeginRelease(t.Context(), request)
		requireConflict(t, err)

		result := pollRelease(t, provider, request)
		require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
		require.Equal(t, string(unboundedv1alpha3.FailureProviderConflict), result.Reason)
		require.Equal(t, before, client.mutations())
	}

	for name, mutate := range present {
		t.Run(name, func(t *testing.T) {
			run(t, false, mutate)
		})
	}

	for name, mutate := range leftovers {
		t.Run(name, func(t *testing.T) {
			run(t, true, mutate)
		})
	}
}

// Release never reads or deletes by the NIC configuration name; a NIC there
// was not created by the allocation VM.
func TestReleaseIgnoresNICUnderConfiguredName(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := allocatedVM(t, client, provider)

	_, err := provider.BeginRelease(t.Context(), request)
	require.NoError(t, err)

	delete(client.vms, testVMName)
	delete(client.nics, testNICName())
	delete(client.disks, testDiskName())
	client.nics[testVMName+nicNameSuffix] = testNIC("unrelated-nic", "")
	client.nicGets = nil

	_, err = provider.BeginRelease(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, machineops.ProviderOperationStateSucceeded, pollRelease(t, provider, request).State)
	require.Zero(t, client.nicDeletes)
	require.NotContains(t, client.nicGets, testVMName+nicNameSuffix)
}

// A NIC Azure created for the VM but the allocation never recorded is found
// by its derived name after the VM is gone. It is never deleted, and release
// does not claim that everything is gone.
func TestReleaseRefusesUnrecordedSuffixedNIC(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := allocatedVM(t, client, provider)

	// Only the VM and the disk were recorded.
	owned := request.Allocation.OwnedResources
	request.Allocation.OwnedResources = []unboundedv1alpha3.ProviderOwnedResource{owned[0], owned[2]}

	delete(client.vms, testVMName)
	delete(client.disks, testDiskName())
	client.nics[testNICName()].Properties.VirtualMachine = nil

	before := client.mutations()

	_, err := provider.BeginRelease(t.Context(), request)
	requireConflict(t, err)
	require.ErrorContains(t, err, testNICName())

	result := pollRelease(t, provider, request)
	require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
	require.Equal(t, string(unboundedv1alpha3.FailureProviderConflict), result.Reason)
	require.Equal(t, before, client.mutations())

	// Once the NIC is gone the release completes, and says what it checked.
	delete(client.nics, testNICName())

	result = pollRelease(t, provider, request)
	require.Equal(t, machineops.ProviderOperationStateSucceeded, result.State)
	require.Contains(t, result.Message, "no NIC or OS disk of the allocation remains")
	require.Positive(t, client.nicLists)
}

// Release looks only at NICs named for this allocation's VM.
func TestReleaseIgnoresOtherAllocationsNICs(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := allocatedVM(t, client, provider)

	other := "ub-ffffffffffffffffffffffffffffffff" + nicNameSuffix + testNICSuffix
	client.nics[other] = testNIC("other-nic", "")

	delete(client.vms, testVMName)
	delete(client.nics, testNICName())
	delete(client.disks, testDiskName())

	require.Equal(t, machineops.ProviderOperationStateSucceeded, pollRelease(t, provider, request).State)
	require.Zero(t, client.nicDeletes)
	require.NotContains(t, client.nicGets, other)
}

func TestReleaseRetriesNICListErrors(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := allocatedVM(t, client, provider)

	delete(client.vms, testVMName)
	client.childGetErr = errors.New("timeout")

	_, err := provider.PollRelease(t.Context(), request, machineops.ProviderOperation{OperationID: testVMID()})
	requireTransient(t, err)
	require.Zero(t, client.nicDeletes)
	require.Zero(t, client.diskDeletes)
}

func TestReleaseRejectsMismatchedTarget(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := allocatedVM(t, client, provider)
	request.ProviderID = strings.Replace(request.ProviderID, testVMName, "ub-ffffffffffffffffffffffffffffffff", 1)

	_, err := provider.BeginRelease(t.Context(), request)
	requirePermanent(t, err, unboundedv1alpha3.FailureInvalidRequest)

	_, err = provider.PollRelease(t.Context(), request, machineops.ProviderOperation{})
	requirePermanent(t, err, unboundedv1alpha3.FailureInvalidRequest)
	require.Zero(t, client.vmDeletes)
}
