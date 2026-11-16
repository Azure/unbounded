// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"maps"
	"slices"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
)

// TestSchedulerOutputProjectsOntoTheOffering takes the NodeClaim the pinned
// upstream scheduler plans and checks that the plugin chooses the class's
// offering for it and sends requirements that select that offering under the
// allocation controller's rule: requirements are matched against the
// offering's label set only. The planned NodeClaim also carries requirements
// on labels only the Node has, such as the NodePool name; sent as they are,
// they would match no offering and fail every launch.
func TestSchedulerOutputProjectsOntoTheOffering(t *testing.T) {
	class, pool := capturedSchedulingInputs(t)
	pod := workerDemoPod(t)
	result := simulateCapturedScheduling(t, class, pool, pod)

	if len(result.NewNodeClaims) != 1 {
		t.Fatalf("expected one planned NodeClaim, got %d", len(result.NewNodeClaims))
	}

	claim := result.NewNodeClaims[0].ToNodeClaim()
	claim.UID = "55361535-e293-488e-aa38-cef580e84311"

	if !slices.ContainsFunc(claim.Spec.Requirements, func(requirement v1.NodeSelectorRequirementWithMinValues) bool {
		return requirement.Key == v1.NodePoolLabelKey
	}) {
		t.Fatal("the scheduler is expected to restate Node labels as requirements; the fixture no longer shows it")
	}

	provider, _ := testProvider(t, class)
	// The captured offering is launchable at the time it was observed.
	provider.now = func() time.Time { return class.Status.Offerings[0].ObservedAt.Add(time.Second) }

	chosen, err := provider.chooseOffering(claim, class)
	must(t, err)

	requirements := pinRequirements(chosen, class.Status.Offerings)
	if matched := matchingOfferings(requirements, class.Status.Offerings); len(matched) != 1 || matched[0] != chosen.Name {
		t.Fatalf("requirements %+v select %v, want only %s", requirements, matched, chosen.Name)
	}
}

// offeringLabelSet mirrors the allocation controller: the offering's labels
// plus the labels derived from its instance type, zone and capacity type.
func offeringLabelSet(offering capacity.Offering) map[string]string {
	labels := maps.Clone(offering.Labels)
	if labels == nil {
		labels = map[string]string{}
	}

	labels[corev1.LabelInstanceTypeStable] = offering.InstanceType
	labels[capacity.CapacityTypeLabel] = offering.CapacityType

	if offering.Zone != "" {
		labels[corev1.LabelTopologyZone] = offering.Zone
	}

	return labels
}

// selects mirrors the allocation controller's label selector evaluation.
func selects(requirement capacity.Requirement, labels map[string]string) bool {
	value, exists := labels[requirement.Key]

	switch requirement.Operator {
	case corev1.NodeSelectorOpIn:
		return exists && slices.Contains(requirement.Values, value)
	case corev1.NodeSelectorOpNotIn:
		return !exists || !slices.Contains(requirement.Values, value)
	case corev1.NodeSelectorOpExists:
		return exists
	case corev1.NodeSelectorOpDoesNotExist:
		return !exists
	case corev1.NodeSelectorOpGt, corev1.NodeSelectorOpLt:
		actual, err := strconv.ParseInt(value, 10, 64)
		if !exists || err != nil {
			return false
		}

		bound, err := strconv.ParseInt(requirement.Values[0], 10, 64)
		if err != nil {
			return false
		}

		if requirement.Operator == corev1.NodeSelectorOpGt {
			return actual > bound
		}

		return actual < bound
	default:
		return false
	}
}
