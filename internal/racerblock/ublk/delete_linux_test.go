// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ublk

import (
	"strings"
	"testing"
)

// TestDeleteRejectsInvalidIDBeforeKernelAccess checks that Delete rejects an
// out-of-range ID with its own validation error. Any error from opening the
// control device, such as a permission or missing-device error, would mean
// the ID reached the kernel path.
func TestDeleteRejectsInvalidIDBeforeKernelAccess(t *testing.T) {
	for _, id := range append(largeDeviceIDs(), -1) {
		err := Delete(id)
		if err == nil || !strings.Contains(err.Error(), "invalid device id") {
			t.Errorf("Delete(%d) = %v, want invalid device id", id, err)
		}
	}
}
