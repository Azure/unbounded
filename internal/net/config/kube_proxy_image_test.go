// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParseImageReference(t *testing.T) {
	tests := []struct {
		name        string
		ref         string
		want        ImageReference
		wantVersion bool
	}{
		{
			name: "repository only",
			ref:  "registry.k8s.io/kube-proxy",
			want: ImageReference{Name: "registry.k8s.io/kube-proxy"},
		},
		{
			name:        "tag",
			ref:         "registry.k8s.io/kube-proxy:v1.34.2",
			want:        ImageReference{Name: "registry.k8s.io/kube-proxy", Tag: "v1.34.2"},
			wantVersion: true,
		},
		{
			name:        "provider tag suffix",
			ref:         "mcr.microsoft.com/oss/v2/kubernetes/kube-proxy:v1.34.2-1",
			want:        ImageReference{Name: "mcr.microsoft.com/oss/v2/kubernetes/kube-proxy", Tag: "v1.34.2-1"},
			wantVersion: true,
		},
		{
			name: "registry port is not a tag",
			ref:  "localhost:5000/kube-proxy",
			want: ImageReference{Name: "localhost:5000/kube-proxy"},
		},
		{
			name:        "registry port and tag",
			ref:         "localhost:5000/kube-proxy:v1.34.2",
			want:        ImageReference{Name: "localhost:5000/kube-proxy", Tag: "v1.34.2"},
			wantVersion: true,
		},
		{
			name: "IPv6 registry with port",
			ref:  "[::1]:5000/kube-proxy",
			want: ImageReference{Name: "[::1]:5000/kube-proxy"},
		},
		{
			name: "uppercase registry host",
			ref:  "My.Registry.example/kube-proxy",
			want: ImageReference{Name: "My.Registry.example/kube-proxy"},
		},
		{
			name: "bare name",
			ref:  "kube-proxy",
			want: ImageReference{Name: "kube-proxy"},
		},
		{
			name:        "bare name with tag",
			ref:         "kube-proxy:v1",
			want:        ImageReference{Name: "kube-proxy", Tag: "v1"},
			wantVersion: true,
		},
		{
			name:        "digest",
			ref:         "registry.k8s.io/kube-proxy@" + testDigest,
			want:        ImageReference{Name: "registry.k8s.io/kube-proxy", Digest: testDigest},
			wantVersion: true,
		},
		{
			name:        "tag and digest",
			ref:         "registry.k8s.io/kube-proxy:v1.34.2@" + testDigest,
			want:        ImageReference{Name: "registry.k8s.io/kube-proxy", Tag: "v1.34.2", Digest: testDigest},
			wantVersion: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseImageReference(tt.ref)
			if err != nil {
				t.Fatalf("ParseImageReference(%q) error = %v", tt.ref, err)
			}

			if got != tt.want {
				t.Fatalf("ParseImageReference(%q) = %+v, want %+v", tt.ref, got, tt.want)
			}

			if got.HasVersion() != tt.wantVersion {
				t.Fatalf("HasVersion() = %v, want %v", got.HasVersion(), tt.wantVersion)
			}

			if got.String() != tt.ref {
				t.Fatalf("String() = %q, want the original %q", got.String(), tt.ref)
			}
		})
	}
}

func TestParseImageReferenceRejectsMalformedReferences(t *testing.T) {
	tests := map[string]string{
		"empty":                    "",
		"empty tag":                "registry.k8s.io/kube-proxy:",
		"empty digest":             "registry.k8s.io/kube-proxy@",
		"short digest":             "registry.k8s.io/kube-proxy@sha256:abc",
		"two digests":              "registry.k8s.io/kube-proxy@" + testDigest + "@" + testDigest,
		"uppercase path":           "registry.k8s.io/Kube-Proxy",
		"trailing slash":           "registry.k8s.io/kube-proxy/",
		"empty path component":     "registry.k8s.io//kube-proxy",
		"empty registry host":      ":5000/kube-proxy",
		"leading space":            " registry.k8s.io/kube-proxy",
		"build metadata in tag":    "registry.k8s.io/kube-proxy:v1.34.2+k3s1",
		"tag starting with a dash": "registry.k8s.io/kube-proxy:-v1",
		"name too long":            "registry.k8s.io/" + strings.Repeat("a", maxImageNameLength),
	}

	for name, ref := range tests {
		t.Run(name, func(t *testing.T) {
			if got, err := ParseImageReference(ref); err == nil {
				t.Fatalf("ParseImageReference(%q) = %+v, want an error", ref, got)
			}
		})
	}
}
