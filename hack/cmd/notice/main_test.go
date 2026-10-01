// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"testing"
)

func TestRegisteredEcosystems(t *testing.T) {
	var names []string
	for _, collector := range collectors() {
		names = append(names, collector.Name())
	}

	slices.Sort(names)

	if !slices.Equal(names, []string{"cargo", "go", "npm"}) {
		t.Fatalf("registered ecosystems = %v", names)
	}
}
