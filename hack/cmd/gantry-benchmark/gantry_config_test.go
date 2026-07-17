// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"testing"

	gantryconfig "github.com/Azure/unbounded/internal/gantry/config"
)

func TestPatchGantryRegistry(t *testing.T) {
	original := []byte(`mirror_listen: "0.0.0.0:5000"
mirror_bind_allow_non_loopback: true
transfer_listen: "0.0.0.0:5001"
metrics_listen: "0.0.0.0:9095"
containerd_socket: "/run/containerd/containerd.sock"
containerd_namespace: "k8s.io"
upstream_registries:
  - name: "other.example.com"
    endpoint: "https://other.example.com"
  - name: "bench.azurecr.io"
    endpoint: "https://bench.azurecr.io"
hrw_k: 3
hrw_topology_scope: "cluster"
log_level: "info"
log_format: "json"
`)
	originalCopy := append([]byte(nil), original...)

	patched, err := patchGantryRegistry(
		original,
		"bench.azurecr.io",
		"http://acr-origin-proxy.gantry-benchmark.svc.cluster.local:5002",
		"10.0.0.42:5002",
	)
	if err != nil {
		t.Fatalf("patchGantryRegistry: %v", err)
	}

	if !bytes.Equal(original, originalCopy) {
		t.Fatal("patchGantryRegistry mutated the original bytes")
	}

	config := gantryconfig.NewDefault()
	if err := config.LoadYAML(bytes.NewReader(patched)); err != nil {
		t.Fatalf("load patched config: %v", err)
	}

	if len(config.UpstreamRegistries) != 2 {
		t.Fatalf("upstream registry count = %d, want 2", len(config.UpstreamRegistries))
	}

	if got := config.UpstreamRegistries[0].Endpoint; got != "https://other.example.com" {
		t.Fatalf("unrelated endpoint = %q", got)
	}

	got := config.UpstreamRegistries[1]
	if got.Endpoint != "http://acr-origin-proxy.gantry-benchmark.svc.cluster.local:5002" {
		t.Fatalf("patched endpoint = %q", got.Endpoint)
	}

	if got.NSAlias != "10.0.0.42:5002" {
		t.Fatalf("patched ns_alias = %q", got.NSAlias)
	}
}

func TestPatchGantryRegistryRequiresExactlyOneMatch(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "missing",
			raw: `upstream_registries:
  - name: other.example.com
    endpoint: https://other.example.com
`,
		},
		{
			name: "duplicate",
			raw: `upstream_registries:
  - name: bench.azurecr.io
    endpoint: https://bench.azurecr.io
  - name: bench.azurecr.io
    endpoint: https://bench.azurecr.io
`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := patchGantryRegistry(
				[]byte(test.raw),
				"bench.azurecr.io",
				"http://proxy:5002",
				"10.0.0.42:5002",
			); err == nil {
				t.Fatal("patchGantryRegistry succeeded")
			}
		})
	}
}
