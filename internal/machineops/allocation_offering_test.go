// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestSelectOffering(t *testing.T) {
	t.Parallel()

	now := allocTestNow

	offering := func(name string, mutate ...func(*unboundedv1alpha3.MachineClassOffering)) unboundedv1alpha3.MachineClassOffering {
		validUntil := metav1.NewTime(now.Add(time.Hour))
		o := unboundedv1alpha3.MachineClassOffering{
			Name:         name,
			InstanceType: "Standard_" + name,
			Zone:         "zone-1",
			CapacityType: unboundedv1alpha3.CapacityTypeOnDemand,
			Capacity:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("16Gi")},
			Availability: unboundedv1alpha3.OfferingAvailable,
			Price:        &unboundedv1alpha3.OfferingPrice{Currency: "USD", PerHour: "1.00"},
			ValidUntil:   &validUntil,
		}

		for _, m := range mutate {
			m(&o)
		}

		return o
	}
	price := func(perHour string) func(*unboundedv1alpha3.MachineClassOffering) {
		return func(o *unboundedv1alpha3.MachineClassOffering) {
			if perHour == "" {
				o.Price = nil
				return
			}

			o.Price = &unboundedv1alpha3.OfferingPrice{Currency: "USD", PerHour: perHour}
		}
	}
	labels := func(values map[string]string) func(*unboundedv1alpha3.MachineClassOffering) {
		return func(o *unboundedv1alpha3.MachineClassOffering) { o.Labels = values }
	}
	zone := func(value string) func(*unboundedv1alpha3.MachineClassOffering) {
		return func(o *unboundedv1alpha3.MachineClassOffering) { o.Zone = value }
	}
	requirement := func(key string, operator unboundedv1alpha3.RequirementOperator, values ...string) unboundedv1alpha3.MachineAllocationRequirement {
		return unboundedv1alpha3.MachineAllocationRequirement{Key: key, Operator: operator, Values: values}
	}
	withMin := func(r unboundedv1alpha3.MachineAllocationRequirement, minValues int32) unboundedv1alpha3.MachineAllocationRequirement {
		r.MinValues = &minValues
		return r
	}
	cpu := func(quantity string) corev1.ResourceList {
		return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(quantity)}
	}

	tests := []struct {
		name         string
		offerings    []unboundedv1alpha3.MachineClassOffering
		resources    corev1.ResourceList
		requirements []unboundedv1alpha3.MachineAllocationRequirement
		want         string
		failure      unboundedv1alpha3.MachineAllocationFailureReason
	}{
		{
			name:      "request fits after overhead",
			offerings: []unboundedv1alpha3.MachineClassOffering{offering("a", func(o *unboundedv1alpha3.MachineClassOffering) { o.Overhead.KubeReserved = cpu("1") })},
			resources: cpu("3"),
			want:      "a",
		},
		{
			name: "request does not fit after overhead",
			offerings: []unboundedv1alpha3.MachineClassOffering{offering("a", func(o *unboundedv1alpha3.MachineClassOffering) {
				o.Overhead.KubeReserved = cpu("500m")
				o.Overhead.SystemReserved = cpu("500m")
				o.Overhead.EvictionThreshold = cpu("100m")
			})},
			resources: cpu("3"),
			failure:   unboundedv1alpha3.FailureInsufficientCapacity,
		},
		{
			name:      "requested resource missing from capacity",
			offerings: []unboundedv1alpha3.MachineClassOffering{offering("a")},
			resources: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
			failure:   unboundedv1alpha3.FailureInsufficientCapacity,
		},
		{
			name:         "In",
			offerings:    []unboundedv1alpha3.MachineClassOffering{offering("a", zone("zone-1")), offering("b", zone("zone-2"), price("0.50"))},
			requirements: []unboundedv1alpha3.MachineAllocationRequirement{requirement(corev1.LabelTopologyZone, unboundedv1alpha3.RequirementOpIn, "zone-1")},
			want:         "a",
		},
		{
			name:         "NotIn",
			offerings:    []unboundedv1alpha3.MachineClassOffering{offering("a", zone("zone-1"), price("0.50")), offering("b", zone("zone-2"))},
			requirements: []unboundedv1alpha3.MachineAllocationRequirement{requirement(corev1.LabelTopologyZone, unboundedv1alpha3.RequirementOpNotIn, "zone-1")},
			want:         "b",
		},
		{
			name:         "NotIn matches a missing label",
			offerings:    []unboundedv1alpha3.MachineClassOffering{offering("a", zone(""))},
			requirements: []unboundedv1alpha3.MachineAllocationRequirement{requirement(corev1.LabelTopologyZone, unboundedv1alpha3.RequirementOpNotIn, "zone-1")},
			want:         "a",
		},
		{
			name:         "Exists",
			offerings:    []unboundedv1alpha3.MachineClassOffering{offering("a", price("0.50")), offering("b", labels(map[string]string{"gpu": "a100"}))},
			requirements: []unboundedv1alpha3.MachineAllocationRequirement{requirement("gpu", unboundedv1alpha3.RequirementOpExists)},
			want:         "b",
		},
		{
			name:         "DoesNotExist",
			offerings:    []unboundedv1alpha3.MachineClassOffering{offering("a", price("0.50"), labels(map[string]string{"gpu": "a100"})), offering("b")},
			requirements: []unboundedv1alpha3.MachineAllocationRequirement{requirement("gpu", unboundedv1alpha3.RequirementOpDoesNotExist)},
			want:         "b",
		},
		{
			name: "Gt compares integers",
			offerings: []unboundedv1alpha3.MachineClassOffering{
				offering("a", price("0.50"), labels(map[string]string{"cores": "8"})),
				offering("b", labels(map[string]string{"cores": "32"})),
			},
			requirements: []unboundedv1alpha3.MachineAllocationRequirement{requirement("cores", unboundedv1alpha3.RequirementOpGt, "16")},
			want:         "b",
		},
		{
			name: "Lt compares integers",
			offerings: []unboundedv1alpha3.MachineClassOffering{
				offering("a", price("0.50"), labels(map[string]string{"cores": "32"})),
				offering("b", labels(map[string]string{"cores": "8"})),
				offering("c", price("0.10"), labels(map[string]string{"cores": "many"})),
			},
			requirements: []unboundedv1alpha3.MachineAllocationRequirement{requirement("cores", unboundedv1alpha3.RequirementOpLt, "16")},
			want:         "b",
		},
		{
			name: "derived labels match",
			offerings: []unboundedv1alpha3.MachineClassOffering{
				offering("a", price("0.50")),
				offering("b", func(o *unboundedv1alpha3.MachineClassOffering) { o.CapacityType = unboundedv1alpha3.CapacityTypeSpot }),
			},
			requirements: []unboundedv1alpha3.MachineAllocationRequirement{
				requirement(unboundedv1alpha3.LabelCapacityType, unboundedv1alpha3.RequirementOpIn, "Spot"),
				requirement(corev1.LabelInstanceTypeStable, unboundedv1alpha3.RequirementOpIn, "Standard_b"),
				requirement(corev1.LabelTopologyZone, unboundedv1alpha3.RequirementOpIn, "zone-1"),
			},
			want: "b",
		},
		{
			name: "conflicting explicit label",
			offerings: []unboundedv1alpha3.MachineClassOffering{
				offering("a", price("0.50"), labels(map[string]string{corev1.LabelTopologyZone: "zone-9"})),
				offering("b"),
			},
			want: "b",
		},
		{
			name: "expired, Unavailable and Unknown excluded",
			offerings: []unboundedv1alpha3.MachineClassOffering{
				offering("expired", price("0.10"), func(o *unboundedv1alpha3.MachineClassOffering) { o.ValidUntil = &metav1.Time{Time: now} }),
				offering("no-expiry", price("0.10"), func(o *unboundedv1alpha3.MachineClassOffering) { o.ValidUntil = nil }),
				offering("unavailable", price("0.10"), func(o *unboundedv1alpha3.MachineClassOffering) {
					o.Availability = unboundedv1alpha3.OfferingUnavailable
				}),
				offering("unknown", price("0.10"), func(o *unboundedv1alpha3.MachineClassOffering) {
					o.Availability = unboundedv1alpha3.OfferingAvailabilityUnknown
				}),
				offering("current", price("9.00")),
			},
			want: "current",
		},
		{
			name: "only excluded offerings",
			offerings: []unboundedv1alpha3.MachineClassOffering{
				offering("expired", func(o *unboundedv1alpha3.MachineClassOffering) {
					o.ValidUntil = &metav1.Time{Time: now.Add(-time.Second)}
				}),
			},
			failure: unboundedv1alpha3.FailureInsufficientCapacity,
		},
		{
			name:      "lower price first",
			offerings: []unboundedv1alpha3.MachineClassOffering{offering("a", price("1.5")), offering("b", price("0.25")), offering("c", price("10"))},
			want:      "b",
		},
		{
			name:      "priced before unpriced",
			offerings: []unboundedv1alpha3.MachineClassOffering{offering("a", price("")), offering("b", price("99.99"))},
			want:      "b",
		},
		{
			name:      "name breaks ties",
			offerings: []unboundedv1alpha3.MachineClassOffering{offering("c", price("")), offering("a", price("")), offering("b", price(""))},
			want:      "a",
		},
		{
			name: "minValues met",
			offerings: []unboundedv1alpha3.MachineClassOffering{
				offering("a", zone("zone-1"), price("0.50")),
				offering("b", zone("zone-2")),
			},
			requirements: []unboundedv1alpha3.MachineAllocationRequirement{withMin(requirement(corev1.LabelTopologyZone, unboundedv1alpha3.RequirementOpExists), 2)},
			want:         "a",
		},
		{
			name: "minValues unmet",
			offerings: []unboundedv1alpha3.MachineClassOffering{
				offering("a", zone("zone-1")),
				offering("b", zone("zone-1")),
				offering("c", zone("zone-2"), func(o *unboundedv1alpha3.MachineClassOffering) {
					o.Availability = unboundedv1alpha3.OfferingUnavailable
				}),
			},
			requirements: []unboundedv1alpha3.MachineAllocationRequirement{withMin(requirement(corev1.LabelTopologyZone, unboundedv1alpha3.RequirementOpExists), 2)},
			failure:      unboundedv1alpha3.FailureInsufficientCapacity,
		},
		{
			name:         "In without values",
			offerings:    []unboundedv1alpha3.MachineClassOffering{offering("a")},
			requirements: []unboundedv1alpha3.MachineAllocationRequirement{requirement("k", unboundedv1alpha3.RequirementOpIn)},
			failure:      unboundedv1alpha3.FailureInvalidRequest,
		},
		{
			name:         "NotIn without values",
			offerings:    []unboundedv1alpha3.MachineClassOffering{offering("a")},
			requirements: []unboundedv1alpha3.MachineAllocationRequirement{requirement("k", unboundedv1alpha3.RequirementOpNotIn)},
			failure:      unboundedv1alpha3.FailureInvalidRequest,
		},
		{
			name:         "Exists with values",
			offerings:    []unboundedv1alpha3.MachineClassOffering{offering("a")},
			requirements: []unboundedv1alpha3.MachineAllocationRequirement{requirement("k", unboundedv1alpha3.RequirementOpExists, "v")},
			failure:      unboundedv1alpha3.FailureInvalidRequest,
		},
		{
			name:         "DoesNotExist with values",
			offerings:    []unboundedv1alpha3.MachineClassOffering{offering("a")},
			requirements: []unboundedv1alpha3.MachineAllocationRequirement{requirement("k", unboundedv1alpha3.RequirementOpDoesNotExist, "v")},
			failure:      unboundedv1alpha3.FailureInvalidRequest,
		},
		{
			name:         "Gt with two values",
			offerings:    []unboundedv1alpha3.MachineClassOffering{offering("a")},
			requirements: []unboundedv1alpha3.MachineAllocationRequirement{requirement("k", unboundedv1alpha3.RequirementOpGt, "1", "2")},
			failure:      unboundedv1alpha3.FailureInvalidRequest,
		},
		{
			name:         "Lt with a non-integer value",
			offerings:    []unboundedv1alpha3.MachineClassOffering{offering("a")},
			requirements: []unboundedv1alpha3.MachineAllocationRequirement{requirement("k", unboundedv1alpha3.RequirementOpLt, "1.5")},
			failure:      unboundedv1alpha3.FailureInvalidRequest,
		},
		{
			name:         "Gt without values",
			offerings:    []unboundedv1alpha3.MachineClassOffering{offering("a")},
			requirements: []unboundedv1alpha3.MachineAllocationRequirement{requirement("k", unboundedv1alpha3.RequirementOpGt)},
			failure:      unboundedv1alpha3.FailureInvalidRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			selected, failure := selectOffering(tt.offerings, tt.resources, tt.requirements, now)
			if tt.failure != "" {
				require.Nil(t, selected)
				require.NotNil(t, failure)
				require.Equal(t, tt.failure, failure.reason)
				require.NotEmpty(t, failure.message)

				return
			}

			require.Nil(t, failure)
			require.NotNil(t, selected)
			require.Equal(t, tt.want, selected.offering.Name)
			require.Equal(t, selected.offering.InstanceType, selected.labels[corev1.LabelInstanceTypeStable])
			require.Equal(t, string(selected.offering.CapacityType), selected.labels[unboundedv1alpha3.LabelCapacityType])
		})
	}
}

func TestAllocationLabelsLayering(t *testing.T) {
	t.Parallel()

	labels := allocationLabels(
		&unboundedv1alpha3.MachineConfigurationTemplate{Kubernetes: &unboundedv1alpha3.MachineConfigurationKubernetes{
			NodeLabels: map[string]string{"a": "template", "b": "template", "c": "template"},
		}},
		&unboundedv1alpha3.MachineAllocationNodeTemplate{Labels: map[string]string{"b": "node-template", "c": "node-template"}},
		map[string]string{"c": "offering"},
	)
	require.Equal(t, map[string]string{"a": "template", "b": "node-template", "c": "offering"}, labels)
	require.Empty(t, allocationLabels(&unboundedv1alpha3.MachineConfigurationTemplate{}, nil, nil))
}
