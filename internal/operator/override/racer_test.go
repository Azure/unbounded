// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package override

import "testing"

func TestRacerKindsAndScope(t *testing.T) {
	for _, tc := range []struct {
		component, kind string
		sites           []string
		valid           bool
	}{
		{"racer-controlplane", "Deployment", nil, true},
		{"racer-controlplane", "DaemonSet", nil, false},
		{"racer-controlplane", "Deployment", []string{"rack-a"}, false},
		{"racer-dataplane", "DaemonSet", nil, true},
		{"racer-dataplane", "DaemonSet", []string{"rack-a"}, true},
		{"racer-dataplane", "Deployment", nil, false},
	} {
		entry := SourcedEntry{Entry: Entry{Component: tc.component, Kind: tc.kind, Sites: tc.sites, Patch: map[string]any{"spec": map[string]any{"minReadySeconds": int64(1)}}}}
		if err := ValidateErr([]SourcedEntry{entry}); (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}

func TestRacerIdentityMetadataReserved(t *testing.T) {
	for _, key := range []string{"racer.unbounded-cloud.io/universe", "racer.unbounded-cloud.io/dataplane", "racer.unbounded-cloud.io/component"} {
		for _, field := range []string{"labels", "annotations"} {
			for _, template := range []bool{false, true} {
				patch := map[string]any{"metadata": map[string]any{field: map[string]any{key: "forged"}}}
				if template {
					patch = map[string]any{"spec": map[string]any{"template": patch}}
				}

				entry := SourcedEntry{Entry: Entry{Component: "racer-dataplane", Kind: "DaemonSet", Patch: patch}}
				if ValidateErr([]SourcedEntry{entry}) == nil {
					t.Fatalf("identity override accepted: %v", patch)
				}
			}
		}
	}
}
