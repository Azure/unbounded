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

func TestDiscoverExistingDevicePaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tun := filepath.Join(dir, "net", "tun")
	vhostNet := filepath.Join(dir, "vhost-net")
	missing := filepath.Join(dir, "missing")

	require.NoError(t, os.Mkdir(filepath.Dir(tun), 0o755))

	for _, path := range []string{tun, vhostNet} {
		f, err := os.Create(path)
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}

	require.Equal(t, []string{tun, vhostNet}, discoverExistingDevicePaths(tun, missing, vhostNet))
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

	got := discoverInfinibandDevicePaths(ibDir, filepath.Join(t.TempDir(), "missing"), false)

	want := []string{
		filepath.Join(ibDir, "rdma_cm"),
		filepath.Join(ibDir, "umad0"),
		filepath.Join(ibDir, "uverbs0"),
	}
	require.Equal(t, want, got)
}

func TestDiscoverInfinibandDevicePaths_MissingDir(t *testing.T) {
	t.Parallel()

	require.Nil(t, discoverInfinibandDevicePaths("/nonexistent/dev/infiniband", filepath.Join(t.TempDir(), "missing"), false))
}

func TestRDMACMDeviceNumber(t *testing.T) {
	t.Parallel()

	devFile := filepath.Join(t.TempDir(), "dev")
	require.NoError(t, os.WriteFile(devFile, []byte("10:263\n"), 0o644))

	major, minor, ok := rdmaCMDeviceNumber(devFile)
	require.True(t, ok)
	require.Equal(t, uint32(10), major)
	require.Equal(t, uint32(263), minor)
}

func TestRDMACMDeviceNumberInvalid(t *testing.T) {
	t.Parallel()

	devFile := filepath.Join(t.TempDir(), "dev")
	require.NoError(t, os.WriteFile(devFile, []byte("not-a-device\n"), 0o644))

	_, _, ok := rdmaCMDeviceNumber(devFile)
	require.False(t, ok)
}

func TestRDMACMDeviceNumberRejectsOverflow(t *testing.T) {
	t.Parallel()

	devFile := filepath.Join(t.TempDir(), "dev")
	require.NoError(t, os.WriteFile(devFile, []byte("4294967296:0\n"), 0o644))

	_, _, ok := rdmaCMDeviceNumber(devFile)
	require.False(t, ok)
}

func TestCreateRDMACMDeviceNode(t *testing.T) {
	t.Parallel()

	ibDir := filepath.Join(t.TempDir(), "infiniband")

	got := createRDMACMDeviceNode(ibDir, 10, 263, func(path string, mode uint32, dev int) error {
		require.Equal(t, filepath.Join(ibDir, "rdma_cm"), path)
		require.NotZero(t, mode&0o666)
		require.NotZero(t, dev)

		f, err := os.Create(path)
		require.NoError(t, err)
		require.NoError(t, f.Close())

		return nil
	})

	require.Equal(t, filepath.Join(ibDir, "rdma_cm"), got)
}

func TestEnsureRDMACMDeviceExisting(t *testing.T) {
	t.Parallel()

	ibDir := t.TempDir()
	path := filepath.Join(ibDir, "rdma_cm")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	require.Equal(t, path, ensureRDMACMDevice(ibDir, filepath.Join(t.TempDir(), "missing")))
}

func TestHostDevices_Paths_MergesDedupesSorts(t *testing.T) {
	t.Parallel()

	d := HostDevices{
		KVM:        []string{"/dev/kvm"},
		Network:    []string{"/dev/net/tun", "/dev/vhost-net"},
		Block:      []string{"/dev/sdb", "/dev/sda", "/dev/kvm"},
		Infiniband: []string{"/dev/infiniband/uverbs0"},
		Additional: []string{"/dev/uinput", "/dev/net/tun"},
	}

	want := []string{
		"/dev/infiniband/uverbs0",
		"/dev/kvm",
		"/dev/net/tun",
		"/dev/sda",
		"/dev/sdb",
		"/dev/uinput",
		"/dev/vhost-net",
	}
	require.Equal(t, want, d.Paths())
}

func TestDiscoverHostDevices_Additional(t *testing.T) {
	t.Parallel()

	additional := []string{"/dev/uinput"}

	got := DiscoverHostDevices(additional)

	require.Equal(t, additional, got.Additional)
	require.Contains(t, got.Paths(), "/dev/uinput")
}

func TestHostDevices_Paths_Empty(t *testing.T) {
	t.Parallel()

	require.Nil(t, HostDevices{}.Paths())
}
