// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package coldstart

import "testing"

func TestProportionalSeedCountDoesNotOverflow(t *testing.T) {
	maxInt := int(^uint(0) >> 1)

	if got := proportionalSeedCount(maxInt, maxInt-1, maxInt); got != maxInt-1 {
		t.Fatalf("proportionalSeedCount at MaxInt = %d, want %d", got, maxInt-1)
	}
}

func TestProportionalSeedCountRoundsUp(t *testing.T) {
	if got := proportionalSeedCount(64, 100, 128); got != 50 {
		t.Fatalf("proportionalSeedCount = %d, want 50", got)
	}

	if got := proportionalSeedCount(65, 100, 128); got != 51 {
		t.Fatalf("rounded proportionalSeedCount = %d, want 51", got)
	}
}
