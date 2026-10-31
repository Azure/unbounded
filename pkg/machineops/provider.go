// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// OperationMode identifies how the lifecycle controller executes an operation.
type OperationMode string

const (
	// OperationModeImmediate executes an operation in one provider callback.
	OperationModeImmediate OperationMode = "Immediate"

	// OperationModeLongRunning starts an operation and polls its persisted handle.
	OperationModeLongRunning OperationMode = "LongRunning"
)

// ExecuteFunc executes an immediate provider operation.
type ExecuteFunc func(context.Context, OperationRequest) (OperationResult, error)

// BeginFunc starts a long-running provider operation. It must be idempotent for
// a stable OperationRequest.OperationUID: repeated calls must return a usable
// handle for the same logical operation until that handle has been persisted.
type BeginFunc func(context.Context, OperationRequest) (BeginResult, error)

// PollFunc observes a previously accepted long-running provider operation.
// Ordinary errors are retried with the same persisted handle. A PermanentError
// terminates the MachineOperation.
type PollFunc func(context.Context, OperationRequest, ProviderOperation) (PollResult, error)

// CleanupFunc performs optional provider cleanup after an operation result has
// been applied to the Machine.
type CleanupFunc func(context.Context, OperationRequest, OperationResult) error

// Provider is an immutable registration of one provider's supported
// MachineOperation lifecycle strategies.
type Provider struct {
	name                string
	providerMachineKind *schema.GroupKind
	operations          map[unboundedv1alpha3.OperationKind]*Operation
}

// WithProviderMachineKind declares the provider-owned Machine GroupKind
// accepted through Machine.spec.host.external.machineRef. Providers may also
// accept host.external.providerID or the deprecated Machine.spec.providerID.
func WithProviderMachineKind(groupKind schema.GroupKind) ProviderOption {
	return providerOptionFunc(func(provider *Provider) error {
		if strings.TrimSpace(groupKind.Group) == "" {
			return fmt.Errorf("provider Machine API group is required")
		}

		if strings.TrimSpace(groupKind.Kind) == "" {
			return fmt.Errorf("provider Machine kind is required")
		}

		if provider.providerMachineKind != nil {
			return fmt.Errorf("provider Machine kind is already registered for provider %q", provider.name)
		}

		provider.providerMachineKind = &groupKind

		return nil
	})
}

// Operation is an immutable lifecycle strategy registered for one operation
// kind.
type Operation struct {
	mode                    OperationMode
	execute                 ExecuteFunc
	begin                   BeginFunc
	poll                    PollFunc
	cleanup                 CleanupFunc
	replaySafe              bool
	requiresReplaceUserData bool
}

// ProviderOption configures a Provider registration.
type ProviderOption interface {
	applyProvider(*Provider) error
}

type providerOptionFunc func(*Provider) error

func (f providerOptionFunc) applyProvider(provider *Provider) error {
	return f(provider)
}

// OperationOption configures one registered operation.
type OperationOption interface {
	applyOperation(*Operation) error
}

type operationOptionFunc func(*Operation) error

func (f operationOptionFunc) applyOperation(operation *Operation) error {
	return f(operation)
}

// NewProvider validates and constructs an immutable provider registration.
func NewProvider(name string, options ...ProviderOption) (*Provider, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("provider name is required")
	}

	provider := &Provider{
		name:       name,
		operations: make(map[unboundedv1alpha3.OperationKind]*Operation),
	}

	for i, option := range options {
		if option == nil {
			return nil, fmt.Errorf("provider option %d is nil", i)
		}

		if err := option.applyProvider(provider); err != nil {
			return nil, err
		}
	}

	if len(provider.operations) == 0 {
		return nil, fmt.Errorf("provider %q must register at least one operation", name)
	}

	return provider, nil
}

// WithImmediateOperation registers an operation executed by one callback.
func WithImmediateOperation(
	kind unboundedv1alpha3.OperationKind,
	execute ExecuteFunc,
	options ...OperationOption,
) ProviderOption {
	return providerOptionFunc(func(provider *Provider) error {
		operation := &Operation{mode: OperationModeImmediate, execute: execute}
		if err := configureOperation(kind, operation, options); err != nil {
			return err
		}

		return registerOperation(provider, kind, operation)
	})
}

// WithLongRunningOperation registers an operation executed as begin and poll
// callbacks.
func WithLongRunningOperation(
	kind unboundedv1alpha3.OperationKind,
	begin BeginFunc,
	poll PollFunc,
	options ...OperationOption,
) ProviderOption {
	return providerOptionFunc(func(provider *Provider) error {
		operation := &Operation{
			mode:  OperationModeLongRunning,
			begin: begin,
			poll:  poll,
		}
		if err := configureOperation(kind, operation, options); err != nil {
			return err
		}

		return registerOperation(provider, kind, operation)
	})
}

// ReplaySafe declares that an immediate operation may be executed again when
// reconciliation resumes after its phase was persisted as InProgress.
func ReplaySafe() OperationOption {
	return operationOptionFunc(func(operation *Operation) error {
		operation.replaySafe = true

		return nil
	})
}

// RequiresReplaceUserData declares that the lifecycle controller must build
// host replacement bootstrap data before invoking the provider callback.
func RequiresReplaceUserData() OperationOption {
	return operationOptionFunc(func(operation *Operation) error {
		operation.requiresReplaceUserData = true

		return nil
	})
}

// WithCleanup registers optional cleanup performed after the controller has
// applied the provider operation result.
func WithCleanup(cleanup CleanupFunc) OperationOption {
	return operationOptionFunc(func(operation *Operation) error {
		if cleanup == nil {
			return fmt.Errorf("cleanup function is required")
		}

		operation.cleanup = cleanup

		return nil
	})
}

// Name returns the external provider name matched against
// Machine.spec.host.external.provider or a deprecated Machine.spec.provider.
func (p *Provider) Name() string {
	if p == nil {
		return ""
	}

	return p.name
}

// ProviderMachineKind returns the provider-owned Machine GroupKind declared by
// this registration.
func (p *Provider) ProviderMachineKind() (schema.GroupKind, bool) {
	if p == nil || p.providerMachineKind == nil {
		return schema.GroupKind{}, false
	}

	return *p.providerMachineKind, true
}

// Operation returns the lifecycle strategy registered for kind.
func (p *Provider) Operation(kind unboundedv1alpha3.OperationKind) (*Operation, bool) {
	if p == nil {
		return nil, false
	}

	operation, ok := p.operations[kind]

	return operation, ok
}

// Mode returns the lifecycle strategy used for the operation.
func (o *Operation) Mode() OperationMode {
	if o == nil {
		return ""
	}

	return o.mode
}

// ReplaySafe reports whether an immediate operation may be executed again
// after reconciliation resumes from InProgress.
func (o *Operation) ReplaySafe() bool {
	return o != nil && o.replaySafe
}

// RequiresReplaceUserData reports whether replacement bootstrap data is needed.
func (o *Operation) RequiresReplaceUserData() bool {
	return o != nil && o.requiresReplaceUserData
}

// Execute invokes the registered immediate operation callback.
func (o *Operation) Execute(ctx context.Context, request OperationRequest) (OperationResult, error) {
	if o == nil || o.mode != OperationModeImmediate || o.execute == nil {
		return OperationResult{}, fmt.Errorf("immediate operation callback is not registered")
	}

	return o.execute(ctx, request)
}

// Begin invokes the registered long-running operation begin callback.
func (o *Operation) Begin(ctx context.Context, request OperationRequest) (BeginResult, error) {
	if o == nil || o.mode != OperationModeLongRunning || o.begin == nil {
		return BeginResult{}, fmt.Errorf("long-running begin callback is not registered")
	}

	return o.begin(ctx, request)
}

// Poll invokes the registered long-running operation poll callback.
func (o *Operation) Poll(ctx context.Context, request OperationRequest, operation ProviderOperation) (PollResult, error) {
	if o == nil || o.mode != OperationModeLongRunning || o.poll == nil {
		return PollResult{}, fmt.Errorf("long-running poll callback is not registered")
	}

	return o.poll(ctx, request, operation)
}

// Cleanup invokes the operation's optional cleanup callback.
func (o *Operation) Cleanup(ctx context.Context, request OperationRequest, result OperationResult) error {
	if o == nil || o.cleanup == nil {
		return nil
	}

	return o.cleanup(ctx, request, result)
}

func configureOperation(
	kind unboundedv1alpha3.OperationKind,
	operation *Operation,
	options []OperationOption,
) error {
	if strings.TrimSpace(string(kind)) == "" {
		return fmt.Errorf("operation kind is required")
	}

	for i, option := range options {
		if option == nil {
			return fmt.Errorf("option %d for operation %q is nil", i, kind)
		}

		if err := option.applyOperation(operation); err != nil {
			return fmt.Errorf("configure operation %q: %w", kind, err)
		}
	}

	switch operation.mode {
	case OperationModeImmediate:
		if operation.execute == nil {
			return fmt.Errorf("execute function is required for immediate operation %q", kind)
		}
	case OperationModeLongRunning:
		if operation.begin == nil {
			return fmt.Errorf("begin function is required for long-running operation %q", kind)
		}

		if operation.poll == nil {
			return fmt.Errorf("poll function is required for long-running operation %q", kind)
		}
	default:
		return fmt.Errorf("operation %q has unknown mode %q", kind, operation.mode)
	}

	if operation.replaySafe && operation.mode != OperationModeImmediate {
		return fmt.Errorf("ReplaySafe is valid only for immediate operations, not %q", kind)
	}

	if operation.requiresReplaceUserData && kind != unboundedv1alpha3.OperationHostReplace {
		return fmt.Errorf("replacement user data is valid only for HostReplace, not %q", kind)
	}

	return nil
}

func registerOperation(provider *Provider, kind unboundedv1alpha3.OperationKind, operation *Operation) error {
	if _, exists := provider.operations[kind]; exists {
		return fmt.Errorf("operation %q is already registered for provider %q", kind, provider.name)
	}

	provider.operations[kind] = operation

	return nil
}
