// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"k8s.io/apimachinery/pkg/types"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
)

// PlanAllocation fixes the VM identity before any Azure side effect, so
// every later step addresses the same deterministic ARM resource.
func (p *Provider) PlanAllocation(_ context.Context, request machineops.AllocationPlanRequest) (machineops.AllocationPlan, error) {
	config, err := parseCapacityConfig(request.ProviderConfig.Data)
	if err != nil {
		return machineops.AllocationPlan{}, invalidRequest(err)
	}

	name, err := VMName(request.AllocationUID)
	if err != nil {
		return machineops.AllocationPlan{}, invalidRequest(err)
	}

	if request.HostName != name {
		return machineops.AllocationPlan{}, invalidRequest(fmt.Errorf("host name %q must equal the allocation VM name %q", request.HostName, name))
	}

	if err := offeringMatchesConfig(request.Offering, config); err != nil {
		return machineops.AllocationPlan{}, invalidRequest(err)
	}

	return machineops.AllocationPlan{
		ProviderID: "azure://" + vmResourceID(config.SubscriptionID, config.ResourceGroup, name),
		HostImage:  config.Image,
	}, nil
}

func offeringMatchesConfig(offering unboundedv1alpha3.MachineClassOffering, config capacityConfig) error {
	if !strings.EqualFold(offering.InstanceType, config.VMSize) {
		return fmt.Errorf("offering instance type %q does not match configured VM size %q", offering.InstanceType, config.VMSize)
	}

	if offering.Zone != config.offeringZone() {
		return fmt.Errorf("offering zone %q does not match configured zone %q", offering.Zone, config.offeringZone())
	}

	if offering.CapacityType != unboundedv1alpha3.CapacityTypeOnDemand {
		return fmt.Errorf("capacity type %q is not supported", offering.CapacityType)
	}

	return nil
}

// workerIdentity is the user-assigned identity attached to the VM, or nil
// when the class names none.
func workerIdentity(config capacityConfig) *armcompute.VirtualMachineIdentity {
	if config.WorkerIdentityResourceID == "" {
		return nil
	}

	return &armcompute.VirtualMachineIdentity{
		Type: new(armcompute.ResourceIdentityTypeUserAssigned),
		UserAssignedIdentities: map[string]*armcompute.UserAssignedIdentitiesValue{
			config.WorkerIdentityResourceID: {},
		},
	}
}

// allocationVM is the deterministic Azure identity of one allocation's host.
type allocationVM struct {
	subscriptionID string
	resourceGroup  string
	name           string
	uid            types.UID
}

func (vm allocationVM) id() string {
	return vmResourceID(vm.subscriptionID, vm.resourceGroup, vm.name)
}

func (vm allocationVM) nicID(name string) string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/networkInterfaces/%s", vm.subscriptionID, vm.resourceGroup, name)
}

func (vm allocationVM) osDiskID() string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s", vm.subscriptionID, vm.resourceGroup, vm.osDiskName())
}

// nicName is the VM's NIC configuration name. The NIC Azure creates from it
// is named "<nicName>-<suffix>"; see createdChildRefs.
func (vm allocationVM) nicName() string {
	return vm.name + nicNameSuffix
}

func (vm allocationVM) osDiskName() string {
	return vm.name + osDiskNameSuffix
}

// allocationTarget binds the Machine providerID to the allocation UID so an
// operation can never address a VM named for a different allocation.
func allocationTarget(request machineops.OperationRequest) (allocationVM, error) {
	if request.Allocation == nil {
		return allocationVM{}, fmt.Errorf("allocation request is required")
	}

	ref, err := parseAzureVMProviderID(request.ProviderID)
	if err != nil {
		return allocationVM{}, err
	}

	name, err := VMName(request.Allocation.UID)
	if err != nil {
		return allocationVM{}, err
	}

	if ref.VMName != name {
		return allocationVM{}, fmt.Errorf("providerID VM %q does not match allocation VM name %q", ref.VMName, name)
	}

	return allocationVM{
		subscriptionID: ref.SubscriptionID,
		resourceGroup:  ref.ResourceGroup,
		name:           name,
		uid:            request.Allocation.UID,
	}, nil
}

// allocateHandle is the HostAllocate resume token. It holds only what Poll
// needs to explain an absent VM and never any user data or credential.
type allocateHandle struct {
	// AcceptedAt is when Azure accepted the VM create.
	AcceptedAt *time.Time `json:"acceptedAt,omitempty"`
	// Rejected records a VM create that Azure definitively refused.
	Rejected *createRejection `json:"rejected,omitempty"`
}

// createRejection is a definitive Azure refusal of the VM create.
type createRejection struct {
	Reason unboundedv1alpha3.MachineAllocationFailureReason `json:"reason"`
	Code   string                                           `json:"code,omitempty"`
	Status int                                              `json:"status,omitempty"`
}

func (r createRejection) describe(vmName string) string {
	return fmt.Sprintf("Azure rejected the creation of VM %s with error code %s (HTTP %d)", vmName, valueOrDefault(r.Code, "unknown"), r.Status)
}

func parseAllocateHandle(token string) (allocateHandle, error) {
	var handle allocateHandle

	if strings.TrimSpace(token) == "" {
		return handle, nil
	}

	if err := json.Unmarshal([]byte(token), &handle); err != nil {
		return allocateHandle{}, fmt.Errorf("parse HostAllocate resume token: %w", err)
	}

	return handle, nil
}

// BeginAllocate creates the allocation VM, or adopts it when a previous
// attempt already created it. Only a VM tagged with this allocation's UID is
// ever adopted.
//
// It returns a PermanentError only before the VM create is sent, when it
// cannot have created anything: for input it rejects, a conflicting resource,
// or a read Azure refuses for the credential (see beforeCreate). A create that Azure definitively refuses
// (see classifyCreateRejection) returns a handle that records the refusal,
// and PollAllocate reports the failure after it confirmed that nothing
// exists. Only a response that leaves the outcome open, such as throttling, a
// server error or a conflict with an operation in progress, is returned as a
// retryable error. The engine treats a
// PermanentError from a replayed Begin as unknown ownership, so reporting the
// refusal through Poll keeps an out-of-capacity or quota failure that follows
// a retried Begin a clean, releasable failure.
func (p *Provider) BeginAllocate(ctx context.Context, request machineops.OperationRequest) (machineops.BeginResult, error) {
	target, err := allocationTarget(request)
	if err != nil {
		return machineops.BeginResult{}, invalidRequest(err)
	}

	config, err := allocationConfig(request, target)
	if err != nil {
		return machineops.BeginResult{}, invalidRequest(err)
	}

	client, err := p.capacityClient(target.subscriptionID, request.Auth)
	if err != nil {
		return machineops.BeginResult{}, fmt.Errorf("create Azure capacity client: %w", err)
	}

	existing, err := client.GetVM(ctx, target.resourceGroup, target.name, false)
	if err != nil {
		return machineops.BeginResult{}, beforeCreate(err)
	}

	// A replayed Begin after a lost status write finds its own VM and resumes
	// observation instead of issuing a second create.
	if existing != nil {
		if err := checkVMOwnership(existing, target.uid); err != nil {
			return machineops.BeginResult{}, providerConflict(err)
		}

		return beginAllocateResult(target, allocateHandle{}, "adopted existing VM "+target.name)
	}

	// A compound VM create updates an existing NIC or disk with the same name
	// instead of failing, so it must never run over resources it did not
	// create. A NIC that already carries a name Azure would derive for this VM
	// could later be mistaken for the one this create makes, so it blocks the
	// create as well.
	refs, err := createdChildRefs(ctx, client, target)
	if err != nil {
		return machineops.BeginResult{}, beforeCreate(err)
	}

	children, err := observeChildren(ctx, client, appendChildRef(refs, configuredNICRef(target)))
	if err != nil {
		return machineops.BeginResult{}, beforeCreate(err)
	}

	for _, child := range children {
		if child.present {
			return machineops.BeginResult{}, providerConflict(fmt.Errorf("%s %s already exists before its VM was created", child.kind, child.name))
		}
	}

	vm, err := buildAllocationVM(config, request.Allocation, target)
	if err != nil {
		return machineops.BeginResult{}, invalidRequest(err)
	}

	if err := client.CreateVM(ctx, target.resourceGroup, target.name, vm); err != nil {
		rejection, definitive := classifyCreateRejection(err)
		if !definitive {
			return machineops.BeginResult{}, err
		}

		return beginAllocateResult(target, allocateHandle{Rejected: &rejection}, rejection.describe(target.name))
	}

	acceptedAt := p.now().UTC()

	return beginAllocateResult(target, allocateHandle{AcceptedAt: &acceptedAt}, "creating VM "+target.name)
}

func beginAllocateResult(target allocationVM, handle allocateHandle, message string) (machineops.BeginResult, error) {
	token, err := json.Marshal(handle)
	if err != nil {
		return machineops.BeginResult{}, fmt.Errorf("encode HostAllocate resume token: %w", err)
	}

	result := beginResult(target, message)
	result.Operation.ResumeToken = string(token)

	return result, nil
}

func allocationConfig(request machineops.OperationRequest, target allocationVM) (capacityConfig, error) {
	config, err := parseCapacityConfig(request.Allocation.ProviderConfig.Data)
	if err != nil {
		return capacityConfig{}, err
	}

	if !strings.EqualFold(config.SubscriptionID, target.subscriptionID) || !strings.EqualFold(config.ResourceGroup, target.resourceGroup) {
		return capacityConfig{}, fmt.Errorf("providerID resource group does not match the frozen provider configuration")
	}

	if request.HostImage != "" && request.HostImage != config.Image {
		return capacityConfig{}, fmt.Errorf("host image %q does not match the frozen provider configuration", request.HostImage)
	}

	if err := offeringMatchesConfig(request.Allocation.Offering, config); err != nil {
		return capacityConfig{}, err
	}

	if request.Allocation.UserData == "" {
		return capacityConfig{}, fmt.Errorf("allocation user data is required")
	}

	if err := validateAzureCustomData(request.Allocation.UserData); err != nil {
		return capacityConfig{}, err
	}

	return config, nil
}

func beginResult(target allocationVM, message string) machineops.BeginResult {
	return machineops.BeginResult{
		Operation: machineops.ProviderOperation{
			OperationID: target.id(),
		},
		Message:      message,
		RequeueAfter: pollInterval,
	}
}

// buildAllocationVM renders the VM model. The NIC and OS disk are created
// with the VM and marked for deletion with it, so release removes every
// resource this allocation owns.
func buildAllocationVM(
	config capacityConfig,
	allocation *machineops.AllocationRequest,
	target allocationVM,
) (armcompute.VirtualMachine, error) {
	image, err := parseAzureImageReference(config.Image)
	if err != nil {
		return armcompute.VirtualMachine{}, err
	}

	vm := armcompute.VirtualMachine{
		Location: new(config.Location),
		Tags: map[string]*string{
			TagAllocationUID:  new(string(allocation.UID)),
			TagAllocationName: new(allocation.Name),
			TagMachineClass:   new(allocation.ClassName),
			TagManagedBy:      new(managedByValue),
		},
		Identity: workerIdentity(config),
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{
				VMSize: new(armcompute.VirtualMachineSizeTypes(config.VMSize)),
			},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: image,
				OSDisk: &armcompute.OSDisk{
					Name:         new(target.osDiskName()),
					CreateOption: new(armcompute.DiskCreateOptionTypesFromImage),
					DeleteOption: new(armcompute.DiskDeleteOptionTypesDelete),
					Caching:      new(armcompute.CachingTypesReadWrite),
					DiskSizeGB:   new(config.OSDiskSizeGB),
					ManagedDisk: &armcompute.ManagedDiskParameters{
						StorageAccountType: new(armcompute.StorageAccountTypes(config.OSDiskType)),
					},
				},
			},
			OSProfile: &armcompute.OSProfile{
				ComputerName:  new(target.name),
				AdminUsername: new(config.AdminUsername),
				CustomData:    new(base64.StdEncoding.EncodeToString([]byte(allocation.UserData))),
				LinuxConfiguration: &armcompute.LinuxConfiguration{
					DisablePasswordAuthentication: new(true),
					SSH: &armcompute.SSHConfiguration{
						PublicKeys: []*armcompute.SSHPublicKey{
							{
								Path:    new("/home/" + config.AdminUsername + "/.ssh/authorized_keys"),
								KeyData: new(config.SSHPublicKey),
							},
						},
					},
				},
			},
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkAPIVersion: new(armcompute.NetworkAPIVersionTwoThousandTwenty1101),
				NetworkInterfaceConfigurations: []*armcompute.VirtualMachineNetworkInterfaceConfiguration{
					{
						Name: new(target.nicName()),
						Properties: &armcompute.VirtualMachineNetworkInterfaceConfigurationProperties{
							Primary:      new(true),
							DeleteOption: new(armcompute.DeleteOptionsDelete),
							IPConfigurations: []*armcompute.VirtualMachineNetworkInterfaceIPConfiguration{
								{
									Name: new("ipconfig1"),
									Properties: &armcompute.VirtualMachineNetworkInterfaceIPConfigurationProperties{
										Primary:                 new(true),
										PrivateIPAddressVersion: new(armcompute.IPVersionsIPv4),
										Subnet: &armcompute.SubResource{
											ID: new(config.SubnetID),
										},
									},
								},
							},
						},
					},
				},
			},
			DiagnosticsProfile: &armcompute.DiagnosticsProfile{
				BootDiagnostics: &armcompute.BootDiagnostics{
					Enabled: new(true),
				},
			},
		},
	}

	if config.Zone != "" {
		vm.Zones = []*string{
			new(config.Zone),
		}
	}

	return vm, nil
}

// PollAllocate observes the VM by its deterministic ID. Only an Azure
// not-found answer for the resource or its resource group is treated as
// absence (see isAzureNotFound); every other read failure is retried.
//
// Every terminal result reports the allocation's owned resources: the VM and
// the NICs and OS disk attached to it, each with its Azure-assigned identity,
// or none when nothing exists. A NIC or OS disk that exists but is not
// attached yet, or not any more, keeps the operation in progress, because
// that state is expected to pass and a terminal result could not report it.
// When ownership is disproved, for example by a child attached to another VM,
// the result carries no allocation report, which the engine records as
// unknown ownership and holds for manual recovery rather than releasing on a
// guess.
func (p *Provider) PollAllocate(
	ctx context.Context,
	request machineops.OperationRequest,
	operation machineops.ProviderOperation,
) (machineops.PollResult, error) {
	target, err := allocationTarget(request)
	if err != nil {
		return machineops.PollResult{}, invalidRequest(err)
	}

	handle, err := parseAllocateHandle(operation.ResumeToken)
	if err != nil {
		return machineops.PollResult{}, invalidRequest(err)
	}

	client, err := p.capacityClient(target.subscriptionID, request.Auth)
	if err != nil {
		return machineops.PollResult{}, fmt.Errorf("create Azure capacity client: %w", err)
	}

	vm, err := client.GetVM(ctx, target.resourceGroup, target.name, false)
	if err != nil {
		return machineops.PollResult{}, err
	}

	if vm == nil {
		return p.absentAllocation(ctx, client, target, handle)
	}

	// A VM under this name that carries another owner's tag proves nothing
	// about what this allocation created.
	if err := checkVMOwnership(vm, target.uid); err != nil {
		return failedPoll(unboundedv1alpha3.FailureProviderConflict, err.Error()), nil
	}

	switch strings.ToLower(provisioningState(vm)) {
	case "succeeded":
		return provisionedAllocation(ctx, client, vm, target, request)
	case "failed", "canceled":
		return failedProvisioning(ctx, client, target, request)
	case "deleting":
		return failedAllocation(ctx, client, vm, target, request, unboundedv1alpha3.FailureProviderFailed, fmt.Sprintf("VM %s is being deleted", target.name))
	default:
		return machineops.PollResult{
			State:        machineops.ProviderOperationStateInProgress,
			Message:      fmt.Sprintf("VM %s provisioning state is %s", target.name, valueOrDefault(provisioningState(vm), "unknown")),
			RequeueAfter: pollInterval,
		}, nil
	}
}

// absentAllocation ends an allocation whose VM does not exist. It reports
// that the allocation owns nothing only after it also found no OS disk under
// the VM's disk name and no NIC under a name Azure derives from the VM's NIC
// configuration. Such a resource that is attached to nothing keeps the
// operation in progress: Azure removes a VM before the NIC and disk it
// cascades to, so the leftover is expected to disappear, and until it does
// the allocation can neither claim it nor say that it owns nothing.
func (p *Provider) absentAllocation(
	ctx context.Context,
	client capacityClient,
	target allocationVM,
	handle allocateHandle,
) (machineops.PollResult, error) {
	// A read just after an accepted create may not see the VM yet.
	if handle.Rejected == nil && handle.AcceptedAt != nil && p.now().Sub(*handle.AcceptedAt) < createVisibilityGrace {
		return machineops.PollResult{
			State:        machineops.ProviderOperationStateInProgress,
			Message:      fmt.Sprintf("waiting for accepted VM %s to become visible", target.name),
			RequeueAfter: pollInterval,
		}, nil
	}

	reason := unboundedv1alpha3.FailureProviderFailed
	message := fmt.Sprintf("VM %s is absent after Azure accepted its creation", target.name)

	if handle.AcceptedAt == nil {
		// Begin adopted a VM an earlier attempt created.
		message = fmt.Sprintf("VM %s no longer exists", target.name)
	}

	if handle.Rejected != nil {
		reason = handle.Rejected.Reason
		message = handle.Rejected.describe(target.name)
	}

	refs, err := createdChildRefs(ctx, client, target)
	if err != nil {
		return machineops.PollResult{}, err
	}

	children, err := observeChildren(ctx, client, refs)
	if err != nil {
		return machineops.PollResult{}, err
	}

	var waiting []string

	for _, child := range children {
		if !child.present {
			continue
		}

		if child.attachedElsewhere(target) {
			return failedPoll(reason, fmt.Sprintf("%s; %s %s exists without the VM, is attached to %s and cannot be proved to belong to the allocation",
				message, child.kind, child.name, child.attachedTo)), nil
		}

		waiting = append(waiting, child.kind+" "+child.name)
	}

	if len(waiting) > 0 {
		return unsettledAllocation(reason, message, fmt.Sprintf("%s to be deleted or attached to VM %s", strings.Join(waiting, ", "), target.name)), nil
	}

	result := failedPoll(reason, message)
	result.Result.Allocation = &machineops.AllocationResult{}

	return result, nil
}

// unsettledAllocation keeps a failing allocate operation in progress while a
// resource it may own is in a state that proves nothing yet. The message
// carries the failure reason and text the operation ends with once the
// resource settles, so they are visible while it waits. The wait has no
// deadline of its own, like the wait for a provisioned VM's children: the
// engine marks the operation stalled and slows its polling.
func unsettledAllocation(reason unboundedv1alpha3.MachineAllocationFailureReason, message, waitingFor string) machineops.PollResult {
	return machineops.PollResult{
		State:        machineops.ProviderOperationStateInProgress,
		Message:      fmt.Sprintf("%s: %s; waiting for %s", reason, message, waitingFor),
		RequeueAfter: pollInterval,
	}
}

// provisionedAllocation records the VM and the NICs and OS disk the VM itself
// references while they are provably attached to the allocation's UID-tagged
// VM. Azure names a VM-managed NIC after its configuration with a random
// suffix, so only the VM's references identify it. Their provider-native IDs
// are the only evidence release later accepts for deleting them.
func provisionedAllocation(
	ctx context.Context,
	client capacityClient,
	vm *armcompute.VirtualMachine,
	target allocationVM,
	request machineops.OperationRequest,
) (machineops.PollResult, error) {
	refs, err := vmChildRefs(vm, target)
	if err != nil {
		return failedPoll(unboundedv1alpha3.FailureProviderConflict, err.Error()), nil
	}

	if err := requireProvisionedChildRefs(refs, target); err != nil {
		return machineops.PollResult{}, err
	}

	owned, err := ownedVM(vm, target)
	if err != nil {
		return machineops.PollResult{}, err
	}

	children, err := observeChildren(ctx, client, refs)
	if err != nil {
		return machineops.PollResult{}, err
	}

	result := allocationResult(vm, target, request)
	result.OwnedResources = append(result.OwnedResources, owned)

	for _, child := range children {
		if !child.present || child.nativeID == "" {
			return machineops.PollResult{}, fmt.Errorf("%s %s of provisioned VM %s is not yet observable", child.kind, child.id, target.name)
		}

		if child.attachedElsewhere(target) {
			return failedPoll(unboundedv1alpha3.FailureProviderConflict, fmt.Sprintf("%s %s is attached to %s, not to allocation VM %s", child.kind, child.id, child.attachedTo, target.name)), nil
		}

		// Azure can report the VM provisioned before the child shows its
		// attachment. That state passes, so it is retried like a child that
		// is not visible yet.
		if child.attachedTo == "" {
			return machineops.PollResult{}, fmt.Errorf("%s %s of provisioned VM %s does not report its attachment yet", child.kind, child.id, target.name)
		}

		result.OwnedResources = append(result.OwnedResources, child.owned())
	}

	return machineops.PollResult{
		State:   machineops.ProviderOperationStateSucceeded,
		Message: fmt.Sprintf("VM %s is provisioned", target.name),
		Result: machineops.OperationResult{
			Allocation: result,
		},
	}, nil
}

// ownedVM returns the VM's ownership record. Its native ID is the VM ID Azure
// assigned, which differs for a VM recreated under the same name.
func ownedVM(vm *armcompute.VirtualMachine, target allocationVM) (unboundedv1alpha3.ProviderOwnedResource, error) {
	nativeID := vmNativeID(vm)
	if nativeID == "" {
		return unboundedv1alpha3.ProviderOwnedResource{}, fmt.Errorf("VM %s does not report its VM ID yet", target.name)
	}

	return unboundedv1alpha3.ProviderOwnedResource{
		ID:       target.id(),
		NativeID: nativeID,
	}, nil
}

func vmNativeID(vm *armcompute.VirtualMachine) string {
	if vm.Properties == nil {
		return ""
	}

	return toValue(vm.Properties.VMID, "")
}

// requireProvisionedChildRefs waits until a provisioned VM references its NIC
// and OS disk, so the allocation never completes without recording them.
func requireProvisionedChildRefs(refs []childRef, target allocationVM) error {
	for _, kind := range []string{childKindNIC, childKindDisk} {
		if !slices.ContainsFunc(refs, func(ref childRef) bool { return ref.kind == kind }) {
			return fmt.Errorf("provisioned VM %s does not yet reference its %s", target.name, kind)
		}
	}

	return nil
}

// failedProvisioning reads the instance view only after provisioning failed,
// to classify the Azure failure code.
func failedProvisioning(
	ctx context.Context,
	client capacityClient,
	target allocationVM,
	request machineops.OperationRequest,
) (machineops.PollResult, error) {
	vm, err := client.GetVM(ctx, target.resourceGroup, target.name, true)
	if err != nil {
		return machineops.PollResult{}, err
	}

	// The VM vanished between the two reads; the next poll observes its
	// absence and what it left behind.
	if vm == nil {
		return machineops.PollResult{}, fmt.Errorf("VM %s disappeared after its provisioning failed", target.name)
	}

	if err := checkVMOwnership(vm, target.uid); err != nil {
		return failedPoll(unboundedv1alpha3.FailureProviderConflict, err.Error()), nil
	}

	code, message := provisioningFailure(vm)

	return failedAllocation(ctx, client, vm, target, request, classifyAzureCode(code, message), fmt.Sprintf("VM %s provisioning failed: %s %s", target.name, code, message))
}

// failedAllocation fails the allocate operation while recording the failed
// VM and every child it provably owns, so release can delete them and can
// verify a NIC whose suffixed name it cannot derive. It looks at the children
// the VM references and at the ones a failed VM may not reference: the OS
// disk under its deterministic name and any NIC under a name Azure derives
// from the VM's NIC configuration.
//
// A child that exists but is not attached, or does not report its native ID,
// keeps the operation in progress: a failed create can leave it detached for
// a moment and a deleting VM detaches it before it is removed. Ending there
// would report no ownership and discard the failure reason. A child attached
// to another VM is not transient and leaves ownership unknown: the result
// then carries no allocation report.
func failedAllocation(
	ctx context.Context,
	client capacityClient,
	vm *armcompute.VirtualMachine,
	target allocationVM,
	request machineops.OperationRequest,
	reason unboundedv1alpha3.MachineAllocationFailureReason,
	message string,
) (machineops.PollResult, error) {
	refs, refsErr := vmChildRefs(vm, target)
	if refsErr != nil {
		return failedPoll(unboundedv1alpha3.FailureProviderConflict, fmt.Sprintf("%s; %v", message, refsErr)), nil
	}

	created, err := createdChildRefs(ctx, client, target)
	if err != nil {
		return machineops.PollResult{}, err
	}

	for _, ref := range created {
		refs = appendChildRef(refs, ref)
	}

	owned, err := ownedVM(vm, target)
	if err != nil {
		return machineops.PollResult{}, err
	}

	children, err := observeChildren(ctx, client, refs)
	if err != nil {
		return machineops.PollResult{}, err
	}

	allocation := allocationResult(vm, target, request)
	allocation.OwnedResources = append(allocation.OwnedResources, owned)

	var waiting []string

	for _, child := range children {
		if !child.present {
			continue
		}

		if child.attachedElsewhere(target) {
			return failedPoll(reason, fmt.Sprintf("%s; %s %s is attached to %s and cannot be proved to belong to allocation VM %s",
				message, child.kind, child.name, child.attachedTo, target.name)), nil
		}

		if child.nativeID == "" || child.attachedTo == "" {
			waiting = append(waiting, child.kind+" "+child.name)
			continue
		}

		allocation.OwnedResources = append(allocation.OwnedResources, child.owned())
	}

	if len(waiting) > 0 {
		return unsettledAllocation(reason, message, fmt.Sprintf("%s to be attached to VM %s or deleted", strings.Join(waiting, ", "), target.name)), nil
	}

	result := failedPoll(reason, message)
	result.Result.Allocation = allocation

	return result, nil
}

func provisioningFailure(vm *armcompute.VirtualMachine) (string, string) {
	if vm == nil || vm.Properties == nil || vm.Properties.InstanceView == nil {
		return "", ""
	}

	for _, status := range vm.Properties.InstanceView.Statuses {
		if status == nil || status.Code == nil {
			continue
		}

		code, found := strings.CutPrefix(*status.Code, "ProvisioningState/failed/")
		if found {
			return code, toValue(status.Message, "")
		}
	}

	return "", ""
}

func allocationResult(
	vm *armcompute.VirtualMachine,
	target allocationVM,
	request machineops.OperationRequest,
) *machineops.AllocationResult {
	result := &machineops.AllocationResult{
		ProviderResourceID: target.id(),
		InstanceType:       request.Allocation.Offering.InstanceType,
		Zone:               request.Allocation.Offering.Zone,
		Image:              request.HostImage,
	}

	if vm.Properties == nil {
		return result
	}

	if hardware := vm.Properties.HardwareProfile; hardware != nil && hardware.VMSize != nil {
		result.InstanceType = string(*hardware.VMSize)
	}

	if image := observedImage(vm.Properties.StorageProfile); image != "" {
		result.Image = image
	}

	return result
}

func observedImage(storage *armcompute.StorageProfile) string {
	if storage == nil || storage.ImageReference == nil {
		return ""
	}

	image := storage.ImageReference
	if image.ID != nil {
		return *image.ID
	}

	version := toValue(image.ExactVersion, toValue(image.Version, ""))
	if image.Publisher == nil || image.Offer == nil || image.SKU == nil || version == "" {
		return ""
	}

	return strings.Join([]string{*image.Publisher, *image.Offer, *image.SKU, version}, ":")
}
