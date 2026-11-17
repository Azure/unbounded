// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Azure/unbounded/pkg/racersdk"
)

const (
	// PageSizeEnv configures the logical page size of Gantry's Racer cache.
	// The value is a byte count or a human-readable KiB/MiB quantity and must
	// match the ClusterCache spec.pageSize.
	PageSizeEnv = "GANTRY_RACER_PAGE_SIZE"
	// PageSizeFallbackEnv is the shared Racer page-size variable, used when
	// PageSizeEnv is unset or empty.
	PageSizeFallbackEnv = "RACER_PAGE_SIZE"

	minPageSize = 4 << 10
	maxPageSize = racersdk.PageSize
)

// PageSizeFromEnv returns the configured page size, zero when no environment
// variable is set, or an error for an invalid value. PageSizeEnv takes
// precedence over PageSizeFallbackEnv.
func PageSizeFromEnv() (int, error) {
	for _, name := range []string{PageSizeEnv, PageSizeFallbackEnv} {
		value, ok := os.LookupEnv(name)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}

		size, err := parsePageSize(value)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", name, err)
		}

		return size, nil
	}

	return 0, nil
}

// parsePageSize accepts bytes or a KiB/MiB quantity. The result must be a
// power of two from 4 KiB through 16 MiB.
func parsePageSize(value string) (int, error) {
	text := strings.TrimSpace(value)
	multiplier := int64(1)

	lower := strings.ToLower(text)
	switch {
	case strings.HasSuffix(lower, "kib"):
		multiplier = 1 << 10
		text = strings.TrimSpace(text[:len(text)-3])
	case strings.HasSuffix(lower, "mib"):
		multiplier = 1 << 20
		text = strings.TrimSpace(text[:len(text)-3])
	}

	size, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid page size %q", value)
	}

	if size <= 0 || size > int64(maxPageSize)/multiplier {
		return 0, fmt.Errorf("page size %q must be a power of two in [4 KiB, 16 MiB]", value)
	}

	size *= multiplier
	if size < minPageSize || size > maxPageSize || size&(size-1) != 0 {
		return 0, fmt.Errorf("page size %q must be a power of two in [4 KiB, 16 MiB]", value)
	}

	return int(size), nil
}

// configuredPageSize returns the configured page size for the SDK. An invalid
// environment value becomes a negative size, which the SDK rejects, so startup
// fails with its canonical configuration error rather than silently using the
// 16 MiB default.
func configuredPageSize() int {
	size, err := PageSizeFromEnv()
	if err != nil {
		return -1
	}

	return size
}

// handlerPageSize returns a valid page size for the HTTP handler. Invalid
// environment values cannot reach this point in production because the SDK
// client and origin reject them at startup.
func handlerPageSize() int {
	size, err := PageSizeFromEnv()
	if err != nil || size == 0 {
		return racersdk.PageSize
	}

	return size
}

// validPageSize reports whether size is a supported non-default page size.
func validPageSize(size int) bool {
	return size >= minPageSize && size <= maxPageSize && size&(size-1) == 0
}
