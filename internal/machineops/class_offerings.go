// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

var (
	offeringCurrencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
	offeringPerHourPattern  = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
)

// normalizeOfferings validates the offerings a provider resolved and returns
// a deep copy sorted by name. Zero offerings is valid.
func normalizeOfferings(offerings []unboundedv1alpha3.MachineClassOffering) ([]unboundedv1alpha3.MachineClassOffering, error) {
	if len(offerings) == 0 {
		return nil, nil
	}

	names := make(map[string]struct{}, len(offerings))

	for i := range offerings {
		if err := validateOffering(&offerings[i]); err != nil {
			return nil, fmt.Errorf("offering %d: %w", i, err)
		}

		name := offerings[i].Name
		if _, exists := names[name]; exists {
			return nil, fmt.Errorf("offering %d: name %q appears more than once", i, name)
		}

		names[name] = struct{}{}
	}

	normalized := make([]unboundedv1alpha3.MachineClassOffering, len(offerings))
	for i := range offerings {
		offerings[i].DeepCopyInto(&normalized[i])
	}

	slices.SortFunc(normalized, func(a, b unboundedv1alpha3.MachineClassOffering) int {
		return cmp.Compare(a.Name, b.Name)
	})

	return normalized, nil
}

func validateOffering(offering *unboundedv1alpha3.MachineClassOffering) error {
	if offering.Name == "" {
		return fmt.Errorf("name is empty")
	}

	if offering.InstanceType == "" {
		return fmt.Errorf("%q has an empty instanceType", offering.Name)
	}

	switch offering.CapacityType {
	case unboundedv1alpha3.CapacityTypeOnDemand, unboundedv1alpha3.CapacityTypeSpot:
	default:
		return fmt.Errorf("%q has capacityType %q, not %s or %s",
			offering.Name, offering.CapacityType, unboundedv1alpha3.CapacityTypeOnDemand, unboundedv1alpha3.CapacityTypeSpot)
	}

	switch offering.Availability {
	case unboundedv1alpha3.OfferingAvailable, unboundedv1alpha3.OfferingUnavailable, unboundedv1alpha3.OfferingAvailabilityUnknown:
	default:
		return fmt.Errorf("%q has availability %q, not %s, %s or %s", offering.Name, offering.Availability,
			unboundedv1alpha3.OfferingAvailable, unboundedv1alpha3.OfferingUnavailable, unboundedv1alpha3.OfferingAvailabilityUnknown)
	}

	if offering.ValidUntil == nil {
		return fmt.Errorf("%q has no validUntil", offering.Name)
	}

	if price := offering.Price; price != nil {
		if !offeringCurrencyPattern.MatchString(price.Currency) {
			return fmt.Errorf("%q has price currency %q, which does not match %s", offering.Name, price.Currency, offeringCurrencyPattern)
		}

		if !offeringPerHourPattern.MatchString(price.PerHour) {
			return fmt.Errorf("%q has price perHour %q, which does not match %s", offering.Name, price.PerHour, offeringPerHourPattern)
		}
	}

	return nil
}
