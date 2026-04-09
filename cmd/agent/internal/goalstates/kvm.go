// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import "os"

const kvmDevicePath = "/dev/kvm"

// DiscoverKVMDevice checks whether the KVM character device is present on the
// host. When present, the device path is returned so it can be bind-mounted
// into the nspawn container, allowing workloads inside the container to use
// hardware virtualisation. Returns an empty string when the device is absent.
func DiscoverKVMDevice() string {
	return discoverKVMDevicePath(kvmDevicePath)
}

// discoverKVMDevicePath is the testable core of DiscoverKVMDevice. It checks
// whether path exists on the filesystem and returns it when found, or an empty
// string when not found or on any stat error.
func discoverKVMDevicePath(path string) string {
	if _, err := os.Stat(path); err != nil {
		// Treat any error (including permission denied) as absent; the
		// device is not accessible to the agent, so don't expose it to the
		// container. os.ErrNotExist is the common case on non-KVM hosts.
		return ""
	}

	return path
}
