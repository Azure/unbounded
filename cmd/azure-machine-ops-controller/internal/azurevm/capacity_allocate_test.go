// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/stretchr/testify/require"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
)

func TestPlanAllocationUsesDeterministicIdentity(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	offering := resolveTestOffering(t, provider)
	reads := client.reads

	plan, err := provider.PlanAllocation(t.Context(), machineops.AllocationPlanRequest{
		AllocationUID:  testAllocationUID,
		HostName:       testVMName,
		ProviderConfig: testProviderConfig(),
		Offering:       offering,
	})
	require.NoError(t, err)
	require.Equal(t, "azure://"+testVMID(), plan.ProviderID)
	require.Equal(t, testImage, plan.HostImage)
	require.Equal(t, reads, client.reads, "planning must not call Azure")
	require.Zero(t, client.mutations())

	_, err = provider.PlanAllocation(t.Context(), machineops.AllocationPlanRequest{
		AllocationUID:  testAllocationUID,
		HostName:       "ub-other",
		ProviderConfig: testProviderConfig(),
		Offering:       offering,
	})
	requirePermanent(t, err, unboundedv1alpha3.FailureInvalidRequest)

	_, err = provider.PlanAllocation(t.Context(), machineops.AllocationPlanRequest{
		AllocationUID: testAllocationUID,
		HostName:      testVMName,
		Offering:      offering,
	})
	requirePermanent(t, err, unboundedv1alpha3.FailureInvalidRequest)

	offering.InstanceType = "Standard_D8s_v3"

	_, err = provider.PlanAllocation(t.Context(), machineops.AllocationPlanRequest{
		AllocationUID:  testAllocationUID,
		HostName:       testVMName,
		ProviderConfig: testProviderConfig(),
		Offering:       offering,
	})
	requirePermanent(t, err, unboundedv1alpha3.FailureInvalidRequest)
}

func TestBeginAllocateCreatesTaggedVM(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))
	request.Allocation.UserData = "#cloud-config\nsecret-bootstrap-token\n"

	begin, err := provider.BeginAllocate(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, testVMID(), begin.Operation.OperationID)
	require.Len(t, client.creates, 1)

	// The persisted handle and message never carry the user data.
	require.NotContains(t, begin.Operation.ResumeToken, "secret-bootstrap-token")
	require.NotContains(t, begin.Message, "secret-bootstrap-token")

	handle, err := parseAllocateHandle(begin.Operation.ResumeToken)
	require.NoError(t, err)
	require.Nil(t, handle.Rejected)
	require.Equal(t, testNow, *handle.AcceptedAt)

	vm := client.creates[0]
	require.Equal(t, string(testAllocationUID), *vm.Tags[TagAllocationUID])
	require.Equal(t, "claim-a", *vm.Tags[TagAllocationName])
	require.Equal(t, "azure", *vm.Tags[TagMachineClass])
	require.Equal(t, "1", *vm.Zones[0])
	require.Equal(t, testVMName, *vm.Properties.OSProfile.ComputerName)
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte(request.Allocation.UserData)), *vm.Properties.OSProfile.CustomData)
	require.Equal(t, testVMName+osDiskNameSuffix, *vm.Properties.StorageProfile.OSDisk.Name)
	require.Equal(t, armcompute.DiskDeleteOptionTypesDelete, *vm.Properties.StorageProfile.OSDisk.DeleteOption)
	require.Contains(t, vm.Identity.UserAssignedIdentities, testIdentityID)

	nic := vm.Properties.NetworkProfile.NetworkInterfaceConfigurations[0]
	require.Equal(t, testVMName+nicNameSuffix, *nic.Name)
	require.Equal(t, armcompute.DeleteOptionsDelete, *nic.Properties.DeleteOption)
	require.Equal(t, testSubnetID, *nic.Properties.IPConfigurations[0].Properties.Subnet.ID)
}

// A Begin repeated for the same allocation, even under another operation
// UID, finds the VM by the allocation UID and never creates a second one.
func TestBeginAllocateReplayAdoptsOwnVM(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))
	request.OperationUID = "operation-1"

	first, err := provider.BeginAllocate(t.Context(), request)
	require.NoError(t, err)

	request.OperationUID = "operation-2"

	begin, err := provider.BeginAllocate(t.Context(), request)
	require.NoError(t, err)
	require.Contains(t, begin.Message, "adopted")
	require.Equal(t, first.Operation.OperationID, begin.Operation.OperationID)
	require.Len(t, client.creates, 1)
}

func TestBeginAllocateRefusesForeignVM(t *testing.T) {
	tests := map[string]map[string]*string{
		"other allocation": {
			TagAllocationUID: new("ffffffff-ffff-ffff-ffff-ffffffffffff"),
		},
		"untagged": nil,
	}

	for name, tags := range tests {
		t.Run(name, func(t *testing.T) {
			client := newFakeCapacityClient()
			client.vms[testVMName] = &armcompute.VirtualMachine{
				Name: new(testVMName),
				Tags: tags,
			}

			provider := newTestProvider(client)
			request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

			_, err := provider.BeginAllocate(t.Context(), request)
			requireConflict(t, err)
			require.Zero(t, client.mutations())
		})
	}
}

func TestBeginAllocateRejectsInvalidInput(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	offering := resolveTestOffering(t, provider)

	tests := map[string]func(*machineops.OperationRequest){
		"no allocation": func(request *machineops.OperationRequest) {
			request.Allocation = nil
		},
		"providerID names another VM": func(request *machineops.OperationRequest) {
			request.ProviderID = strings.Replace(request.ProviderID, testVMName, "ub-ffffffffffffffffffffffffffffffff", 1)
		},
		"providerID in another resource group": func(request *machineops.OperationRequest) {
			request.ProviderID = strings.Replace(request.ProviderID, testResourceGroup, "elsewhere", 1)
		},
		"missing provider config": func(request *machineops.OperationRequest) {
			request.Allocation.ProviderConfig = machineops.ProviderConfig{}
		},
		"host image differs from the config": func(request *machineops.OperationRequest) {
			request.HostImage = "Canonical:ubuntu-24_04-lts:server:24.04.1"
		},
		"offering differs from the config": func(request *machineops.OperationRequest) {
			request.Allocation.Offering.Zone = "eastus2-2"
		},
		"missing user data": func(request *machineops.OperationRequest) {
			request.Allocation.UserData = ""
		},
		"oversized user data": func(request *machineops.OperationRequest) {
			request.Allocation.UserData = strings.Repeat("x", azureCustomDataMaxBytes+1)
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, offering)
			mutate(&request)

			_, err := provider.BeginAllocate(t.Context(), request)
			requirePermanent(t, err, unboundedv1alpha3.FailureInvalidRequest)
			require.Zero(t, client.mutations())
		})
	}
}

// A create Azure definitively refuses returns a handle, never a
// PermanentError, and Poll reports the failure with an empty ownership
// report after it confirmed nothing exists. A definitive refusal is one where
// Azure accepted nothing and the same request cannot succeed later. Anything
// that leaves the outcome open is retried.
func TestBeginAllocateCreateRejections(t *testing.T) {
	tests := map[string]struct {
		err        error
		definitive bool
		reason     unboundedv1alpha3.MachineAllocationFailureReason
	}{
		"SKU not available": {
			err:        azureResponseError(http.StatusConflict, "SkuNotAvailable"),
			definitive: true,
			reason:     unboundedv1alpha3.FailureInsufficientCapacity,
		},
		"zonal allocation failed": {
			err:        azureResponseError(http.StatusBadRequest, "ZonalAllocationFailed"),
			definitive: true,
			reason:     unboundedv1alpha3.FailureInsufficientCapacity,
		},
		"quota": {
			err:        azureResponseError(http.StatusBadRequest, "QuotaExceeded"),
			definitive: true,
			reason:     unboundedv1alpha3.FailureQuotaExceeded,
		},
		"operation not allowed for quota": {
			err:        fmt.Errorf("exceeding approved cores quota: %w", azureResponseError(http.StatusConflict, "OperationNotAllowed")),
			definitive: true,
			reason:     unboundedv1alpha3.FailureQuotaExceeded,
		},
		"operation not allowed without quota": {
			err:        azureResponseError(http.StatusConflict, "OperationNotAllowed"),
			definitive: true,
			reason:     unboundedv1alpha3.FailureProviderFailed,
		},
		"missing subscription registration": {
			err:        azureResponseError(http.StatusConflict, "MissingSubscriptionRegistration"),
			definitive: true,
			reason:     unboundedv1alpha3.FailureProviderFailed,
		},
		"read-only disabled subscription": {
			err:        azureResponseError(http.StatusConflict, "readonlydisabledsubscription"),
			definitive: true,
			reason:     unboundedv1alpha3.FailureProviderFailed,
		},
		"forbidden": {
			err:        azureResponseError(http.StatusForbidden, "AuthorizationFailed"),
			definitive: true,
			reason:     unboundedv1alpha3.FailureAuthFailed,
		},
		"unauthorized without a known code": {
			err:        azureResponseError(http.StatusUnauthorized, "InvalidAuthenticationToken"),
			definitive: true,
			reason:     unboundedv1alpha3.FailureAuthFailed,
		},
		"bad request": {
			err:        azureResponseError(http.StatusBadRequest, "InvalidParameter"),
			definitive: true,
			reason:     unboundedv1alpha3.FailureInvalidRequest,
		},
		"resource group not found": {
			err:        azureResponseError(http.StatusNotFound, "ResourceGroupNotFound"),
			definitive: true,
			reason:     unboundedv1alpha3.FailureInvalidRequest,
		},
		"image not found": {
			err:        azureResponseError(http.StatusNotFound, "PlatformImageNotFound"),
			definitive: true,
			reason:     unboundedv1alpha3.FailureInvalidRequest,
		},
		"not found without an Azure code": {
			err:        azureResponseError(http.StatusNotFound, ""),
			definitive: true,
			reason:     unboundedv1alpha3.FailureInvalidRequest,
		},
		"unprocessable": {
			err:        azureResponseError(http.StatusUnprocessableEntity, "InvalidTemplate"),
			definitive: true,
			reason:     unboundedv1alpha3.FailureInvalidRequest,
		},
		// A conflict that may mean an operation on the VM is under way is
		// never definitive: something may exist.
		"conflict": {
			err: azureResponseError(http.StatusConflict, "Conflict"),
		},
		"another operation in progress": {
			err: azureResponseError(http.StatusConflict, "AnotherOperationInProgress"),
		},
		"operation preempted": {
			err: azureResponseError(http.StatusConflict, "OperationPreempted"),
		},
		"conflict with an unknown code": {
			err: azureResponseError(http.StatusConflict, "SomethingNew"),
		},
		"conflict without a code": {
			err: azureResponseError(http.StatusConflict, ""),
		},
		"request timeout": {
			err: azureResponseError(http.StatusRequestTimeout, "RequestTimeout"),
		},
		"throttled": {
			err: azureResponseError(http.StatusTooManyRequests, "TooManyRequests"),
		},
		"server error": {
			err: azureResponseError(http.StatusInternalServerError, "InternalError"),
		},
		"service unavailable": {
			err: azureResponseError(http.StatusServiceUnavailable, "ServiceUnavailable"),
		},
		"transport": {
			err: errors.New("connection reset"),
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			client := newFakeCapacityClient()
			client.createErr = test.err

			provider := newTestProvider(client)
			request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

			begin, err := provider.BeginAllocate(t.Context(), request)
			if !test.definitive {
				requireTransient(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, testVMID(), begin.Operation.OperationID)

			result, err := provider.PollAllocate(t.Context(), pollRequestFor(t, request), begin.Operation)
			require.NoError(t, err)
			require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
			require.Equal(t, string(test.reason), result.Reason)
			require.Contains(t, result.Message, "Azure rejected the creation")
			require.NotNil(t, result.Result.Allocation, "a terminal result reports ownership")
			require.Empty(t, result.Result.Allocation.OwnedResources)
		})
	}
}

// A conflict that turned out to be the allocation's own create in progress is
// adopted by the next Begin instead of ending the allocation.
func TestBeginAllocateConflictThenAdoptsOwnVM(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

	_, err := provider.BeginAllocate(t.Context(), request)
	require.NoError(t, err)

	// The first response was lost. The VM is not visible to the replayed
	// Begin's read and its children are not created yet, so the create is sent
	// again and Azure answers that an operation is in progress.
	created := client.vms[testVMName]
	nic := client.nics[testNICName()]
	disk := client.disks[testDiskName()]

	delete(client.vms, testVMName)
	delete(client.nics, testNICName())
	delete(client.disks, testDiskName())

	client.createErr = azureResponseError(http.StatusConflict, "AnotherOperationInProgress")

	_, err = provider.BeginAllocate(t.Context(), request)
	requireTransient(t, err)

	client.vms[testVMName] = created
	client.nics[testNICName()] = nic
	client.disks[testDiskName()] = disk

	begin, err := provider.BeginAllocate(t.Context(), request)
	require.NoError(t, err)
	require.Contains(t, begin.Message, "adopted")
	require.Len(t, client.creates, 1)
}

// A refused create that is retried and accepted is a normal allocation.
func TestBeginAllocateRejectionThenRetrySucceeds(t *testing.T) {
	client := newFakeCapacityClient()
	client.createErr = azureResponseError(http.StatusConflict, "SkuNotAvailable")

	provider := newTestProvider(client)
	request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

	rejected, err := provider.BeginAllocate(t.Context(), request)
	require.NoError(t, err)

	// The handle was lost and Begin replayed; this time Azure accepts.
	client.createErr = nil

	_, err = provider.BeginAllocate(t.Context(), request)
	require.NoError(t, err)

	client.vms[testVMName].Properties.ProvisioningState = new("Succeeded")

	// Even the stale rejected handle observes the VM that now exists.
	result, err := provider.PollAllocate(t.Context(), pollRequestFor(t, request), rejected.Operation)
	require.NoError(t, err)
	require.Equal(t, machineops.ProviderOperationStateSucceeded, result.State)
	require.Equal(t, testOwnedResources(), result.Result.Allocation.OwnedResources)
}

func TestPollAllocate(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

	begin, err := provider.BeginAllocate(t.Context(), request)
	require.NoError(t, err)

	pollRequest := pollRequestFor(t, request)

	result, err := provider.PollAllocate(t.Context(), pollRequest, begin.Operation)
	require.NoError(t, err)
	require.Equal(t, machineops.ProviderOperationStateInProgress, result.State)
	require.Nil(t, result.Result.Allocation)

	vm := client.vms[testVMName]
	vm.Properties.ProvisioningState = new("Succeeded")
	vm.Properties.StorageProfile.ImageReference.ExactVersion = new("24.04.202610160")

	result, err = provider.PollAllocate(t.Context(), pollRequest, begin.Operation)
	require.NoError(t, err)
	require.Equal(t, machineops.ProviderOperationStateSucceeded, result.State)
	require.Equal(t, testVMID(), result.Result.Allocation.ProviderResourceID)
	require.Equal(t, "Standard_D4s_v3", result.Result.Allocation.InstanceType)
	require.Equal(t, "eastus2-1", result.Result.Allocation.Zone)
	require.Equal(t, testImage, result.Result.Allocation.Image)
	require.Empty(t, result.Result.ProviderID)
	require.Equal(t, testOwnedResources(), result.Result.Allocation.OwnedResources)
	require.Equal(t, 1, client.mutations(), "Poll never mutates")
}

func TestPollAllocateRefusesChildAttachedElsewhere(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

	begin, err := provider.BeginAllocate(t.Context(), request)
	require.NoError(t, err)

	client.vms[testVMName].Properties.ProvisioningState = new("Succeeded")
	client.disks[testDiskName()] = testDisk("other-disk", vmResourceID(testSubscription, testResourceGroup, "other"))

	result, err := provider.PollAllocate(t.Context(), request, begin.Operation)
	require.NoError(t, err)
	require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
	require.Equal(t, string(unboundedv1alpha3.FailureProviderConflict), result.Reason)
	require.Nil(t, result.Result.Allocation, "ownership cannot be proved, so none is reported")
}

// Live regression: Azure names the VM-managed NIC "<config>-<8 hex>", so a
// read by the configured name 404s forever. Allocation must follow the ID the
// VM references, and a replayed poll after a lost status write must record
// the same identities.
func TestPollAllocateRecordsSuffixedNICFromVMReference(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

	begin, err := provider.BeginAllocate(t.Context(), request)
	require.NoError(t, err)

	client.vms[testVMName].Properties.ProvisioningState = new("Succeeded")
	require.Nil(t, client.nics[testVMName+nicNameSuffix], "Azure never creates the NIC under its configured name")

	for range 2 {
		client.nicGets = nil

		result, err := provider.PollAllocate(t.Context(), request, begin.Operation)
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateSucceeded, result.State)
		require.Equal(t, testOwnedResources(), result.Result.Allocation.OwnedResources)
		require.Equal(t, []string{testNICName()}, client.nicGets)
	}

	require.Len(t, client.creates, 1)
}

// A restarted controller with a fresh provider resumes from a persisted
// handle that has no resume token, records the suffixed NIC by native
// identity, and release later verifies and deletes it by that record. Azure
// dropped the NIC's cascade delete, and Poll reissues it: the engine never
// begins a second HostRelease.
func TestPendingAllocationResumesAndReleasesSuffixedNIC(t *testing.T) {
	client := newFakeCapacityClient()
	request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, newTestProvider(client)))

	_, err := newTestProvider(client).BeginAllocate(t.Context(), request)
	require.NoError(t, err)

	client.vms[testVMName].Properties.ProvisioningState = new("Succeeded")

	persisted := machineops.ProviderOperation{
		OperationID: testVMID(),
	}

	restarted := newTestProvider(client)

	allocated, err := restarted.PollAllocate(t.Context(), pollRequestFor(t, request), persisted)
	require.NoError(t, err)
	require.Equal(t, machineops.ProviderOperationStateSucceeded, allocated.State)
	require.Contains(t, allocated.Result.Allocation.OwnedResources, unboundedv1alpha3.ProviderOwnedResource{
		ID:       testNICID(),
		NativeID: "nic-guid-" + testVMName,
	})

	release := testOperationRequest(t, unboundedv1alpha3.OperationHostRelease, request.Allocation.Offering)
	release.Allocation.OwnedResources = allocated.Result.Allocation.OwnedResources

	_, err = restarted.BeginRelease(t.Context(), release)
	require.NoError(t, err)
	require.Equal(t, 1, client.vmDeletes)

	// The VM is gone but its delete-option cascade left the NIC behind.
	delete(client.vms, testVMName)
	delete(client.disks, testDiskName())
	client.nics[testNICName()].Properties.VirtualMachine = nil

	stalled := pollRelease(t, restarted, release)
	require.Equal(t, machineops.ProviderOperationStateInProgress, stalled.State)
	require.Contains(t, stalled.Message, testNICName())
	require.Equal(t, 1, client.nicDeletes)

	// The delete is in progress and is not sent again.
	require.Equal(t, machineops.ProviderOperationStateInProgress, pollRelease(t, restarted, release).State)
	require.Equal(t, 1, client.nicDeletes)

	delete(client.nics, testNICName())

	require.Equal(t, machineops.ProviderOperationStateSucceeded, pollRelease(t, restarted, release).State)
	require.Equal(t, 1, client.vmDeletes)
	require.Zero(t, client.diskDeletes)
	require.Len(t, client.creates, 1)
}

func TestPollAllocateWaitsForProvableOwnership(t *testing.T) {
	tests := map[string]struct {
		mutate  func(*fakeCapacityClient, *armcompute.VirtualMachine)
		message string
	}{
		"NIC reference": {
			mutate: func(_ *fakeCapacityClient, vm *armcompute.VirtualMachine) {
				vm.Properties.NetworkProfile.NetworkInterfaces = nil
			},
			message: "does not yet reference",
		},
		"OS disk reference": {
			mutate: func(_ *fakeCapacityClient, vm *armcompute.VirtualMachine) {
				vm.Properties.StorageProfile.OSDisk.ManagedDisk.ID = nil
			},
			message: "does not yet reference",
		},
		"VM ID": {
			mutate: func(_ *fakeCapacityClient, vm *armcompute.VirtualMachine) {
				vm.Properties.VMID = nil
			},
			message: "does not report its VM ID",
		},
		"NIC native ID": {
			mutate: func(client *fakeCapacityClient, _ *armcompute.VirtualMachine) {
				client.nics[testNICName()].Properties.ResourceGUID = nil
			},
			message: "not yet observable",
		},
		"disk not visible": {
			mutate: func(client *fakeCapacityClient, _ *armcompute.VirtualMachine) {
				delete(client.disks, testDiskName())
			},
			message: "not yet observable",
		},
		"NIC attachment": {
			mutate: func(client *fakeCapacityClient, _ *armcompute.VirtualMachine) {
				client.nics[testNICName()].Properties.VirtualMachine = nil
			},
			message: "does not report its attachment yet",
		},
		"disk attachment": {
			mutate: func(client *fakeCapacityClient, _ *armcompute.VirtualMachine) {
				client.disks[testDiskName()].ManagedBy = nil
			},
			message: "does not report its attachment yet",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			client := newFakeCapacityClient()
			provider := newTestProvider(client)
			request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

			begin, err := provider.BeginAllocate(t.Context(), request)
			require.NoError(t, err)

			vm := client.vms[testVMName]
			vm.Properties.ProvisioningState = new("Succeeded")
			test.mutate(client, vm)

			// A retryable error keeps the operation in progress instead of
			// completing without a record of every resource.
			_, err = provider.PollAllocate(t.Context(), request, begin.Operation)
			requireTransient(t, err)
			require.ErrorContains(t, err, test.message)
		})
	}
}

func TestPollAllocateRefusesChildOutsideResourceGroup(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

	begin, err := provider.BeginAllocate(t.Context(), request)
	require.NoError(t, err)

	vm := client.vms[testVMName]
	vm.Properties.ProvisioningState = new("Succeeded")
	vm.Properties.NetworkProfile.NetworkInterfaces[0].ID = new("/subscriptions/" + testSubscription + "/resourceGroups/other/providers/Microsoft.Network/networkInterfaces/" + testNICName())
	client.nicGets = nil

	result, err := provider.PollAllocate(t.Context(), request, begin.Operation)
	require.NoError(t, err)
	require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
	require.Equal(t, string(unboundedv1alpha3.FailureProviderConflict), result.Reason)
	require.Nil(t, result.Result.Allocation)
	require.Empty(t, client.nicGets)
}

// A failed create still records the VM and the children attached to it, so
// release deletes them and can verify a suffixed NIC left behind after the VM
// is gone.
func TestPollAllocateFailureRecordsOwnedResources(t *testing.T) {
	t.Run("VM with attached children", func(t *testing.T) {
		client := newFakeCapacityClient()
		provider := newTestProvider(client)
		request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

		begin, err := provider.BeginAllocate(t.Context(), request)
		require.NoError(t, err)

		vm := client.vms[testVMName]
		vm.Properties.ProvisioningState = new("Failed")
		vm.Properties.InstanceView = failedTestVM("OSProvisioningTimedOut").Properties.InstanceView

		result, err := provider.PollAllocate(t.Context(), request, begin.Operation)
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
		require.Equal(t, string(unboundedv1alpha3.FailureProviderFailed), result.Reason)
		require.Contains(t, result.Message, "OSProvisioningTimedOut")
		require.NotNil(t, result.Result.Allocation)
		require.Equal(t, testOwnedResources(), result.Result.Allocation.OwnedResources)

		// Release cleans up exactly what the failure reported.
		release := testOperationRequest(t, unboundedv1alpha3.OperationHostRelease, request.Allocation.Offering)
		release.Allocation.OwnedResources = result.Result.Allocation.OwnedResources

		_, err = provider.BeginRelease(t.Context(), release)
		require.NoError(t, err)
		require.Equal(t, 1, client.vmDeletes)
	})

	t.Run("VM without children is classified and reported alone", func(t *testing.T) {
		client := newFakeCapacityClient()
		client.vms[testVMName] = failedTestVM("ZonalAllocationFailed")

		provider := newTestProvider(client)
		request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

		result, err := provider.PollAllocate(t.Context(), request, machineops.ProviderOperation{})
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
		require.Equal(t, string(unboundedv1alpha3.FailureInsufficientCapacity), result.Reason)
		require.Equal(t, testOwnedResources()[:1], result.Result.Allocation.OwnedResources)
	})

	t.Run("VM being deleted", func(t *testing.T) {
		client := newFakeCapacityClient()
		client.vms[testVMName] = failedTestVM("Unused")
		client.vms[testVMName].Properties.ProvisioningState = new("Deleting")

		provider := newTestProvider(client)
		request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

		result, err := provider.PollAllocate(t.Context(), request, machineops.ProviderOperation{})
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
		require.Equal(t, testOwnedResources()[:1], result.Result.Allocation.OwnedResources)
	})

	// A capacity failure can leave the OS disk present and detached for a
	// moment. Ending there would report no ownership and lose the reason, so
	// the operation stays in progress until the disk settles.
	t.Run("unattached OS disk is waited for and the reason survives", func(t *testing.T) {
		for name, settle := range map[string]struct {
			apply func(*fakeCapacityClient)
			owned int
		}{
			"disk is deleted": {
				apply: func(client *fakeCapacityClient) {
					delete(client.disks, testDiskName())
				},
				owned: 1,
			},
			"disk is attached": {
				apply: func(client *fakeCapacityClient) {
					client.disks[testDiskName()].ManagedBy = new(testVMID())
				},
				owned: 2,
			},
		} {
			t.Run(name, func(t *testing.T) {
				client := newFakeCapacityClient()
				client.vms[testVMName] = failedTestVM("ZonalAllocationFailed")
				client.disks[testDiskName()] = testDisk("disk-guid", "")

				provider := newTestProvider(client)
				request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

				for range 2 {
					result, err := provider.PollAllocate(t.Context(), request, machineops.ProviderOperation{})
					require.NoError(t, err)
					require.Equal(t, machineops.ProviderOperationStateInProgress, result.State)
					require.Equal(t, pollInterval, result.RequeueAfter)
					require.Nil(t, result.Result.Allocation)
					require.Contains(t, result.Message, string(unboundedv1alpha3.FailureInsufficientCapacity))
					require.Contains(t, result.Message, "ZonalAllocationFailed")
					require.Contains(t, result.Message, testDiskName())
				}

				settle.apply(client)

				result, err := provider.PollAllocate(t.Context(), request, machineops.ProviderOperation{})
				require.NoError(t, err)
				require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
				require.Equal(t, string(unboundedv1alpha3.FailureInsufficientCapacity), result.Reason)
				require.NotNil(t, result.Result.Allocation)
				require.Len(t, result.Result.Allocation.OwnedResources, settle.owned)
				require.Zero(t, client.mutations(), "Poll never mutates")
			})
		}
	})

	t.Run("attached OS disk without a native ID is waited for", func(t *testing.T) {
		client := newFakeCapacityClient()
		client.vms[testVMName] = failedTestVM("ZonalAllocationFailed")
		client.disks[testDiskName()] = testDisk("", testVMID())

		provider := newTestProvider(client)
		request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

		result, err := provider.PollAllocate(t.Context(), request, machineops.ProviderOperation{})
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateInProgress, result.State)
	})

	// A child attached to another VM is not a passing state: ownership is
	// disproved, so the result is terminal and reports none.
	t.Run("OS disk attached to another VM leaves ownership unknown", func(t *testing.T) {
		client := newFakeCapacityClient()
		client.vms[testVMName] = failedTestVM("ZonalAllocationFailed")
		client.disks[testDiskName()] = testDisk("disk-guid", vmResourceID(testSubscription, testResourceGroup, "other"))

		provider := newTestProvider(client)
		request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

		result, err := provider.PollAllocate(t.Context(), request, machineops.ProviderOperation{})
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
		require.Equal(t, string(unboundedv1alpha3.FailureInsufficientCapacity), result.Reason)
		require.Nil(t, result.Result.Allocation, "an unprovable disk leaves ownership unknown")
	})

	// A deleting VM detaches its children before Azure removes them. The
	// operation waits, and ends with nothing owned once everything is gone.
	t.Run("deleting VM whose disk just detached converges", func(t *testing.T) {
		client := newFakeCapacityClient()
		provider := newTestProvider(client)
		request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

		begin, err := provider.BeginAllocate(t.Context(), request)
		require.NoError(t, err)

		client.vms[testVMName].Properties.ProvisioningState = new("Deleting")
		client.disks[testDiskName()].ManagedBy = nil

		poll := func() machineops.PollResult {
			result, err := provider.PollAllocate(t.Context(), pollRequestFor(t, request), begin.Operation)
			require.NoError(t, err)

			return result
		}

		result := poll()
		require.Equal(t, machineops.ProviderOperationStateInProgress, result.State)
		require.Contains(t, result.Message, "is being deleted")
		require.Contains(t, result.Message, testDiskName())

		// The VM is gone first; its NIC and disk follow.
		delete(client.vms, testVMName)
		client.nics[testNICName()].Properties.VirtualMachine = nil

		provider.Now = func() time.Time {
			return testNow.Add(createVisibilityGrace)
		}

		result = poll()
		require.Equal(t, machineops.ProviderOperationStateInProgress, result.State)
		require.Contains(t, result.Message, testNICName())
		require.Contains(t, result.Message, testDiskName())

		delete(client.nics, testNICName())
		delete(client.disks, testDiskName())

		result = poll()
		require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
		require.Equal(t, string(unboundedv1alpha3.FailureProviderFailed), result.Reason)
		require.NotNil(t, result.Result.Allocation)
		require.Empty(t, result.Result.Allocation.OwnedResources)
		require.Equal(t, 1, client.mutations(), "Poll never mutates")
	})

	// A failed VM may not reference the NIC Azure created for it. The NIC is
	// found by the name Azure derives and recorded only because it is attached
	// to the allocation VM, so release deletes it with the VM.
	t.Run("suffixed NIC the failed VM does not reference is recorded", func(t *testing.T) {
		client := newFakeCapacityClient()
		provider := newTestProvider(client)
		request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

		begin, err := provider.BeginAllocate(t.Context(), request)
		require.NoError(t, err)

		vm := client.vms[testVMName]
		vm.Properties.ProvisioningState = new("Failed")
		vm.Properties.NetworkProfile.NetworkInterfaces = nil
		vm.Properties.StorageProfile.OSDisk.ManagedDisk.ID = nil

		// Another allocation's NIC in the same resource group is not this
		// allocation's business.
		client.nics["ub-ffffffffffffffffffffffffffffffff"+nicNameSuffix+testNICSuffix] = testNIC("other-nic", "")

		result, err := provider.PollAllocate(t.Context(), request, begin.Operation)
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
		require.NotNil(t, result.Result.Allocation)
		require.ElementsMatch(t, []unboundedv1alpha3.ProviderOwnedResource{
			testOwnedResources()[0],
			{
				ID:       createdChildNICID(),
				NativeID: "nic-guid-" + testVMName,
			},
			{
				ID:       allocationVM{subscriptionID: testSubscription, resourceGroup: testResourceGroup, name: testVMName}.osDiskID(),
				NativeID: "disk-guid-" + testVMName,
			},
		}, result.Result.Allocation.OwnedResources)

		release := testOperationRequest(t, unboundedv1alpha3.OperationHostRelease, request.Allocation.Offering)
		release.Allocation.OwnedResources = result.Result.Allocation.OwnedResources

		_, err = provider.BeginRelease(t.Context(), release)
		require.NoError(t, err)
		require.Equal(t, 1, client.vmDeletes)

		// Azure does not cascade to a NIC the VM does not reference.
		delete(client.vms, testVMName)
		delete(client.disks, testDiskName())
		client.nics[testNICName()].Properties.VirtualMachine = nil

		require.Equal(t, machineops.ProviderOperationStateInProgress, pollRelease(t, provider, release).State)
		require.Equal(t, 1, client.nicDeletes)

		delete(client.nics, testNICName())

		require.Equal(t, machineops.ProviderOperationStateSucceeded, pollRelease(t, provider, release).State)
		require.Equal(t, 1, client.nicDeletes)
	})

	t.Run("unattached suffixed NIC of a failed VM is waited for", func(t *testing.T) {
		client := newFakeCapacityClient()
		client.vms[testVMName] = failedTestVM("ZonalAllocationFailed")
		client.nics[testNICName()] = testNIC("nic-guid", "")

		provider := newTestProvider(client)
		request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

		result, err := provider.PollAllocate(t.Context(), request, machineops.ProviderOperation{})
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateInProgress, result.State)
		require.Contains(t, result.Message, testNICName())
		require.Contains(t, result.Message, string(unboundedv1alpha3.FailureInsufficientCapacity))
	})

	t.Run("NIC list errors are retried", func(t *testing.T) {
		client := newFakeCapacityClient()
		client.vms[testVMName] = failedTestVM("ZonalAllocationFailed")
		client.childGetErr = azureResponseError(http.StatusNotFound, "SubscriptionNotFound")

		provider := newTestProvider(client)
		request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

		_, err := provider.PollAllocate(t.Context(), request, machineops.ProviderOperation{})
		requireTransient(t, err)
	})

	t.Run("unreferenced OS disk attached to the VM is recorded", func(t *testing.T) {
		client := newFakeCapacityClient()
		client.vms[testVMName] = failedTestVM("ZonalAllocationFailed")
		client.disks[testDiskName()] = testDisk("disk-guid", testVMID())

		provider := newTestProvider(client)
		request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

		result, err := provider.PollAllocate(t.Context(), request, machineops.ProviderOperation{})
		require.NoError(t, err)
		require.Len(t, result.Result.Allocation.OwnedResources, 2)
		require.Equal(t, "disk-guid", result.Result.Allocation.OwnedResources[1].NativeID)
	})

	t.Run("VM ID not reported yet is retried", func(t *testing.T) {
		client := newFakeCapacityClient()
		client.vms[testVMName] = failedTestVM("ZonalAllocationFailed")
		client.vms[testVMName].Properties.VMID = nil

		provider := newTestProvider(client)
		request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

		_, err := provider.PollAllocate(t.Context(), request, machineops.ProviderOperation{})
		requireTransient(t, err)
	})
}

// A compound create would update an existing NIC or disk with the same name,
// so Begin refuses to create the VM over resources it does not own.
func TestBeginAllocateRefusesExistingChildResources(t *testing.T) {
	tests := map[string]func(*fakeCapacityClient){
		"NIC under the configured name": func(client *fakeCapacityClient) {
			client.nics[testVMName+nicNameSuffix] = testNIC("foreign-nic", "")
		},
		"NIC under a name Azure derives for this VM": func(client *fakeCapacityClient) {
			client.nics[testNICName()] = testNIC("foreign-nic", "")
		},
		"NIC under a derived name in another case": func(client *fakeCapacityClient) {
			client.nics[strings.ToUpper(testNICName())] = testNIC("foreign-nic", "")
		},
		"disk": func(client *fakeCapacityClient) {
			client.disks[testDiskName()] = testDisk("foreign-disk", "")
		},
	}

	for name, seed := range tests {
		t.Run(name, func(t *testing.T) {
			client := newFakeCapacityClient()
			seed(client)

			provider := newTestProvider(client)
			request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

			_, err := provider.BeginAllocate(t.Context(), request)
			requireConflict(t, err)
			require.Zero(t, client.mutations())
		})
	}
}

// Resources of other allocations and VMs in the resource group never block a
// create.
func TestBeginAllocateIgnoresUnrelatedChildResources(t *testing.T) {
	client := newFakeCapacityClient()
	client.nics["ub-ffffffffffffffffffffffffffffffff"+nicNameSuffix+testNICSuffix] = testNIC("other-nic", "")
	client.nics[testVMName+"x"+nicNameSuffix+testNICSuffix] = testNIC("other-nic", "")
	client.nics["unrelated"] = testNIC("other-nic", "")

	provider := newTestProvider(client)
	request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

	_, err := provider.BeginAllocate(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, client.creates, 1)
}

func TestBeginAllocateRetriesNICListErrors(t *testing.T) {
	client := newFakeCapacityClient()
	client.childGetErr = errors.New("timeout")

	provider := newTestProvider(client)
	request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

	_, err := provider.BeginAllocate(t.Context(), request)
	requireTransient(t, err)
	require.Zero(t, client.mutations())
}

// Nothing exists before the create, so a credential Azure refuses ends the
// allocation instead of leaving a Begin that is retried without end.
func TestBeginAllocateFailsWhenAzureRefusesTheCredential(t *testing.T) {
	forbidden := &azcore.ResponseError{StatusCode: http.StatusForbidden, ErrorCode: "AuthorizationFailed"}
	unauthorized := &azcore.ResponseError{StatusCode: http.StatusUnauthorized, ErrorCode: "InvalidAuthenticationToken"}

	for name, fail := range map[string]func(*fakeCapacityClient){
		"VM read forbidden":    func(client *fakeCapacityClient) { client.getErr = fmt.Errorf("get VM: %w", forbidden) },
		"VM read unauthorized": func(client *fakeCapacityClient) { client.getErr = fmt.Errorf("get VM: %w", unauthorized) },
		"child read forbidden": func(client *fakeCapacityClient) { client.childGetErr = fmt.Errorf("list NICs: %w", forbidden) },
		"token cannot be issued": func(client *fakeCapacityClient) {
			client.getErr = fmt.Errorf("get VM: %w", &azidentity.AuthenticationFailedError{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			client := newFakeCapacityClient()
			provider := newTestProvider(client)
			request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

			fail(client)

			_, err := provider.BeginAllocate(t.Context(), request)
			requirePermanent(t, err, unboundedv1alpha3.FailureAuthFailed)
			require.Zero(t, client.mutations())
		})
	}
}

// Canceled is a terminal Azure provisioning state: the VM and what it
// references are reported so release can remove them.
func TestPollAllocateTreatsCanceledAsFailed(t *testing.T) {
	client := newFakeCapacityClient()
	provider := newTestProvider(client)
	request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

	begin, err := provider.BeginAllocate(t.Context(), request)
	require.NoError(t, err)

	client.vms[testVMName].Properties.ProvisioningState = new("Canceled")

	result, err := provider.PollAllocate(t.Context(), request, begin.Operation)
	require.NoError(t, err)
	require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
	require.NotNil(t, result.Result.Allocation)
	require.Equal(t, testOwnedResources(), result.Result.Allocation.OwnedResources)
}

func TestPollAllocateWithoutVM(t *testing.T) {
	accepted := func(t *testing.T) (*fakeCapacityClient, *Provider, machineops.OperationRequest, machineops.ProviderOperation) {
		t.Helper()

		client := newFakeCapacityClient()
		provider := newTestProvider(client)
		request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

		begin, err := provider.BeginAllocate(t.Context(), request)
		require.NoError(t, err)

		// The VM and everything created with it vanished.
		delete(client.vms, testVMName)
		delete(client.nics, testNICName())
		delete(client.disks, testDiskName())

		return client, provider, pollRequestFor(t, request), begin.Operation
	}

	t.Run("read errors are not absence", func(t *testing.T) {
		client, provider, request, operation := accepted(t)
		client.getErr = azureResponseError(http.StatusServiceUnavailable, "ServiceUnavailable")

		_, err := provider.PollAllocate(t.Context(), request, operation)
		requireTransient(t, err)
	})

	t.Run("just accepted create waits to become visible", func(t *testing.T) {
		_, provider, request, operation := accepted(t)

		result, err := provider.PollAllocate(t.Context(), request, operation)
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateInProgress, result.State)
	})

	t.Run("absent after the grace period owns nothing", func(t *testing.T) {
		_, provider, request, operation := accepted(t)
		provider.Now = func() time.Time {
			return testNow.Add(createVisibilityGrace)
		}

		result, err := provider.PollAllocate(t.Context(), request, operation)
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
		require.Equal(t, string(unboundedv1alpha3.FailureProviderFailed), result.Reason)
		require.NotNil(t, result.Result.Allocation)
		require.Empty(t, result.Result.Allocation.OwnedResources)
	})

	t.Run("absent with a handle that has no token owns nothing", func(t *testing.T) {
		_, provider, request, _ := accepted(t)

		result, err := provider.PollAllocate(t.Context(), request, machineops.ProviderOperation{OperationID: testVMID()})
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
		require.NotNil(t, result.Result.Allocation)
		require.Empty(t, result.Result.Allocation.OwnedResources)
	})

	// Azure removes a VM before the NIC and disk it cascades to, so a leftover
	// attached to nothing is waited for. While it exists the allocation can
	// neither claim it nor say that it owns nothing.
	leftovers := map[string]struct {
		seed func(*fakeCapacityClient)
		name string
	}{
		"disk": {
			seed: func(client *fakeCapacityClient) {
				client.disks[testDiskName()] = testDisk("disk-guid", "")
			},
			name: testDiskName(),
		},
		"suffixed NIC": {
			seed: func(client *fakeCapacityClient) {
				client.nics[testNICName()] = testNIC("nic-guid", "")
			},
			name: testNICName(),
		},
	}

	for name, leftover := range leftovers {
		t.Run("a "+name+" left without the VM is waited for", func(t *testing.T) {
			client, provider, request, _ := accepted(t)
			leftover.seed(client)

			operation := machineops.ProviderOperation{OperationID: testVMID()}

			result, err := provider.PollAllocate(t.Context(), request, operation)
			require.NoError(t, err)
			require.Equal(t, machineops.ProviderOperationStateInProgress, result.State)
			require.Nil(t, result.Result.Allocation)
			require.Contains(t, result.Message, leftover.name)

			delete(client.disks, testDiskName())
			delete(client.nics, testNICName())

			result, err = provider.PollAllocate(t.Context(), request, operation)
			require.NoError(t, err)
			require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
			require.NotNil(t, result.Result.Allocation)
			require.Empty(t, result.Result.Allocation.OwnedResources)
		})
	}

	t.Run("a NIC under the configured name is not the allocation's", func(t *testing.T) {
		client, provider, request, _ := accepted(t)
		client.nics[testVMName+nicNameSuffix] = testNIC("unrelated-nic", "")

		result, err := provider.PollAllocate(t.Context(), request, machineops.ProviderOperation{OperationID: testVMID()})
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
		require.NotNil(t, result.Result.Allocation)
		require.Empty(t, result.Result.Allocation.OwnedResources)
	})

	t.Run("a suffixed NIC attached to another VM leaves ownership unknown", func(t *testing.T) {
		client, provider, request, _ := accepted(t)
		client.nics[testNICName()] = testNIC("nic-guid", vmResourceID(testSubscription, testResourceGroup, "other"))

		result, err := provider.PollAllocate(t.Context(), request, machineops.ProviderOperation{OperationID: testVMID()})
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
		require.Nil(t, result.Result.Allocation)
		require.Contains(t, result.Message, testNICName())
	})

	t.Run("a leftover after a refused create keeps the refusal reason", func(t *testing.T) {
		client := newFakeCapacityClient()
		client.createErr = azureResponseError(http.StatusConflict, "SkuNotAvailable")

		provider := newTestProvider(client)
		request := testOperationRequest(t, unboundedv1alpha3.OperationHostAllocate, resolveTestOffering(t, provider))

		begin, err := provider.BeginAllocate(t.Context(), request)
		require.NoError(t, err)

		client.nics[testNICName()] = testNIC("nic-guid", "")

		result, err := provider.PollAllocate(t.Context(), pollRequestFor(t, request), begin.Operation)
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateInProgress, result.State)
		require.Contains(t, result.Message, string(unboundedv1alpha3.FailureInsufficientCapacity))
		require.Contains(t, result.Message, "SkuNotAvailable")

		delete(client.nics, testNICName())

		result, err = provider.PollAllocate(t.Context(), pollRequestFor(t, request), begin.Operation)
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
		require.Equal(t, string(unboundedv1alpha3.FailureInsufficientCapacity), result.Reason)
		require.Empty(t, result.Result.Allocation.OwnedResources)
	})

	t.Run("child read errors are retried", func(t *testing.T) {
		client, provider, request, _ := accepted(t)
		client.childGetErr = errors.New("timeout")

		_, err := provider.PollAllocate(t.Context(), request, machineops.ProviderOperation{OperationID: testVMID()})
		requireTransient(t, err)
	})

	t.Run("foreign VM leaves ownership unknown", func(t *testing.T) {
		client, provider, request, operation := accepted(t)
		client.vms[testVMName] = &armcompute.VirtualMachine{
			Name: new(testVMName),
		}

		result, err := provider.PollAllocate(t.Context(), request, operation)
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateFailed, result.State)
		require.Equal(t, string(unboundedv1alpha3.FailureProviderConflict), result.Reason)
		require.Nil(t, result.Result.Allocation)
	})

	t.Run("unreadable resume token is permanent", func(t *testing.T) {
		_, provider, request, _ := accepted(t)

		_, err := provider.PollAllocate(t.Context(), request, machineops.ProviderOperation{
			OperationID: testVMID(),
			ResumeToken: "{not json",
		})
		requirePermanent(t, err, unboundedv1alpha3.FailureInvalidRequest)
	})
}

func TestWorkerIdentityIsOptional(t *testing.T) {
	data := testConfig()
	delete(data, ConfigWorkerIdentityResourceID)

	config, err := parseCapacityConfig(data)
	require.NoError(t, err)
	require.Nil(t, workerIdentity(config), "no identity may be attached when the class names none")

	config, err = parseCapacityConfig(testConfig())
	require.NoError(t, err)

	identity := workerIdentity(config)
	require.NotNil(t, identity)
	require.Contains(t, identity.UserAssignedIdentities, testIdentityID)
}
