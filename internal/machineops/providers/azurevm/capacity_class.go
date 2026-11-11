// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurevm

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	publicmachineops "github.com/Azure/unbounded/pkg/machineops"
)

const (
	offeringTTL        = 15 * time.Minute
	ephemeralFraction  = 0.95
	evictionMemory     = "100Mi"
	evictionDiskFactor = 0.10
	gibibyte           = int64(1) << 30
	mebibyte           = int64(1) << 20
)

// ResolveClass observes one offering for the class: the configured VM size
// in the configured region and zone, with capacity and availability from the
// Azure Resource SKU list. Price is the operator-supplied observation, if any.
// It reads Azure and allocates nothing.
func (p *Provider) ResolveClass(ctx context.Context, request publicmachineops.ClassRequest) (publicmachineops.ClassResolution, error) {
	config, err := parseCapacityConfig(request.ProviderConfig.Data)
	if err != nil {
		return publicmachineops.ClassResolution{}, invalidRequest(err)
	}

	client, err := p.capacityClient(config.SubscriptionID, request.Auth)
	if err != nil {
		return publicmachineops.ClassResolution{}, fmt.Errorf("create Azure capacity client: %w", err)
	}

	observation, err := p.skus.get(ctx, client, config.SubscriptionID, config.Location, p.now())
	if err != nil {
		return publicmachineops.ClassResolution{}, err
	}

	offering, err := buildOffering(config, observation)
	if err != nil {
		return publicmachineops.ClassResolution{}, err
	}

	return publicmachineops.ClassResolution{
		Offerings: []unboundedv1alpha3.MachineClassOffering{
			offering,
		},
	}, nil
}

// buildOffering returns the class's offering. Every offering it returns has
// positive cpu, memory and pods capacity and an overhead within that
// capacity, so a consumer can size a Node from it even while it is
// Unavailable. A VM size the subscription is not offered in the region, or an
// overhead larger than the VM, is invalid configuration; SKU data that cannot
// be read is retried.
func buildOffering(config capacityConfig, observation skuCacheEntry) (unboundedv1alpha3.MachineClassOffering, error) {
	sku, ok := observation.skus[strings.ToLower(config.VMSize)]
	if !ok {
		return unboundedv1alpha3.MachineClassOffering{}, invalidRequest(
			fmt.Errorf("VM size %s is not offered to subscription %s in %s", config.VMSize, config.SubscriptionID, config.Location),
		)
	}

	shape, err := skuShape(sku)
	if err != nil {
		return unboundedv1alpha3.MachineClassOffering{}, fmt.Errorf("read Azure SKU %s in %s: %w", config.VMSize, config.Location, err)
	}

	if shape.noPremiumIO && strings.HasPrefix(config.OSDiskType, "Premium") {
		return unboundedv1alpha3.MachineClassOffering{}, invalidRequest(
			fmt.Errorf("%s %s needs premium storage, which VM size %s does not support", ConfigOSDiskType, config.OSDiskType, config.VMSize),
		)
	}

	observedAt := metav1.NewTime(observation.observedAt)
	validUntil := metav1.NewTime(observation.observedAt.Add(offeringTTL))

	offering := unboundedv1alpha3.MachineClassOffering{
		Name:         offeringName(config),
		InstanceType: config.VMSize,
		Zone:         config.offeringZone(),
		CapacityType: unboundedv1alpha3.CapacityTypeOnDemand,
		Labels: map[string]string{
			corev1.LabelArchStable:              shape.arch,
			corev1.LabelOSStable:                "linux",
			corev1.LabelTopologyRegion:          config.Location,
			corev1.LabelTopologyZone:            config.offeringZone(),
			corev1.LabelInstanceTypeStable:      config.VMSize,
			unboundedv1alpha3.LabelCapacityType: string(unboundedv1alpha3.CapacityTypeOnDemand),
		},
		Capacity:     offeringCapacity(config, shape),
		Overhead:     offeringOverhead(config),
		Availability: skuAvailability(sku, config),
		ObservedAt:   &observedAt,
		ValidUntil:   &validUntil,
	}

	if err := validateOverhead(offering); err != nil {
		return unboundedv1alpha3.MachineClassOffering{}, invalidRequest(err)
	}

	if config.Price != nil {
		priceObservedAt := metav1.NewTime(config.Price.ObservedAt)
		offering.Price = &unboundedv1alpha3.OfferingPrice{
			Currency:   config.Price.Currency,
			PerHour:    config.Price.PerHour,
			Source:     config.Price.Source,
			ObservedAt: &priceObservedAt,
		}
	}

	return offering, nil
}

// validateOverhead rejects reservations that leave a resource with nothing
// for workloads: the kube-reserved, system-reserved and eviction amounts of a
// resource must add up to less than its capacity. A reserved resource the VM
// does not have has zero capacity, so it is rejected too.
func validateOverhead(offering unboundedv1alpha3.MachineClassOffering) error {
	total := corev1.ResourceList{}

	for _, reserved := range []corev1.ResourceList{
		offering.Overhead.KubeReserved,
		offering.Overhead.SystemReserved,
		offering.Overhead.EvictionThreshold,
	} {
		for name, quantity := range reserved {
			sum := total[name]
			sum.Add(quantity)
			total[name] = sum
		}
	}

	for _, name := range reservableResources {
		reserved, ok := total[name]
		if !ok {
			continue
		}

		capacity := offering.Capacity[name]
		if reserved.Cmp(capacity) >= 0 {
			return fmt.Errorf("reserved %s %s must be less than the %s capacity %s of VM size %s",
				name, reserved.String(), name, capacity.String(), offering.InstanceType)
		}
	}

	return nil
}

func offeringName(config capacityConfig) string {
	return strings.ToLower(config.VMSize) + "-" + config.offeringZone() + "-on-demand"
}

type vmShape struct {
	vCPUs    int64
	memoryMi int64
	arch     string
	// noPremiumIO reports that the SKU says it cannot attach premium disks.
	noPremiumIO bool
}

func skuShape(sku *armcompute.ResourceSKU) (vmShape, error) {
	capabilities := map[string]string{}

	for _, capability := range sku.Capabilities {
		if capability == nil || capability.Name == nil || capability.Value == nil {
			continue
		}

		capabilities[*capability.Name] = *capability.Value
	}

	vCPUs, err := strconv.ParseInt(capabilities["vCPUs"], 10, 64)
	if err != nil || vCPUs <= 0 {
		return vmShape{}, fmt.Errorf("SKU vCPUs capability %q is invalid", capabilities["vCPUs"])
	}

	memoryGB, err := strconv.ParseFloat(capabilities["MemoryGB"], 64)
	if err != nil || memoryGB <= 0 {
		return vmShape{}, fmt.Errorf("SKU MemoryGB capability %q is invalid", capabilities["MemoryGB"])
	}

	arch := "amd64"
	if strings.EqualFold(capabilities["CpuArchitectureType"], "Arm64") {
		arch = "arm64"
	}

	return vmShape{
		vCPUs:       vCPUs,
		memoryMi:    int64(memoryGB * 1024),
		arch:        arch,
		noPremiumIO: strings.EqualFold(capabilities["PremiumIO"], "False"),
	}, nil
}

// offeringCapacity reports what the registered Node is expected to expose.
// Guest-visible memory is below the SKU size, so the configured overhead
// percentage is removed; the OS disk loses space to partitions and
// filesystem metadata before kubelet sees it.
//
// The pods capacity is the class's maxPods value, published as given. This
// adapter does not pass it to the kubelet: maxPods describes the limit the
// operator's machine configuration gives the kubelet and must match it. A
// value above the kubelet's real limit makes a consumer pack more pods onto
// the Node than it accepts.
func offeringCapacity(config capacityConfig, shape vmShape) corev1.ResourceList {
	memoryMi := int64(float64(shape.memoryMi) * (1 - config.MemoryOverheadPercent/100))
	diskMi := int64(float64(int64(config.OSDiskSizeGB)*gibibyte/mebibyte) * ephemeralFraction)

	return corev1.ResourceList{
		corev1.ResourceCPU:              *resource.NewQuantity(shape.vCPUs, resource.DecimalSI),
		corev1.ResourceMemory:           *resource.NewQuantity(memoryMi*mebibyte, resource.BinarySI),
		corev1.ResourceEphemeralStorage: *resource.NewQuantity(diskMi*mebibyte, resource.BinarySI),
		corev1.ResourcePods:             *resource.NewQuantity(config.MaxPods, resource.DecimalSI),
	}
}

// offeringOverhead reports the class's kubeReserved and systemReserved values
// plus the kubelet default hard eviction thresholds.
//
// None of these is applied to the host. This adapter does not configure the
// kubelet, so kubeReserved and systemReserved describe the reservations the
// operator's machine configuration gives the kubelet and must match them, and
// the eviction thresholds are right only while that configuration leaves the
// kubelet defaults unchanged.
func offeringOverhead(config capacityConfig) unboundedv1alpha3.OfferingOverhead {
	diskBytes := float64(int64(config.OSDiskSizeGB)*gibibyte) * ephemeralFraction
	evictionDiskMi := int64(diskBytes*evictionDiskFactor) / mebibyte

	return unboundedv1alpha3.OfferingOverhead{
		KubeReserved:   config.KubeReserved.DeepCopy(),
		SystemReserved: config.SystemReserved.DeepCopy(),
		EvictionThreshold: corev1.ResourceList{
			corev1.ResourceMemory:           resource.MustParse(evictionMemory),
			corev1.ResourceEphemeralStorage: *resource.NewQuantity(evictionDiskMi*mebibyte, resource.BinarySI),
		},
	}
}

func skuAvailability(sku *armcompute.ResourceSKU, config capacityConfig) unboundedv1alpha3.OfferingAvailability {
	for _, restriction := range sku.Restrictions {
		if restrictionApplies(restriction, config) {
			return unboundedv1alpha3.OfferingUnavailable
		}
	}

	if config.Zone == "" {
		return unboundedv1alpha3.OfferingAvailable
	}

	for _, info := range sku.LocationInfo {
		if info == nil || !strings.EqualFold(toValue(info.Location, ""), config.Location) {
			continue
		}

		if containsFold(info.Zones, config.Zone) {
			return unboundedv1alpha3.OfferingAvailable
		}
	}

	return unboundedv1alpha3.OfferingUnavailable
}

func restrictionApplies(restriction *armcompute.ResourceSKURestrictions, config capacityConfig) bool {
	if restriction == nil || restriction.Type == nil {
		return false
	}

	switch *restriction.Type {
	case armcompute.ResourceSKURestrictionsTypeLocation:
		return containsFold(restriction.Values, config.Location)
	case armcompute.ResourceSKURestrictionsTypeZone:
		if config.Zone == "" || restriction.RestrictionInfo == nil {
			return false
		}

		return containsFold(restriction.RestrictionInfo.Zones, config.Zone)
	default:
		return false
	}
}

func containsFold(values []*string, want string) bool {
	return slices.ContainsFunc(values, func(value *string) bool {
		return value != nil && strings.EqualFold(*value, want)
	})
}

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}

	return time.Now()
}

func invalidRequest(err error) error {
	return &publicmachineops.PermanentError{
		Reason: string(unboundedv1alpha3.FailureInvalidRequest),
		Err:    err,
	}
}
