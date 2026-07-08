// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func (i *Installer) setupMounts() error {
	for _, dir := range []string{"/proc", "/sys", "/dev", "/tmp", "/run", i.MountRoot, i.ESPMountPoint} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	mounts := []struct {
		source string
		target string
		fstype string
	}{
		{source: "proc", target: "/proc", fstype: "proc"},
		{source: "sysfs", target: "/sys", fstype: "sysfs"},
		{source: "devtmpfs", target: "/dev", fstype: "devtmpfs"},
	}

	for _, m := range mounts {
		if err := i.System.Mount(m.source, m.target, m.fstype); err != nil && !errors.Is(err, unix.EBUSY) {
			// The Ubuntu netboot initrd may already have some pseudo filesystems
			// mounted before this overlay init runs. Keep startup tolerant.
			continue
		}
	}

	return nil
}

func (i *Installer) loadKernelModules(ctx context.Context) error {
	storageModules := []string{"virtio_pci", "virtio_blk", "ahci", "sd_mod", "nvme", "xfs", "ext4"}
	networkModules := []string{"virtio_net", "e1000", "e1000e", "igb", "ixgbe", "i40e", "ice", "mlx5_core", "mlx4_core", "bnxt_en", "tg3", "be2net", "ena"}
	bootModules := []string{"nls_cp437", "nls_ascii", "nls_utf8", "fat", "vfat", "efivarfs"}

	for _, mod := range append(append(storageModules, networkModules...), bootModules...) {
		runBestEffort(ctx, i.Runner, "modprobe", mod)
	}

	kver, err := i.System.KernelRelease()
	if err != nil {
		return nil
	}

	patterns := []string{
		filepath.Join("/lib/modules", kver, "kernel/drivers/net/ethernet/*/*.ko*"),
		filepath.Join("/lib/modules", kver, "kernel/drivers/net/ethernet/*/*/*.ko*"),
	}

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}

		for _, match := range matches {
			mod := moduleNameFromPath(match)
			if mod != "" {
				runBestEffort(ctx, i.Runner, "modprobe", mod)
			}
		}
	}

	return nil
}

func moduleNameFromPath(path string) string {
	base := filepath.Base(path)

	idx := strings.Index(base, ".ko")
	if idx < 0 {
		return ""
	}

	return base[:idx]
}
