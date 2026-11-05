// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/machineops"
)

func TestAddCapacityToManagerRequiresManager(t *testing.T) {
	t.Parallel()

	provider := newTestCapacityProvider(t, "ExampleCloud")

	err := AddCapacityToManager(nil, []*machineops.Provider{provider}, validCapacityOptions())
	require.ErrorContains(t, err, "manager is required")
}

func TestSelectCapacityProvidersSelectsRegistrations(t *testing.T) {
	t.Parallel()

	cloudA := newTestCapacityProvider(t, "CloudA")
	cloudB := newTestCapacityProvider(t, "CloudB")
	plain := newTestProvider(t, "PlainCloud")

	tests := []struct {
		name         string
		providers    []*machineops.Provider
		providerName string
		want         []*machineops.Provider
	}{
		{name: "all capacity providers", providers: []*machineops.Provider{cloudA, cloudB}, want: []*machineops.Provider{cloudA, cloudB}},
		{name: "only scoped provider", providers: []*machineops.Provider{cloudA, cloudB, plain}, providerName: "CloudB", want: []*machineops.Provider{cloudB}},
		{name: "host-operation-only registrations skipped", providers: []*machineops.Provider{plain, cloudA, cloudB}, want: []*machineops.Provider{cloudA, cloudB}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			options := validCapacityOptions()
			options.ProviderName = tt.providerName

			selected, err := selectCapacityProviders(tt.providers, options)
			require.NoError(t, err)
			require.Equal(t, tt.want, selected)
		})
	}
}

func TestSelectCapacityProvidersRejectsInvalidRegistrations(t *testing.T) {
	t.Parallel()

	cloud := newTestCapacityProvider(t, "ExampleCloud")
	plain := newTestProvider(t, "PlainCloud")

	tests := []struct {
		name         string
		providers    []*machineops.Provider
		providerName string
		mutate       func(*CapacityOptions)
		message      string
	}{
		{name: "none", message: "at least one"},
		{name: "nil", providers: []*machineops.Provider{nil}, message: "nil"},
		{name: "duplicate", providers: []*machineops.Provider{cloud, cloud}, message: "more than once"},
		{name: "scope missing", providers: []*machineops.Provider{cloud}, providerName: "OtherCloud", message: "not registered"},
		{name: "scoped provider without capacity", providers: []*machineops.Provider{cloud, plain}, providerName: "PlainCloud", message: `"PlainCloud" does not declare capacity`},
		{name: "no capacity provider", providers: []*machineops.Provider{plain}, message: "no provider registration declares capacity"},
		{
			name:      "provider config namespace missing",
			providers: []*machineops.Provider{cloud},
			mutate:    func(options *CapacityOptions) { options.ProviderConfigNamespace = "" },
			message:   "ProviderConfigNamespace is required",
		},
		{
			name:      "credential secret namespace missing",
			providers: []*machineops.Provider{cloud},
			mutate:    func(options *CapacityOptions) { options.CredentialSecretNamespace = "" },
			message:   "CredentialSecretNamespace is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			options := validCapacityOptions()
			options.ProviderName = tt.providerName

			if tt.mutate != nil {
				tt.mutate(&options)
			}

			selected, err := selectCapacityProviders(tt.providers, options)
			require.ErrorContains(t, err, tt.message)
			require.Nil(t, selected)
		})
	}
}

func TestSelectCapacityProvidersValidatesIntervals(t *testing.T) {
	t.Parallel()

	cloud := newTestCapacityProvider(t, "ExampleCloud")

	tests := []struct {
		name                 string
		classRefreshInterval time.Duration
		minRequeueInterval   time.Duration
		message              string
	}{
		{name: "both unset"},
		{name: "refresh at default floor", classRefreshInterval: 10 * time.Second},
		{name: "both at lower bound", classRefreshInterval: time.Second, minRequeueInterval: time.Second},
		{name: "equal values", classRefreshInterval: 30 * time.Second, minRequeueInterval: 30 * time.Second},
		{name: "floor at default refresh", minRequeueInterval: 5 * time.Minute},
		{name: "negative refresh", classRefreshInterval: -time.Second, message: "ClassRefreshInterval -1s must not be negative"},
		{name: "negative floor", minRequeueInterval: -time.Second, message: "MinRequeueInterval -1s must not be negative"},
		{name: "floor below lower bound", minRequeueInterval: 500 * time.Millisecond, message: "MinRequeueInterval 500ms must be at least 1s"},
		{name: "refresh below default floor", classRefreshInterval: 9 * time.Second, message: "ClassRefreshInterval 9s must be at least the effective MinRequeueInterval 10s"},
		{name: "refresh below custom floor", classRefreshInterval: 20 * time.Second, minRequeueInterval: 30 * time.Second, message: "ClassRefreshInterval 20s must be at least the effective MinRequeueInterval 30s"},
		{name: "floor above default refresh", minRequeueInterval: 6 * time.Minute, message: "ClassRefreshInterval 5m0s must be at least the effective MinRequeueInterval 6m0s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			options := validCapacityOptions()
			options.ClassRefreshInterval = tt.classRefreshInterval
			options.MinRequeueInterval = tt.minRequeueInterval

			selected, err := selectCapacityProviders([]*machineops.Provider{cloud}, options)
			if tt.message == "" {
				require.NoError(t, err)
				require.Equal(t, []*machineops.Provider{cloud}, selected)

				return
			}

			require.ErrorContains(t, err, tt.message)
			require.Nil(t, selected)
		})
	}
}

func TestMinimalCapacityMainImportsOnlyPublicPackages(t *testing.T) {
	t.Parallel()

	const module = "github.com/Azure/unbounded"

	allowed := map[string]bool{
		module + "/api/machina/v1alpha3":      true,
		module + "/pkg/machineops":            true,
		module + "/pkg/machineops/controller": true,
	}

	file, err := parser.ParseFile(token.NewFileSet(), "capacity_envtest_test.go", nil, parser.ImportsOnly)
	require.NoError(t, err)

	imported := false

	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		require.NoError(t, err)

		if path != module && !strings.HasPrefix(path, module+"/") {
			continue
		}

		require.True(t, allowed[path], "capacity_envtest_test.go imports non-public package %s", path)

		if path == module+"/pkg/machineops/controller" {
			imported = true
		}
	}

	require.True(t, imported, "capacity_envtest_test.go does not import pkg/machineops/controller")
}

func validCapacityOptions() CapacityOptions {
	return CapacityOptions{
		CredentialSecretNamespace: "unbounded-system",
		ProviderConfigNamespace:   "capacity-config",
	}
}

func newTestCapacityProvider(t *testing.T, name string) *machineops.Provider {
	t.Helper()

	begin := func(context.Context, machineops.OperationRequest) (machineops.BeginResult, error) {
		return machineops.BeginResult{}, nil
	}
	poll := func(context.Context, machineops.OperationRequest, machineops.ProviderOperation) (machineops.PollResult, error) {
		return machineops.PollResult{}, nil
	}

	provider, err := machineops.NewProvider(
		name,
		machineops.WithCapacity(
			func(context.Context, machineops.ClassRequest) (machineops.ClassResolution, error) {
				return machineops.ClassResolution{}, nil
			},
			func(context.Context, machineops.AllocationPlanRequest) (machineops.AllocationPlan, error) {
				return machineops.AllocationPlan{}, nil
			},
		),
		machineops.WithLongRunningOperation(unboundedv1alpha3.OperationHostAllocate, begin, poll),
		machineops.WithLongRunningOperation(unboundedv1alpha3.OperationHostRelease, begin, poll),
	)
	require.NoError(t, err)

	return provider
}
