// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
)

const rackKey = "inventory.example.com/rack"

func requirement(key string, operator corev1.NodeSelectorOperator, values ...string) v1.NodeSelectorRequirementWithMinValues {
	return v1.NodeSelectorRequirementWithMinValues{Key: key, Operator: operator, Values: values}
}

// namedOffering is the test offering under another name, changed.
func namedOffering(name string, changes ...func(*capacity.Offering)) capacity.Offering {
	offering := testOffering()
	offering.Name = name

	for _, change := range changes {
		change(&offering)
	}

	return offering
}

func rack(value string) func(*capacity.Offering) {
	return func(offering *capacity.Offering) { offering.Labels[rackKey] = value }
}

func price(perHour string) func(*capacity.Offering) {
	return func(offering *capacity.Offering) { offering.Price.PerHour = perHour }
}

func zone(name string) func(*capacity.Offering) {
	return func(offering *capacity.Offering) {
		offering.Zone = name
		offering.Labels[corev1.LabelTopologyZone] = name
	}
}

func zoneless(offering *capacity.Offering) {
	offering.Zone = ""
	delete(offering.Labels, corev1.LabelTopologyZone)
}

func withoutLabel(key string) func(*capacity.Offering) {
	return func(offering *capacity.Offering) { delete(offering.Labels, key) }
}

// matchingOfferings returns the names of the offerings the allocation
// controller would consider for the requirements.
func matchingOfferings(requirements []capacity.Requirement, offerings []capacity.Offering) []string {
	var names []string

	for _, offering := range offerings {
		matches := true

		for _, requirement := range requirements {
			matches = matches && selects(requirement, offeringLabelSet(offering))
		}

		if matches {
			names = append(names, offering.Name)
		}
	}

	return names
}

func TestChooseOffering(t *testing.T) {
	for _, test := range []struct {
		name         string
		offerings    []capacity.Offering
		labels       map[string]string
		requirements []v1.NodeSelectorRequirementWithMinValues
		requests     corev1.ResourceList
		failed       []string
		want         string
	}{
		{
			name:      "the cheapest offering wins",
			offerings: []capacity.Offering{namedOffering("b", price("0.2")), namedOffering("a", zone("z2"), price("0.3"))},
			want:      "b",
		},
		{
			name:      "equal prices go to the lower name",
			offerings: []capacity.Offering{namedOffering("b"), namedOffering("a", zone("z2"))},
			want:      "a",
		},
		{
			// Karpenter copies a single-valued requirement into the NodeClaim's
			// labels. The label must not make the requirement look satisfied.
			name: "a requirement on a provider label selects among offerings",
			offerings: []capacity.Offering{
				namedOffering("one", rack("rack-one"), price("0.1")),
				namedOffering("two", rack("rack-two"), price("0.2")),
			},
			labels:       map[string]string{rackKey: "rack-two"},
			requirements: []v1.NodeSelectorRequirementWithMinValues{requirement(rackKey, corev1.NodeSelectorOpIn, "rack-two")},
			want:         "two",
		},
		{
			name: "a provider label can be excluded",
			offerings: []capacity.Offering{
				namedOffering("one", rack("rack-one"), price("0.1")),
				namedOffering("two", rack("rack-two"), price("0.2")),
			},
			requirements: []v1.NodeSelectorRequirementWithMinValues{requirement(rackKey, corev1.NodeSelectorOpNotIn, "rack-one")},
			want:         "two",
		},
		{
			name: "a provider label can be required to be absent",
			offerings: []capacity.Offering{
				namedOffering("one", rack("rack-one"), price("0.1")),
				namedOffering("none", price("0.2")),
			},
			requirements: []v1.NodeSelectorRequirementWithMinValues{requirement(rackKey, corev1.NodeSelectorOpDoesNotExist)},
			want:         "none",
		},
		{
			name:      "requirements on labels only the Node has are ignored",
			offerings: []capacity.Offering{namedOffering("a")},
			labels:    map[string]string{v1.NodePoolLabelKey: "workers", "example.com/team": "batch"},
			requirements: []v1.NodeSelectorRequirementWithMinValues{
				requirement(v1.NodePoolLabelKey, corev1.NodeSelectorOpIn, "workers"),
				requirement("unbounded-cloud.io/machineclass", corev1.NodeSelectorOpIn, "workers"),
				requirement("example.com/team", corev1.NodeSelectorOpIn, "batch"),
				requirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "test-region"),
			},
			want: "a",
		},
		{
			name:      "a region requirement on an offering without a region is ignored",
			offerings: []capacity.Offering{namedOffering("a", withoutLabel(corev1.LabelTopologyRegion))},
			requirements: []v1.NodeSelectorRequirementWithMinValues{
				requirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "elsewhere"),
			},
			want: "a",
		},
		{
			name:      "the defaulted operating system satisfies its requirement",
			offerings: []capacity.Offering{namedOffering("a", withoutLabel(corev1.LabelOSStable))},
			requirements: []v1.NodeSelectorRequirementWithMinValues{
				requirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
			},
			want: "a",
		},
		{
			name:      "the defaulted operating system can fail its requirement",
			offerings: []capacity.Offering{namedOffering("a", withoutLabel(corev1.LabelOSStable))},
			requirements: []v1.NodeSelectorRequirementWithMinValues{
				requirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "windows"),
			},
		},
		{
			name: "the Site name is the zone of a zoneless offering",
			offerings: []capacity.Offering{
				namedOffering("zoned", price("0.1")),
				namedOffering("zoneless", zoneless, price("0.2")),
			},
			requirements: []v1.NodeSelectorRequirementWithMinValues{
				requirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "test-site"),
			},
			want: "zoneless",
		},
		{
			name: "a zone requirement excludes the zoneless offering",
			offerings: []capacity.Offering{
				namedOffering("zoneless", zoneless, price("0.1")),
				namedOffering("zoned", price("0.2")),
			},
			requirements: []v1.NodeSelectorRequirementWithMinValues{
				requirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "test-region-1"),
			},
			want: "zoned",
		},
		{
			name:      "the purchase model is matched in Karpenter's terms",
			offerings: []capacity.Offering{namedOffering("a")},
			requirements: []v1.NodeSelectorRequirementWithMinValues{
				requirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeReserved, v1.CapacityTypeOnDemand),
			},
			want: "a",
		},
		{
			name:      "another purchase model does not match",
			offerings: []capacity.Offering{namedOffering("a")},
			requirements: []v1.NodeSelectorRequirementWithMinValues{
				requirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeSpot),
			},
		},
		{
			name: "an inclusive comparison is evaluated",
			offerings: []capacity.Offering{
				namedOffering("old", func(offering *capacity.Offering) { offering.Labels["example.com/generation"] = "6" }, price("0.1")),
				namedOffering("new", func(offering *capacity.Offering) { offering.Labels["example.com/generation"] = "7" }, price("0.2")),
			},
			requirements: []v1.NodeSelectorRequirementWithMinValues{requirement("example.com/generation", "Gte", "7")},
			want:         "new",
		},
		{
			name:      "a recently failed offering is passed over",
			offerings: []capacity.Offering{namedOffering("a", price("0.1")), namedOffering("b", zone("z2"), price("0.2"))},
			failed:    []string{"a"},
			want:      "b",
		},
		{
			name:      "nothing is left when every offering failed recently",
			offerings: []capacity.Offering{namedOffering("a"), namedOffering("b")},
			failed:    []string{"a", "b"},
		},
		{
			name: "an unavailable offering is passed over",
			offerings: []capacity.Offering{
				namedOffering("a", price("0.1"), func(offering *capacity.Offering) { offering.Availability = "Unavailable" }),
				namedOffering("b", zone("z2"), price("0.2")),
			},
			want: "b",
		},
		{
			name: "an expired offering is passed over",
			offerings: []capacity.Offering{
				namedOffering("a", price("0.1"), func(offering *capacity.Offering) {
					expired := metav1.NewTime(time.Now().Add(-time.Minute))
					offering.ValidUntil = &expired
				}),
				namedOffering("b", zone("z2"), price("0.2")),
			},
			want: "b",
		},
		{
			name: "an offering Karpenter cannot use is passed over",
			offerings: []capacity.Offering{
				namedOffering("a", price("0.1"), withoutLabel(corev1.LabelArchStable)),
				namedOffering("b", price("0.2")),
			},
			want: "b",
		},
		{
			name:      "the request must fit after overhead",
			offerings: []capacity.Offering{namedOffering("a")},
			requests:  corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3950m")},
		},
		{
			name:      "the request must name resources the offering has",
			offerings: []capacity.Offering{namedOffering("a")},
			requests:  corev1.ResourceList{"example.com/gpu": resource.MustParse("1")},
		},
		{
			name:      "a flexibility floor does not block the launch",
			offerings: []capacity.Offering{namedOffering("a")},
			requirements: []v1.NodeSelectorRequirementWithMinValues{{
				Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn,
				Values: []string{"test.medium", "test.large"}, MinValues: new(2),
			}},
			want: "a",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			class := testClass()
			class.Status.Offerings = test.offerings
			provider, _ := testProvider(t, class)

			for _, name := range test.failed {
				provider.MarkUnavailable(class.UID, name)
			}

			claim := testClaim()
			claim.Labels = test.labels
			claim.Spec.Requirements = test.requirements

			if test.requests != nil {
				claim.Spec.Resources.Requests = test.requests
			}

			chosen, err := provider.chooseOffering(claim, class)
			if test.want == "" {
				if !cloudprovider.IsInsufficientCapacityError(err) {
					t.Fatalf("expected insufficient capacity, got offering %v and error %v", chosen, err)
				}

				return
			}

			must(t, err)

			if chosen.Name != test.want {
				t.Fatalf("chose offering %s, want %s", chosen.Name, test.want)
			}

			// The requirements sent must make the allocation controller
			// consider the chosen offering and no other.
			pinned := pinRequirements(chosen, class.Status.Offerings)
			if matched := matchingOfferings(pinned, class.Status.Offerings); !reflect.DeepEqual(matched, []string{test.want}) {
				t.Fatalf("requirements %+v select %v under the controller's rule, want only %s", pinned, matched, test.want)
			}
		})
	}
}

func TestPinRequirementsDescribeTheOfferingInTheControllersTerms(t *testing.T) {
	chosen := namedOffering("chosen", zoneless, withoutLabel(corev1.LabelOSStable))
	other := namedOffering("other", rack("rack-one"))

	got := pinRequirements(&chosen, []capacity.Offering{chosen, other})
	want := []capacity.Requirement{
		{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}},
		{Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"test.medium"}},
		{Key: corev1.LabelTopologyRegion, Operator: corev1.NodeSelectorOpIn, Values: []string{"test-region"}},
		{Key: capacity.CapacityTypeLabel, Operator: corev1.NodeSelectorOpIn, Values: []string{"OnDemand"}},
		// Labels the chosen offering lacks and another offering has. The
		// plugin's defaults for them (linux, the Site name) are never sent:
		// the controller would match them against labels that are not there.
		{Key: rackKey, Operator: corev1.NodeSelectorOpDoesNotExist},
		{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpDoesNotExist},
		{Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpDoesNotExist},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pinned requirements:\n got %+v\nwant %+v", got, want)
	}

	// Purchase models round-trip into the NodeClaim that inventory reports.
	restored, err := restoredSpec(&capacity.Allocation{Spec: capacity.AllocationSpec{Requirements: got}})
	must(t, err)

	for _, requirement := range restored.Requirements {
		if requirement.Key == capacity.CapacityTypeLabel {
			t.Fatal("the shared capacity-type label leaked into the NodeClaim")
		}

		if requirement.Key == v1.CapacityTypeLabelKey && !reflect.DeepEqual(requirement.Values, []string{v1.CapacityTypeOnDemand}) {
			t.Fatalf("purchase model was not translated back: %v", requirement.Values)
		}
	}
}

func TestCreateWithoutACandidateMakesNoAllocation(t *testing.T) {
	ctx := t.Context()
	class := testClass()
	provider, store := testProvider(t, class)
	provider.MarkUnavailable(class.UID, class.Status.Offerings[0].Name)

	_, err := provider.Create(ctx, testClaim())
	if !cloudprovider.IsInsufficientCapacityError(err) {
		t.Fatalf("expected insufficient capacity, got %v", err)
	}

	list := allocationList()
	must(t, store.List(ctx, list))

	if len(list.Items) != 0 {
		t.Fatal("a launch with no candidate offering must not leave an allocation behind")
	}
}
