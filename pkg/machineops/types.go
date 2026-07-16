// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
	ProviderRef       *unboundedv1alpha3.ProviderMachineSnapshot
	ProviderID        string
	HostImage         string
	Operation         unboundedv1alpha3.OperationKind
	Parameters        map[string]string
	ReplaceUserData   string
	Auth              *OperationAuth
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
}

// ProviderOperation is the provider-neutral representation of a resumable
// external operation.
type ProviderOperation struct {
	OperationID string
	ResumeToken string
}

// BeginResult contains the durable handle returned by an idempotent Begin
// callback. The controller may invoke Begin again with the same OperationUID
// until a handle for the same logical operation has been persisted.
type BeginResult struct {
	Operation    ProviderOperation
	Message      string
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
	State        ProviderOperationState
	Result       OperationResult
	Reason       string
	Message      string
	RequeueAfter time.Duration
}

// PermanentError reports that retrying a provider callback cannot make
// progress. Providers should return ordinary errors for transient failures.
type PermanentError struct {
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
