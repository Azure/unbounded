// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"math"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
)

func TestOfferingsNeverInventAvailability(t *testing.T) {
	for name, change := range map[string]func(*capacity.Offering){
		"unknown availability": func(offering *capacity.Offering) {
			offering.Availability = "Unknown"
		},
		"unavailable": func(offering *capacity.Offering) {
			offering.Availability = "Unavailable"
		},
		"expired observation": func(offering *capacity.Offering) {
			expired := metav1.NewTime(time.Now().Add(-time.Minute))
			offering.ValidUntil = &expired
		},
		"missing expiry": func(offering *capacity.Offering) {
			offering.ValidUntil = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			provider, _ := testProvider(t, testClass())
			offering := testOffering()
			change(&offering)

			converted, estimated, err := provider.convertOffering(offering, "test-site", time.Now())
			must(t, err)

			if converted.Available || estimated || converted.Price != 0.192 {
				t.Fatal("unavailable inventory must be returned unavailable with its real price")
			}
		})
	}
}

func TestOfferingPrice(t *testing.T) {
	// 4 CPU and 16Gi: 4*0.025 + 17.179869184*0.001.
	const estimate = 0.117179869184

	for _, test := range []struct {
		name          string
		change        func(*capacity.Offering)
		currency      string
		wantPrice     float64
		wantEstimated bool
		wantError     bool
	}{
		{name: "priced", change: func(*capacity.Offering) {}, wantPrice: 0.192},
		{
			name:          "no price",
			change:        func(offering *capacity.Offering) { offering.Price = nil },
			wantPrice:     estimate,
			wantEstimated: true,
		},
		{
			name:          "another currency",
			change:        func(offering *capacity.Offering) { offering.Price.Currency = "EUR" },
			wantPrice:     estimate,
			wantEstimated: true,
		},
		{
			name:      "configured currency",
			change:    func(offering *capacity.Offering) { offering.Price.Currency = "EUR" },
			currency:  "EUR",
			wantPrice: 0.192,
		},
		{
			name:      "free",
			change:    func(offering *capacity.Offering) { offering.Price.PerHour = "0" },
			wantPrice: 0,
		},
		{
			name:      "NaN price",
			change:    func(offering *capacity.Offering) { offering.Price.PerHour = "NaN" },
			wantError: true,
		},
		{
			name:      "negative price",
			change:    func(offering *capacity.Offering) { offering.Price.PerHour = "-1" },
			wantError: true,
		},
		{
			name:      "empty price",
			change:    func(offering *capacity.Offering) { offering.Price.PerHour = "" },
			wantError: true,
		},
		{
			name: "positive price underflow",
			change: func(offering *capacity.Offering) {
				offering.Price.PerHour = "0." + strings.Repeat("0", 400) + "1"
			},
			wantError: true,
		},
		{
			// The class controller's clock can run ahead of this process.
			name: "price observed ahead of the local clock",
			change: func(offering *capacity.Offering) {
				future := metav1.NewTime(time.Now().Add(time.Hour))
				offering.Price.ObservedAt = &future
			},
			wantPrice: 0.192,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider, _ := testProvider(t, testClass())
			if test.currency != "" {
				provider.options.PriceCurrency = test.currency
			}

			offering := testOffering()
			test.change(&offering)

			converted, estimated, err := provider.convertOffering(offering, "test-site", time.Now())
			if test.wantError {
				if err == nil {
					t.Fatal("a malformed price must not be replaced by an estimate")
				}

				return
			}

			must(t, err)

			if math.Abs(converted.Price-test.wantPrice) > 1e-9 || estimated != test.wantEstimated {
				t.Fatalf("price %v (estimated %t), want %v (estimated %t)",
					converted.Price, estimated, test.wantPrice, test.wantEstimated)
			}

			if test.wantEstimated && converted.Price <= 0 {
				t.Fatal("an estimate must never be zero")
			}
		})
	}
}

func TestOfferingDefaultsAndRequirements(t *testing.T) {
	for _, test := range []struct {
		name      string
		change    func(*capacity.Offering)
		site      string
		wantZone  string
		wantOS    string
		wantError bool
	}{
		{name: "complete", change: func(*capacity.Offering) {}, site: "test-site", wantZone: "test-region-1", wantOS: "linux"},
		{
			name: "no zone uses the Site",
			change: func(offering *capacity.Offering) {
				offering.Zone = ""
				delete(offering.Labels, corev1.LabelTopologyZone)
			},
			site: "test-site", wantZone: "test-site", wantOS: "linux",
		},
		{
			name: "no zone and no Site",
			change: func(offering *capacity.Offering) {
				offering.Zone = ""
				delete(offering.Labels, corev1.LabelTopologyZone)
			},
			wantError: true,
		},
		{
			name:   "no operating system is linux",
			change: func(offering *capacity.Offering) { delete(offering.Labels, corev1.LabelOSStable) },
			site:   "test-site", wantZone: "test-region-1", wantOS: "linux",
		},
		{
			name:   "no region is accepted",
			change: func(offering *capacity.Offering) { delete(offering.Labels, corev1.LabelTopologyRegion) },
			site:   "test-site", wantZone: "test-region-1", wantOS: "linux",
		},
		{
			name: "derived labels may be omitted",
			change: func(offering *capacity.Offering) {
				delete(offering.Labels, corev1.LabelInstanceTypeStable)
				delete(offering.Labels, corev1.LabelTopologyZone)
				delete(offering.Labels, capacity.CapacityTypeLabel)
			},
			site: "test-site", wantZone: "test-region-1", wantOS: "linux",
		},
		{
			name:      "no architecture",
			change:    func(offering *capacity.Offering) { delete(offering.Labels, corev1.LabelArchStable) },
			site:      "test-site",
			wantError: true,
		},
		{
			name:      "label disagrees with the zone field",
			change:    func(offering *capacity.Offering) { offering.Labels[corev1.LabelTopologyZone] = "elsewhere" },
			site:      "test-site",
			wantError: true,
		},
		{
			name:      "no pods capacity",
			change:    func(offering *capacity.Offering) { delete(offering.Capacity, corev1.ResourcePods) },
			site:      "test-site",
			wantError: true,
		},
		{
			name: "overhead exceeds capacity",
			change: func(offering *capacity.Offering) {
				offering.Overhead.KubeReserved[corev1.ResourceCPU] = resource.MustParse("8")
			},
			site:      "test-site",
			wantError: true,
		},
		{
			name:      "unknown purchase model",
			change:    func(offering *capacity.Offering) { offering.CapacityType = "Reserved" },
			site:      "test-site",
			wantError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider, _ := testProvider(t, testClass())
			offering := testOffering()
			test.change(&offering)

			converted, _, err := provider.convertOffering(offering, test.site, time.Now())
			if test.wantError {
				if err == nil {
					t.Fatal("this offering cannot be used by Karpenter")
				}

				return
			}

			must(t, err)

			requirements := converted.Requirements
			if requirements.Get(corev1.LabelTopologyZone).Any() != test.wantZone ||
				requirements.Get(corev1.LabelOSStable).Any() != test.wantOS ||
				requirements.Get(corev1.LabelInstanceTypeStable).Any() != "test.medium" ||
				requirements.Get(v1.CapacityTypeLabelKey).Any() != v1.CapacityTypeOnDemand ||
				requirements.Has(capacity.CapacityTypeLabel) {
				t.Fatalf("unexpected offering requirements: %v", requirements)
			}
		})
	}
}

func testPool() *v1.NodePool {
	return &v1.NodePool{
		Spec: v1.NodePoolSpec{
			Template: v1.NodeClaimTemplate{
				Spec: v1.NodeClaimTemplateSpec{
					NodeClassRef: testClaim().Spec.NodeClassRef,
				},
			},
		},
	}
}

func TestDiscoverySkipsUnusableOfferings(t *testing.T) {
	class := testClass()
	unusable := testOffering()
	unusable.Name = "no-architecture"
	unusable.InstanceType = "test.large"
	unusable.Labels[corev1.LabelInstanceTypeStable] = "test.large"
	delete(unusable.Labels, corev1.LabelArchStable)
	class.Status.Offerings = append(class.Status.Offerings, unusable)
	provider, _ := testProvider(t, class)

	types, err := provider.GetInstanceTypes(t.Context(), testPool())
	must(t, err)

	if len(types) != 1 || types[0].Name != "test.medium" {
		t.Fatal("one unusable offering must be skipped without failing the class")
	}

	class = testClass()
	delete(class.Status.Offerings[0].Labels, corev1.LabelArchStable)
	provider, _ = testProvider(t, class)

	if _, err := provider.GetInstanceTypes(t.Context(), testPool()); err == nil {
		t.Fatal("a class with no usable offering must fail discovery")
	}
}

func TestDiscoveryRemembersFailedLaunches(t *testing.T) {
	class := testClass()
	later := metav1.NewTime(time.Now().Add(time.Hour))
	class.Status.Offerings[0].ValidUntil = &later
	provider, _ := testProvider(t, class)
	now := time.Now()
	provider.now = func() time.Time { return now }

	available := func() bool {
		types, err := provider.GetInstanceTypes(t.Context(), testPool())
		must(t, err)

		return types[0].Offerings[0].Available
	}

	if !available() {
		t.Fatal("the offering starts available")
	}

	provider.MarkUnavailable(class.UID, "another-offering")
	provider.MarkUnavailable("another-class", class.Status.Offerings[0].Name)

	if !available() {
		t.Fatal("a failure of another class or offering must not affect this one")
	}

	provider.MarkUnavailable(class.UID, class.Status.Offerings[0].Name)

	if available() {
		t.Fatal("an offering whose launch just failed must be reported unavailable")
	}

	now = now.Add(defaultUnavailableTTL)

	if !available() {
		t.Fatal("the failure must be forgotten after its period")
	}
}

func TestDiscoveryRequiresAReadyClassWithOfferings(t *testing.T) {
	class := testClass()
	class.Status.Offerings = nil
	provider, _ := testProvider(t, class)

	if _, err := provider.GetInstanceTypes(t.Context(), testPool()); err == nil {
		t.Fatal("a class without offerings must fail discovery")
	}

	class = testClass()
	class.Generation = 2
	provider, _ = testProvider(t, class)

	if _, err := provider.GetInstanceTypes(t.Context(), testPool()); err == nil {
		t.Fatal("a class not resolved for its generation must fail discovery")
	}

	pool := testPool()
	pool.Spec.Template.Spec.NodeClassRef.Kind = "EC2NodeClass"

	if _, err := provider.GetInstanceTypes(t.Context(), pool); err == nil {
		t.Fatal("a NodePool of another NodeClass kind is not supported")
	}
}

func TestDiscoveryRetainsCorrelatedOfferingsAndOverhead(t *testing.T) {
	class := testClass()
	second := testOffering()
	second.Name = "medium-zone-two-spot"
	second.Zone = "test-region-2"
	second.CapacityType = "Spot"
	second.Labels[capacity.CapacityTypeLabel] = "Spot"
	second.Labels[corev1.LabelTopologyZone] = second.Zone
	class.Status.Offerings = append(class.Status.Offerings, second)
	provider, _ := testProvider(t, class)
	pool := &v1.NodePool{
		Spec: v1.NodePoolSpec{
			Template: v1.NodeClaimTemplate{
				Spec: v1.NodeClaimTemplateSpec{
					NodeClassRef: testClaim().Spec.NodeClassRef,
				},
			},
		},
	}

	types, err := provider.GetInstanceTypes(t.Context(), pool)
	must(t, err)

	if len(types) != 1 || len(types[0].Offerings) != 2 {
		t.Fatal("same-shaped correlated offerings must share an instance type")
	}

	for _, offering := range types[0].Offerings {
		zone := offering.Requirements.Get(corev1.LabelTopologyZone).Any()

		model := offering.Requirements.Get(v1.CapacityTypeLabelKey).Any()
		if zone == "test-region-1" && model != "on-demand" ||
			zone == "test-region-2" && model != "spot" {
			t.Fatal("discovery invented a topology and purchase-model combination")
		}

		if offering.OverheadOverride.KubeReserved.Cpu().String() != "100m" ||
			offering.OverheadOverride.SystemReserved.Memory().String() != "1Gi" ||
			offering.OverheadOverride.EvictionThreshold.Memory().String() != "100Mi" {
			t.Fatal("reserved and eviction resources were not preserved")
		}
	}
}
