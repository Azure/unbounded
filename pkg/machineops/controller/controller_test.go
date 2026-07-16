// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
)

func TestValidateProvidersAcceptsUniqueRegistrations(t *testing.T) {
	t.Parallel()

	provider := newTestProvider(t, "ExampleProvider")

	require.NoError(t, validateProviders([]*machineops.Provider{provider}, provider.Name()))
}

func TestValidateProvidersRejectsInvalidRegistrations(t *testing.T) {
	t.Parallel()

	provider := newTestProvider(t, "ExampleProvider")

	tests := []struct {
		name      string
		providers []*machineops.Provider
		scope     string
		message   string
	}{
		{name: "none", message: "at least one"},
		{name: "nil", providers: []*machineops.Provider{nil}, message: "nil"},
		{name: "duplicate", providers: []*machineops.Provider{provider, provider}, message: "more than once"},
		{name: "scope missing", providers: []*machineops.Provider{provider}, scope: "OtherProvider", message: "not registered"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateProviders(tt.providers, tt.scope)
			require.ErrorContains(t, err, tt.message)
		})
	}
}

func newTestProvider(t *testing.T, name string) *machineops.Provider {
	t.Helper()

	provider, err := machineops.NewProvider(
		name,
		machineops.WithImmediateOperation(
			unboundedv1alpha3.OperationHostPowerOn,
			func(context.Context, machineops.OperationRequest) (machineops.OperationResult, error) {
				return machineops.OperationResult{}, nil
			},
		),
	)
	require.NoError(t, err)

	return provider
}
