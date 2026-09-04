// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestNewProviderRegistersMixedOperationStrategies(t *testing.T) {
	t.Parallel()

	execute := func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{}, nil
	}
	begin := func(context.Context, OperationRequest) (BeginResult, error) {
		return BeginResult{}, nil
	}
	poll := func(context.Context, OperationRequest, ProviderOperation) (PollResult, error) {
		return PollResult{}, nil
	}

	provider, err := NewProvider(
		"ExampleCloud",
		WithProviderMachineKind(schema.GroupKind{Group: "infrastructure.example.io", Kind: "ExampleMachine"}),
		WithImmediateOperation(
			unboundedv1alpha3.OperationHostReplace,
			execute,
			ReplaySafe(),
			RequiresReplaceUserData(),
		),
		WithLongRunningOperation(
			unboundedv1alpha3.OperationHostReboot,
			begin,
			poll,
		),
	)
	require.NoError(t, err)
	require.Equal(t, "ExampleCloud", provider.Name())
	require.Equal(t, schema.GroupKind{Group: "infrastructure.example.io", Kind: "ExampleMachine"}, mustProviderMachineKind(t, provider))

	immediate, ok := provider.Operation(unboundedv1alpha3.OperationHostReplace)
	require.True(t, ok)
	require.Equal(t, OperationModeImmediate, immediate.Mode())
	require.True(t, immediate.ReplaySafe())
	require.True(t, immediate.RequiresReplaceUserData())

	longRunning, ok := provider.Operation(unboundedv1alpha3.OperationHostReboot)
	require.True(t, ok)
	require.Equal(t, OperationModeLongRunning, longRunning.Mode())
	require.False(t, longRunning.ReplaySafe())
	require.False(t, longRunning.RequiresReplaceUserData())

	_, ok = provider.Operation(unboundedv1alpha3.OperationNodeReboot)
	require.False(t, ok)
}

func TestNewProviderRejectsInvalidRegistrations(t *testing.T) {
	t.Parallel()

	execute := func(context.Context, OperationRequest) (OperationResult, error) {
		return OperationResult{}, nil
	}

	tests := []struct {
		name    string
		make    func() (*Provider, error)
		message string
	}{
		{name: "empty name", make: func() (*Provider, error) {
			return NewProvider("", WithImmediateOperation(unboundedv1alpha3.OperationHostPowerOn, execute))
		}, message: "name"},
		{name: "no operations", make: func() (*Provider, error) {
			return NewProvider("ExampleCloud")
		}, message: "operation"},
		{name: "empty provider Machine group", make: func() (*Provider, error) {
			return NewProvider(
				"ExampleCloud",
				WithProviderMachineKind(schema.GroupKind{Kind: "ExampleMachine"}),
				WithImmediateOperation(unboundedv1alpha3.OperationHostPowerOn, execute),
			)
		}, message: "API group"},
		{name: "empty provider Machine kind", make: func() (*Provider, error) {
			return NewProvider(
				"ExampleCloud",
				WithProviderMachineKind(schema.GroupKind{Group: "infrastructure.example.io"}),
				WithImmediateOperation(unboundedv1alpha3.OperationHostPowerOn, execute),
			)
		}, message: "kind"},
		{name: "duplicate provider Machine kind", make: func() (*Provider, error) {
			return NewProvider(
				"ExampleCloud",
				WithProviderMachineKind(schema.GroupKind{Group: "infrastructure.example.io", Kind: "ExampleMachine"}),
				WithProviderMachineKind(schema.GroupKind{Group: "infrastructure.example.io", Kind: "OtherMachine"}),
				WithImmediateOperation(unboundedv1alpha3.OperationHostPowerOn, execute),
			)
		}, message: "already registered"},
		{name: "duplicate operation", make: func() (*Provider, error) {
			return NewProvider(
				"ExampleCloud",
				WithImmediateOperation(unboundedv1alpha3.OperationHostPowerOn, execute),
				WithImmediateOperation(unboundedv1alpha3.OperationHostPowerOn, execute),
			)
		}, message: "already registered"},
		{name: "nil immediate function", make: func() (*Provider, error) {
			return NewProvider("ExampleCloud", WithImmediateOperation(unboundedv1alpha3.OperationHostPowerOn, nil))
		}, message: "execute"},
		{name: "missing begin function", make: func() (*Provider, error) {
			return NewProvider("ExampleCloud", WithLongRunningOperation(
				unboundedv1alpha3.OperationHostReplace,
				nil,
				func(context.Context, OperationRequest, ProviderOperation) (PollResult, error) {
					return PollResult{}, nil
				},
			))
		}, message: "begin"},
		{name: "replacement data on non-replace operation", make: func() (*Provider, error) {
			return NewProvider(
				"ExampleCloud",
				WithImmediateOperation(
					unboundedv1alpha3.OperationHostPowerOn,
					execute,
					RequiresReplaceUserData(),
				),
			)
		}, message: "HostReplace"},
		{name: "replay safe long-running operation", make: func() (*Provider, error) {
			return NewProvider(
				"ExampleCloud",
				WithLongRunningOperation(
					unboundedv1alpha3.OperationHostReboot,
					func(context.Context, OperationRequest) (BeginResult, error) { return BeginResult{}, nil },
					func(context.Context, OperationRequest, ProviderOperation) (PollResult, error) {
						return PollResult{}, nil
					},
					ReplaySafe(),
				),
			)
		}, message: "immediate"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := tt.make()
			require.ErrorContains(t, err, tt.message)
		})
	}
}

func mustProviderMachineKind(t *testing.T, provider *Provider) schema.GroupKind {
	t.Helper()

	groupKind, ok := provider.ProviderMachineKind()
	require.True(t, ok)

	return groupKind
}
