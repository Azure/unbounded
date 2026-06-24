// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
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

func TestDiscoverBlockDevicePaths(t *testing.T) {
	t.Parallel()

	sysDir := t.TempDir()
	devDir := t.TempDir()

	// Entries that appear in /sys/class/block. Whole disks, partitions,
	// device-mapper, and software RAID are real storage; loop/ram/zram/fd/sr
	// are virtual and must be excluded.
	sysEntries := []string{
		"sda", "sda1", "nvme0n1", "nvme0n1p1", "dm-0", "md0",
		"loop0", "ram0", "zram0", "sr0", "fd0",
	}
	for _, name := range sysEntries {
		require.NoError(t, os.Mkdir(filepath.Join(sysDir, name), 0o755))
	}

	// Only create device nodes for a subset to prove a sysfs entry without a
	// matching /dev node is dropped (here: md0 has no node).
	devNodes := []string{
		"sda", "sda1", "nvme0n1", "nvme0n1p1", "dm-0",
		"loop0", "ram0", "zram0", "sr0", "fd0",
	}
	for _, name := range devNodes {
		f, err := os.Create(filepath.Join(devDir, name))
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}

	got := discoverBlockDevicePaths(sysDir, devDir)

	want := []string{
		filepath.Join(devDir, "dm-0"),
		filepath.Join(devDir, "nvme0n1"),
		filepath.Join(devDir, "nvme0n1p1"),
		filepath.Join(devDir, "sda"),
		filepath.Join(devDir, "sda1"),
	}
	require.Equal(t, want, got)
}

func TestDiscoverBlockDevicePaths_SysfsBangTranslation(t *testing.T) {
	t.Parallel()

	sysDir := t.TempDir()
	devDir := t.TempDir()

	// sysfs encodes '/' as '!': cciss!c0d0 maps to /dev/cciss/c0d0.
	require.NoError(t, os.Mkdir(filepath.Join(sysDir, "cciss!c0d0"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(devDir, "cciss"), 0o755))

	f, err := os.Create(filepath.Join(devDir, "cciss", "c0d0"))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	got := discoverBlockDevicePaths(sysDir, devDir)
	require.Equal(t, []string{filepath.Join(devDir, "cciss", "c0d0")}, got)
}

func TestDiscoverBlockDevicePaths_MissingDir(t *testing.T) {
	t.Parallel()

	require.Nil(t, discoverBlockDevicePaths("/nonexistent/sys/class/block", "/dev"))
}

func TestDiscoverInfinibandDevicePaths(t *testing.T) {
	t.Parallel()

	ibDir := t.TempDir()

	for _, name := range []string{"uverbs0", "umad0", "rdma_cm"} {
		f, err := os.Create(filepath.Join(ibDir, name))
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}

	// A subdirectory must be skipped.
	require.NoError(t, os.Mkdir(filepath.Join(ibDir, "subdir"), 0o755))

	got := discoverInfinibandDevicePaths(ibDir)

	want := []string{
		filepath.Join(ibDir, "rdma_cm"),
		filepath.Join(ibDir, "umad0"),
		filepath.Join(ibDir, "uverbs0"),
	}
	require.Equal(t, want, got)
}

func TestDiscoverInfinibandDevicePaths_MissingDir(t *testing.T) {
	t.Parallel()

	require.Nil(t, discoverInfinibandDevicePaths("/nonexistent/dev/infiniband"))
}

func TestHostDevices_Paths_MergesDedupesSorts(t *testing.T) {
	t.Parallel()

	d := HostDevices{
		KVM:        []string{"/dev/kvm"},
		Block:      []string{"/dev/sdb", "/dev/sda", "/dev/kvm"},
		Infiniband: []string{"/dev/infiniband/uverbs0"},
	}

	want := []string{
		"/dev/infiniband/uverbs0",
		"/dev/kvm",
		"/dev/sda",
		"/dev/sdb",
	}
	require.Equal(t, want, d.Paths())
}

func TestHostDevices_Paths_Empty(t *testing.T) {
	t.Parallel()

	require.Nil(t, HostDevices{}.Paths())
}
