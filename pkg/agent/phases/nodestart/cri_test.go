// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import "testing"

func TestPlainHTTPRegistryHost(t *testing.T) {
	tests := []struct {
		name         string
		image        string
		wantRegistry string
		wantOK       bool
	}{
		{
			name:         "private IPv4 with port",
			image:        "192.168.110.1:5000/rootfs/pause:3.9",
			wantRegistry: "192.168.110.1:5000",
			wantOK:       true,
		},
		{
			name:         "localhost",
			image:        "localhost:5000/pause:3.9",
			wantRegistry: "localhost:5000",
			wantOK:       true,
		},
		{
			name:   "public registry",
			image:  "mcr.microsoft.com/oss/kubernetes/pause:3.9",
			wantOK: false,
		},
		{
			name:   "docker hub short name",
			image:  "pause:3.9",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRegistry, gotOK := plainHTTPRegistryHost(tt.image)
			if gotOK != tt.wantOK {
				t.Fatalf("plainHTTPRegistryHost(%q) ok = %v, want %v", tt.image, gotOK, tt.wantOK)
			}

			if gotRegistry != tt.wantRegistry {
				t.Fatalf("plainHTTPRegistryHost(%q) registry = %q, want %q", tt.image, gotRegistry, tt.wantRegistry)
			}
		})
	}
}
