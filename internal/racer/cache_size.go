// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"fmt"
	"math"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// Cache capacities are disk bytes, independent of memory and shard layout.
const (
	DefaultCacheSizeBytes int64 = 10 << 30
	MinCacheSizeBytes     int64 = 32 << 20
	CacheSizeAlignment    int64 = 4 << 20
	MaxCacheSizeBytes     int64 = math.MaxInt64 / CacheSizeAlignment * CacheSizeAlignment
)

// NormalizeCacheSize validates a quantity and rounds it up to 4MiB disk extents.
// It rejects requests below 32MiB, fractional bytes, and sizes that would exceed
// signed 64-bit file offsets after alignment. It does not mutate the quantity.
// Compare returned bytes, rather than quantity spellings, for effective changes.
func NormalizeCacheSize(size resource.Quantity) (int64, error) {
	if size.CmpInt64(MinCacheSizeBytes) < 0 || size.CmpInt64(MaxCacheSizeBytes) > 0 {
		return 0, fmt.Errorf("cache size must be between %d and %d bytes", MinCacheSizeBytes, MaxCacheSizeBytes)
	}

	// Value rounds fractional bytes up and can overflow without the range check
	// above. AsInt64 alone is insufficient: valid large or decimal quantities
	// can use the arbitrary-precision representation and fail its fast path.
	bytes := size.Value()
	if size.CmpInt64(bytes) != 0 {
		return 0, fmt.Errorf("cache size must be a whole number of bytes")
	}

	return (bytes + CacheSizeAlignment - 1) / CacheSizeAlignment * CacheSizeAlignment, nil
}

// ParseCacheSize parses a Kubernetes quantity and returns normalized disk bytes.
// An empty string is invalid, not an instruction to inherit a default.
func ParseCacheSize(value string) (int64, error) {
	size, err := resource.ParseQuantity(value)
	if err != nil {
		return 0, fmt.Errorf("parse cache size %q: %w", value, err)
	}

	return NormalizeCacheSize(size)
}

// ResolveCacheSize returns the effective capacity from the Node annotation, then
// the current Site default, then 10GiB. A present but invalid override errors;
// only absence permits inheritance. Nil Node/Site values have no override.
// The caller supplies the Node's current Site and handles lookup failures.
// This helper neither checks membership/enabled state nor mutates either object;
// call it again with current objects to observe live inheritance.
func ResolveCacheSize(node *corev1.Node, site *machinav1alpha3.Site) (int64, error) {
	if node != nil {
		if value, present := node.Annotations[CacheSizeAnnotationKey]; present {
			bytes, err := ParseCacheSize(value)
			if err != nil {
				return 0, fmt.Errorf("node %q annotation %s: %w", node.Name, CacheSizeAnnotationKey, err)
			}

			return bytes, nil
		}
	}

	if site != nil && site.Spec.Components.Racer != nil && site.Spec.Components.Racer.CacheSize != nil {
		bytes, err := NormalizeCacheSize(*site.Spec.Components.Racer.CacheSize)
		if err != nil {
			return 0, fmt.Errorf("site %q spec.components.racer.cacheSize: %w", site.Name, err)
		}

		return bytes, nil
	}

	return DefaultCacheSizeBytes, nil
}
