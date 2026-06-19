// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverKVMDevicePath_Present(t *testing.T) {
	// Create a temporary file to simulate the KVM device.
	dir := t.TempDir()
	fakeKVM := filepath.Join(dir, "kvm")

	f, err := os.Create(fakeKVM)
	if err != nil {
		t.Fatalf("create fake kvm device: %v", err)
	}

	f.Close()

	got := discoverKVMDevicePath(fakeKVM)
	if got != fakeKVM {
		t.Errorf("discoverKVMDevicePath(%q) = %q, want %q", fakeKVM, got, fakeKVM)
	}
}

func TestDiscoverKVMDevicePath_Absent(t *testing.T) {
	got := discoverKVMDevicePath("/nonexistent/path/to/kvm")
	if got != "" {
		t.Errorf("discoverKVMDevicePath(absent) = %q, want empty string", got)
	}
}

func TestDiscoverHostDevicePaths_DisabledReturnsNil(t *testing.T) {
	// When passthrough is disabled the host must not be probed and no
	// devices may be exposed, regardless of whether /dev/kvm exists on the
	// machine running the test.
	if got := DiscoverHostDevicePaths(false); got != nil {
		t.Errorf("DiscoverHostDevicePaths(false) = %v, want nil", got)
	}
}

func TestDiscoverHostDevicePaths_EnabledMatchesProbe(t *testing.T) {
	// When enabled the result must equal the underlying probe for /dev/kvm:
	// either ["/dev/kvm"] on a KVM-capable host or nil otherwise. This keeps
	// the test host-independent while still exercising the enabled path.
	got := DiscoverHostDevicePaths(true)

	var want []string
	if p := discoverKVMDevicePath(kvmDevicePath); p != "" {
		want = append(want, p)
	}

	if len(got) != len(want) {
		t.Fatalf("DiscoverHostDevicePaths(true) = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("DiscoverHostDevicePaths(true)[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
