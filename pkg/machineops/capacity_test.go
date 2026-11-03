// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func testExecute(context.Context, OperationRequest) (OperationResult, error) {
	return OperationResult{}, nil
}

func testBegin(context.Context, OperationRequest) (BeginResult, error) {
	return BeginResult{}, nil
}

func testPoll(context.Context, OperationRequest, ProviderOperation) (PollResult, error) {
	return PollResult{}, nil
}

func testCleanup(context.Context, OperationRequest, OperationResult) error {
	return nil
}

func testResolveClass(context.Context, ClassRequest) (ClassResolution, error) {
	return ClassResolution{}, nil
}

func testPlanAllocation(context.Context, AllocationPlanRequest) (AllocationPlan, error) {
	return AllocationPlan{}, nil
}

func TestNewProviderRegistersCapacity(t *testing.T) {
	t.Parallel()

	classRequest := ClassRequest{
		ClassName:      "general",
		ClassUID:       "class-uid",
		SiteName:       "site-a",
		ProviderConfig: ProviderConfig{Data: map[string]string{"region": "westus3", "subnet": "subnet-a"}},
		Auth:           &OperationAuth{Mode: unboundedv1alpha3.MachineOperationCredentialAuthWorkloadIdentity},
	}
	classResolution := ClassResolution{Offerings: []unboundedv1alpha3.MachineClassOffering{{
		Name:         "standard-d4-1",
		InstanceType: "Standard_D4s_v5",
		Zone:         "1",
		CapacityType: unboundedv1alpha3.CapacityTypeOnDemand,
		Availability: unboundedv1alpha3.OfferingAvailable,
	}}}
	planRequest := AllocationPlanRequest{
		AllocationName: "alloc-1",
		AllocationUID:  "alloc-uid",
		HostName:       "general-abc12",
		ProviderConfig: ProviderConfig{Data: map[string]string{"image": "ubuntu-24.04", "region": "westus3"}},
		Offering:       classResolution.Offerings[0],
	}
	plan := AllocationPlan{ProviderID: "example://westus3/general-abc12", HostImage: "ubuntu-24.04"}

	bundle := func(resolveClass ResolveClassFunc, planAllocation PlanAllocationFunc) []ProviderOption {
		return []ProviderOption{
			WithLongRunningOperation(unboundedv1alpha3.OperationHostAllocate, testBegin, testPoll),
			WithLongRunningOperation(unboundedv1alpha3.OperationHostRelease, testBegin, testPoll),
			WithCapacity(resolveClass, planAllocation),
		}
	}

	tests := []struct {
		name           string
		options        func(ResolveClassFunc, PlanAllocationFunc) []ProviderOption
		hostOperations map[unboundedv1alpha3.OperationKind]OperationMode
	}{
		{
			name: "capacity first",
			options: func(resolveClass ResolveClassFunc, planAllocation PlanAllocationFunc) []ProviderOption {
				return []ProviderOption{
					WithCapacity(resolveClass, planAllocation),
					WithLongRunningOperation(unboundedv1alpha3.OperationHostAllocate, testBegin, testPoll),
					WithLongRunningOperation(unboundedv1alpha3.OperationHostRelease, testBegin, testPoll),
				}
			},
		},
		{
			name:    "capacity last",
			options: bundle,
		},
		{
			name: "mixed with host operations",
			options: func(resolveClass ResolveClassFunc, planAllocation PlanAllocationFunc) []ProviderOption {
				return append(
					bundle(resolveClass, planAllocation),
					WithImmediateOperation(unboundedv1alpha3.OperationHostPowerOn, testExecute),
					WithLongRunningOperation(
						unboundedv1alpha3.OperationHostReplace,
						testBegin,
						testPoll,
						RequiresReplaceUserData(),
						WithCleanup(testCleanup),
					),
				)
			},
			hostOperations: map[unboundedv1alpha3.OperationKind]OperationMode{
				unboundedv1alpha3.OperationHostPowerOn: OperationModeImmediate,
				unboundedv1alpha3.OperationHostReplace: OperationModeLongRunning,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var (
				resolvedRequests []ClassRequest
				plannedRequests  []AllocationPlanRequest
			)

			resolveClass := func(_ context.Context, request ClassRequest) (ClassResolution, error) {
				resolvedRequests = append(resolvedRequests, request)
				return classResolution, nil
			}
			planAllocation := func(_ context.Context, request AllocationPlanRequest) (AllocationPlan, error) {
				plannedRequests = append(plannedRequests, request)
				return plan, nil
			}

			provider, err := NewProvider("ExampleCloud", tt.options(resolveClass, planAllocation)...)
			require.NoError(t, err)

			capacity, ok := provider.Capacity()
			require.True(t, ok)
			require.NotNil(t, capacity)

			for _, kind := range []unboundedv1alpha3.OperationKind{
				unboundedv1alpha3.OperationHostAllocate,
				unboundedv1alpha3.OperationHostRelease,
			} {
				operation, ok := provider.Operation(kind)
				require.True(t, ok, kind)
				require.Equal(t, OperationModeLongRunning, operation.Mode(), kind)
			}

			for kind, mode := range tt.hostOperations {
				operation, ok := provider.Operation(kind)
				require.True(t, ok, kind)
				require.Equal(t, mode, operation.Mode(), kind)
			}

			resolution, err := capacity.ResolveClass(context.Background(), classRequest)
			require.NoError(t, err)
			require.Equal(t, classResolution, resolution)
			require.Equal(t, []ClassRequest{classRequest}, resolvedRequests)
			require.Equal(t, classRequest.ProviderConfig, resolvedRequests[0].ProviderConfig)

			planned, err := capacity.PlanAllocation(context.Background(), planRequest)
			require.NoError(t, err)
			require.Equal(t, plan, planned)
			require.Equal(t, []AllocationPlanRequest{planRequest}, plannedRequests)
			require.Equal(t, planRequest.ProviderConfig, plannedRequests[0].ProviderConfig)
		})
	}
}

func TestCapacityInvokersReturnCallbackErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       error
		permanent bool
	}{
		{
			name:      "permanent error",
			err:       &PermanentError{Reason: "InvalidConfig", Err: errors.New("region is required")},
			permanent: true,
		},
		{
			name: "transient error",
			err:  errors.New("throttled"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resolveClass := func(context.Context, ClassRequest) (ClassResolution, error) {
				return ClassResolution{}, tt.err
			}
			planAllocation := func(context.Context, AllocationPlanRequest) (AllocationPlan, error) {
				return AllocationPlan{}, tt.err
			}

			provider, err := NewProvider(
				"ExampleCloud",
				WithCapacity(resolveClass, planAllocation),
				WithLongRunningOperation(unboundedv1alpha3.OperationHostAllocate, testBegin, testPoll),
				WithLongRunningOperation(unboundedv1alpha3.OperationHostRelease, testBegin, testPoll),
			)
			require.NoError(t, err)

			capacity, ok := provider.Capacity()
			require.True(t, ok)

			resolution, err := capacity.ResolveClass(context.Background(), ClassRequest{ClassName: "general"})
			require.Same(t, tt.err, err)
			require.Equal(t, ClassResolution{}, resolution)

			planned, err := capacity.PlanAllocation(context.Background(), AllocationPlanRequest{AllocationName: "alloc-1"})
			require.Same(t, tt.err, err)
			require.Equal(t, AllocationPlan{}, planned)

			var permanent *PermanentError
			require.Equal(t, tt.permanent, errors.As(err, &permanent))

			if tt.permanent {
				require.Equal(t, "InvalidConfig", permanent.Reason)
			}
		})
	}
}

func TestNewProviderWithoutCapacityHasNoCapacity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		option ProviderOption
	}{
		{
			name:   "immediate only",
			option: WithImmediateOperation(unboundedv1alpha3.OperationHostPowerOn, testExecute),
		},
		{
			name:   "long-running only",
			option: WithLongRunningOperation(unboundedv1alpha3.OperationHostHardReboot, testBegin, testPoll),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			provider, err := NewProvider("ExampleCloud", tt.option)
			require.NoError(t, err)

			capacity, ok := provider.Capacity()
			require.False(t, ok)
			require.Nil(t, capacity)
		})
	}
}

func TestNewProviderRejectsInvalidCapacityRegistration(t *testing.T) {
	t.Parallel()

	allocateLongRunning := func(options ...OperationOption) ProviderOption {
		return WithLongRunningOperation(unboundedv1alpha3.OperationHostAllocate, testBegin, testPoll, options...)
	}
	releaseLongRunning := func(options ...OperationOption) ProviderOption {
		return WithLongRunningOperation(unboundedv1alpha3.OperationHostRelease, testBegin, testPoll, options...)
	}
	capacity := func() ProviderOption {
		return WithCapacity(testResolveClass, testPlanAllocation)
	}

	tests := []struct {
		name    string
		options []ProviderOption
		message string
	}{
		{
			name:    "nil resolve callback",
			options: []ProviderOption{WithCapacity(nil, testPlanAllocation), allocateLongRunning(), releaseLongRunning()},
			message: "resolve class function is required",
		},
		{
			name:    "nil plan callback",
			options: []ProviderOption{WithCapacity(testResolveClass, nil), allocateLongRunning(), releaseLongRunning()},
			message: "plan allocation function is required",
		},
		{
			name:    "duplicate capacity",
			options: []ProviderOption{capacity(), capacity(), allocateLongRunning(), releaseLongRunning()},
			message: `capacity is already registered for provider "ExampleCloud"`,
		},
		{
			name:    "capacity with no operations",
			options: []ProviderOption{capacity()},
			message: `provider "ExampleCloud" registers capacity without HostAllocate`,
		},
		{
			name: "capacity with only a host operation",
			options: []ProviderOption{
				capacity(),
				WithLongRunningOperation(unboundedv1alpha3.OperationHostHardReboot, testBegin, testPoll),
			},
			message: `provider "ExampleCloud" registers capacity without HostAllocate`,
		},
		{
			name:    "capacity without HostAllocate",
			options: []ProviderOption{capacity(), releaseLongRunning()},
			message: `provider "ExampleCloud" registers capacity without HostAllocate`,
		},
		{
			name:    "capacity without HostRelease",
			options: []ProviderOption{capacity(), allocateLongRunning()},
			message: `provider "ExampleCloud" registers capacity without HostRelease`,
		},
		{
			name: "capacity with immediate HostAllocate",
			options: []ProviderOption{
				capacity(),
				WithImmediateOperation(unboundedv1alpha3.OperationHostAllocate, testExecute),
				releaseLongRunning(),
			},
			message: `provider "ExampleCloud" must register HostAllocate as a long-running operation`,
		},
		{
			name: "capacity with immediate HostRelease",
			options: []ProviderOption{
				capacity(),
				allocateLongRunning(),
				WithImmediateOperation(unboundedv1alpha3.OperationHostRelease, testExecute),
			},
			message: `provider "ExampleCloud" must register HostRelease as a long-running operation`,
		},
		{
			name:    "long-running HostAllocate without capacity",
			options: []ProviderOption{allocateLongRunning()},
			message: `provider "ExampleCloud" registers HostAllocate without capacity`,
		},
		{
			name:    "long-running HostRelease without capacity",
			options: []ProviderOption{releaseLongRunning()},
			message: `provider "ExampleCloud" registers HostRelease without capacity`,
		},
		{
			name:    "both long-running without capacity",
			options: []ProviderOption{allocateLongRunning(), releaseLongRunning()},
			message: `provider "ExampleCloud" registers HostAllocate without capacity`,
		},
		{
			name:    "immediate HostAllocate without capacity",
			options: []ProviderOption{WithImmediateOperation(unboundedv1alpha3.OperationHostAllocate, testExecute)},
			message: `provider "ExampleCloud" registers HostAllocate without capacity`,
		},
		{
			name: "immediate HostRelease without capacity beside HostHardReboot",
			options: []ProviderOption{
				WithLongRunningOperation(unboundedv1alpha3.OperationHostHardReboot, testBegin, testPoll),
				WithImmediateOperation(unboundedv1alpha3.OperationHostRelease, testExecute),
			},
			message: `provider "ExampleCloud" registers HostRelease without capacity`,
		},
		{
			name:    "cleanup on HostAllocate in a full bundle",
			options: []ProviderOption{capacity(), allocateLongRunning(WithCleanup(testCleanup)), releaseLongRunning()},
			message: `cleanup is not valid for allocation operation "HostAllocate"`,
		},
		{
			name:    "cleanup on HostRelease in a full bundle",
			options: []ProviderOption{capacity(), allocateLongRunning(), releaseLongRunning(WithCleanup(testCleanup))},
			message: `cleanup is not valid for allocation operation "HostRelease"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			provider, err := NewProvider("ExampleCloud", tt.options...)
			require.ErrorContains(t, err, tt.message)
			require.Nil(t, provider)
		})
	}
}

func TestCapacityAccessorsAreNilSafe(t *testing.T) {
	t.Parallel()

	capacity, ok := (*Provider)(nil).Capacity()
	require.False(t, ok)
	require.Nil(t, capacity)

	for name, capacity := range map[string]*Capacity{
		"nil capacity":   nil,
		"empty capacity": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			resolution, err := capacity.ResolveClass(context.Background(), ClassRequest{})
			require.EqualError(t, err, "class resolution callback is not registered")
			require.Equal(t, ClassResolution{}, resolution)

			plan, err := capacity.PlanAllocation(context.Background(), AllocationPlanRequest{})
			require.EqualError(t, err, "allocation planning callback is not registered")
			require.Equal(t, AllocationPlan{}, plan)
		})
	}
}
