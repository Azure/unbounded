// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// OperationRequest is the generic provider-facing view of a MachineOperation.
type OperationRequest struct {
	Machine         *unboundedv1alpha3.Machine
	OperationName   string
	OperationUID    types.UID
	ProviderID      string
	Operation       unboundedv1alpha3.OperationKind
	Parameters      map[string]string
	ReplaceUserData string
	Auth            *OperationAuth
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

// Provider executes MachineOperation requests for a specific external provider.
type Provider interface {
	Name() string
	Supports(operation unboundedv1alpha3.OperationKind) bool
	Execute(ctx context.Context, request OperationRequest) (OperationResult, error)
	Cleanup(ctx context.Context, request OperationRequest, result OperationResult) error
}
