// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
)

// BeginRelease starts deleting what the allocation recorded as owned. See
// releaseStep.
func (p *Provider) BeginRelease(ctx context.Context, request machineops.OperationRequest) (machineops.BeginResult, error) {
	target, err := allocationTarget(request)
	if err != nil {
		return machineops.BeginResult{}, invalidRequest(err)
	}

	client, err := p.capacityClient(target.subscriptionID, request.Auth)
	if err != nil {
		return machineops.BeginResult{}, fmt.Errorf("create Azure capacity client: %w", err)
	}

	step, err := releaseStep(ctx, client, target, request.Allocation.OwnedResources)
	if err != nil {
		return machineops.BeginResult{}, err
	}

	if step.conflict != nil {
		return machineops.BeginResult{}, providerConflict(step.conflict)
	}

	return beginResult(target, step.message), nil
}

// PollRelease repeats releaseStep until nothing the allocation recorded
// exists. The engine never begins a second HostRelease for an allocation, so
// Poll reissues a delete that Azure dropped instead of failing the operation.
// Only a resource that cannot be proved to be the recorded one fails the
// release; Azure errors are retried with the same handle.
func (p *Provider) PollRelease(
	ctx context.Context,
	request machineops.OperationRequest,
	_ machineops.ProviderOperation,
) (machineops.PollResult, error) {
	target, err := allocationTarget(request)
	if err != nil {
		return machineops.PollResult{}, invalidRequest(err)
	}

	client, err := p.capacityClient(target.subscriptionID, request.Auth)
	if err != nil {
		return machineops.PollResult{}, fmt.Errorf("create Azure capacity client: %w", err)
	}

	step, err := releaseStep(ctx, client, target, request.Allocation.OwnedResources)
	if err != nil {
		return machineops.PollResult{}, err
	}

	switch {
	case step.conflict != nil:
		return failedPoll(unboundedv1alpha3.FailureProviderConflict, step.conflict.Error()), nil
	case step.done:
		return machineops.PollResult{
			State:   machineops.ProviderOperationStateSucceeded,
			Message: step.message,
		}, nil
	default:
		return machineops.PollResult{
			State:        machineops.ProviderOperationStateInProgress,
			Message:      step.message,
			RequeueAfter: pollInterval,
		}, nil
	}
}

// releaseOutcome is one observation of a release.
type releaseOutcome struct {
	// done reports that nothing the allocation recorded exists any more.
	done bool
	// conflict is why release refuses to continue: a present resource is not
	// provably the one the allocation recorded.
	conflict error
	message  string
}

// releaseStep observes the recorded resources and issues the deletes that
// are still needed. It is idempotent, so Begin and every Poll run it.
//
// Release acts only on what the allocation recorded as owned. An allocation
// that recorded nothing created nothing, so its release is complete without
// touching Azure, even when a foreign VM has the allocation's name. The VM is
// deleted only when it carries the allocation's UID tag and the recorded VM
// ID, and only when every child it references was recorded: deleting the VM
// also deletes the NIC and OS disk it references. A NIC or disk left behind
// after the VM is gone is deleted only when its provider-native ID matches
// the recorded one; anything else fails closed.
//
// Once the VM is gone, release reads every recorded child and also looks for
// what the VM create may have made without it being recorded: the OS disk
// under its deterministic name and any NIC under a name Azure derives from
// the VM's NIC configuration. Release reports completion only after none of
// these exists, and an unrecorded one is never deleted.
func releaseStep(
	ctx context.Context,
	client capacityClient,
	target allocationVM,
	owned []unboundedv1alpha3.ProviderOwnedResource,
) (releaseOutcome, error) {
	if len(owned) == 0 {
		return releaseOutcome{
			done:    true,
			message: fmt.Sprintf("allocation VM %s recorded no Azure resources; nothing to release", target.name),
		}, nil
	}

	refs, err := releaseChildRefs(target, owned)
	if err != nil {
		return releaseOutcome{conflict: err}, nil
	}

	vm, err := client.GetVM(ctx, target.resourceGroup, target.name, false)
	if err != nil {
		return releaseOutcome{}, err
	}

	if vm != nil {
		return releaseVM(ctx, client, vm, target, owned)
	}

	created, err := createdChildRefs(ctx, client, target)
	if err != nil {
		return releaseOutcome{}, err
	}

	for _, ref := range created {
		refs = appendChildRef(refs, ref)
	}

	children, err := observeChildren(ctx, client, refs)
	if err != nil {
		return releaseOutcome{}, err
	}

	// Prove every leftover before deleting any, so a conflict leaves the
	// remaining resources untouched for manual inspection.
	var remaining []string

	for _, child := range children {
		if !child.present {
			continue
		}

		if err := proveOwnedChild(child, target, owned); err != nil {
			return releaseOutcome{conflict: err}, nil
		}

		remaining = append(remaining, child.kind+" "+child.name)
	}

	if len(remaining) == 0 {
		return releaseOutcome{
			done:    true,
			message: fmt.Sprintf("VM %s is deleted and no NIC or OS disk of the allocation remains", target.name),
		}, nil
	}

	for _, child := range children {
		if !child.present || child.deleting {
			continue
		}

		if err := child.delete(ctx, client); err != nil {
			return releaseOutcome{}, err
		}
	}

	return releaseOutcome{message: fmt.Sprintf("waiting for deletion of %s left by VM %s", strings.Join(remaining, ", "), target.name)}, nil
}

// releaseVM deletes the allocation VM after proving it is the recorded one.
func releaseVM(
	ctx context.Context,
	client capacityClient,
	vm *armcompute.VirtualMachine,
	target allocationVM,
	owned []unboundedv1alpha3.ProviderOwnedResource,
) (releaseOutcome, error) {
	// Never delete a VM another owner created under this name.
	if err := checkVMOwnership(vm, target.uid); err != nil {
		return releaseOutcome{conflict: err}, nil
	}

	recorded, ok := recordedVM(target, owned)
	if !ok {
		return releaseOutcome{conflict: fmt.Errorf("VM %s was never recorded as owned by the allocation; refusing to delete it", target.name)}, nil
	}

	if nativeID := vmNativeID(vm); nativeID == "" || nativeID != recorded.NativeID {
		return releaseOutcome{conflict: fmt.Errorf("VM %s was replaced after the allocation recorded it; refusing to delete it", target.name)}, nil
	}

	// After the VM is gone, release can find a suffixed NIC only through the
	// allocation's record, so the VM is never deleted while a child it
	// references is present but unrecorded.
	conflict, err := unrecordedChild(ctx, client, vm, target, owned)
	if err != nil {
		return releaseOutcome{}, err
	}

	if conflict != "" {
		return releaseOutcome{conflict: errors.New(conflict)}, nil
	}

	// A repeated step observes the delete it already issued.
	if strings.EqualFold(provisioningState(vm), "Deleting") {
		return releaseOutcome{message: fmt.Sprintf("waiting for VM %s deletion", target.name)}, nil
	}

	if err := client.DeleteVM(ctx, target.resourceGroup, target.name); err != nil {
		return releaseOutcome{}, err
	}

	return releaseOutcome{message: "deleting VM " + target.name}, nil
}

// recordedVM returns the allocation's ownership record of its VM.
func recordedVM(target allocationVM, owned []unboundedv1alpha3.ProviderOwnedResource) (unboundedv1alpha3.ProviderOwnedResource, bool) {
	for _, resource := range owned {
		if strings.EqualFold(resource.ID, target.id()) {
			return resource, true
		}
	}

	return unboundedv1alpha3.ProviderOwnedResource{}, false
}

// unrecordedChild returns why the VM must not be deleted, or "": it
// references a present child the allocation never recorded with its
// provider-native ID.
func unrecordedChild(
	ctx context.Context,
	client capacityClient,
	vm *armcompute.VirtualMachine,
	target allocationVM,
	owned []unboundedv1alpha3.ProviderOwnedResource,
) (string, error) {
	refs, err := vmChildRefs(vm, target)
	if err != nil {
		return err.Error(), nil
	}

	children, err := observeChildren(ctx, client, refs)
	if err != nil {
		return "", err
	}

	for _, child := range children {
		if !child.present {
			continue
		}

		if err := proveOwnedChild(child, target, owned); err != nil {
			return fmt.Sprintf("refusing to delete VM %s: %v", target.name, err), nil
		}
	}

	return "", nil
}
