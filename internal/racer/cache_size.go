// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"fmt"
	"math"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Cache capacities are disk bytes, independent of memory and shard layout.
const (
	DefaultCacheSizeBytes int64 = 10 << 30
	MinCacheSizeBytes     int64 = 512 << 20
	CacheSizeAlignment    int64 = 64 << 20
	MaxCacheSizeBytes     int64 = math.MaxInt64 / CacheSizeAlignment * CacheSizeAlignment
)

// NormalizeCacheSize validates a quantity and rounds it up to 64MiB disk extents.
// It rejects requests below 512MiB, fractional bytes, and sizes that would exceed
// signed 64-bit file offsets after alignment. It does not mutate the quantity.
// Compare returned bytes, rather than quantity spellings, for effective changes.
func NormalizeCacheSize(size resource.Quantity) (int64, error) {
	if size.CmpInt64(MinCacheSizeBytes) < 0 || size.CmpInt64(MaxCacheSizeBytes) > 0 {
		return 0, fmt.Errorf("cache size must be between 512Mi and 8589934591.9375Gi")
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

// ResolveCacheSize returns the Node annotation's capacity, or 10GiB when absent.
// A present but invalid annotation errors. A nil Node has no override.
// Resolution does not depend on Site membership and never mutates the Node;
// call it again with the current Node to observe live size changes.
func ResolveCacheSize(node *corev1.Node) (int64, error) {
	if node != nil {
		if value, present := node.Annotations[CacheSizeAnnotationKey]; present {
			bytes, err := ParseCacheSize(value)
			if err != nil {
				return 0, fmt.Errorf("node %q annotation %s: %w", node.Name, CacheSizeAnnotationKey, err)
			}

			return bytes, nil
		}
	}

	return DefaultCacheSizeBytes, nil
}
