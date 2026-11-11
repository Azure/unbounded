// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"cmp"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
	"github.com/Azure/unbounded/plugins/karpenter/internal/nodeclass"
)

// chooseOffering returns the class offering the NodeClaim launches from: the
// cheapest one that Karpenter can use, is launchable, did not fail a launch
// recently, fits the requested resources and satisfies the NodeClaim's
// requirements. Ties go to the lower name.
//
// The NodeClaim's requirements are evaluated here, with the same meaning
// Karpenter gave them when it planned the NodeClaim, because the allocation
// controller cannot evaluate them: it matches requirements against the
// offering's labels only. A NodeClaim also carries requirements on labels
// that only the Node has (the NodePool name, the NodeClass label, template
// labels) and on labels this plugin defaulted for Karpenter (the zone of a
// zoneless offering, the operating system). A requirement on a key the
// offering does not define cannot tell offerings apart and is ignored;
// Karpenter accepted it because the Node gets that label from the NodeClaim.
//
// Flexibility floors (minValues) are not evaluated: Karpenter enforces them
// when it plans the NodeClaim, and a launch uses exactly one offering.
func (p *Provider) chooseOffering(claim *v1.NodeClaim, class *nodeclass.MachineClass) (*capacity.Offering, error) {
	required := scheduling.NewNodeSelectorRequirementsWithMinValues(claim.Spec.Requirements...)
	now := p.now()

	type candidate struct {
		offering *capacity.Offering
		price    float64
	}

	var candidates []candidate

	for index := range class.Status.Offerings {
		observed := &class.Status.Offerings[index]

		converted, _, err := p.convertOffering(*observed, class.Spec.SiteRef.Name, now)
		if err != nil || !converted.Available || p.recentlyFailed(class.UID, observed.Name) {
			continue
		}

		if !fits(claim.Spec.Resources.Requests, observed) || required.Intersects(converted.Requirements) != nil {
			continue
		}

		candidates = append(candidates, candidate{offering: observed, price: converted.Price})
	}

	if len(candidates) == 0 {
		return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf(
			"MachineClass %s has no launchable offering that fits NodeClaim %s and did not fail recently",
			class.Name, claim.Name,
		))
	}

	best := slices.MinFunc(candidates, func(a, b candidate) int {
		return cmp.Or(cmp.Compare(a.price, b.price), cmp.Compare(a.offering.Name, b.offering.Name))
	})

	return best.offering, nil
}

// fits reports whether every requested resource is in the offering's
// capacity and fits after its overhead. It is the allocation controller's
// rule.
func fits(requests corev1.ResourceList, offering *capacity.Offering) bool {
	for name, requested := range requests {
		available, found := offering.Capacity[name]
		if !found {
			return false
		}

		available = available.DeepCopy()

		for _, reserved := range []corev1.ResourceList{
			offering.Overhead.KubeReserved,
			offering.Overhead.SystemReserved,
			offering.Overhead.EvictionThreshold,
		} {
			if quantity, found := reserved[name]; found {
				available.Sub(quantity)
			}
		}

		if requested.Cmp(available) > 0 {
			return false
		}
	}

	return true
}

// selectorLabels returns the label set the allocation controller matches
// requirements against: the offering's labels plus the labels derived from
// its instance type, zone and capacity type. It has none of the defaults
// this plugin adds for Karpenter.
func selectorLabels(offering *capacity.Offering) map[string]string {
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

// pinRequirements returns allocation requirements that describe the chosen
// offering exactly in the allocation controller's terms: every label it has,
// with its value, and the absence of every label that another offering of
// the class has and it does not. Only offerings with the chosen offering's
// whole label set match, so the controller launches what Karpenter planned,
// and still decides for itself whether that offering is launchable and fits.
func pinRequirements(chosen *capacity.Offering, offerings []capacity.Offering) []capacity.Requirement {
	labels := selectorLabels(chosen)
	absent := sets.New[string]()

	for index := range offerings {
		for key := range selectorLabels(&offerings[index]) {
			if _, found := labels[key]; !found {
				absent.Insert(key)
			}
		}
	}

	requirements := make([]capacity.Requirement, 0, len(labels)+absent.Len())

	for _, key := range slices.Sorted(maps.Keys(labels)) {
		requirements = append(requirements, capacity.Requirement{
			Key:      key,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{labels[key]},
		})
	}

	for _, key := range sets.List(absent) {
		requirements = append(requirements, capacity.Requirement{
			Key:      key,
			Operator: corev1.NodeSelectorOpDoesNotExist,
		})
	}

	return requirements
}
