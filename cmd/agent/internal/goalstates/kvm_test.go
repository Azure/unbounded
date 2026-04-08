package goalstates

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveKVMHost_Valid(t *testing.T) {
	// On any host, ResolveKVMHost should return a valid struct without
	// panicking. The exact DevicePaths depend on the test environment
	// (CI containers typically lack /dev/kvm).
	host := ResolveKVMHost()

	for _, path := range host.DevicePaths {
		if path == "" {
			t.Error("ResolveKVMHost() returned an empty device path")
		}
	}
}

func TestIsDeviceNode_CharDevice(t *testing.T) {
	// /dev/null is always a character device.
	if !isDeviceNode("/dev/null") {
		t.Error("isDeviceNode(/dev/null) = false, want true")
	}

	// /dev/zero is always a character device.
	if !isDeviceNode("/dev/zero") {
		t.Error("isDeviceNode(/dev/zero) = false, want true")
	}
}

func TestIsDeviceNode_RegularFile(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(tmpFile, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	if isDeviceNode(tmpFile) {
		t.Error("isDeviceNode(regular file) = true, want false")
	}
}

func TestIsDeviceNode_Directory(t *testing.T) {
	if isDeviceNode(t.TempDir()) {
		t.Error("isDeviceNode(directory) = true, want false")
	}
}

func TestIsDeviceNode_NonExistent(t *testing.T) {
	if isDeviceNode("/dev/nonexistent-device-xyzzy-12345") {
		t.Error("isDeviceNode(nonexistent) = true, want false")
	}
}
