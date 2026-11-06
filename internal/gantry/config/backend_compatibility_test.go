// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func TestArtifactStreamingAndRacerCompatibility(t *testing.T) {
	for _, test := range []struct {
		name      string
		streaming bool
		racer     bool
	}{
		{name: "containerd"},
		{name: "artifact streaming", streaming: true},
		{name: "racer", racer: true},
		{name: "unsupported combination", streaming: true, racer: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := NewDefault()
			config.UpstreamRegistries = []UpstreamRegistry{{Name: "registry.example.com", Endpoint: "https://registry.example.com"}}
			config.ArtifactStreamingEnabled = test.streaming
			config.RacerEnabled = test.racer

			err := config.Validate()
			if test.streaming && test.racer {
				if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
					t.Fatalf("Validate = %v, want an explicit unsupported-backend error", err)
				}
			} else if err != nil {
				t.Fatalf("supported backend configuration rejected: %v", err)
			}
		})
	}
}
