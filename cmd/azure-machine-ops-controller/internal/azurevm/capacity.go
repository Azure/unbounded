// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"k8s.io/apimachinery/pkg/types"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
)

// Azure tag keys stamped on every VM this adapter creates. Azure tag names
// cannot contain '/', so these do not reuse the Kubernetes label keys.
const (
	TagAllocationUID  = "unbounded-allocation-uid"
	TagAllocationName = "unbounded-allocation-name"
	TagMachineClass   = "unbounded-machine-class"
	TagManagedBy      = "unbounded-managed-by"

	managedByValue = "unbounded-machine-ops"
)

const (
	vmNamePrefix     = "ub-"
	nicNameSuffix    = "-nic"
	osDiskNameSuffix = "-osdisk"
	pollInterval     = 15 * time.Second
	// createVisibilityGrace is how long an accepted VM create may stay
	// invisible to a read before the VM is treated as absent.
	createVisibilityGrace = 2 * time.Minute
)

var allocationUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Azure error codes that mean the requested capacity cannot be placed now.
var insufficientCapacityCodes = []string{
	"AllocationFailed",
	"ZonalAllocationFailed",
	"OverconstrainedAllocationRequest",
	"OverconstrainedZonalAllocationRequest",
	"SkuNotAvailable",
}

// Azure 409 codes that refuse a VM create outright: Azure accepted nothing,
// and repeating the same request cannot succeed until an operator changes the
// subscription, the resource group or the class. Every other 409 stays
// retryable, because a conflict can also mean that an operation on the VM is
// already in progress, in which case something may exist.
var definitiveConflictCodes = []string{
	"OperationNotAllowed",
	"MissingSubscriptionRegistration",
	"ReadOnlyDisabledSubscription",
	"ScopeLocked",
	"ResourceGroupBeingDeleted",
}

// VMName returns the deterministic VM, Machine, and Node name for an
// allocation: the full allocation UID without dashes, so names never collide
// across allocations.
func VMName(uid types.UID) (string, error) {
	if !allocationUIDPattern.MatchString(string(uid)) {
		return "", fmt.Errorf("allocation UID %q is not a lowercase UUID", uid)
	}

	return vmNamePrefix + strings.ReplaceAll(string(uid), "-", ""), nil
}

func vmResourceID(subscriptionID, resourceGroup, name string) string {
	return fmt.Sprintf(
		"/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/virtualMachines/%s",
		subscriptionID,
		resourceGroup,
		name,
	)
}

// capacityRegistration returns the provider options that make this adapter
// a capacity provider. Allocation and release are long-running so the engine
// persists the deterministic VM ID and resumes observation after restarts.
// Replay is keyed on the allocation UID: the VM name is derived from it and
// the VM carries it as a tag, so a Begin repeated for any operation UID of
// the same allocation finds the same VM.
func (p *Provider) capacityRegistration() []machineops.ProviderOption {
	return []machineops.ProviderOption{
		machineops.WithCapacity(p.ResolveClass, p.PlanAllocation),
		machineops.WithLongRunningOperation(unboundedv1alpha3.OperationHostAllocate, p.BeginAllocate, p.PollAllocate),
		machineops.WithLongRunningOperation(unboundedv1alpha3.OperationHostRelease, p.BeginRelease, p.PollRelease),
	}
}

func providerConflict(err error) error {
	return &machineops.PermanentError{
		Reason: string(unboundedv1alpha3.FailureProviderConflict),
		Err:    err,
	}
}

// beforeCreate classifies a failed Azure read that precedes the VM create.
// Nothing exists yet, so a read Azure refuses for the credential ends the
// allocation as AuthFailed instead of being retried without end; every other
// failure is returned unchanged and retried.
func beforeCreate(err error) error {
	if _, ok := errors.AsType[*azidentity.AuthenticationFailedError](err); ok {
		return authFailed(err)
	}

	responseErr, ok := errors.AsType[*azcore.ResponseError](err)
	if !ok {
		return err
	}

	if responseErr.StatusCode == http.StatusUnauthorized || responseErr.StatusCode == http.StatusForbidden {
		return authFailed(err)
	}

	return err
}

func authFailed(err error) error {
	return &machineops.PermanentError{
		Reason: string(unboundedv1alpha3.FailureAuthFailed),
		Err:    err,
	}
}

// classifyCreateRejection reports whether Azure definitively refused the VM
// create, and with which normalized reason. A refusal is definitive when
// Azure accepted nothing and repeating the request cannot help.
//
// Azure reports capacity and quota rejections as 409, so the error code is
// checked before the status. After that:
//   - 401 and 403 are credential or authorization failures.
//   - 408, 429 and 5xx are retried: the request may still succeed or may
//     already have been applied.
//   - 409 is definitive only for the codes in definitiveConflictCodes. Any
//     other conflict is retried, because it can mean that an operation on the
//     VM is already in progress; the next Begin then finds and adopts the VM.
//   - Every other 4xx, including a 404 for a missing resource group, image or
//     subnet, means the request as written cannot be satisfied.
//
// A transport failure carries no Azure answer and is retried.
func classifyCreateRejection(err error) (createRejection, bool) {
	responseErr, ok := errors.AsType[*azcore.ResponseError](err)
	if !ok {
		return createRejection{}, false
	}

	status := responseErr.StatusCode
	reason := classifyAzureCode(responseErr.ErrorCode, err.Error())

	switch {
	case reason != unboundedv1alpha3.FailureProviderFailed:
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		reason = unboundedv1alpha3.FailureAuthFailed
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests:
		return createRejection{}, false
	case status == http.StatusConflict:
		if !containsFoldString(definitiveConflictCodes, responseErr.ErrorCode) {
			return createRejection{}, false
		}
	case status >= 400 && status < 500:
		reason = unboundedv1alpha3.FailureInvalidRequest
	default:
		return createRejection{}, false
	}

	return createRejection{
		Reason: reason,
		Code:   responseErr.ErrorCode,
		Status: status,
	}, true
}

func containsFoldString(values []string, want string) bool {
	return slices.ContainsFunc(values, func(value string) bool {
		return strings.EqualFold(value, want)
	})
}

func classifyAzureCode(code, message string) unboundedv1alpha3.MachineAllocationFailureReason {
	for _, capacityCode := range insufficientCapacityCodes {
		if strings.EqualFold(code, capacityCode) {
			return unboundedv1alpha3.FailureInsufficientCapacity
		}
	}

	if strings.EqualFold(code, "QuotaExceeded") {
		return unboundedv1alpha3.FailureQuotaExceeded
	}

	if strings.EqualFold(code, "OperationNotAllowed") && strings.Contains(strings.ToLower(message), "quota") {
		return unboundedv1alpha3.FailureQuotaExceeded
	}

	if strings.EqualFold(code, "AuthorizationFailed") || strings.EqualFold(code, "LinkedAuthorizationFailed") {
		return unboundedv1alpha3.FailureAuthFailed
	}

	return unboundedv1alpha3.FailureProviderFailed
}

// failedPoll is a failed poll without an allocation report. A HostAllocate
// that returns it unchanged declares that it cannot say what the allocation
// owns.
func failedPoll(reason unboundedv1alpha3.MachineAllocationFailureReason, message string) machineops.PollResult {
	return machineops.PollResult{
		State:   machineops.ProviderOperationStateFailed,
		Reason:  string(reason),
		Message: message,
	}
}
