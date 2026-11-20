// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
)

const azureCustomDataMaxBytes = 65535

// azureVMClient is the narrow ARM surface the host operations use. Every
// mutating method only sends its request and returns once Azure accepted it:
// the operations observe completion through GetVM, so a controller restart can
// resume from Azure state and the persisted handle alone. GetVM returns nil
// without error only when Azure reports that the VM or its resource group does
// not exist (see isAzureNotFound).
type azureVMClient interface {
	GetVM(ctx context.Context, resourceGroup, name string, instanceView bool) (*armcompute.VirtualMachine, error)
	// CreateVM sends a PUT of the full VM model that Azure accepts only when
	// no VM has the name.
	CreateVM(ctx context.Context, resourceGroup, name string, vm armcompute.VirtualMachine) error
	// UpdateVM sends a PUT of the full VM model that Azure accepts only when
	// the VM still has the given etag.
	UpdateVM(ctx context.Context, resourceGroup, name string, vm armcompute.VirtualMachine, etag string) error
	DeleteVM(ctx context.Context, resourceGroup, name string) error
	StartVM(ctx context.Context, resourceGroup, name string) error
	PowerOffVM(ctx context.Context, resourceGroup, name string) error
	RestartVM(ctx context.Context, resourceGroup, name string) error
}

type azureVMClientFactory func(subscriptionID string, auth *machineops.OperationAuth) (azureVMClient, error)

// Provider executes operations against Azure virtual machines and allocates
// capacity for MachineClasses backed by Azure VMs.
type Provider struct {
	NewVMClient       azureVMClientFactory
	NewCapacityClient capacityClientFactory
	Now               func() time.Time

	credentials credentialCache
	skus        skuCache
}

func (p *Provider) Name() string {
	return unboundedv1alpha3.ExternalProviderAzureVM
}

// Registration returns this Azure adapter's registration: its host
// operations and its capacity lifecycle (class resolution, allocation
// planning, HostAllocate and HostRelease).
//
// Every host operation is long-running. An immediate operation that is not
// replay-safe is never executed again once its MachineOperation is
// InProgress, so a controller restart during a blocking Azure call would
// leave it InProgress forever. A long-running Begin is retried until its
// handle is persisted, and Poll resumes from that handle after a restart.
func (p *Provider) Registration() (*machineops.Provider, error) {
	options := []machineops.ProviderOption{
		machineops.WithLongRunningOperation(unboundedv1alpha3.OperationHostHardReboot, p.BeginPowerOperation, p.PollPowerOperation),
		machineops.WithLongRunningOperation(unboundedv1alpha3.OperationHostPowerOff, p.BeginPowerOperation, p.PollPowerOperation),
		machineops.WithLongRunningOperation(unboundedv1alpha3.OperationHostPowerOn, p.BeginPowerOperation, p.PollPowerOperation),
		machineops.WithLongRunningOperation(
			unboundedv1alpha3.OperationHostReplace,
			p.BeginReplace,
			p.PollReplace,
			machineops.RequiresReplaceUserData(),
		),
	}

	options = append(options, p.capacityRegistration()...)

	return machineops.NewProvider(p.Name(), options...)
}

// client returns the host operation client. The default client shares the
// provider's credential cache with the capacity client, so an operation does
// not perform a fresh token exchange on every Begin and Poll.
func (p *Provider) client(subscriptionID string, auth *machineops.OperationAuth) (azureVMClient, error) {
	if p.NewVMClient != nil {
		return p.NewVMClient(subscriptionID, auth)
	}

	credential, err := p.credentials.credential(auth)
	if err != nil {
		return nil, err
	}

	capacity, err := newARMCapacityClient(subscriptionID, credential)
	if err != nil {
		return nil, err
	}

	return armAzureVMClient{armCapacityClient: capacity}, nil
}

// armAzureVMClient reuses the capacity client's VM reads and deletes and adds
// the conditional writes and the power actions.
type armAzureVMClient struct {
	*armCapacityClient
}

func (c armAzureVMClient) CreateVM(ctx context.Context, resourceGroup, name string, vm armcompute.VirtualMachine) error {
	options := &armcompute.VirtualMachinesClientBeginCreateOrUpdateOptions{IfNoneMatch: new("*")}
	if _, err := c.vms.BeginCreateOrUpdate(ctx, resourceGroup, name, vm, options); err != nil {
		return fmt.Errorf("create VM %s/%s: %w", resourceGroup, name, err)
	}

	return nil
}

func (c armAzureVMClient) UpdateVM(ctx context.Context, resourceGroup, name string, vm armcompute.VirtualMachine, etag string) error {
	if etag == "" {
		return fmt.Errorf("update VM %s/%s: an etag is required", resourceGroup, name)
	}

	options := &armcompute.VirtualMachinesClientBeginCreateOrUpdateOptions{IfMatch: new(etag)}
	if _, err := c.vms.BeginCreateOrUpdate(ctx, resourceGroup, name, vm, options); err != nil {
		return fmt.Errorf("update VM %s/%s: %w", resourceGroup, name, err)
	}

	return nil
}

func (c armAzureVMClient) StartVM(ctx context.Context, resourceGroup, name string) error {
	if _, err := c.vms.BeginStart(ctx, resourceGroup, name, nil); err != nil {
		return fmt.Errorf("start VM %s/%s: %w", resourceGroup, name, err)
	}

	return nil
}

// PowerOffVM stops the VM without deallocating it: the host keeps its
// resources, and Azure keeps billing for them, so that HostPowerOn brings the
// same host back.
func (c armAzureVMClient) PowerOffVM(ctx context.Context, resourceGroup, name string) error {
	if _, err := c.vms.BeginPowerOff(ctx, resourceGroup, name, nil); err != nil {
		return fmt.Errorf("power off VM %s/%s: %w", resourceGroup, name, err)
	}

	return nil
}

func (c armAzureVMClient) RestartVM(ctx context.Context, resourceGroup, name string) error {
	if _, err := c.vms.BeginRestart(ctx, resourceGroup, name, nil); err != nil {
		return fmt.Errorf("restart VM %s/%s: %w", resourceGroup, name, err)
	}

	return nil
}

type azureVMResourceRef struct {
	SubscriptionID string
	ResourceGroup  string
	VMName         string
}

func (r azureVMResourceRef) id() string {
	return vmResourceID(r.SubscriptionID, r.ResourceGroup, r.VMName)
}

func parseAzureVMProviderID(providerID string) (azureVMResourceRef, error) {
	providerID = strings.TrimSpace(providerID)
	providerID = strings.TrimPrefix(providerID, "azure://")
	providerID = strings.TrimPrefix(providerID, "azure:")
	providerID = strings.Trim(providerID, "/")

	if providerID == "" {
		return azureVMResourceRef{}, fmt.Errorf("azure VM providerID is required")
	}

	parts := strings.Split(providerID, "/")
	if len(parts) != 8 {
		return azureVMResourceRef{}, fmt.Errorf("azure VM providerID must be azure:///subscriptions/{subscription}/resourceGroups/{resourceGroup}/providers/Microsoft.Compute/virtualMachines/{name}")
	}

	if !strings.EqualFold(parts[0], "subscriptions") ||
		!strings.EqualFold(parts[2], "resourceGroups") ||
		!strings.EqualFold(parts[4], "providers") ||
		!strings.EqualFold(parts[5], "Microsoft.Compute") ||
		!strings.EqualFold(parts[6], "virtualMachines") {
		return azureVMResourceRef{}, fmt.Errorf("azure VM providerID must identify a Microsoft.Compute/virtualMachines resource")
	}

	ref := azureVMResourceRef{
		SubscriptionID: parts[1],
		ResourceGroup:  parts[3],
		VMName:         parts[7],
	}

	if ref.SubscriptionID == "" || ref.ResourceGroup == "" || ref.VMName == "" {
		return azureVMResourceRef{}, fmt.Errorf("azure VM providerID is missing subscription, resource group, or VM name")
	}

	return ref, nil
}

// NormalizeResourceID returns the canonical Azure Resource Manager ID for a
// raw ARM ID or Kubernetes-style Azure provider ID.
func NormalizeResourceID(providerID string) (string, error) {
	ref, err := parseAzureVMProviderID(providerID)
	if err != nil {
		return "", err
	}

	return ref.id(), nil
}

// hostTarget resolves the VM a host operation addresses. A persisted handle
// must name the same VM, so a Poll can never act on a different one.
func hostTarget(request machineops.OperationRequest, operation *machineops.ProviderOperation) (azureVMResourceRef, error) {
	ref, err := parseAzureVMProviderID(request.ProviderID)
	if err != nil {
		return azureVMResourceRef{}, err
	}

	if operation != nil && !strings.EqualFold(operation.OperationID, ref.id()) {
		return azureVMResourceRef{}, fmt.Errorf("persisted operation %q does not address VM %s", operation.OperationID, ref.id())
	}

	return ref, nil
}

// hostHandle is the resume token of a power operation. It never holds user
// data or credentials. HostReplace has its own handle, replaceHandle.
type hostHandle struct {
	// AcceptedAt is when Azure accepted the request this operation sent. It
	// is nil when Begin found the operation already done or in flight and
	// sent nothing.
	AcceptedAt *time.Time `json:"acceptedAt,omitempty"`
}

func parseHostHandle(token string) (hostHandle, error) {
	var handle hostHandle

	if strings.TrimSpace(token) == "" {
		return handle, nil
	}

	if err := json.Unmarshal([]byte(token), &handle); err != nil {
		return hostHandle{}, fmt.Errorf("parse host operation resume token: %w", err)
	}

	return handle, nil
}

func hostBeginResult(target azureVMResourceRef, handle hostHandle, message string) (machineops.BeginResult, error) {
	token, err := json.Marshal(handle)
	if err != nil {
		return machineops.BeginResult{}, fmt.Errorf("encode host operation resume token: %w", err)
	}

	return machineops.BeginResult{
		Operation: machineops.ProviderOperation{
			OperationID: target.id(),
			ResumeToken: string(token),
		},
		Message:      message,
		RequeueAfter: pollInterval,
	}, nil
}

// classifyActionError classifies a failed mutating call against an existing
// VM. A VM that Azure reports absent ends the operation, and so does a
// refusal that classifyCreateRejection calls definitive, which includes Azure
// refusing the call because it is still changing the VM. Callers therefore
// wait for a VM to settle before they send anything. Every other failure is
// retried.
func classifyActionError(target azureVMResourceRef, action string, err error) error {
	if isAzureNotFound(err) {
		return vmNotFound(target, err)
	}

	if rejection, ok := classifyCreateRejection(err); ok {
		return &machineops.PermanentError{
			Reason: string(rejection.Reason),
			Err:    fmt.Errorf("%s of VM %s rejected by Azure with error code %s (HTTP %d): %w", action, target.VMName, valueOrDefault(rejection.Code, "unknown"), rejection.Status, err),
		}
	}

	return err
}

// reasonVMNotFound ends a host operation whose VM does not exist.
const reasonVMNotFound = "VMNotFound"

func vmNotFound(target azureVMResourceRef, err error) error {
	if err == nil {
		err = fmt.Errorf("VM %s not found", target.id())
	}

	return &machineops.PermanentError{
		Reason: reasonVMNotFound,
		Err:    err,
	}
}

func parseAzureImageReference(hostImage string) (*armcompute.ImageReference, error) {
	hostImage = strings.TrimSpace(hostImage)
	if hostImage == "" {
		return nil, nil
	}

	if strings.HasPrefix(hostImage, "/") {
		lowerImage := strings.ToLower(strings.TrimRight(hostImage, "/"))
		if !strings.HasPrefix(lowerImage, "/subscriptions/") ||
			!strings.Contains(lowerImage, "/resourcegroups/") ||
			!strings.Contains(lowerImage, "/providers/microsoft.compute/") ||
			!strings.Contains(lowerImage, "/images/") {
			return nil, fmt.Errorf("host image Azure resource ID must identify a Microsoft.Compute image or gallery image")
		}

		return &armcompute.ImageReference{ID: to.Ptr(hostImage)}, nil
	}

	parts := strings.Split(hostImage, ":")
	if len(parts) != 4 {
		return nil, fmt.Errorf("host image must be an Azure resource ID or publisher:offer:sku:version reference")
	}

	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
		if parts[i] == "" {
			return nil, fmt.Errorf("host image publisher, offer, SKU, and version must be non-empty")
		}
	}

	return &armcompute.ImageReference{
		Publisher: to.Ptr(parts[0]),
		Offer:     to.Ptr(parts[1]),
		SKU:       to.Ptr(parts[2]),
		Version:   to.Ptr(parts[3]),
	}, nil
}

func validateAzureCustomData(userData string) error {
	if len([]byte(userData)) > azureCustomDataMaxBytes {
		return fmt.Errorf("cloud-init is %d bytes, exceeding Azure customData limit of %d bytes", len([]byte(userData)), azureCustomDataMaxBytes)
	}

	return nil
}

// Azure error codes that say the addressed resource, or the resource group
// that would hold it, does not exist.
var azureNotFoundCodes = []string{
	"ResourceNotFound",
	"ResourceGroupNotFound",
	"NotFound",
}

// isAzureNotFound reports that Azure said the addressed resource is absent.
// The status alone is not enough: a 404 with another error code, such as
// SubscriptionNotFound, or with no Azure error code at all, such as one
// answered by an intermediary, says nothing about the resource and is an
// error.
func isAzureNotFound(err error) bool {
	responseErr, ok := errors.AsType[*azcore.ResponseError](err)
	if !ok || responseErr.StatusCode != http.StatusNotFound {
		return false
	}

	return slices.ContainsFunc(azureNotFoundCodes, func(code string) bool {
		return strings.EqualFold(responseErr.ErrorCode, code)
	})
}

func toValue(value *string, fallback string) string {
	if value == nil || *value == "" {
		return fallback
	}

	return *value
}
