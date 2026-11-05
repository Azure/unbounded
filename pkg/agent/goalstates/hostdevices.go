// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/pkg/agent/config"
)

const (
	kvmDevicePath      = "/dev/kvm"
	tunDevicePath      = "/dev/net/tun"
	vhostNetDevicePath = "/dev/vhost-net"
	// sysClassBlockDir is the canonical kernel listing of block devices.
	// It contains both whole disks (e.g. sda, nvme0n1) and their partitions
	// (e.g. sda1, nvme0n1p1), which is why it is preferred over /sys/block.
	sysClassBlockDir = "/sys/class/block"
	// infinibandDir holds InfiniBand/RDMA HCA character devices
	// (e.g. uverbs0, umad0, issm0, rdma_cm). It is bind-mounted as a directory
	// so devices created later are visible in the machine.
	infinibandDir = "/dev/infiniband"
	// rdmaCMMiscDevPath is the kernel-published major:minor for the RDMA-CM
	// misc device. systemd-nspawn needs the corresponding /dev/infiniband/rdma_cm
	// node bind-mounted for workloads using RDMA-CM.
	rdmaCMMiscDevPath = "/sys/class/misc/rdma_cm/dev"
)

// infinibandDeviceAllow grants cgroup access to every RDMA device class in
// /dev/infiniband by major: uverbs (infiniband_verbs), umad and issm
// (infiniband_mad), and ucm (infiniband_cm). rdma_cm is a misc device whose
// major is shared with unrelated devices, so it is allowed by path only.
var infinibandDeviceAllow = []string{
	"char-infiniband_verbs",
	"char-infiniband_mad",
	"char-infiniband_cm",
	infinibandDir + "/rdma_cm",
}

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
	// Network holds host networking device paths used by virtualization
	// workloads (for example /dev/net/tun and /dev/vhost-net) when present.
	Network []string
	// Block holds storage block device node paths derived from
	// /sys/class/block (whole disks and partitions, excluding virtual
	// devices such as loop/ram/zram).
	Block []string
	// DeviceDirectories holds whole-directory device mounts (for example
	// /dev/infiniband) that are bind-mounted as directories, with cgroup
	// device permissions covering nodes created after the machine starts.
	DeviceDirectories []config.AdditionalHostMount
	// Additional holds extra host device node paths and systemd device group
	// specifiers requested by config.
	Additional []string
}

// Paths returns every discovered device node path merged into a single
// de-duplicated, sorted slice. This is the form the nspawn templates consume
// to emit Bind= directives.
func (d HostDevices) Paths() []string {
	seen := make(map[string]bool)

	var paths []string

	for _, group := range [][]string{d.KVM, d.Network, d.Block, d.Additional} {
		for _, p := range group {
			if config.IsSystemdDeviceGroupSpecifier(p) {
				continue
			}

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

// DeviceGroupSpecifiers returns additional systemd DeviceAllow device group
// specifiers in a de-duplicated, sorted slice.
func (d HostDevices) DeviceGroupSpecifiers() []string {
	seen := make(map[string]bool)

	var specifiers []string

	for _, device := range d.Additional {
		if !config.IsSystemdDeviceGroupSpecifier(device) || seen[device] {
			continue
		}

		seen[device] = true
		specifiers = append(specifiers, device)
	}

	sort.Strings(specifiers)

	return specifiers
}

// DiscoverHostDevices probes the host for device nodes that should be
// bind-mounted into the nspawn container, grouped by category. Repeated calls
// produce the same output because each group is returned in a stable order.
func DiscoverHostDevices(additional []string) HostDevices {
	var devices HostDevices

	if p := discoverKVMDevicePath(kvmDevicePath); p != "" {
		devices.KVM = append(devices.KVM, p)
	}

	devices.Network = discoverExistingDevicePaths(tunDevicePath, vhostNetDevicePath)
	devices.Block = discoverBlockDevicePaths(sysClassBlockDir, devDir)
	devices.DeviceDirectories = discoverInfinibandDeviceDirectories(infinibandDir, rdmaCMMiscDevPath, true)
	devices.Additional = additional

	return devices
}

// discoverKVMDevicePath checks whether path exists on the filesystem and
// returns it when accessible, or an empty string on any error.
func discoverKVMDevicePath(path string) string {
	return discoverExistingDevicePath(path)
}

// discoverExistingDevicePaths checks each path and returns the accessible ones
// in the same stable order.
func discoverExistingDevicePaths(paths ...string) []string {
	var discovered []string

	for _, path := range paths {
		if p := discoverExistingDevicePath(path); p != "" {
			discovered = append(discovered, p)
		}
	}

	return discovered
}

// discoverExistingDevicePath checks whether path exists on the filesystem and
// returns it when accessible, or an empty string on any error.
func discoverExistingDevicePath(path string) string {
	if _, err := os.Stat(path); err != nil {
		// Treat any error (including permission denied) as absent; the
		// device is not accessible to the agent, so don't expose it to the
		// container. os.ErrNotExist is the common case when the host lacks
		// the corresponding hardware or kernel module.
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

// discoverInfinibandDeviceDirectories returns a mount of infinibandDir when
// the directory exists after RDMA-CM setup, including when it is empty, so
// devices created later appear in the machine. When the directory is absent
// (no RDMA hardware or drivers) nothing is mounted, because nspawn fails to
// start when a bind source is missing.
func discoverInfinibandDeviceDirectories(infinibandDir, rdmaCMMiscDevPath string, loadRDMACM bool) []config.AdditionalHostMount {
	if loadRDMACM {
		ensureRDMACMDevice(infinibandDir, rdmaCMMiscDevPath)
	}

	info, err := os.Stat(infinibandDir)
	if err != nil || !info.IsDir() {
		return nil
	}

	return []config.AdditionalHostMount{{
		Source:      infinibandDir,
		Target:      infinibandDir,
		DeviceAllow: slices.Clone(infinibandDeviceAllow),
	}}
}

func ensureRDMACMDevice(infinibandDir, miscDevPath string) string {
	nodePath := filepath.Join(infinibandDir, "rdma_cm")
	if _, err := os.Stat(nodePath); err == nil {
		return nodePath
	}

	major, minor, ok := rdmaCMDeviceNumber(miscDevPath)
	if !ok {
		loadRDMACMModules()

		major, minor, ok = rdmaCMDeviceNumber(miscDevPath)
		if !ok {
			return ""
		}
	}

	return createRDMACMDeviceNode(infinibandDir, major, minor, unix.Mknod)
}

func loadRDMACMModules() {
	modprobe, err := exec.LookPath("modprobe")
	if err != nil {
		return
	}

	if err := exec.Command(modprobe, "rdma_ucm").Run(); err == nil {
		return
	}

	if err := exec.Command(modprobe, "rdma_cm").Run(); err != nil {
		return
	}
}

func rdmaCMDeviceNumber(path string) (uint32, uint32, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}

	parts := strings.Split(strings.TrimSpace(string(data)), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}

	major, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return 0, 0, false
	}

	minor, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return 0, 0, false
	}

	return uint32(major), uint32(minor), true
}

type mknodFunc func(path string, mode uint32, dev int) error

func createRDMACMDeviceNode(infinibandDir string, major, minor uint32, mknod mknodFunc) string {
	if err := os.MkdirAll(infinibandDir, 0o755); err != nil {
		return ""
	}

	nodePath := filepath.Join(infinibandDir, "rdma_cm")

	dev := int(unix.Mkdev(major, minor))
	if err := mknod(nodePath, unix.S_IFCHR|0o666, dev); err != nil && !os.IsExist(err) {
		return ""
	}

	if _, err := os.Stat(nodePath); err != nil {
		return ""
	}

	return nodePath
}
