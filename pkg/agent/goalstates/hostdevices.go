// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	kvmDevicePath = "/dev/kvm"
	// sysClassBlockDir is the canonical kernel listing of block devices.
	// It contains both whole disks (e.g. sda, nvme0n1) and their partitions
	// (e.g. sda1, nvme0n1p1), which is why it is preferred over /sys/block.
	sysClassBlockDir = "/sys/class/block"
	// infinibandDir holds InfiniBand/RDMA HCA character devices
	// (e.g. uverbs0, umad0, issm0, rdma_cm).
	infinibandDir = "/dev/infiniband"
)

// excludedBlockDevicePrefixes lists /sys/class/block entry name prefixes that
// are pseudo or virtual devices rather than real storage and so should not be
// bind-mounted into the container. Device-mapper (dm-*) and software RAID
// (md*) are intentionally NOT excluded: they back real storage.
var excludedBlockDevicePrefixes = []string{
	"loop", // loopback devices
	"ram",  // ramdisks
	"zram", // compressed ramdisks
	"fd",   // floppy devices
	"sr",   // SCSI optical (cdrom) devices
}

// HostDevices groups host device node paths by category so that callers can
// report per-type discovery results while still bind-mounting the union of all
// paths into the nspawn container.
type HostDevices struct {
	// KVM holds the KVM character device path (/dev/kvm) when present.
	KVM []string
	// Block holds storage block device node paths derived from
	// /sys/class/block (whole disks and partitions, excluding virtual
	// devices such as loop/ram/zram).
	Block []string
	// Infiniband holds InfiniBand/RDMA HCA character device node paths from
	// /dev/infiniband.
	Infiniband []string
}

// Paths returns every discovered device node path merged into a single
// de-duplicated, sorted slice. This is the form the nspawn templates consume
// to emit Bind= and DeviceAllow= directives.
func (d HostDevices) Paths() []string {
	seen := make(map[string]bool)

	var paths []string

	for _, group := range [][]string{d.KVM, d.Block, d.Infiniband} {
		for _, p := range group {
			if seen[p] {
				continue
			}

			seen[p] = true
			paths = append(paths, p)
		}
	}

	sort.Strings(paths)

	return paths
}

// DiscoverHostDevices probes the host for device nodes that should be
// bind-mounted into the nspawn container, grouped by category. Repeated calls
// produce the same output because each group is returned in a stable order.
func DiscoverHostDevices() HostDevices {
	var devices HostDevices

	if p := discoverKVMDevicePath(kvmDevicePath); p != "" {
		devices.KVM = append(devices.KVM, p)
	}

	devices.Block = discoverBlockDevicePaths(sysClassBlockDir, devDir)
	devices.Infiniband = discoverInfinibandDevicePaths(infinibandDir)

	return devices
}

// discoverKVMDevicePath checks whether path exists on the filesystem and
// returns it when accessible, or an empty string on any error.
func discoverKVMDevicePath(path string) string {
	if _, err := os.Stat(path); err != nil {
		// Treat any error (including permission denied) as absent; the
		// device is not accessible to the agent, so don't expose it to the
		// container. os.ErrNotExist is the common case on non-KVM hosts.
		return ""
	}

	return path
}

// discoverBlockDevicePaths enumerates storage block devices from
// sysClassBlockDir and returns the corresponding device node paths under
// devDir in sorted order. Whole disks and partitions are included; pseudo and
// virtual devices (loop/ram/zram/fd/sr) are excluded. A sysfs entry is only
// included when its device node actually exists under devDir. Returns nil (not
// an error) when the directory cannot be read.
func discoverBlockDevicePaths(sysClassBlockDir, devDir string) []string {
	entries, err := os.ReadDir(sysClassBlockDir)
	if err != nil {
		return nil
	}

	var paths []string

	for _, e := range entries {
		name := e.Name()
		if isExcludedBlockDevice(name) {
			continue
		}

		// sysfs encodes '/' in device names as '!' (e.g. cciss!c0d0 maps to
		// the device node /dev/cciss/c0d0).
		nodeName := strings.ReplaceAll(name, "!", "/")
		nodePath := filepath.Join(devDir, nodeName)

		if _, err := os.Stat(nodePath); err != nil {
			continue
		}

		paths = append(paths, nodePath)
	}

	sort.Strings(paths)

	return paths
}

// isExcludedBlockDevice reports whether a /sys/class/block entry name belongs
// to a pseudo or virtual device that should not be bind-mounted.
func isExcludedBlockDevice(name string) bool {
	for _, prefix := range excludedBlockDevicePrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}

	return false
}

// discoverInfinibandDevicePaths enumerates InfiniBand/RDMA HCA character
// devices under infinibandDir and returns their device node paths in sorted
// order. Returns nil (not an error) when the directory is absent, which is the
// common case on hosts without RDMA hardware.
func discoverInfinibandDevicePaths(infinibandDir string) []string {
	entries, err := os.ReadDir(infinibandDir)
	if err != nil {
		return nil
	}

	var paths []string

	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		paths = append(paths, filepath.Join(infinibandDir, e.Name()))
	}

	sort.Strings(paths)

	return paths
}
