// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverKVMDevice_Present(t *testing.T) {
	// Create a temporary file to simulate the KVM device.
	dir := t.TempDir()
	fakeKVM := filepath.Join(dir, "kvm")

	f, err := os.Create(fakeKVM)
	if err != nil {
		t.Fatalf("create fake kvm device: %v", err)
	}

	f.Close()

	// Temporarily replace the package-level constant by patching via the
	// function under test. Since kvmDevicePath is a constant we test the
	// function directly against a real path instead.
	//
	// discoverKVMDevicePath is the testable variant that accepts a path
	// argument; DiscoverKVMDevice() calls it with the real /dev/kvm path.
	got := discoverKVMDevicePath(fakeKVM)
	if got != fakeKVM {
		t.Errorf("discoverKVMDevicePath(%q) = %q, want %q", fakeKVM, got, fakeKVM)
	}
}

func TestDiscoverKVMDevice_Absent(t *testing.T) {
	got := discoverKVMDevicePath("/nonexistent/path/to/kvm")
	if got != "" {
		t.Errorf("discoverKVMDevicePath(absent) = %q, want empty string", got)
	}
}
