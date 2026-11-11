// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/log"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
)

const (
	// The estimate for an offering without a usable price, per hour: the
	// coefficients Karpenter's kwok provider uses, so relative cost follows
	// machine size. It orders instance types; it is not a real price.
	estimatePerCPU      = 0.025
	estimatePerMemoryGB = 0.001
)

// GetInstanceTypes presents the offerings of the NodePool's MachineClass as
// Karpenter instance types. Every usable offering is returned, available or
// not. An offering Karpenter cannot use is skipped and counted, and the call
// fails only when the class has none it can use.
func (p *Provider) GetInstanceTypes(ctx context.Context, pool *v1.NodePool) ([]*cloudprovider.InstanceType, error) {
	if pool == nil || !supportedClass(pool.Spec.Template.Spec.NodeClassRef) {
		return nil, fmt.Errorf("NodePool must reference an Unbounded MachineClass")
	}

	class, err := p.readyClass(ctx, pool.Spec.Template.Spec.NodeClassRef.Name)
	if err != nil {
		return nil, err
	}

	if len(class.Status.Offerings) == 0 {
		return nil, fmt.Errorf("MachineClass %s has no observed offerings", class.Name)
	}

	types := make(map[string]*cloudprovider.InstanceType)
	offeringKeys := offeringRequirementKeys(pool)
	now := p.now()

	var unusable, estimated int

	for _, observed := range class.Status.Offerings {
		offering, priceEstimated, err := p.convertOffering(observed, class.Spec.SiteRef.Name, now)
		if err != nil {
			unusable++

			log.FromContext(ctx).V(1).Info("skipping offering Karpenter cannot use",
				"MachineClass", class.Name, "offering", observed.Name, "reason", err.Error())

			continue
		}

		if priceEstimated {
			estimated++
		}

		if p.recentlyFailed(class.UID, observed.Name) {
			offering.Available = false
		}

		instanceType := types[observed.InstanceType]
		if instanceType == nil {
			instanceType = &cloudprovider.InstanceType{
				Name:         observed.InstanceType,
				Capacity:     observed.Capacity.DeepCopy(),
				Overhead:     convertOverhead(observed.Overhead),
				Requirements: scheduling.NewRequirements(),
			}
			types[observed.InstanceType] = instanceType
		}

		// Keep each topology and purchase-model tuple correlated, including its exact resources.
		offering.CapacityOverride = observed.Capacity.DeepCopy()
		offering.OverheadOverride = convertOverhead(observed.Overhead)
		instanceType.Offerings = append(instanceType.Offerings, offering)

		for key, requirement := range offering.Requirements {
			if !instanceType.Requirements.Has(key) {
				instanceType.Requirements.Add(scheduling.NewRequirement(
					key,
					corev1.NodeSelectorOpIn,
					requirement.Values()...,
				))

				continue
			}

			instanceType.Requirements.Get(key).Insert(requirement.Values()...)
		}

		// Upstream rejects undeclared custom offering keys, even when the instance type matches.
		// Retain all provider labels above, but only constrain offerings with scheduler-known keys.
		for key := range offering.Requirements {
			if !offeringKeys.Has(key) {
				delete(offering.Requirements, key)
			}
		}
	}

	unusableOfferings.WithLabelValues(class.Name).Set(float64(unusable))
	estimatedPriceOfferings.WithLabelValues(class.Name).Set(float64(estimated))

	if len(types) == 0 {
		return nil, fmt.Errorf("MachineClass %s has no offering Karpenter can use", class.Name)
	}

	names := sets.List(sets.KeySet(types))

	result := make([]*cloudprovider.InstanceType, 0, len(names))
	for _, name := range names {
		result = append(result, types[name])
	}

	return result, nil
}

func offeringRequirementKeys(pool *v1.NodePool) sets.Set[string] {
	keys := v1.WellKnownLabels.Clone()
	keys.Insert(
		v1.NodeClassLabelKey(pool.Spec.Template.Spec.NodeClassRef.GroupKind()),
		v1.NodeRegisteredLabelKey,
		v1.NodeInitializedLabelKey,
	)

	for key := range pool.Spec.Template.Labels {
		keys.Insert(key)
	}

	for _, requirement := range pool.Spec.Template.Spec.Requirements {
		keys.Insert(requirement.Key)
	}

	return keys
}

// convertOffering converts one MachineClass offering. It requires what
// cannot be guessed (capacity and architecture), defaults the zone to the
// class's Site and the operating system to linux, and estimates a missing
// price.
// It also reports whether the price is an estimate.
func (p *Provider) convertOffering(observed capacity.Offering, siteName string, now time.Time) (*cloudprovider.Offering, bool, error) {
	fail := func(format string, arguments ...any) (*cloudprovider.Offering, bool, error) {
		return nil, false, fmt.Errorf(format, arguments...)
	}

	if observed.InstanceType == "" || observed.Name == "" {
		return fail("offering identity is missing")
	}

	purchaseModel, err := capacityType(observed.CapacityType)
	if err != nil {
		return nil, false, err
	}

	labels := maps.Clone(observed.Labels)
	if labels == nil {
		labels = map[string]string{}
	}

	// The allocation controller derives these labels from the offering's
	// fields, and rejects an offering whose explicit label disagrees.
	derived := map[string]string{
		corev1.LabelInstanceTypeStable: observed.InstanceType,
		capacity.CapacityTypeLabel:     observed.CapacityType,
	}
	if observed.Zone != "" {
		derived[corev1.LabelTopologyZone] = observed.Zone
	}

	for key, value := range derived {
		if existing, found := labels[key]; found && existing != value {
			return fail("offering label %s disagrees with its fields", key)
		}

		labels[key] = value
	}

	if labels[corev1.LabelArchStable] == "" {
		return fail("offering has no %s label", corev1.LabelArchStable)
	}

	defaultLabels(labels, siteName)

	if labels[corev1.LabelTopologyZone] == "" {
		return fail("offering has no zone and its class has no Site name")
	}

	for _, name := range []corev1.ResourceName{
		corev1.ResourceCPU,
		corev1.ResourceMemory,
		corev1.ResourcePods,
	} {
		quantity := observed.Capacity[name]
		if quantity.Sign() <= 0 {
			return fail("offering capacity %s is missing or nonpositive", name)
		}
	}

	if err := validateOverhead(observed); err != nil {
		return nil, false, err
	}

	price, estimated, err := p.offeringPrice(observed)
	if err != nil {
		return nil, false, err
	}

	// The shared label is not a Karpenter scheduling key.
	delete(labels, capacity.CapacityTypeLabel)
	labels[v1.CapacityTypeLabelKey] = purchaseModel

	// The same rule the allocation controller applies before it launches.
	launchable := observed.Availability == "Available" &&
		observed.ValidUntil != nil && observed.ValidUntil.After(now)

	return &cloudprovider.Offering{
		Requirements: scheduling.NewLabelRequirements(labels),
		Price:        price,
		Available:    launchable,
	}, estimated, nil
}

func validateOverhead(offering capacity.Offering) error {
	remaining := offering.Capacity.DeepCopy()

	for _, reserved := range []corev1.ResourceList{
		offering.Overhead.KubeReserved,
		offering.Overhead.SystemReserved,
		offering.Overhead.EvictionThreshold,
	} {
		for name, quantity := range reserved {
			if quantity.Sign() < 0 {
				return fmt.Errorf("offering overhead %s is negative", name)
			}

			available := remaining[name]
			available.Sub(quantity)

			if available.Sign() < 0 {
				return fmt.Errorf("offering overhead %s exceeds capacity", name)
			}

			remaining[name] = available
		}
	}

	return nil
}

// offeringPrice returns the offering's hourly price in the configured
// currency. An offering with no price, or a price in another currency, gets
// an estimate from its size. A price that is present but malformed is an
// error: a wrong number must not look like a real one.
func (p *Provider) offeringPrice(offering capacity.Offering) (float64, bool, error) {
	price := offering.Price
	if price == nil || price.Currency != p.options.PriceCurrency {
		return estimatedPrice(offering.Capacity), true, nil
	}

	if price.PerHour == "" {
		return 0, false, fmt.Errorf("offering price is not a decimal")
	}

	for _, character := range []byte(price.PerHour) {
		if (character < '0' || character > '9') && character != '.' {
			return 0, false, fmt.Errorf("offering price is not a decimal")
		}
	}

	value, err := strconv.ParseFloat(price.PerHour, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) || value < 0 {
		return 0, false, fmt.Errorf("offering price is not a finite nonnegative decimal")
	}

	if value == 0 && strings.ContainsAny(price.PerHour, "123456789") {
		return 0, false, fmt.Errorf("positive offering price underflows Karpenter's numeric representation")
	}

	return value, false, nil
}

// estimatedPrice is a relative hourly cost from CPU and memory. It is never
// zero for an offering with capacity.
func estimatedPrice(resources corev1.ResourceList) float64 {
	cpu := resources[corev1.ResourceCPU]
	memory := resources[corev1.ResourceMemory]

	return estimatePerCPU*cpu.AsApproximateFloat64() + estimatePerMemoryGB*memory.AsApproximateFloat64()/1e9
}

func convertOverhead(overhead capacity.Overhead) *cloudprovider.InstanceTypeOverhead {
	return &cloudprovider.InstanceTypeOverhead{
		KubeReserved:      overhead.KubeReserved.DeepCopy(),
		SystemReserved:    overhead.SystemReserved.DeepCopy(),
		EvictionThreshold: overhead.EvictionThreshold.DeepCopy(),
	}
}

func capacityType(value string) (string, error) {
	switch value {
	case "OnDemand":
		return v1.CapacityTypeOnDemand, nil
	case "Spot":
		return v1.CapacityTypeSpot, nil
	default:
		return "", fmt.Errorf("unknown capacity purchase model %q", value)
	}
}
