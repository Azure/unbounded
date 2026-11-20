// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
)

// Azure power states as reported by the VM instance view, without the
// "PowerState/" prefix and in lower case.
const (
	powerStateRunning      = "running"
	powerStateStarting     = "starting"
	powerStateStopped      = "stopped"
	powerStateStopping     = "stopping"
	powerStateDeallocated  = "deallocated"
	powerStateDeallocating = "deallocating"
)

// evidenceClockSkew bounds how far the controller clock and the Azure clock
// may disagree when Poll compares the time Azure completed an operation on
// the VM with the time Azure accepted this operation's request.
const evidenceClockSkew = 30 * time.Second

// powerOperation describes one host power operation. Begin sends its request
// without waiting for Azure to finish; Poll decides completion only by
// observing the VM's power and provisioning state, so a restarted controller
// resumes from the persisted handle without any poller held in memory.
//
// Restart guarantees:
//   - HostPowerOn and HostPowerOff converge: Begin sends nothing when the VM
//     is already in, or moving to, the requested power state, so a Begin
//     replayed after a restart never sends a second request for a transition
//     Azure already started. Both requests are idempotent in Azure anyway.
//   - HostHardReboot is at least once. A restart cannot be told apart from
//     the steady state it ends in, so a replayed Begin restarts the VM again.
//     A second reboot happens only when the controller stops, or loses the
//     status write, after Azure accepted the restart and before the engine
//     persisted the handle. Skipping the restart on a guess instead could
//     report success for a host that was never rebooted.
type powerOperation struct {
	// action names the Azure request in messages.
	action string
	// targets are the power states that complete the operation.
	targets []string
	// inFlight are the power states in which Azure is moving the VM toward a
	// target.
	inFlight []string
	// convergent operations send nothing when the VM is already in a target
	// or in-flight state.
	convergent bool
	// send is an azureVMClient method expression.
	send func(client azureVMClient, ctx context.Context, resourceGroup, name string) error
}

var powerOperations = map[unboundedv1alpha3.OperationKind]powerOperation{
	unboundedv1alpha3.OperationHostPowerOn: {
		action:     "start",
		targets:    []string{powerStateRunning},
		inFlight:   []string{powerStateStarting},
		convergent: true,
		send:       azureVMClient.StartVM,
	},
	unboundedv1alpha3.OperationHostPowerOff: {
		action:     "power off",
		targets:    []string{powerStateStopped, powerStateDeallocated},
		inFlight:   []string{powerStateStopping, powerStateDeallocating},
		convergent: true,
		send:       azureVMClient.PowerOffVM,
	},
	unboundedv1alpha3.OperationHostHardReboot: {
		action:   "restart",
		targets:  []string{powerStateRunning},
		inFlight: []string{powerStateStarting},
		send:     azureVMClient.RestartVM,
	},
}

func powerOperationFor(kind unboundedv1alpha3.OperationKind) (powerOperation, error) {
	operation, ok := powerOperations[kind]
	if !ok {
		return powerOperation{}, invalidRequest(fmt.Errorf("unsupported Azure VM power operation %q", kind))
	}

	return operation, nil
}

// BeginPowerOperation sends the HostPowerOn, HostPowerOff or HostHardReboot
// request and returns once Azure accepted it. See powerOperation for what a
// replayed Begin does.
//
// A VM that does not exist, a read Azure refuses for the credential and a
// request Azure definitively refuses end the operation; every other failure
// is retried by the engine with a new Begin.
func (p *Provider) BeginPowerOperation(ctx context.Context, request machineops.OperationRequest) (machineops.BeginResult, error) {
	operation, err := powerOperationFor(request.Operation)
	if err != nil {
		return machineops.BeginResult{}, err
	}

	target, err := hostTarget(request, nil)
	if err != nil {
		return machineops.BeginResult{}, invalidRequest(err)
	}

	client, err := p.client(target.SubscriptionID, request.Auth)
	if err != nil {
		return machineops.BeginResult{}, fmt.Errorf("create Azure VM client: %w", err)
	}

	vm, err := client.GetVM(ctx, target.ResourceGroup, target.VMName, true)
	if err != nil {
		return machineops.BeginResult{}, beforeCreate(err)
	}

	if vm == nil {
		return machineops.BeginResult{}, vmNotFound(target, nil)
	}

	if strings.EqualFold(provisioningState(vm), "Deleting") {
		return machineops.BeginResult{}, providerConflict(fmt.Errorf("VM %s is being deleted", target.VMName))
	}

	if power := powerState(vm); operation.convergent && (slices.Contains(operation.targets, power) || slices.Contains(operation.inFlight, power)) {
		return hostBeginResult(target, hostHandle{}, fmt.Sprintf("VM %s is already %s", target.VMName, power))
	}

	// Azure refuses a power request while it is changing the VM, and that
	// refusal would end the operation. Wait instead: the engine calls Begin
	// again.
	if state := strings.ToLower(provisioningState(vm)); isSettling(state) {
		return machineops.BeginResult{}, fmt.Errorf("VM %s is %s; the %s is sent when that is done", target.VMName, state, operation.action)
	}

	if err := operation.send(client, ctx, target.ResourceGroup, target.VMName); err != nil {
		return machineops.BeginResult{}, classifyActionError(target, operation.action, err)
	}

	acceptedAt := p.now().UTC()

	return hostBeginResult(target, hostHandle{AcceptedAt: &acceptedAt}, fmt.Sprintf("Azure accepted the %s of VM %s", operation.action, target.VMName))
}

// PollPowerOperation observes the VM's instance view. Read failures are
// retried with the same handle; a VM that disappeared or an Azure operation
// that failed after the request was accepted ends the operation.
func (p *Provider) PollPowerOperation(
	ctx context.Context,
	request machineops.OperationRequest,
	providerOperation machineops.ProviderOperation,
) (machineops.PollResult, error) {
	operation, err := powerOperationFor(request.Operation)
	if err != nil {
		return machineops.PollResult{}, err
	}

	target, err := hostTarget(request, &providerOperation)
	if err != nil {
		return machineops.PollResult{}, invalidRequest(err)
	}

	handle, err := parseHostHandle(providerOperation.ResumeToken)
	if err != nil {
		return machineops.PollResult{}, invalidRequest(err)
	}

	client, err := p.client(target.SubscriptionID, request.Auth)
	if err != nil {
		return machineops.PollResult{}, fmt.Errorf("create Azure VM client: %w", err)
	}

	vm, err := client.GetVM(ctx, target.ResourceGroup, target.VMName, true)
	if err != nil {
		return machineops.PollResult{}, err
	}

	if vm == nil {
		return hostFailed(reasonVMNotFound, fmt.Sprintf("VM %s no longer exists", target.VMName)), nil
	}

	return observePower(operation, vm, handle, target), nil
}

// observePower decides a power operation's state from one instance view.
//
// Azure reports when it completed the latest operation on the VM. That time
// is the evidence that an observed state is the result of this operation
// rather than the state the VM was in before Azure started acting on the
// request. HostHardReboot needs it to tell a restarted VM from one that has
// not restarted yet; every operation needs it before it treats a failed or
// unexpected state as its own outcome.
func observePower(operation powerOperation, vm *armcompute.VirtualMachine, handle hostHandle, target azureVMResourceRef) machineops.PollResult {
	completedAfter, unknown := completionEvidence(vm, handle)
	power := powerState(vm)

	state := strings.ToLower(provisioningState(vm))
	failed := state == "failed" || state == "canceled"
	evidence := completedAfter || unknown

	failure := func() machineops.PollResult {
		code, message := provisioningFailure(vm)

		return hostFailed(string(classifyAzureCode(code, message)), fmt.Sprintf("%s of VM %s failed: %s %s", operation.action, target.VMName, valueOrDefault(code, state), message))
	}

	switch {
	case state == "deleting":
		return hostFailed(reasonVMNotFound, fmt.Sprintf("VM %s is being deleted", target.VMName))
	case isSettling(state):
		return hostInProgress(fmt.Sprintf("waiting for the %s of VM %s: provisioning state is %s", operation.action, target.VMName, state))
	case failed && !operation.convergent && !evidence:
		return hostInProgress(fmt.Sprintf("waiting for Azure to start the %s of VM %s", operation.action, target.VMName))
	case failed && !operation.convergent:
		return failure()
	case slices.Contains(operation.inFlight, power):
		return hostInProgress(fmt.Sprintf("waiting for the %s of VM %s: power state is %s", operation.action, target.VMName, power))
	}

	// A convergent operation is judged by the power state it asked for. A
	// failed provisioning state does not fail it while the VM is in that
	// state: the failure may be an older one, such as a failed extension.
	reached := slices.Contains(operation.targets, power)

	switch {
	case reached && (operation.convergent || evidence):
		return machineops.PollResult{
			State:   machineops.ProviderOperationStateSucceeded,
			Message: fmt.Sprintf("VM %s is %s", target.VMName, power),
		}
	case !reached && failed && evidence:
		return failure()
	case !reached && completedAfter:
		return hostFailed(string(unboundedv1alpha3.FailureProviderFailed), fmt.Sprintf("VM %s power state is %s after Azure completed the %s", target.VMName, valueOrDefault(power, "unknown"), operation.action))
	default:
		return hostInProgress(fmt.Sprintf("waiting for Azure to complete the %s of VM %s: power state is %s", operation.action, target.VMName, valueOrDefault(power, "unknown")))
	}
}

// completionEvidence reports whether Azure completed an operation on the VM
// after it accepted this operation's request, and whether that is unknown
// because the instance view carries no completion time. A handle without an
// acceptance time comes from a Begin that sent nothing because the VM was
// already in, or moving to, the requested state; there is no request to wait
// for, so every observation counts.
func completionEvidence(vm *armcompute.VirtualMachine, handle hostHandle) (bool, bool) {
	if handle.AcceptedAt == nil {
		return true, false
	}

	completed := provisioningStatusTime(vm)
	if completed == nil {
		return false, true
	}

	return !completed.Before(handle.AcceptedAt.Add(-evidenceClockSkew)), false
}

// powerState returns the VM's power state from its instance view, such as
// "running", or "" when the instance view does not report one.
func powerState(vm *armcompute.VirtualMachine) string {
	status := instanceViewStatus(vm, "PowerState/")
	if status == nil {
		return ""
	}

	code, _ := strings.CutPrefix(*status.Code, "PowerState/")

	return strings.ToLower(code)
}

// provisioningStatusTime is when Azure completed the latest operation on the
// VM, or nil when the instance view does not say.
func provisioningStatusTime(vm *armcompute.VirtualMachine) *time.Time {
	status := instanceViewStatus(vm, "ProvisioningState/")
	if status == nil {
		return nil
	}

	return status.Time
}

func instanceViewStatus(vm *armcompute.VirtualMachine, prefix string) *armcompute.InstanceViewStatus {
	if vm == nil || vm.Properties == nil || vm.Properties.InstanceView == nil {
		return nil
	}

	for _, status := range vm.Properties.InstanceView.Statuses {
		if status != nil && status.Code != nil && strings.HasPrefix(*status.Code, prefix) {
			return status
		}
	}

	return nil
}

func hostInProgress(message string) machineops.PollResult {
	return machineops.PollResult{
		State:        machineops.ProviderOperationStateInProgress,
		Message:      message,
		RequeueAfter: pollInterval,
	}
}

func hostFailed(reason, message string) machineops.PollResult {
	return machineops.PollResult{
		State:   machineops.ProviderOperationStateFailed,
		Reason:  reason,
		Message: message,
	}
}
