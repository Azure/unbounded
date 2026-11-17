// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/Azure/unbounded/pkg/racersdk"
)

const (
	minPageSize = 4 << 10
	maxPageSize = racersdk.PageSize
)

// validatePageSize accepts zero (the SDK default) or a power of two from
// 4 KiB through 16 MiB.
func validatePageSize(size int) error {
	if size == 0 {
		return nil
	}

	if size < minPageSize || size > maxPageSize || size&(size-1) != 0 {
		return fmt.Errorf("page-size must be a power of two in [%d, %d]", minPageSize, maxPageSize)
	}

	return nil
}

// effectivePageSize returns the page size used by diagnostics.
func effectivePageSize(size int) int {
	if size == 0 {
		return racersdk.PageSize
	}

	return size
}
