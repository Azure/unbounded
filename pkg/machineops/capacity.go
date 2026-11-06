// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// ResolveClassFunc normalizes a class's provider configuration into capacity
// offerings. It must not allocate infrastructure. A PermanentError reports
// invalid configuration; other errors are transient and retried.
type ResolveClassFunc func(context.Context, ClassRequest) (ClassResolution, error)

// PlanAllocationFunc computes the host identity for one allocation. It must be
// pure: deterministic for the same request, with no remote calls and no
// reservations. A PermanentError reports that the request can never be
// planned; other errors are retried.
type PlanAllocationFunc func(context.Context, AllocationPlanRequest) (AllocationPlan, error)

// ProviderConfig is the class's provider configuration, which only the
// provider interprets. Further optional fields, for example a structured
// object for typed per-provider config resources, may be added later as an
// additive change.
type ProviderConfig struct {
	// Data is the data of the ConfigMap that MachineClass.spec.providerConfigRef
	// names, the only kind of reference accepted today.
	Data map[string]string
}

// ClassRequest asks a provider to resolve a MachineClass into offerings.
type ClassRequest struct {
	ClassName string
	ClassUID  types.UID
	SiteName  string
	// ProviderConfig is the content of the class's immutable provider
	// configuration.
	ProviderConfig ProviderConfig
	Auth           *OperationAuth
}

// ClassResolution is the provider's normalized view of a class. Offerings must
// carry truthful availability and a ValidUntil after which consumers must not
// launch from them.
type ClassResolution struct {
	Offerings []unboundedv1alpha3.MachineClassOffering
}

// AllocationPlanRequest asks a provider for the host identity of one
// allocation.
type AllocationPlanRequest struct {
	AllocationName string
	AllocationUID  types.UID
	// HostName is the planned name of the allocation's Machine.
	HostName       string
	ProviderConfig ProviderConfig
	Offering       unboundedv1alpha3.MachineClassOffering
}

// AllocationPlan is the planned identity of an allocation's host.
type AllocationPlan struct {
	// ProviderID is the pre-host locator used as the Machine's
	// host.external.providerID before the host exists. It is not necessarily
	// a native instance ID.
	ProviderID string

	// HostImage is the provider-interpreted image the host launches with.
	HostImage string
}

// AllocationRequest is the allocation input passed with HostAllocate and
// HostRelease operations.
type AllocationRequest struct {
	Name string
	// UID is the MachineAllocation incarnation that owns every provider
	// effect. Begin must key replay on it as well as on
	// OperationRequest.OperationUID.
	UID       types.UID
	ClassName string
	ClassUID  types.UID
	// HostName is the planned name of the allocation's Machine.
	HostName string
	// ProviderConfig is supplied only when HostAllocate begins. For Poll and
	// for HostRelease it is the zero ProviderConfig.
	ProviderConfig ProviderConfig
	Offering       unboundedv1alpha3.MachineClassOffering
	// UserData is first-boot data that may contain credentials: providers
	// must never log it, persist it, or copy it into a ResumeToken, status
	// message or error. It is supplied only when HostAllocate begins and is
	// empty for Poll and for HostRelease.
	UserData string
	// OwnedResources are the resources recorded as owned by the allocation,
	// including those a failed HostAllocate reported. HostRelease may act only
	// on these.
	OwnedResources []unboundedv1alpha3.ProviderOwnedResource
}

// AllocationResult reports the host confirmed by HostAllocate. Field names
// match v1alpha3.MachineOperationAllocationResult.
type AllocationResult struct {
	// ProviderResourceID is the provider-native host identifier.
	ProviderResourceID string
	InstanceType       string
	Zone               string
	Image              string
	// OwnedResources are the resources created for the allocation, with
	// provider-native identity. A failed HostAllocate may still report them
	// so release can clean them up.
	OwnedResources []unboundedv1alpha3.ProviderOwnedResource
}

// Capacity is a provider's registered capacity lifecycle. Capacity is all or
// nothing: it requires HostAllocate and HostRelease registered as long-running
// operations, and those kinds require capacity.
type Capacity struct {
	resolveClass   ResolveClassFunc
	planAllocation PlanAllocationFunc
}

// WithCapacity registers class resolution and allocation planning. A provider
// that registers capacity must also register HostAllocate and HostRelease as
// long-running operations, and a provider that registers either kind must
// register capacity.
func WithCapacity(resolveClass ResolveClassFunc, planAllocation PlanAllocationFunc) ProviderOption {
	return providerOptionFunc(func(provider *Provider) error {
		if resolveClass == nil {
			return fmt.Errorf("resolve class function is required")
		}

		if planAllocation == nil {
			return fmt.Errorf("plan allocation function is required")
		}

		if provider.capacity != nil {
			return fmt.Errorf("capacity is already registered for provider %q", provider.name)
		}

		provider.capacity = &Capacity{
			resolveClass:   resolveClass,
			planAllocation: planAllocation,
		}

		return nil
	})
}

// Capacity returns the provider's capacity registration.
func (p *Provider) Capacity() (*Capacity, bool) {
	if p == nil || p.capacity == nil {
		return nil, false
	}

	return p.capacity, true
}

// ResolveClass invokes the registered class resolution callback.
func (c *Capacity) ResolveClass(ctx context.Context, request ClassRequest) (ClassResolution, error) {
	if c == nil || c.resolveClass == nil {
		return ClassResolution{}, fmt.Errorf("class resolution callback is not registered")
	}

	return c.resolveClass(ctx, request)
}

// PlanAllocation invokes the registered allocation planning callback.
func (c *Capacity) PlanAllocation(ctx context.Context, request AllocationPlanRequest) (AllocationPlan, error) {
	if c == nil || c.planAllocation == nil {
		return AllocationPlan{}, fmt.Errorf("allocation planning callback is not registered")
	}

	return c.planAllocation(ctx, request)
}

func validateCapacity(provider *Provider) error {
	for _, kind := range []unboundedv1alpha3.OperationKind{
		unboundedv1alpha3.OperationHostAllocate,
		unboundedv1alpha3.OperationHostRelease,
	} {
		operation, registered := provider.operations[kind]

		if provider.capacity == nil {
			if registered {
				return fmt.Errorf("provider %q registers %s without capacity", provider.name, kind)
			}

			continue
		}

		if !registered {
			return fmt.Errorf("provider %q registers capacity without %s", provider.name, kind)
		}

		if operation.mode != OperationModeLongRunning {
			return fmt.Errorf("provider %q must register %s as a long-running operation", provider.name, kind)
		}
	}

	return nil
}
