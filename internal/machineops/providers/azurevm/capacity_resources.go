// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v7"
	"k8s.io/apimachinery/pkg/types"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// childRef identifies a NIC or OS disk by ARM ID inside the allocation's
// resource group.
type childRef struct {
	kind          string
	id            string
	resourceGroup string
	name          string
}

// childResource is the observed state of a NIC or OS disk.
type childResource struct {
	childRef
	present    bool
	nativeID   string
	attachedTo string
	deleting   bool
}

const (
	childKindNIC  = "network interface"
	childKindDisk = "disk"

	nicResourceType  = "Microsoft.Network/networkInterfaces"
	diskResourceType = "Microsoft.Compute/disks"
)

// attachedElsewhere reports that the child is attached to something other
// than the allocation VM.
func (c childResource) attachedElsewhere(target allocationVM) bool {
	return c.attachedTo != "" && !strings.EqualFold(c.attachedTo, target.id())
}

func (c childResource) owned() unboundedv1alpha3.ProviderOwnedResource {
	return unboundedv1alpha3.ProviderOwnedResource{
		ID:       c.id,
		NativeID: c.nativeID,
	}
}

func (c childResource) delete(ctx context.Context, client capacityClient) error {
	if c.kind == childKindNIC {
		return client.DeleteNIC(ctx, c.resourceGroup, c.name)
	}

	return client.DeleteDisk(ctx, c.resourceGroup, c.name)
}

// parseChildRef accepts only a NIC or managed disk in the allocation VM's
// subscription and resource group, so a recorded or VM-referenced ID can
// never direct reads or deletes elsewhere.
func parseChildRef(id string, target allocationVM) (childRef, error) {
	parsed, err := arm.ParseResourceID(id)
	if err != nil {
		return childRef{}, fmt.Errorf("parse child resource ID %q: %w", id, err)
	}

	if !strings.EqualFold(parsed.SubscriptionID, target.subscriptionID) || !strings.EqualFold(parsed.ResourceGroupName, target.resourceGroup) {
		return childRef{}, fmt.Errorf("child resource %s is outside allocation VM %s resource group", id, target.name)
	}

	ref := childRef{
		id:            id,
		resourceGroup: parsed.ResourceGroupName,
		name:          parsed.Name,
	}

	switch resourceType := parsed.ResourceType.String(); {
	case strings.EqualFold(resourceType, nicResourceType):
		ref.kind = childKindNIC
	case strings.EqualFold(resourceType, diskResourceType):
		ref.kind = childKindDisk
	default:
		return childRef{}, fmt.Errorf("child resource %s has unsupported type %s", id, resourceType)
	}

	return ref, nil
}

// vmChildRefs returns the NICs and OS disk the VM references. These IDs are
// authoritative: Azure appends a random suffix to the name of a NIC created
// from a network interface configuration.
func vmChildRefs(vm *armcompute.VirtualMachine, target allocationVM) ([]childRef, error) {
	if vm.Properties == nil {
		return nil, nil
	}

	var ids []string

	if network := vm.Properties.NetworkProfile; network != nil {
		for _, nic := range network.NetworkInterfaces {
			if nic != nil && nic.ID != nil {
				ids = append(ids, *nic.ID)
			}
		}
	}

	if id := osDiskID(vm.Properties.StorageProfile); id != "" {
		ids = append(ids, id)
	}

	refs := make([]childRef, 0, len(ids))

	for _, id := range ids {
		ref, err := parseChildRef(id, target)
		if err != nil {
			return nil, fmt.Errorf("VM %s references an unexpected child: %w", target.name, err)
		}

		refs = append(refs, ref)
	}

	return refs, nil
}

func osDiskID(storage *armcompute.StorageProfile) string {
	if storage == nil || storage.OSDisk == nil || storage.OSDisk.ManagedDisk == nil {
		return ""
	}

	return toValue(storage.OSDisk.ManagedDisk.ID, "")
}

// configuredNICRef is the NIC under the exact NIC configuration name. Azure
// never gives a VM-managed NIC this name, so it is checked only to keep a
// compound create away from an existing resource.
func configuredNICRef(target allocationVM) childRef {
	return nicRef(target, target.nicName())
}

func nicRef(target allocationVM, name string) childRef {
	return childRef{
		kind:          childKindNIC,
		id:            target.nicID(name),
		resourceGroup: target.resourceGroup,
		name:          name,
	}
}

// osDiskRef is the OS disk under the name the allocation VM is created with.
// Azure honors the disk name exactly.
func osDiskRef(target allocationVM) childRef {
	return childRef{
		kind:          childKindDisk,
		id:            target.osDiskID(),
		resourceGroup: target.resourceGroup,
		name:          target.osDiskName(),
	}
}

// createdChildRefs finds, by name, what the VM create may have made when the
// VM does not reference it or no longer exists: the OS disk, and every NIC in
// the allocation's resource group named "<NIC configuration name>-<suffix>",
// which is how Azure names a NIC it creates from a VM's network interface
// configuration. The configuration name holds the full allocation UID, so the
// prefix is specific to this allocation.
//
// The NIC cannot be tagged at creation, because the VM's network interface
// configuration has no tags, so the name is the only way to find it. A name
// is a lead and never an ownership proof: callers record or delete what they
// find only after the attachment and native ID checks they apply to every
// other child.
func createdChildRefs(ctx context.Context, client capacityClient, target allocationVM) ([]childRef, error) {
	names, err := client.ListNICNames(ctx, target.resourceGroup)
	if err != nil {
		return nil, err
	}

	slices.Sort(names)

	refs := []childRef{
		osDiskRef(target),
	}

	prefix := strings.ToLower(target.nicName() + "-")

	for _, name := range names {
		if strings.HasPrefix(strings.ToLower(name), prefix) {
			refs = appendChildRef(refs, nicRef(target, name))
		}
	}

	return refs, nil
}

// releaseChildRefs lists the recorded children release must observe after
// the VM is gone, plus the deterministic OS disk so an unrecorded disk left
// behind by a recorded VM fails closed instead of being reported as released.
// The caller adds the NICs found by name. A recorded resource that is not the
// VM, a NIC or a disk in the allocation's resource group is refused.
func releaseChildRefs(target allocationVM, owned []unboundedv1alpha3.ProviderOwnedResource) ([]childRef, error) {
	refs := make([]childRef, 0, len(owned)+1)

	for _, resource := range owned {
		if strings.EqualFold(resource.ID, target.id()) {
			continue
		}

		ref, err := parseChildRef(resource.ID, target)
		if err != nil {
			return nil, fmt.Errorf("recorded resource of allocation VM %s: %w", target.name, err)
		}

		refs = appendChildRef(refs, ref)
	}

	return appendChildRef(refs, osDiskRef(target)), nil
}

func appendChildRef(refs []childRef, ref childRef) []childRef {
	for _, existing := range refs {
		if strings.EqualFold(existing.id, ref.id) {
			return refs
		}
	}

	return append(refs, ref)
}

// observeChildren reads each child by its ARM ID. Only an Azure not-found
// answer for the resource is absence; any other error is returned.
func observeChildren(ctx context.Context, client capacityClient, refs []childRef) ([]childResource, error) {
	children := make([]childResource, 0, len(refs))

	for _, ref := range refs {
		child, err := observeChild(ctx, client, ref)
		if err != nil {
			return nil, err
		}

		children = append(children, child)
	}

	return children, nil
}

func observeChild(ctx context.Context, client capacityClient, ref childRef) (childResource, error) {
	if ref.kind == childKindNIC {
		return observeNIC(ctx, client, ref)
	}

	return observeDisk(ctx, client, ref)
}

func observeNIC(ctx context.Context, client capacityClient, ref childRef) (childResource, error) {
	child := childResource{
		childRef: ref,
	}

	nic, err := client.GetNIC(ctx, ref.resourceGroup, ref.name)
	if err != nil || nic == nil {
		return child, err
	}

	child.present = true
	child.attachedTo = nicAttachment(nic)

	if nic.Properties != nil {
		child.nativeID = toValue(nic.Properties.ResourceGUID, "")
		child.deleting = nic.Properties.ProvisioningState != nil && *nic.Properties.ProvisioningState == armnetwork.ProvisioningStateDeleting
	}

	return child, nil
}

func observeDisk(ctx context.Context, client capacityClient, ref childRef) (childResource, error) {
	child := childResource{
		childRef: ref,
	}

	disk, err := client.GetDisk(ctx, ref.resourceGroup, ref.name)
	if err != nil || disk == nil {
		return child, err
	}

	child.present = true
	child.attachedTo = toValue(disk.ManagedBy, "")

	if disk.Properties != nil {
		child.nativeID = toValue(disk.Properties.UniqueID, "")
		child.deleting = strings.EqualFold(toValue(disk.Properties.ProvisioningState, ""), "Deleting")
	}

	return child, nil
}

// proveOwnedChild accepts a leftover only when its provider-native ID equals
// the one recorded while it was attached to this allocation's VM. A matching
// name alone could be a foreign or recreated resource.
func proveOwnedChild(child childResource, target allocationVM, owned []unboundedv1alpha3.ProviderOwnedResource) error {
	if child.attachedTo != "" && !strings.EqualFold(child.attachedTo, target.id()) {
		return fmt.Errorf("%s %s is attached to %s, not allocation VM %s", child.kind, child.name, child.attachedTo, target.name)
	}

	for _, resource := range owned {
		if !strings.EqualFold(resource.ID, child.id) {
			continue
		}

		if child.nativeID != "" && resource.NativeID == child.nativeID {
			return nil
		}

		return fmt.Errorf("%s %s was replaced after the allocation recorded it; refusing to delete it", child.kind, child.name)
	}

	return fmt.Errorf("%s %s was never recorded as owned by the allocation; refusing to delete it", child.kind, child.name)
}

func nicAttachment(nic *armnetwork.Interface) string {
	if nic.Properties == nil || nic.Properties.VirtualMachine == nil {
		return ""
	}

	return toValue(nic.Properties.VirtualMachine.ID, "")
}

func checkVMOwnership(vm *armcompute.VirtualMachine, uid types.UID) error {
	owner := tagValue(vm.Tags, TagAllocationUID)
	if owner == string(uid) {
		return nil
	}

	if owner == "" {
		return fmt.Errorf("VM %s exists without allocation tag %s; refusing to adopt it", toValue(vm.Name, ""), TagAllocationUID)
	}

	return fmt.Errorf("VM %s belongs to allocation %s, not %s", toValue(vm.Name, ""), owner, uid)
}

func tagValue(tags map[string]*string, key string) string {
	for name, value := range tags {
		if strings.EqualFold(name, key) && value != nil {
			return *value
		}
	}

	return ""
}

func provisioningState(vm *armcompute.VirtualMachine) string {
	if vm.Properties == nil {
		return ""
	}

	return toValue(vm.Properties.ProvisioningState, "")
}
