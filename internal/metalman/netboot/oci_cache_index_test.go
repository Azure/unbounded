// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"os"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestOCICacheIndexRoundTrip(t *testing.T) {
	cacheDir := t.TempDir()

	digest := "sha256:" + repeatHex("a")
	writeLayout(t, cacheDir, digest)
	writeDisk(t, cacheDir, digest, "amd64")

	cache := NewOCICache(cacheDir)
	cache.SetDigestForArchitecture("ghcr.io/test/image:v1", "amd64", digest)

	restored := NewOCICache(cacheDir)
	if err := restored.LoadIndex(); err != nil {
		t.Fatalf("LoadIndex: %v", err)
	}

	got, ok := restored.DigestForRepositoryReference("ghcr.io/test/image", "v1")
	if !ok || got != digest {
		t.Fatalf("digest after reload: got %q (ok=%t), want %q", got, ok, digest)
	}

	if !restored.IsCachedForArchitecture(digest, "amd64") {
		t.Fatal("unpacked image not reported as cached after reload")
	}
}

func TestOCICacheLoadIndexDropsMissingContent(t *testing.T) {
	cacheDir := t.TempDir()

	present := "sha256:" + repeatHex("b")
	writeLayout(t, cacheDir, present)

	cache := NewOCICache(cacheDir)
	cache.SetDigestForArchitecture("ghcr.io/test/present:v1", "amd64", present)
	cache.SetDigestForArchitecture("ghcr.io/test/gone:v1", "amd64", "sha256:"+repeatHex("c"))

	restored := NewOCICache(cacheDir)
	if err := restored.LoadIndex(); err != nil {
		t.Fatalf("LoadIndex: %v", err)
	}

	if _, ok := restored.DigestForRepositoryReference("ghcr.io/test/present", "v1"); !ok {
		t.Error("present entry dropped from index")
	}

	if _, ok := restored.DigestForRepositoryReference("ghcr.io/test/gone", "v1"); ok {
		t.Error("entry without content was kept in the index")
	}
}

func TestOCICacheLoadIndexMissingFile(t *testing.T) {
	if err := NewOCICache(t.TempDir()).LoadIndex(); err != nil {
		t.Fatalf("LoadIndex on empty cache: %v", err)
	}
}

func TestOCICacheDigestForRepositoryReference(t *testing.T) {
	cacheDir := t.TempDir()

	digest := "sha256:" + repeatHex("d")
	cache := NewOCICache(cacheDir)
	cache.SetDigestForArchitecture("ghcr.io/test/image:v1", "arm64", digest)
	cache.SetDigestForArchitecture("ghcr.io/test/pinned@"+digest, "arm64", digest)

	tests := []struct {
		name       string
		repository string
		reference  string
		want       string
	}{
		{name: "tag", repository: "ghcr.io/test/image", reference: "v1", want: digest},
		{name: "digest", repository: "ghcr.io/test/pinned", reference: digest, want: digest},
		{name: "unknown tag", repository: "ghcr.io/test/image", reference: "v2"},
		{name: "unknown repository", repository: "ghcr.io/other/image", reference: "v1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := cache.DigestForRepositoryReference(tt.repository, tt.reference)
			if tt.want == "" {
				if ok {
					t.Fatalf("unexpected digest %q", got)
				}

				return
			}

			if !ok || got != tt.want {
				t.Fatalf("got %q (ok=%t), want %q", got, ok, tt.want)
			}
		})
	}
}

func TestAgentImageRequests(t *testing.T) {
	r := &OCIReconciler{}

	defaults := r.agentImageRequests(&v1alpha3.Machine{}, "amd64")
	if len(defaults) != 6 {
		t.Fatalf("default agent images: got %d, want 6", len(defaults))
	}

	for _, req := range defaults {
		if req.Unpack {
			t.Errorf("agent image %q must stay packed", req.ImageRef)
		}
	}

	explicit := r.agentImageRequests(&v1alpha3.Machine{
		Spec: v1alpha3.MachineSpec{Agent: &v1alpha3.AgentSpec{Image: "ghcr.io/test/rootfs:v1"}},
	}, "arm64")

	if len(explicit) != 1 || explicit[0].ImageRef != "ghcr.io/test/rootfs:v1" || explicit[0].Architecture != "arm64" {
		t.Fatalf("explicit agent image: got %#v", explicit)
	}
}

func TestDaemonSetSchedulesOn(t *testing.T) {
	daemonSet := func(selector map[string]string) *appsv1.DaemonSet {
		return &appsv1.DaemonSet{Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{NodeSelector: selector}},
		}}
	}

	if !daemonSetSchedulesOn(daemonSet(nil), map[string]string{"kubernetes.io/os": "linux"}) {
		t.Error("empty selector must match")
	}

	if !daemonSetSchedulesOn(daemonSet(map[string]string{"kubernetes.io/os": "linux"}), map[string]string{"kubernetes.io/os": "linux"}) {
		t.Error("matching selector must match")
	}

	if daemonSetSchedulesOn(daemonSet(map[string]string{"unbounded.cloud.io/gpu": "true"}), map[string]string{"kubernetes.io/os": "linux"}) {
		t.Error("unsatisfied selector must not match")
	}
}

func writeLayout(t *testing.T, cacheDir, digest string) {
	t.Helper()

	dir := NewOCICache(cacheDir).LayoutDir(digest)
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		t.Fatalf("create layout: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatalf("write oci-layout: %v", err)
	}
}

func writeDisk(t *testing.T, cacheDir, digest, architecture string) {
	t.Helper()

	dir := NewOCICache(cacheDir).DiskDirForArchitecture(digest, architecture)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create disk dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "disk.img.gz"), []byte("image"), 0o644); err != nil {
		t.Fatalf("write disk image: %v", err)
	}
}

func repeatHex(char string) string {
	chars := make([]byte, 64)
	for i := range chars {
		chars[i] = char[0]
	}

	return string(chars)
}
