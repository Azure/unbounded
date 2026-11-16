// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
)

func TestObservedZeroResourcesThroughCreateGetAndList(t *testing.T) {
	for _, test := range []struct {
		name         string
		zeroCapacity bool
		extraZero    bool
	}{
		{
			name: "observed hugepages omitted from advertised capacity",
		},
		{
			name:         "explicit zero capacity",
			zeroCapacity: true,
		},
		{
			name:      "additional unadvertised zero resources",
			extraZero: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			observed := capturedAllocationResources(t)

			if test.zeroCapacity {
				observed.Capacity["hugepages-1Gi"] = resource.MustParse("0Gi")
				observed.Capacity["hugepages-2Mi"] = resource.MustParse("0Mi")
			}

			if test.extraZero {
				observed.Allocatable["example.com/device"] = resource.MustParse("0")
				observed.Allocatable["hugepages-1Gi"] = resource.MustParse("0Gi")
			}

			checkResourceConsumers(t, observed, "")
		})
	}
}

func TestInvalidObservedResourcesFailCreateButNotInventory(t *testing.T) {
	for _, test := range []struct {
		name        string
		resource    corev1.ResourceName
		capacity    string
		allocatable string
		missing     bool
	}{
		{
			name:        "unadvertised negative resource",
			resource:    "example.com/device",
			allocatable: "-1",
		},
		{
			name:        "negative advertised allocatable",
			resource:    corev1.ResourceEphemeralStorage,
			capacity:    "10Gi",
			allocatable: "-1",
		},
		{
			name:        "required capacity must be positive",
			resource:    corev1.ResourceCPU,
			capacity:    "0",
			allocatable: "0",
		},
		{
			name:        "required allocatable must be positive",
			resource:    corev1.ResourceMemory,
			allocatable: "0",
		},
		{
			name:     "required allocatable must exist",
			resource: corev1.ResourcePods,
			missing:  true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			offering := testOffering()
			observed := capacity.AllocationStatus{
				Capacity:    offering.Capacity.DeepCopy(),
				Allocatable: offering.Capacity.DeepCopy(),
			}

			if test.capacity != "" {
				observed.Capacity[test.resource] = resource.MustParse(test.capacity)
			}

			if test.missing {
				delete(observed.Allocatable, test.resource)
			} else {
				observed.Allocatable[test.resource] = resource.MustParse(test.allocatable)
			}

			checkResourceConsumers(t, observed, string(test.resource))
		})
	}
}

// TestNodeWithMoreThanItsOfferingAdvertisedIsReported uses the values a real
// AzureVM Node registered with on the lab: the offering advertised 7577Mi of
// memory and the Node reported 8020584Ki allocatable. The observed value is
// the truth, and failing would fail List, and with it Karpenter's garbage
// collection, for every NodeClaim.
func TestNodeWithMoreThanItsOfferingAdvertisedIsReported(t *testing.T) {
	ctx := t.Context()
	provider, store := testProvider(t, testClass())
	request := testClaim()
	object := claimAllocation(t, provider, request)

	allocation := setSnapshot(t, store, object)
	allocation.Status.Capacity = corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("2"),
		corev1.ResourceEphemeralStorage: resource.MustParse("62259Mi"),
		corev1.ResourceMemory:           resource.MustParse("7577Mi"),
		corev1.ResourcePods:             resource.MustParse("110"),
	}
	allocation.Status.Allocatable = corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("2"),
		corev1.ResourceEphemeralStorage: resource.MustParse("58901232748"),
		"hugepages-1Gi":                 resource.MustParse("0"),
		"hugepages-2Mi":                 resource.MustParse("0"),
		corev1.ResourceMemory:           resource.MustParse("8020584Ki"),
		corev1.ResourcePods:             resource.MustParse("110"),
		"example.com/device":            resource.MustParse("1"),
	}
	writeAllocation(t, store, object, allocation)

	hydrated, err := provider.Get(ctx, allocation.Status.ProviderID)
	must(t, err)

	claims, err := provider.List(ctx)
	must(t, err)

	created, err := provider.Create(ctx, request)
	must(t, err)

	for _, claim := range append(claims, hydrated, created) {
		memory := claim.Status.Capacity[corev1.ResourceMemory]
		device := claim.Status.Capacity["example.com/device"]
		storage := claim.Status.Capacity[corev1.ResourceEphemeralStorage]

		if !memory.Equal(resource.MustParse("8020584Ki")) || !device.Equal(resource.MustParse("1")) {
			t.Fatalf("capacity must be raised to what the Node has, got %v", claim.Status.Capacity)
		}

		if !storage.Equal(resource.MustParse("62259Mi")) {
			t.Fatalf("capacity above the Node's allocatable must be kept, got %v", storage.String())
		}

		if !apiequality.Semantic.DeepEqual(claim.Status.Allocatable, allocation.Status.Allocatable) {
			t.Fatal("the Node's allocatable must be reported unchanged")
		}
	}
}

func capturedAllocationResources(t *testing.T) capacity.AllocationStatus {
	t.Helper()

	path := os.Getenv("UNBOUNDED_LIVE_ALLOCATION_RESOURCES")
	if path == "" {
		path = "testdata/live-allocation-resources.json"
	}

	content, err := os.ReadFile(path)
	must(t, err)

	var allocations []capacity.Allocation
	must(t, json.Unmarshal(content, &allocations))

	if len(allocations) != 1 {
		t.Fatalf("expected one captured allocation, got %d", len(allocations))
	}

	return allocations[0].Status
}

func checkResourceConsumers(t *testing.T, observed capacity.AllocationStatus, invalidResource string) {
	t.Helper()

	ctx := t.Context()
	provider, store := testProvider(t, testClass())
	request := testClaim()

	if _, err := provider.Create(ctx, request); err == nil {
		t.Fatal("expected the original allocation to start pending")
	}

	object, err := provider.readAllocation(ctx, capacity.AllocationName(request.UID))
	must(t, err)
	setAllocated(t, store, object)
	allocation, err := decodeAllocation(object)
	must(t, err)

	allocation.Status.Capacity = observed.Capacity.DeepCopy()
	allocation.Status.Allocatable = observed.Allocatable.DeepCopy()
	writeAllocation(t, store, object, allocation)

	for _, method := range []string{"Create", "Get", "List"} {
		t.Run(method, func(t *testing.T) {
			var (
				claims []*v1.NodeClaim
				err    error
			)

			switch method {
			case "Create":
				claim, createErr := provider.Create(ctx, request)
				claims, err = []*v1.NodeClaim{claim}, createErr
			case "Get":
				claim, getErr := provider.Get(ctx, allocation.Status.ProviderID)
				claims, err = []*v1.NodeClaim{claim}, getErr
			case "List":
				claims, err = provider.List(ctx)
			}

			if invalidResource != "" && method == "Create" {
				if err == nil || !strings.Contains(err.Error(), invalidResource) ||
					cloudprovider.IsNodeClaimNotFoundError(err) {
					t.Fatalf("expected invalid resource %s, not absent infrastructure: %v", invalidResource, err)
				}

				return
			}

			if invalidResource != "" {
				// Inventory answers whether the allocation still exists. A
				// description Create would reject must not hide the
				// allocation, or fail List for every other NodeClaim.
				must(t, err)

				if len(claims) != 1 || claims[0] == nil ||
					claims[0].Status.ProviderID != allocation.Status.ProviderID || claims[0].UID != request.UID {
					t.Fatalf("inventory must still report the allocation, got %v", claims)
				}

				return
			}

			must(t, err)

			if len(claims) != 1 || claims[0] == nil {
				t.Fatalf("expected one hydrated claim, got %d", len(claims))
			}

			if claims[0].Status.ProviderID != allocation.Status.ProviderID ||
				claims[0].UID != request.UID ||
				!apiequality.Semantic.DeepEqual(claims[0].Status.Capacity, observed.Capacity) ||
				!apiequality.Semantic.DeepEqual(claims[0].Status.Allocatable, observed.Allocatable) {
				t.Fatal("hydration changed identity or observed resources")
			}
		})
	}

	allocations := allocationList()
	must(t, store.List(ctx, allocations))

	if len(allocations.Items) != 1 {
		t.Fatal("resource hydration must not create replacement allocations")
	}
}
