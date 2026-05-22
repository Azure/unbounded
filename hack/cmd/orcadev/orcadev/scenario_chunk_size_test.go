// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import "testing"

func TestResolveScenarioChunkSizeUsesOverride(t *testing.T) {
	t.Parallel()

	g := defaultGlobalFlags()
	got, err := resolveScenarioChunkSize(g, "256KiB")
	if err != nil {
		t.Fatalf("resolveScenarioChunkSize() error = %v", err)
	}

	const want = 256 * 1024
	if got != want {
		t.Fatalf("resolveScenarioChunkSize() = %d, want %d", got, want)
	}
}
