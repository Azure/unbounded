// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"net/http"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/stretchr/testify/require"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
)

func TestPowerOperationsSendOnceAndObserveCompletion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind      unboundedv1alpha3.OperationKind
		initial   string
		call      string
		wantPower string
	}{
		{kind: unboundedv1alpha3.OperationHostPowerOn, initial: powerStateDeallocated, call: "start:vm1", wantPower: powerStateRunning},
		{kind: unboundedv1alpha3.OperationHostPowerOff, initial: powerStateRunning, call: "powerOff:vm1", wantPower: powerStateStopped},
		{kind: unboundedv1alpha3.OperationHostHardReboot, initial: powerStateRunning, call: "restart:vm1", wantPower: powerStateRunning},
	}

	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			t.Parallel()

			client := newFakeVMClient(replaceableVM())
			client.power = tt.initial

			begin, err := newHostProvider(client).BeginPowerOperation(t.Context(), hostRequest(tt.kind))
			require.NoError(t, err)
			require.Equal(t, []string{tt.call}, client.calls)
			require.Equal(t, hostVMID(), begin.Operation.OperationID)
			require.Positive(t, begin.RequeueAfter)

			// Each Poll comes from a new Provider holding only the persisted
			// handle, as after a controller restart.
			poll, err := newHostProvider(client).PollPowerOperation(t.Context(), hostRequest(tt.kind), begin.Operation)
			require.NoError(t, err)
			require.Equal(t, machineops.ProviderOperationStateInProgress, poll.State)

			client.settle()

			poll, err = newHostProvider(client).PollPowerOperation(t.Context(), hostRequest(tt.kind), begin.Operation)
			require.NoError(t, err)
			require.Equal(t, machineops.ProviderOperationStateSucceeded, poll.State, poll.Message)
			require.Contains(t, poll.Message, tt.wantPower)
			require.Len(t, client.calls, 1, "Poll must never send a request")
		})
	}
}

func TestPowerOperationBeginReplay(t *testing.T) {
	t.Parallel()

	t.Run("power off replay while stopping sends nothing", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())
		provider := newHostProvider(client)

		_, err := provider.BeginPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOff))
		require.NoError(t, err)

		// The handle was never persisted, so the engine calls Begin again on
		// a restarted controller.
		replayed, err := newHostProvider(client).BeginPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOff))
		require.NoError(t, err)
		require.Equal(t, []string{"powerOff:vm1"}, client.calls)
		require.Contains(t, replayed.Message, "already stopping")

		client.settle()

		poll, err := newHostProvider(client).PollPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOff), replayed.Operation)
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateSucceeded, poll.State)
	})

	t.Run("power on of a running VM sends nothing", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())

		begin, err := newHostProvider(client).BeginPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOn))
		require.NoError(t, err)
		require.Empty(t, client.calls)

		poll, err := newHostProvider(client).PollPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOn), begin.Operation)
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateSucceeded, poll.State)
	})

	t.Run("power on of a running VM ignores an older failure", func(t *testing.T) {
		t.Parallel()

		// The VM runs, and an earlier operation on it, such as an extension
		// update, failed. Nothing is sent, so that failure is not this
		// operation's.
		client := newFakeVMClient(replaceableVM())
		client.vm.Properties.ProvisioningState = new("Failed")
		client.failCode = "VMExtensionProvisioningError"

		begin, err := newHostProvider(client).BeginPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOn))
		require.NoError(t, err)
		require.Empty(t, client.calls)

		poll, err := newHostProvider(client).PollPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOn), begin.Operation)
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateSucceeded, poll.State, poll.Message)
	})

	t.Run("a request waits while Azure is changing the VM", func(t *testing.T) {
		t.Parallel()

		// Azure would refuse the request, and that refusal would end the
		// operation.
		client := newFakeVMClient(replaceableVM())
		client.vm.Properties.ProvisioningState = new("Updating")

		_, err := newHostProvider(client).BeginPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostHardReboot))
		requireTransient(t, err)
		require.Empty(t, client.calls)
	})

	t.Run("hard reboot replay restarts again", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())

		_, err := newHostProvider(client).BeginPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostHardReboot))
		require.NoError(t, err)

		client.settle()

		// A restart that already finished cannot be told apart from a VM
		// that was never restarted: the documented at-least-once guarantee.
		_, err = newHostProvider(client).BeginPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostHardReboot))
		require.NoError(t, err)
		require.Equal(t, []string{"restart:vm1", "restart:vm1"}, client.calls)
	})
}

func TestPollHardRebootWaitsForEvidenceOfTheRestart(t *testing.T) {
	t.Parallel()

	client := newFakeVMClient(replaceableVM())
	provider := newHostProvider(client)

	begin, err := provider.BeginPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostHardReboot))
	require.NoError(t, err)

	// Azure has not started the restart yet: the VM still reports the state
	// and completion time from before the request.
	client.pending = nil
	client.vm.Properties.ProvisioningState = new("Succeeded")
	client.power = powerStateRunning

	poll, err := provider.PollPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostHardReboot), begin.Operation)
	require.NoError(t, err)
	require.Equal(t, machineops.ProviderOperationStateInProgress, poll.State)

	client.statusTime = client.clock.Add(time.Second)

	poll, err = provider.PollPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostHardReboot), begin.Operation)
	require.NoError(t, err)
	require.Equal(t, machineops.ProviderOperationStateSucceeded, poll.State)
}

func TestPowerOperationVMAbsent(t *testing.T) {
	t.Parallel()

	client := newFakeVMClient(nil)
	provider := newHostProvider(client)

	_, err := provider.BeginPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOn))
	requirePermanentReason(t, err, reasonVMNotFound)
	require.Empty(t, client.calls)

	poll, err := provider.PollPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOn), machineops.ProviderOperation{OperationID: hostVMID()})
	require.NoError(t, err)
	require.Equal(t, machineops.ProviderOperationStateFailed, poll.State)
	require.Equal(t, reasonVMNotFound, poll.Reason)
}

func TestPowerOperationAzureRejection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantReason string
	}{
		{name: "operation not allowed", err: azureResponseError(http.StatusConflict, "OperationNotAllowed"), wantReason: string(unboundedv1alpha3.FailureProviderFailed)},
		{name: "allocation failed", err: azureResponseError(http.StatusConflict, "AllocationFailed"), wantReason: string(unboundedv1alpha3.FailureInsufficientCapacity)},
		{name: "forbidden", err: azureResponseError(http.StatusForbidden, "AuthorizationFailed"), wantReason: string(unboundedv1alpha3.FailureAuthFailed)},
		{name: "VM vanished", err: azureResponseError(http.StatusNotFound, "ResourceNotFound"), wantReason: reasonVMNotFound},
		{name: "operation in progress", err: azureResponseError(http.StatusConflict, "Conflict")},
		{name: "throttled", err: azureResponseError(http.StatusTooManyRequests, "TooManyRequests")},
		{name: "server error", err: azureResponseError(http.StatusInternalServerError, "InternalServerError")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := newFakeVMClient(replaceableVM())
			client.power = powerStateStopped
			client.actionErrs = []error{tt.err}

			_, err := newHostProvider(client).BeginPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOn))
			if tt.wantReason == "" {
				requireTransient(t, err)
				return
			}

			requirePermanentReason(t, err, tt.wantReason)
		})
	}
}

func TestPowerOperationBeginReadFailures(t *testing.T) {
	t.Parallel()

	client := newFakeVMClient(replaceableVM())
	client.getErrs = []error{
		azureResponseError(http.StatusForbidden, "AuthorizationFailed"),
		&azcore.ResponseError{StatusCode: http.StatusServiceUnavailable},
	}
	provider := newHostProvider(client)

	_, err := provider.BeginPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOff))
	requirePermanentReason(t, err, string(unboundedv1alpha3.FailureAuthFailed))

	_, err = provider.BeginPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOff))
	requireTransient(t, err)
	require.Empty(t, client.calls)
}

func TestPollPowerOperationFailures(t *testing.T) {
	t.Parallel()

	t.Run("provisioning failed after the request", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())
		client.power = powerStateDeallocated
		provider := newHostProvider(client)

		begin, err := provider.BeginPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOn))
		require.NoError(t, err)

		client.pending = nil
		client.vm.Properties.ProvisioningState = new("Failed")
		client.failCode = "AllocationFailed"
		client.power = powerStateDeallocated
		client.statusTime = client.clock

		poll, err := provider.PollPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOn), begin.Operation)
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateFailed, poll.State)
		require.Equal(t, string(unboundedv1alpha3.FailureInsufficientCapacity), poll.Reason)
		require.Contains(t, poll.Message, "AllocationFailed")
	})

	t.Run("stale failure is not this operation's", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())
		client.power = powerStateDeallocated
		provider := newHostProvider(client)

		begin, err := provider.BeginPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOn))
		require.NoError(t, err)

		client.pending = nil
		client.vm.Properties.ProvisioningState = new("Failed")
		client.failCode = "AllocationFailed"

		poll, err := provider.PollPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOn), begin.Operation)
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateInProgress, poll.State)
	})

	t.Run("power off completed but VM runs", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())
		provider := newHostProvider(client)

		begin, err := provider.BeginPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOff))
		require.NoError(t, err)

		client.pending = nil
		client.vm.Properties.ProvisioningState = new("Succeeded")
		client.power = powerStateRunning
		client.statusTime = client.clock.Add(time.Second)

		poll, err := provider.PollPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOff), begin.Operation)
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateFailed, poll.State)
		require.Contains(t, poll.Message, "power state is running")
	})

	t.Run("VM being deleted", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())
		client.vm.Properties.ProvisioningState = new("Deleting")

		_, err := newHostProvider(client).BeginPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOff))
		requirePermanentReason(t, err, string(unboundedv1alpha3.FailureProviderConflict))

		poll, err := newHostProvider(client).PollPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOff), machineops.ProviderOperation{OperationID: hostVMID()})
		require.NoError(t, err)
		require.Equal(t, machineops.ProviderOperationStateFailed, poll.State)
	})

	t.Run("handle for another VM", func(t *testing.T) {
		t.Parallel()

		client := newFakeVMClient(replaceableVM())

		_, err := newHostProvider(client).PollPowerOperation(t.Context(), hostRequest(unboundedv1alpha3.OperationHostPowerOff), machineops.ProviderOperation{
			OperationID: vmResourceID(hostSubscription, hostResourceGroup, "other"),
		})
		requirePermanentReason(t, err, string(unboundedv1alpha3.FailureInvalidRequest))
		require.Zero(t, client.gets)
	})
}

func TestPowerStateReadsInstanceView(t *testing.T) {
	t.Parallel()

	vm := &armcompute.VirtualMachine{Properties: &armcompute.VirtualMachineProperties{InstanceView: &armcompute.VirtualMachineInstanceView{
		Statuses: []*armcompute.InstanceViewStatus{
			{Code: new("ProvisioningState/succeeded"), Time: new(testNow)},
			{Code: new("PowerState/Deallocated")},
		},
	}}}

	require.Equal(t, powerStateDeallocated, powerState(vm))
	require.Equal(t, testNow, *provisioningStatusTime(vm))
	require.Empty(t, powerState(&armcompute.VirtualMachine{}))
	require.Nil(t, provisioningStatusTime(&armcompute.VirtualMachine{}))
}

// Before anything is sent, a credential that cannot be built ends a host
// operation, as it ends an allocation. Once a handle exists it is retried,
// so an operator can repair the credential and Poll resumes the operation.
func TestHostOperationsAndTheCredentialThatCannotBeBuilt(t *testing.T) {
	t.Parallel()

	provider := &Provider{}

	power := hostRequest(unboundedv1alpha3.OperationHostPowerOff)
	power.Auth = authWithoutClientSecret()

	_, err := provider.BeginPowerOperation(t.Context(), power)
	requirePermanentReason(t, err, string(unboundedv1alpha3.FailureAuthFailed))
	require.ErrorContains(t, err, "clientSecret")

	_, err = provider.PollPowerOperation(t.Context(), power, machineops.ProviderOperation{OperationID: hostVMID()})
	requireTransient(t, err)

	replace := replaceRequest()
	replace.Auth = authWithoutClientSecret()

	_, err = provider.BeginReplace(t.Context(), replace)
	requirePermanentReason(t, err, string(unboundedv1alpha3.FailureAuthFailed))
	require.ErrorContains(t, err, "clientSecret")

	operation := beginTestReplace(t, newFakeVMClient(replaceableVM()))

	_, err = provider.PollReplace(t.Context(), replace, operation)
	requireTransient(t, err)
}
