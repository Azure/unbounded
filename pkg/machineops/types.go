// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// OperationRequest is the generic provider-facing view of a MachineOperation.
type OperationRequest struct {
	MachineName       string
	MachineUID        types.UID
	MachineGeneration int64
	OperationName     string
	OperationUID      types.UID
	// ProviderRef snapshots Machine.spec.host.external.machineRef when present.
	ProviderRef *unboundedv1alpha3.ProviderMachineSnapshot
	// ProviderID is the current canonical or legacy provider-specific host ID.
	ProviderID string
	HostImage  string
	Operation  unboundedv1alpha3.OperationKind
	Parameters map[string]string
	// ReplaceUserData is the bootstrap data for the host that replaces this
	// one. It is set only for a HostReplace operation registered with
	// RequiresReplaceUserData, and it carries a bootstrap token. It is never
	// empty in Execute or Begin; in Poll it is empty when it could not be
	// built.
	ReplaceUserData string
	Auth            *OperationAuth
	// Allocation is the allocation input for HostAllocate and HostRelease. It
	// is nil for every other operation kind.
	Allocation *AllocationRequest
}

// OperationAuth is the provider-facing credential material resolved for an
// operation. Provider packages own the interpretation of SecretData.
type OperationAuth struct {
	Mode       unboundedv1alpha3.MachineOperationCredentialAuthMode
	SecretData map[string]string
}

// RequiredSecretValue returns a non-empty provider-specific secret value.
func (a *OperationAuth) RequiredSecretValue(key string) (string, error) {
	if a == nil || a.SecretData == nil {
		return "", fmt.Errorf("auth secret data is required")
	}

	value := a.SecretData[key]
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("auth secret data key %q is required", key)
	}

	return value, nil
}

// OperationResult describes provider-side changes that must be reflected after
// execution, such as replacement of an underlying cloud resource identity.
type OperationResult struct {
	ProviderID        string
	CleanupProviderID string
	// Allocation is the allocation result reported by HostAllocate. It is
	// nil for every other operation kind. Every terminal HostAllocate Poll
	// result must set it, as AllocationResult describes.
	Allocation *AllocationResult
}

// ProviderOperation is the provider-neutral representation of a resumable
// external operation: the handle Begin returns and every Poll receives.
type ProviderOperation struct {
	// OperationID identifies the operation. It is required: the engine
	// treats a Begin that returns it empty as a retryable Begin error.
	OperationID string
	// ResumeToken is opaque provider data. It is the only state the engine
	// keeps for Poll, and Poll cannot change it. It is stored in the
	// MachineOperation status, so it must hold no secrets. The API sets no
	// maximum length, but the status is one etcd object: keep the token
	// small and bound its size.
	ResumeToken string
}

// BeginResult contains the durable handle returned by an idempotent Begin
// callback. The controller may invoke Begin again with the same OperationUID
// until a handle for the same logical operation has been persisted.
type BeginResult struct {
	Operation ProviderOperation
	Message   string
	// RequeueAfter is the wait before the first Poll. Zero means the
	// controller's poll interval.
	RequeueAfter time.Duration
}

// ProviderOperationState is the provider-neutral lifecycle of a resumable
// operation.
type ProviderOperationState string

const (
	ProviderOperationStateInProgress ProviderOperationState = "InProgress"
	ProviderOperationStateSucceeded  ProviderOperationState = "Succeeded"
	ProviderOperationStateFailed     ProviderOperationState = "Failed"
	ProviderOperationStateCanceled   ProviderOperationState = "Canceled"
)

// PollResult describes one status observation for a resumable provider
// operation.
type PollResult struct {
	State  ProviderOperationState
	Result OperationResult
	// Reason is the failure reason of a Failed or Canceled result. Empty
	// means the State. It follows the rules of PermanentError.Reason.
	Reason  string
	Message string
	// RequeueAfter is the wait before the next Poll of an InProgress
	// result. Zero means the controller's poll interval. It is ignored once
	// the operation is stalled.
	RequeueAfter time.Duration
}

// PermanentError reports that retrying a provider callback cannot make
// progress. Providers should return ordinary errors for transient failures.
//
// From Begin and from Poll it ends the operation. ResolveClassFunc and
// PlanAllocationFunc document their own use of it. An immediate operation's
// Execute has no use for it: every Execute error is terminal and Reason is
// ignored (see ExecuteFunc).
type PermanentError struct {
	// Reason becomes the reason of the operation's Completed condition.
	// Empty means PermanentProviderFailure from Begin and
	// PermanentPollFailure from Poll.
	//
	// A host operation's reason is free-form. The engine makes it a valid
	// condition reason, so use CamelCase without spaces or punctuation.
	//
	// A HostAllocate reason should be a
	// v1alpha3.MachineAllocationFailureReason value. The MachineAllocation
	// reports any other reason as AuthFailed when it starts with Auth and
	// otherwise, in almost every case, as ProviderFailed.
	Reason string
	Err    error
}

func (e *PermanentError) Error() string {
	if e == nil || e.Err == nil {
		return "permanent provider failure"
	}

	return e.Err.Error()
}

func (e *PermanentError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}
