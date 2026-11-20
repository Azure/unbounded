// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/stretchr/testify/require"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
)

func replaceRequest() machineops.OperationRequest {
	return hostRequest(unboundedv1alpha3.OperationHostReplace)
}

func beginTestReplace(t *testing.T, client *fakeVMClient) machineops.ProviderOperation {
	t.Helper()

	begin, err := newHostProvider(client).BeginReplace(t.Context(), replaceRequest())
	require.NoError(t, err)

	return begin.Operation
}

// pollTestReplace polls once with a new Provider that holds only the
// persisted handle, as a controller that restarted since the last call would.
func pollTestReplace(t *testing.T, client *fakeVMClient, operation machineops.ProviderOperation) machineops.PollResult {
	t.Helper()

	poll, err := newHostProvider(client).PollReplace(t.Context(), replaceRequest(), operation)
	require.NoError(t, err)

	return poll
}

// finishTestReplace polls until the operation ends, letting time pass
// between the Polls.
func finishTestReplace(t *testing.T, client *fakeVMClient, operation machineops.ProviderOperation) machineops.PollResult {
	t.Helper()

	for range 50 {
		poll := pollTestReplace(t, client, operation)
		if poll.State != machineops.ProviderOperationStateInProgress {
			return poll
		}

		client.tick()
	}

	require.FailNow(t, "the replacement did not end")

	return machineops.PollResult{}
}

// pollUntilCall polls until the client recorded count calls of the kind.
func pollUntilCall(t *testing.T, client *fakeVMClient, operation machineops.ProviderOperation, call string, count int) {
	t.Helper()

	for range 50 {
		if client.count(call) >= count {
			return
		}

		poll := pollTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateInProgress, poll.State, poll.Message)

		if client.count(call) < count {
			client.tick()
		}
	}

	require.FailNow(t, "the replacement never sent "+call)
}

func TestReplaceSucceeds(t *testing.T) {
	t.Parallel()

	client := newFakeVMClient(replaceableVM())

	operation := beginTestReplace(t, client)
	require.Equal(t, hostVMID(), operation.OperationID)
	require.Empty(t, client.calls, "Begin must change nothing in Azure")

	poll := finishTestReplace(t, client, operation)
	require.Equal(t, machineops.ProviderOperationStateSucceeded, poll.State, poll.Message)
	require.Empty(t, poll.Result.ProviderID, "the replacement keeps the VM's resource ID")
	require.Equal(t, []string{"update:vm1", "delete:vm1", "create:vm1"}, client.calls)

	// The preparation keeps the NIC and data disks and removes the OS disk,
	// and records what it changed.
	require.Len(t, client.updates, 1)
	prepared := client.updates[0]
	require.Equal(t, armcompute.DiskDeleteOptionTypesDelete, *prepared.Properties.StorageProfile.OSDisk.DeleteOption)
	require.Equal(t, armcompute.DeleteOptionsDetach, *prepared.Properties.NetworkProfile.NetworkInterfaces[0].Properties.DeleteOption)

	for _, disk := range prepared.Properties.StorageProfile.DataDisks {
		require.Equal(t, armcompute.DiskDeleteOptionTypesDetach, *disk.DeleteOption)
	}

	require.Equal(t, string(hostOperationUID), *prepared.Tags[TagReplacePending])
	require.Equal(t, "os=;nic=0;lun=0", *prepared.Tags[TagReplaceRestore])
	require.Nil(t, prepared.Properties.OSProfile.CustomData)

	require.Len(t, client.creates, 1)
	requireReplacement(t, client.creates[0])
}

// requireReplacement checks the create payload built from replaceableVM.
func requireReplacement(t *testing.T, vm armcompute.VirtualMachine) {
	t.Helper()

	properties := vm.Properties

	// The cloud-init reaches Azure as customData only.
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte(hostUserData)), *properties.OSProfile.CustomData)
	require.Nil(t, properties.UserData)

	// Tags carry the operation, never the preparation markers.
	require.Equal(t, "prod", *vm.Tags["env"])
	require.Equal(t, string(hostOperationUID), *vm.Tags[TagReplacedBy])
	require.NotContains(t, vm.Tags, TagReplacePending)
	require.NotContains(t, vm.Tags, TagReplaceRestore)

	// A fresh OS disk from the image, named after the operation.
	osDisk := properties.StorageProfile.OSDisk
	require.Equal(t, armcompute.DiskCreateOptionTypesFromImage, *osDisk.CreateOption)
	require.Equal(t, "vm1-osdisk-0b5c7d2e", *osDisk.Name)
	require.Nil(t, osDisk.ManagedDisk.ID)
	require.Nil(t, osDisk.DeleteOption, "the original OS disk had Azure's default delete option")

	// Surviving managed data disks are attached, never created again, and
	// keep their original delete options.
	require.Len(t, properties.StorageProfile.DataDisks, 2)

	empty := properties.StorageProfile.DataDisks[0]
	require.Equal(t, int32(0), *empty.Lun)
	require.Equal(t, armcompute.DiskCreateOptionTypesAttach, *empty.CreateOption)
	require.Equal(t, hostDiskID("vm1-data0"), *empty.ManagedDisk.ID)
	require.Nil(t, empty.ManagedDisk.StorageAccountType)
	require.Nil(t, empty.DiskSizeGB)
	require.Equal(t, armcompute.CachingTypesReadOnly, *empty.Caching)
	require.Equal(t, armcompute.DiskDeleteOptionTypesDelete, *empty.DeleteOption)

	attached := properties.StorageProfile.DataDisks[1]
	require.Equal(t, int32(3), *attached.Lun)
	require.Equal(t, armcompute.DiskCreateOptionTypesAttach, *attached.CreateOption)
	require.Equal(t, hostDiskID("shared-data"), *attached.ManagedDisk.ID)
	require.Equal(t, armcompute.DiskDeleteOptionTypesDetach, *attached.DeleteOption)

	nic := properties.NetworkProfile.NetworkInterfaces[0]
	require.Equal(t, hostNICID("vm1-nic"), *nic.ID)
	require.Equal(t, armcompute.DeleteOptionsDelete, *nic.Properties.DeleteOption)

	require.Equal(t, &armcompute.UserAssignedIdentitiesValue{}, vm.Identity.UserAssignedIdentities[hostIdentityID])
	require.Nil(t, vm.ID)
	require.Nil(t, properties.VMID)
	require.Nil(t, properties.ProvisioningState)
}

func TestReplaceBeginCanRunAgain(t *testing.T) {
	t.Parallel()

	client := newFakeVMClient(replaceableVM())

	// The engine lost the first handle, so it begins again.
	first := beginTestReplace(t, client)
	second := beginTestReplace(t, client)

	require.Equal(t, first, second)
	require.Empty(t, client.calls)
}

// TestReplaceSendsEveryStepOnce polls repeatedly while each Azure operation
// is still in progress: a step that was sent is never sent again.
func TestReplaceSendsEveryStepOnce(t *testing.T) {
	t.Parallel()

	client := newFakeVMClient(replaceableVM())
	operation := beginTestReplace(t, client)

	for _, step := range []string{"update", "delete", "create"} {
		for range 3 {
			poll := pollTestReplace(t, client, operation)
			require.Equal(t, machineops.ProviderOperationStateInProgress, poll.State, poll.Message)
		}

		require.Equal(t, 1, client.count(step), step)

		client.settle()
	}

	require.Equal(t, []string{"update:vm1", "delete:vm1", "create:vm1"}, client.calls)
	require.Equal(t, machineops.ProviderOperationStateSucceeded, pollTestReplace(t, client, operation).State)
}

// TestReplaceCreatesTheReplacementAfterTheVMIsGone covers the point of no
// return: Azure finished deleting the VM, and the controller that sent the
// delete is gone. A new controller has only the handle and creates the
// replacement from it, with the cloud-init as the engine builds it then.
func TestReplaceCreatesTheReplacementAfterTheVMIsGone(t *testing.T) {
	t.Parallel()

	client := newFakeVMClient(replaceableVM())
	operation := beginTestReplace(t, client)

	pollUntilCall(t, client, operation, "delete", 1)
	client.settle()
	require.Nil(t, client.vm)
	require.Empty(t, client.creates)

	request := replaceRequest()
	request.ReplaceUserData = "#cloud-config\nbootstrap-token: abcdef.rotated\n"

	poll, err := newHostProvider(client).PollReplace(t.Context(), request, operation)
	require.NoError(t, err)
	require.Equal(t, machineops.ProviderOperationStateInProgress, poll.State)
	require.Len(t, client.creates, 1)

	created := client.creates[0]
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte(request.ReplaceUserData)), *created.Properties.OSProfile.CustomData)

	// Apart from the cloud-init it is the VM requireReplacement describes.
	created.Properties.OSProfile.CustomData = new(base64.StdEncoding.EncodeToString([]byte(hostUserData)))
	requireReplacement(t, created)

	poll = finishTestReplace(t, client, operation)
	require.Equal(t, machineops.ProviderOperationStateSucceeded, poll.State, poll.Message)
	require.Equal(t, 1, client.count("create"))
}

// TestReplaceSurvivesALostResponse covers a request Azure accepted while the
// controller never saw the answer: the next Poll finds the step under way and
// does not send it again.
func TestReplaceSurvivesALostResponse(t *testing.T) {
	t.Parallel()

	for _, step := range []string{"update", "delete", "create"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()

			client := newFakeVMClient(replaceableVM())
			client.loseResponse = step
			operation := beginTestReplace(t, client)

			var lost int

			for range 50 {
				poll, err := newHostProvider(client).PollReplace(t.Context(), replaceRequest(), operation)
				if err != nil {
					require.ErrorIs(t, err, errResponseLost)
					requireTransient(t, err)

					lost++

					continue
				}

				if poll.State != machineops.ProviderOperationStateInProgress {
					require.Equal(t, machineops.ProviderOperationStateSucceeded, poll.State, poll.Message)

					break
				}

				client.tick()
			}

			require.Equal(t, 1, lost)
			require.Equal(t, []string{"update:vm1", "delete:vm1", "create:vm1"}, client.calls)
		})
	}
}

func TestReplaceNeverOverwritesAVMItDidNotExpect(t *testing.T) {
	t.Parallel()

	t.Run("the VM changed after Begin recorded its model", func(t *testing.T) {
		t.Parallel()

		for _, when := range []string{"before the preparation", "before the delete"} {
			client := newFakeVMClient(replaceableVM())
			operation := beginTestReplace(t, client)

			if when == "before the delete" {
				pollUntilCall(t, client, operation, "update", 1)
				client.settle()
			}

			// A disk is attached in the meantime.
			client.vm.Properties.StorageProfile.DataDisks = append(client.vm.Properties.StorageProfile.DataDisks, &armcompute.DataDisk{
				Lun:          new(int32(7)),
				Name:         new("late-disk"),
				CreateOption: new(armcompute.DiskCreateOptionTypesAttach),
				DeleteOption: new(armcompute.DiskDeleteOptionTypesDetach),
				ManagedDisk:  &armcompute.ManagedDiskParameters{ID: new(hostDiskID("late-disk"))},
			})

			poll := pollTestReplace(t, client, operation)
			require.Equal(t, machineops.ProviderOperationStateFailed, poll.State, when)
			require.Equal(t, string(unboundedv1alpha3.FailureProviderConflict), poll.Reason, when)
			require.Zero(t, client.count("delete"), when)
			require.NotNil(t, client.vm, when)
		}
	})

	t.Run("the preparation is conditional on the etag that was read", func(t *testing.T) {
		t.Parallel()

		// The fake refuses an update whose etag is not the VM's current one,
		// so an accepted preparation proves the etag was passed on.
		client := newFakeVMClient(replaceableVM())
		operation := beginTestReplace(t, client)

		poll := pollTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateInProgress, poll.State)
		require.Len(t, client.updates, 1)
	})

	t.Run("the VM changed while it was prepared", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())
		operation := beginTestReplace(t, client)
		client.updateErrs = []error{azureResponseError(http.StatusPreconditionFailed, "PreconditionFailed")}

		poll := pollTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateInProgress, poll.State)
		require.Empty(t, client.updates)

		poll = finishTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateSucceeded, poll.State, poll.Message)
	})

	t.Run("a VM has the name when the replacement is created", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())
		operation := beginTestReplace(t, client)

		pollUntilCall(t, client, operation, "delete", 1)
		client.settle()

		// An earlier create was accepted but the read does not show it yet.
		client.createErrs = []error{azureResponseError(http.StatusPreconditionFailed, "PreconditionFailed")}

		poll := pollTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateInProgress, poll.State, "a refused conditional create is not a failure")
	})

	t.Run("someone else recreated the VM", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())
		operation := beginTestReplace(t, client)
		client.vm.Properties.VMID = new("another-vm-guid")

		poll := pollTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateFailed, poll.State)
		require.Equal(t, string(unboundedv1alpha3.FailureProviderConflict), poll.Reason)
		require.Empty(t, client.calls)
	})

	t.Run("the original is being deleted", func(t *testing.T) {
		t.Parallel()

		// Whoever sent the delete, the VM is going: the operation waits and
		// sends nothing.
		client := newFakeVMClient(replaceableVM())
		operation := beginTestReplace(t, client)
		client.vm.Properties.ProvisioningState = new("Deleting")

		poll := pollTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateInProgress, poll.State)
		require.Empty(t, client.calls)
	})

	t.Run("the original tagged as its own replacement", func(t *testing.T) {
		t.Parallel()

		// Anyone who can tag the VM can copy the operation UID into the
		// replacement's tag. Azure's unique ID still says it is the original.
		client := newFakeVMClient(replaceableVM())
		operation := beginTestReplace(t, client)
		client.vm.Tags[TagReplacedBy] = new(string(hostOperationUID))

		poll := pollTestReplace(t, client, operation)
		require.NotEqual(t, machineops.ProviderOperationStateSucceeded, poll.State)
	})

	t.Run("the OS disk was swapped after Begin", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())
		operation := beginTestReplace(t, client)
		client.vm.Properties.StorageProfile.OSDisk.ManagedDisk.ID = new(hostDiskID("recovery-osdisk"))

		poll := pollTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateFailed, poll.State)
		require.Equal(t, string(unboundedv1alpha3.FailureProviderConflict), poll.Reason)
		require.Empty(t, client.calls)
	})
}

// requirePreparationUndone checks that the VM built from replaceableVM is
// back to the delete options it had and carries no replacement tags.
func requirePreparationUndone(t *testing.T, client *fakeVMClient) {
	t.Helper()

	client.settle()

	vm := client.vm
	require.NotNil(t, vm)
	require.NotContains(t, vm.Tags, TagReplacePending)
	require.NotContains(t, vm.Tags, TagReplaceRestore)
	require.Equal(t, "prod", *vm.Tags["env"])
	require.False(t, preparedForDelete(vm))

	storage := vm.Properties.StorageProfile
	require.Nil(t, storage.OSDisk.DeleteOption)
	require.Equal(t, armcompute.DiskDeleteOptionTypesDelete, *storage.DataDisks[0].DeleteOption)
	require.Equal(t, armcompute.DiskDeleteOptionTypesDetach, *storage.DataDisks[1].DeleteOption)
	require.Equal(t, armcompute.DeleteOptionsDelete, *vm.Properties.NetworkProfile.NetworkInterfaces[0].Properties.DeleteOption)
}

// TestReplaceUndoesItsPreparationWhenItGivesUp covers every way the
// operation ends after it prepared the VM and before it deleted it. Left
// prepared, the VM would lose its OS disk and keep its NIC and data disk the
// next time anyone deleted it.
func TestReplaceUndoesItsPreparationWhenItGivesUp(t *testing.T) {
	t.Parallel()

	prepared := func(t *testing.T) (*fakeVMClient, machineops.ProviderOperation) {
		t.Helper()

		client := newFakeVMClient(replaceableVM())
		operation := beginTestReplace(t, client)
		pollUntilCall(t, client, operation, "update", 1)
		client.settle()
		require.True(t, preparedForDelete(client.vm))

		return client, operation
	}

	t.Run("Azure refuses the delete", func(t *testing.T) {
		t.Parallel()

		client, operation := prepared(t)
		client.deleteErrs = []error{azureResponseError(http.StatusConflict, "ScopeLocked")}

		poll := pollTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateFailed, poll.State)
		require.Equal(t, string(unboundedv1alpha3.FailureProviderFailed), poll.Reason)
		require.Zero(t, client.count("create"))
		requirePreparationUndone(t, client)
	})

	t.Run("the cloud-init is one Azure would refuse", func(t *testing.T) {
		t.Parallel()

		client, operation := prepared(t)
		request := replaceRequest()
		request.ReplaceUserData = strings.Repeat("a", azureCustomDataMaxBytes+1)

		poll, err := newHostProvider(client).PollReplace(t.Context(), request, operation)
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateFailed, poll.State)
		require.Equal(t, string(unboundedv1alpha3.FailureInvalidRequest), poll.Reason)
		require.Zero(t, client.count("delete"))
		requirePreparationUndone(t, client)
	})

	t.Run("the VM changed, and what changed keeps its own option", func(t *testing.T) {
		t.Parallel()

		client, operation := prepared(t)
		client.vm.Properties.StorageProfile.DataDisks = append(client.vm.Properties.StorageProfile.DataDisks, &armcompute.DataDisk{
			Lun:          new(int32(7)),
			Name:         new("late-disk"),
			CreateOption: new(armcompute.DiskCreateOptionTypesAttach),
			DeleteOption: new(armcompute.DiskDeleteOptionTypesDelete),
			ManagedDisk:  &armcompute.ManagedDiskParameters{ID: new(hostDiskID("late-disk"))},
		})

		poll := pollTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateFailed, poll.State)
		require.Equal(t, string(unboundedv1alpha3.FailureProviderConflict), poll.Reason)
		require.Zero(t, client.count("delete"))
		requirePreparationUndone(t, client)
		require.Equal(t, armcompute.DiskDeleteOptionTypesDelete, *client.vm.Properties.StorageProfile.DataDisks[2].DeleteOption)
	})

	t.Run("the failure is reported only once the undo is accepted", func(t *testing.T) {
		t.Parallel()

		client, operation := prepared(t)
		client.deleteErrs = []error{
			azureResponseError(http.StatusConflict, "ScopeLocked"),
			azureResponseError(http.StatusConflict, "ScopeLocked"),
		}
		client.updateErrs = []error{azureResponseError(http.StatusServiceUnavailable, "ServiceUnavailable")}

		_, err := newHostProvider(client).PollReplace(t.Context(), replaceRequest(), operation)
		requireTransient(t, err)
		require.True(t, preparedForDelete(client.vm))

		poll := pollTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateFailed, poll.State)
		requirePreparationUndone(t, client)
	})

	t.Run("Azure refuses the undo as well", func(t *testing.T) {
		t.Parallel()

		client, operation := prepared(t)
		client.deleteErrs = []error{azureResponseError(http.StatusConflict, "ScopeLocked")}
		client.updateErrs = []error{azureResponseError(http.StatusConflict, "ScopeLocked")}

		poll := pollTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateFailed, poll.State)
		require.Contains(t, poll.Message, "could not be restored")
	})
}

func TestReplaceOfAVMInAFailedProvisioningState(t *testing.T) {
	t.Parallel()

	// A failed VM is a common reason to replace it.
	vm := replaceableVM()
	vm.Properties.ProvisioningState = new("Failed")
	client := newFakeVMClient(vm)

	poll := finishTestReplace(t, client, beginTestReplace(t, client))
	require.Equal(t, machineops.ProviderOperationStateSucceeded, poll.State, poll.Message)
	require.Equal(t, []string{"update:vm1", "delete:vm1", "create:vm1"}, client.calls)
}

func TestBeginReplaceWaitsForAnOperationInProgress(t *testing.T) {
	t.Parallel()

	vm := replaceableVM()
	vm.Properties.ProvisioningState = new("Updating")
	client := newFakeVMClient(vm)

	_, err := newHostProvider(client).BeginReplace(t.Context(), replaceRequest())
	requireTransient(t, err)
	require.Empty(t, client.calls)
}

func TestReplaceRestoresDeleteOptionsRecordedByAnEarlierReplacement(t *testing.T) {
	t.Parallel()

	// An earlier replacement prepared the VM and then failed before the
	// delete: the VM's current options are the prepared ones.
	vm := replaceableVM()
	vm.Tags[TagReplacePending] = new("earlier-operation")
	vm.Tags[TagReplaceRestore] = new("os=Detach;nic=0;lun=0")
	vm = copyVM(new(prepareVMForReplacementDelete(*vm)))
	vm.ID = new(hostVMID())
	vm.Name = new(hostVMName)

	client := newFakeVMClient(vm)
	operation := beginTestReplace(t, client)

	poll := finishTestReplace(t, client, operation)
	require.Equal(t, machineops.ProviderOperationStateSucceeded, poll.State, poll.Message)

	require.Equal(t, string(hostOperationUID), *client.updates[0].Tags[TagReplacePending])
	require.Equal(t, "os=Detach;nic=0;lun=0", *client.updates[0].Tags[TagReplaceRestore])

	created := client.creates[0]
	require.Equal(t, armcompute.DiskDeleteOptionTypesDetach, *created.Properties.StorageProfile.OSDisk.DeleteOption)
	require.Equal(t, armcompute.DeleteOptionsDelete, *created.Properties.NetworkProfile.NetworkInterfaces[0].Properties.DeleteOption)
	require.Equal(t, armcompute.DiskDeleteOptionTypesDelete, *created.Properties.StorageProfile.DataDisks[0].DeleteOption)
	require.Equal(t, armcompute.DiskDeleteOptionTypesDetach, *created.Properties.StorageProfile.DataDisks[1].DeleteOption)
}

func TestReplaceRejectsUnsupportedVMBeforeChangingIt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*armcompute.VirtualMachine)
		wantErr string
	}{
		{
			name: "unmanaged data disk",
			mutate: func(vm *armcompute.VirtualMachine) {
				disk := vm.Properties.StorageProfile.DataDisks[1]
				disk.ManagedDisk = nil
				disk.Vhd = &armcompute.VirtualHardDisk{URI: new("https://account.blob.core.windows.net/vhds/data.vhd")}
			},
			wantErr: "LUN 3 is an unmanaged VHD",
		},
		{
			name:    "data disk without managed disk ID",
			mutate:  func(vm *armcompute.VirtualMachine) { vm.Properties.StorageProfile.DataDisks[0].ManagedDisk.ID = nil },
			wantErr: "LUN 0 has no managed disk ID",
		},
		{
			name: "data disk being detached",
			mutate: func(vm *armcompute.VirtualMachine) {
				vm.Properties.StorageProfile.DataDisks[1].ToBeDetached = new(true)
			},
			wantErr: "LUN 3 is being detached",
		},
		{
			name: "VM-managed network interfaces",
			mutate: func(vm *armcompute.VirtualMachine) {
				vm.Properties.NetworkProfile.NetworkInterfaceConfigurations = []*armcompute.VirtualMachineNetworkInterfaceConfiguration{{Name: new("vm1-nic")}}
			},
			wantErr: "network interface configurations",
		},
		{
			name:    "no network interface",
			mutate:  func(vm *armcompute.VirtualMachine) { vm.Properties.NetworkProfile.NetworkInterfaces = nil },
			wantErr: "no network interface",
		},
		{
			name: "unmanaged OS disk",
			mutate: func(vm *armcompute.VirtualMachine) {
				vm.Properties.StorageProfile.OSDisk.Vhd = &armcompute.VirtualHardDisk{URI: new("https://a/os.vhd")}
			},
			wantErr: "OS disk is an unmanaged VHD",
		},
		{
			name: "Windows",
			mutate: func(vm *armcompute.VirtualMachine) {
				vm.Properties.StorageProfile.OSDisk.OSType = new(armcompute.OperatingSystemTypesWindows)
			},
			wantErr: "Windows",
		},
		{
			name: "password authentication",
			mutate: func(vm *armcompute.VirtualMachine) {
				vm.Properties.OSProfile.LinuxConfiguration.DisablePasswordAuthentication = new(false)
			},
			wantErr: "password authentication",
		},
		{
			name:    "specialized VM without OS profile",
			mutate:  func(vm *armcompute.VirtualMachine) { vm.Properties.OSProfile = nil },
			wantErr: "no OS profile",
		},
		{
			name:    "no image and no host image",
			mutate:  func(vm *armcompute.VirtualMachine) { vm.Properties.StorageProfile.ImageReference = nil },
			wantErr: "not created from an image",
		},
		{
			name:    "no etag",
			mutate:  func(vm *armcompute.VirtualMachine) { vm.Tags["test-no-etag"] = new("true") },
			wantErr: "no etag",
		},
		{
			name:    "no unique VM ID",
			mutate:  func(vm *armcompute.VirtualMachine) { vm.Properties.VMID = nil },
			wantErr: "no unique ID",
		},
		{
			name: "tagged by an unfinished replacement and changed since",
			mutate: func(vm *armcompute.VirtualMachine) {
				vm.Tags[TagReplacePending] = new("earlier-operation")
				vm.Tags[TagReplaceRestore] = new("os=;nic=0;lun=0")
			},
			wantErr: "an unfinished replacement left it tagged",
		},
		{
			name: "malformed restore record",
			mutate: func(vm *armcompute.VirtualMachine) {
				leftPrepared(vm, "garbage")
			},
			wantErr: "malformed",
		},
		{
			name: "restore record names a NIC the VM does not have",
			mutate: func(vm *armcompute.VirtualMachine) {
				leftPrepared(vm, "os=;nic=4;lun=")
			},
			wantErr: "network interface 4",
		},
		{
			name: "restore record names a LUN the VM does not have",
			mutate: func(vm *armcompute.VirtualMachine) {
				leftPrepared(vm, "os=;nic=;lun=9")
			},
			wantErr: "LUN 9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			vm := replaceableVM()
			tt.mutate(vm)
			client := newFakeVMClient(vm)

			_, err := newHostProvider(client).BeginReplace(t.Context(), hostRequest(unboundedv1alpha3.OperationHostReplace))
			requirePermanentReason(t, err, reasonUnsupportedVM)
			require.ErrorContains(t, err, tt.wantErr)
			require.Empty(t, client.calls, "an unsupported VM must not be changed")
		})
	}
}

// leftPrepared puts the VM in the state an unfinished replacement leaves it
// in, with the given restore record.
func leftPrepared(vm *armcompute.VirtualMachine, restore string) {
	prepared := prepareVMForReplacementDelete(*vm)
	vm.Properties = prepared.Properties
	vm.Tags[TagReplacePending] = new("earlier-operation")
	vm.Tags[TagReplaceRestore] = new(restore)
}

func TestReplaceValidatesRequestBeforeReadingAzure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*machineops.OperationRequest)
		wantErr string
	}{
		{name: "missing cloud-init", mutate: func(r *machineops.OperationRequest) { r.ReplaceUserData = "" }, wantErr: "replacement user data is required"},
		{name: "oversized cloud-init", mutate: func(r *machineops.OperationRequest) {
			r.ReplaceUserData = strings.Repeat("a", azureCustomDataMaxBytes+1)
		}, wantErr: "customData limit"},
		{name: "missing operation UID", mutate: func(r *machineops.OperationRequest) { r.OperationUID = "" }, wantErr: "operation UID"},
		{name: "invalid host image", mutate: func(r *machineops.OperationRequest) { r.HostImage = "not-an-image" }, wantErr: "publisher:offer:sku:version"},
		{name: "invalid providerID", mutate: func(r *machineops.OperationRequest) { r.ProviderID = "aws:///i-123" }, wantErr: "providerID"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := newFakeVMClient(replaceableVM())
			request := hostRequest(unboundedv1alpha3.OperationHostReplace)
			tt.mutate(&request)

			_, err := newHostProvider(client).BeginReplace(t.Context(), request)
			requirePermanentReason(t, err, string(unboundedv1alpha3.FailureInvalidRequest))
			require.ErrorContains(t, err, tt.wantErr)
			require.Zero(t, client.gets)
		})
	}
}

func TestReplaceVMAbsent(t *testing.T) {
	t.Parallel()

	client := newFakeVMClient(nil)

	_, err := newHostProvider(client).BeginReplace(t.Context(), hostRequest(unboundedv1alpha3.OperationHostReplace))
	requirePermanentReason(t, err, reasonVMNotFound)
	require.Empty(t, client.calls)
}

func TestReplaceVMDeletedOutsideTheReplacement(t *testing.T) {
	t.Parallel()

	vm := replaceableVM()
	vm.Properties.ProvisioningState = new("Deleting")
	client := newFakeVMClient(vm)

	_, err := newHostProvider(client).BeginReplace(t.Context(), hostRequest(unboundedv1alpha3.OperationHostReplace))
	requirePermanentReason(t, err, string(unboundedv1alpha3.FailureProviderConflict))
	require.Empty(t, client.calls)
}

func TestReplaceAzureRejection(t *testing.T) {
	t.Parallel()

	t.Run("read refused for the credential", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())
		client.getErrs = []error{azureResponseError(http.StatusForbidden, "AuthorizationFailed")}

		_, err := newHostProvider(client).BeginReplace(t.Context(), replaceRequest())
		requirePermanentReason(t, err, string(unboundedv1alpha3.FailureAuthFailed))
		require.Empty(t, client.calls)
	})

	t.Run("preparation refused", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())
		operation := beginTestReplace(t, client)
		client.updateErrs = []error{azureResponseError(http.StatusConflict, "OperationNotAllowed")}

		poll := pollTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateFailed, poll.State)
		require.Equal(t, string(unboundedv1alpha3.FailureProviderFailed), poll.Reason)
		require.Equal(t, []string{"update:vm1"}, client.calls)
	})

	t.Run("preparation throttled", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())
		operation := beginTestReplace(t, client)
		client.updateErrs = []error{azureResponseError(http.StatusTooManyRequests, "TooManyRequests")}

		_, err := newHostProvider(client).PollReplace(t.Context(), replaceRequest(), operation)
		requireTransient(t, err)
		require.Zero(t, client.count("delete"))

		poll := finishTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateSucceeded, poll.State, poll.Message)
	})

	t.Run("create fails and is sent again by a later Poll", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())
		operation := beginTestReplace(t, client)
		client.createErrs = []error{
			azureResponseError(http.StatusInternalServerError, "InternalServerError"),
			azureResponseError(http.StatusServiceUnavailable, "ServiceUnavailable"),
		}

		pollUntilCall(t, client, operation, "delete", 1)
		client.settle()

		for range 2 {
			_, err := newHostProvider(client).PollReplace(t.Context(), replaceRequest(), operation)
			requireTransient(t, err)
			require.ErrorContains(t, err, "no longer exists and is not recreated yet")
		}

		poll := finishTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateSucceeded, poll.State, poll.Message)
		require.Equal(t, 3, client.count("create"))
	})

	t.Run("create refused after the delete", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())
		operation := beginTestReplace(t, client)
		client.createErrs = []error{azureResponseError(http.StatusConflict, "QuotaExceeded")}

		poll := finishTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateFailed, poll.State)
		require.Equal(t, string(unboundedv1alpha3.FailureQuotaExceeded), poll.Reason)
		require.Contains(t, poll.Message, "recreate the VM manually")
	})
}

func TestReplaceNeedsTheCloudInitInPoll(t *testing.T) {
	t.Parallel()

	withoutCloudInit := replaceRequest()
	withoutCloudInit.ReplaceUserData = ""

	t.Run("the VM is not deleted without it", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())
		operation := beginTestReplace(t, client)

		pollUntilCall(t, client, operation, "update", 1)
		client.settle()

		// The engine could not build it this time: retried.
		_, err := newHostProvider(client).PollReplace(t.Context(), withoutCloudInit, operation)
		requireTransient(t, err)
		require.Zero(t, client.count("delete"))

		require.NotNil(t, client.vm)
	})

	t.Run("after the delete the create is retried", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())
		operation := beginTestReplace(t, client)

		pollUntilCall(t, client, operation, "delete", 1)
		client.settle()

		_, err := newHostProvider(client).PollReplace(t.Context(), withoutCloudInit, operation)
		requireTransient(t, err)
		require.Zero(t, client.count("create"))

		poll := finishTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateSucceeded, poll.State, poll.Message)
	})
}

func TestReplaceNeverPersistsTheCloudInit(t *testing.T) {
	t.Parallel()

	client := newFakeVMClient(replaceableVM())

	begin, err := newHostProvider(client).BeginReplace(t.Context(), replaceRequest())
	require.NoError(t, err)
	require.NotContains(t, begin.Message, "bootstrap-token")

	// The handle is all the engine stores: the fields below and nothing else.
	var fields map[string]string
	require.NoError(t, json.Unmarshal([]byte(begin.Operation.ResumeToken), &fields))
	require.ElementsMatch(t, []string{"originalVMID", "originalOSDiskID", "model"}, slices.Collect(maps.Keys(fields)))
	require.Equal(t, "vm-guid-1", fields["originalVMID"])
	require.Equal(t, hostDiskID("vm1-osdisk"), fields["originalOSDiskID"])

	// Its model is the replacement without any cloud-init.
	_, model, err := parseReplaceHandle(begin.Operation.ResumeToken)
	require.NoError(t, err)
	require.Nil(t, model.Properties.OSProfile.CustomData)
	require.Nil(t, model.Properties.UserData)

	rawModel, err := json.Marshal(model)
	require.NoError(t, err)
	require.NotContains(t, string(rawModel), "bootstrap-token")

	poll := finishTestReplace(t, client, begin.Operation)
	require.Equal(t, machineops.ProviderOperationStateSucceeded, poll.State, poll.Message)

	encoded := base64.StdEncoding.EncodeToString([]byte(hostUserData))

	for _, update := range client.updates {
		raw, err := json.Marshal(update)
		require.NoError(t, err)
		require.NotContains(t, string(raw), encoded)
		require.NotContains(t, string(raw), "bootstrap-token")
	}

	for _, tag := range client.creates[0].Tags {
		require.NotContains(t, *tag, "bootstrap-token")
	}
}

func TestPollReplaceObservesTheReplacement(t *testing.T) {
	t.Parallel()

	// created returns a client whose replacement VM Azure is creating.
	created := func(t *testing.T) (*fakeVMClient, machineops.ProviderOperation) {
		t.Helper()

		client := newFakeVMClient(replaceableVM())
		operation := beginTestReplace(t, client)
		pollUntilCall(t, client, operation, "create", 1)

		return client, operation
	}

	t.Run("provisioning failed", func(t *testing.T) {
		t.Parallel()

		client, operation := created(t)
		client.pending = nil
		client.vm.Properties.ProvisioningState = new("Failed")
		client.failCode = "SkuNotAvailable"

		poll := pollTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateFailed, poll.State)
		require.Equal(t, string(unboundedv1alpha3.FailureInsufficientCapacity), poll.Reason)
	})

	t.Run("replacement is being deleted", func(t *testing.T) {
		t.Parallel()

		client, operation := created(t)
		client.pending = nil
		client.vm.Properties.ProvisioningState = new("Deleting")

		poll := pollTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateFailed, poll.State)
		require.Equal(t, reasonReplacementLost, poll.Reason)
		require.Equal(t, 1, client.count("create"))
	})

	t.Run("replacement vanishes while its failure is read", func(t *testing.T) {
		t.Parallel()

		client, operation := created(t)
		client.pending = nil
		client.vm.Properties.ProvisioningState = new("Failed")
		client.vanishAfterGets = 1

		poll := pollTestReplace(t, client, operation)
		require.Equal(t, machineops.ProviderOperationStateFailed, poll.State)
		require.Equal(t, reasonReplacementLost, poll.Reason)
	})

	t.Run("read failure is retried", func(t *testing.T) {
		t.Parallel()

		client, operation := created(t)
		client.getErrs = []error{azureResponseError(http.StatusServiceUnavailable, "ServiceUnavailable")}

		_, err := newHostProvider(client).PollReplace(t.Context(), replaceRequest(), operation)
		requireTransient(t, err)
	})
}

func TestPollReplaceRejectsAHandleItCannotUse(t *testing.T) {
	t.Parallel()

	valid := beginTestReplace(t, newFakeVMClient(replaceableVM()))

	var handle replaceHandle
	require.NoError(t, json.Unmarshal([]byte(valid.ResumeToken), &handle))

	token := func(mutate func(*replaceHandle)) string {
		mutated := handle
		mutate(&mutated)

		raw, err := json.Marshal(mutated)
		require.NoError(t, err)

		return string(raw)
	}

	tests := []struct {
		name      string
		operation machineops.ProviderOperation
		noUID     bool
		wantErr   string
	}{
		{name: "no operation UID", operation: valid, noUID: true, wantErr: "operation UID"},
		{name: "not JSON", operation: machineops.ProviderOperation{OperationID: hostVMID(), ResumeToken: "{"}, wantErr: "parse HostReplace resume token"},
		{name: "empty", operation: machineops.ProviderOperation{OperationID: hostVMID()}, wantErr: "parse HostReplace resume token"},
		{name: "no model", operation: machineops.ProviderOperation{OperationID: hostVMID(), ResumeToken: "{}"}, wantErr: "names no VM model"},
		{
			name:      "model is not base64",
			operation: machineops.ProviderOperation{OperationID: hostVMID(), ResumeToken: token(func(h *replaceHandle) { h.Model = "!" })},
			wantErr:   "decode HostReplace VM model",
		},
		{
			name: "model is not compressed",
			operation: machineops.ProviderOperation{OperationID: hostVMID(), ResumeToken: token(func(h *replaceHandle) {
				h.Model = base64.StdEncoding.EncodeToString([]byte("{}"))
			})},
			wantErr: "decompress HostReplace VM model",
		},
		{
			name:      "another VM",
			operation: machineops.ProviderOperation{OperationID: hostVMID() + "-other", ResumeToken: valid.ResumeToken},
			wantErr:   "does not address VM",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := newFakeVMClient(replaceableVM())

			request := replaceRequest()
			if tt.noUID {
				request.OperationUID = ""
			}

			_, err := newHostProvider(client).PollReplace(t.Context(), request, tt.operation)
			requirePermanentReason(t, err, string(unboundedv1alpha3.FailureInvalidRequest))
			require.ErrorContains(t, err, tt.wantErr)
			require.Zero(t, client.gets)
		})
	}
}

// TestReplaceRefusesAModelTooLargeToRecord covers the bound on what the
// handle adds to the MachineOperation status. Azure's own limits on a VM keep
// a real model far below it.
func TestReplaceRefusesAModelTooLargeToRecord(t *testing.T) {
	t.Parallel()

	vm := replaceableVM()

	for i := range 2000 {
		value := make([]byte, 192)
		_, err := rand.Read(value)
		require.NoError(t, err)

		vm.Tags[fmt.Sprintf("tag-%d", i)] = new(base64.StdEncoding.EncodeToString(value))
	}

	client := newFakeVMClient(vm)

	_, err := newHostProvider(client).BeginReplace(t.Context(), replaceRequest())
	requirePermanentReason(t, err, reasonUnsupportedVM)
	require.ErrorContains(t, err, "too large")
	require.Empty(t, client.calls)
}

func TestReplaceRestoreRoundTrip(t *testing.T) {
	t.Parallel()

	restore := replaceRestore{
		osDisk:    new(armcompute.DiskDeleteOptionTypesDelete),
		nicDelete: []int{0, 2},
		lunDelete: []int32{1, 63},
	}

	parsed, err := parseReplaceRestore(restore.encode())
	require.NoError(t, err)
	require.Equal(t, restore, parsed)

	parsed, err = parseReplaceRestore("os=;nic=;lun=")
	require.NoError(t, err)
	require.Equal(t, replaceRestore{}, parsed)

	for _, malformed := range []string{"", "os=Delete", "os=Bogus;nic=;lun=", "os=;nic=x;lun=", "os=;nic=;lun=-1", "os=;nic=;lun=;extra="} {
		_, err := parseReplaceRestore(malformed)
		require.Error(t, err, malformed)
	}
}

func TestReplacementOSDiskNameFitsAzureLimit(t *testing.T) {
	t.Parallel()

	name := replacementOSDiskName(strings.Repeat("v", 70), hostOperationUID)
	require.Len(t, name, azureDiskNameMaxLength)
	require.True(t, strings.HasSuffix(name, "-osdisk-0b5c7d2e"))
	require.Equal(t, "vm1-osdisk-0b5c7d2e", replacementOSDiskName("vm1", hostOperationUID))
}
