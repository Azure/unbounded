// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"cmp"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// offeringFailure is why no offering can be selected for an allocation.
type offeringFailure struct {
	reason  unboundedv1alpha3.MachineAllocationFailureReason
	message string
}

// selectedOffering is the offering chosen for an allocation and its label
// set.
type selectedOffering struct {
	offering unboundedv1alpha3.MachineClassOffering
	labels   map[string]string
}

// selectOffering returns the first candidate offering for resources and
// requirements: priced before unpriced, then by lower hourly price, then by
// name. A candidate is Available with a validUntil after now, fits resources
// after overhead, and matches every requirement with its label set. Each
// requirement with minValues needs that many distinct values of its key among
// the candidates.
func selectOffering(
	offerings []unboundedv1alpha3.MachineClassOffering,
	resources corev1.ResourceList,
	requirements []unboundedv1alpha3.MachineAllocationRequirement,
	now time.Time,
) (*selectedOffering, *offeringFailure) {
	if failure := validateRequirements(requirements); failure != nil {
		return nil, failure
	}

	var candidates []selectedOffering

	for i := range offerings {
		offering := &offerings[i]
		if !offeringLaunchable(offering, now) || !resourcesFit(resources, offering) {
			continue
		}

		labels, ok := offeringLabelSet(offering)
		if !ok || !requirementsMatch(requirements, labels) {
			continue
		}

		candidates = append(candidates, selectedOffering{offering: *offering.DeepCopy(), labels: labels})
	}

	if len(candidates) == 0 {
		return nil, &offeringFailure{
			reason:  unboundedv1alpha3.FailureInsufficientCapacity,
			message: "no Available, unexpired offering fits the requested resources and requirements",
		}
	}

	for _, requirement := range requirements {
		if requirement.MinValues == nil {
			continue
		}

		values := map[string]struct{}{}

		for _, candidate := range candidates {
			if value, ok := candidate.labels[requirement.Key]; ok {
				values[value] = struct{}{}
			}
		}

		if len(values) < int(*requirement.MinValues) {
			return nil, &offeringFailure{
				reason: unboundedv1alpha3.FailureInsufficientCapacity,
				message: fmt.Sprintf("requirement %s needs at least %d distinct values among the candidate offerings, which have %d",
					requirement.Key, *requirement.MinValues, len(values)),
			}
		}
	}

	slices.SortStableFunc(candidates, func(a, b selectedOffering) int {
		return compareOfferings(&a.offering, &b.offering)
	})

	return &candidates[0], nil
}

// offeringLaunchable reports whether the offering's own fields allow a launch
// at now.
func offeringLaunchable(offering *unboundedv1alpha3.MachineClassOffering, now time.Time) bool {
	return offering.Availability == unboundedv1alpha3.OfferingAvailable &&
		offering.ValidUntil != nil && offering.ValidUntil.After(now)
}

// offeringLabelSet returns the offering's labels plus the labels derived from
// its instance type, zone and capacity type. It reports false when an explicit
// label disagrees with a derived one.
func offeringLabelSet(offering *unboundedv1alpha3.MachineClassOffering) (map[string]string, bool) {
	labels := maps.Clone(offering.Labels)
	if labels == nil {
		labels = map[string]string{}
	}

	derived := map[string]string{
		corev1.LabelInstanceTypeStable:      offering.InstanceType,
		unboundedv1alpha3.LabelCapacityType: string(offering.CapacityType),
	}

	if offering.Zone != "" {
		derived[corev1.LabelTopologyZone] = offering.Zone
	}

	for key, value := range derived {
		if existing, ok := labels[key]; ok && existing != value {
			return nil, false
		}

		labels[key] = value
	}

	return labels, true
}

func validateRequirements(requirements []unboundedv1alpha3.MachineAllocationRequirement) *offeringFailure {
	for _, requirement := range requirements {
		invalid := func(format string) *offeringFailure {
			return &offeringFailure{
				reason:  unboundedv1alpha3.FailureInvalidRequest,
				message: fmt.Sprintf("requirement %s %s "+format, requirement.Key, requirement.Operator),
			}
		}

		switch requirement.Operator {
		case unboundedv1alpha3.RequirementOpIn, unboundedv1alpha3.RequirementOpNotIn:
			if len(requirement.Values) == 0 {
				return invalid("needs at least one value")
			}
		case unboundedv1alpha3.RequirementOpExists, unboundedv1alpha3.RequirementOpDoesNotExist:
			if len(requirement.Values) > 0 {
				return invalid("must not have values")
			}
		case unboundedv1alpha3.RequirementOpGt, unboundedv1alpha3.RequirementOpLt:
			if len(requirement.Values) != 1 {
				return invalid("needs exactly one integer value")
			}

			if _, err := strconv.ParseInt(requirement.Values[0], 10, 64); err != nil {
				return invalid("needs exactly one integer value")
			}
		default:
			return invalid("is not a supported operator")
		}
	}

	return nil
}

func requirementsMatch(requirements []unboundedv1alpha3.MachineAllocationRequirement, labels map[string]string) bool {
	for _, requirement := range requirements {
		if !requirementMatches(requirement, labels) {
			return false
		}
	}

	return true
}

// requirementMatches evaluates one validated requirement with label selector
// meaning.
func requirementMatches(requirement unboundedv1alpha3.MachineAllocationRequirement, labels map[string]string) bool {
	value, exists := labels[requirement.Key]

	switch requirement.Operator {
	case unboundedv1alpha3.RequirementOpIn:
		return exists && slices.Contains(requirement.Values, value)
	case unboundedv1alpha3.RequirementOpNotIn:
		return !exists || !slices.Contains(requirement.Values, value)
	case unboundedv1alpha3.RequirementOpExists:
		return exists
	case unboundedv1alpha3.RequirementOpDoesNotExist:
		return !exists
	case unboundedv1alpha3.RequirementOpGt, unboundedv1alpha3.RequirementOpLt:
		if !exists {
			return false
		}

		actual, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return false
		}

		bound, err := strconv.ParseInt(requirement.Values[0], 10, 64)
		if err != nil {
			return false
		}

		if requirement.Operator == unboundedv1alpha3.RequirementOpGt {
			return actual > bound
		}

		return actual < bound
	default:
		return false
	}
}

// resourcesFit reports whether every requested resource is in the offering's
// capacity and fits after its overhead.
func resourcesFit(requests corev1.ResourceList, offering *unboundedv1alpha3.MachineClassOffering) bool {
	allocatable := capacityAfterOverhead(offering)

	for name, requested := range requests {
		available, ok := allocatable[name]
		if !ok || requested.Cmp(available) > 0 {
			return false
		}
	}

	return true
}

// capacityAfterOverhead is the offering's capacity minus its kubeReserved,
// systemReserved and evictionThreshold.
func capacityAfterOverhead(offering *unboundedv1alpha3.MachineClassOffering) corev1.ResourceList {
	if offering.Capacity == nil {
		return nil
	}

	allocatable := make(corev1.ResourceList, len(offering.Capacity))

	for name, capacity := range offering.Capacity {
		available := capacity.DeepCopy()

		for _, reserved := range []corev1.ResourceList{
			offering.Overhead.KubeReserved,
			offering.Overhead.SystemReserved,
			offering.Overhead.EvictionThreshold,
		} {
			if quantity, ok := reserved[name]; ok {
				available.Sub(quantity)
			}
		}

		allocatable[name] = available
	}

	return allocatable
}

func compareOfferings(a, b *unboundedv1alpha3.MachineClassOffering) int {
	aPrice, aPriced := offeringPrice(a)
	bPrice, bPriced := offeringPrice(b)

	switch {
	case aPriced && !bPriced:
		return -1
	case !aPriced && bPriced:
		return 1
	case aPriced && bPriced:
		if c := aPrice.Cmp(bPrice); c != 0 {
			return c
		}
	}

	return cmp.Compare(a.Name, b.Name)
}

func offeringPrice(offering *unboundedv1alpha3.MachineClassOffering) (*big.Rat, bool) {
	if offering.Price == nil {
		return nil, false
	}

	price, ok := new(big.Rat).SetString(offering.Price.PerHour)

	return price, ok
}

// allocationLabels layers the configuration template's node labels, the
// allocation's node template labels and the offering's label set, later ones
// winning.
func allocationLabels(
	template *unboundedv1alpha3.MachineConfigurationTemplate,
	nodeTemplate *unboundedv1alpha3.MachineAllocationNodeTemplate,
	offeringLabels map[string]string,
) map[string]string {
	labels := map[string]string{}

	if template.Kubernetes != nil {
		maps.Copy(labels, template.Kubernetes.NodeLabels)
	}

	if nodeTemplate != nil {
		maps.Copy(labels, nodeTemplate.Labels)
	}

	maps.Copy(labels, offeringLabels)

	return labels
}
