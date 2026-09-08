// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cdsub

import "testing"

func TestRegistryFromImage(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"docker hub canonical", "docker.io/library/alpine:3.18", "docker.io"},
		{"k8s registry", "registry.k8s.io/pause:3.9", "registry.k8s.io"},
		{"ghcr by digest", "ghcr.io/owner/repo@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "ghcr.io"},
		{"localhost with port", "localhost:5000/myimg:tag", "localhost:5000"},
		{"localhost no port", "localhost/myimg:tag", "localhost"},
		{"port-only host", "internal-host:5001/x:y", "internal-host:5001"},
		{"bare short name", "alpine", ""},
		{"docker library shorthand", "library/alpine:3.18", ""},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := registryFromImage(tc.in)
			if got != tc.want {
				t.Fatalf("registryFromImage(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
