// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func (i *Installer) createUEFIBootEntry(ctx context.Context, targetDisk string) error {
	if !pathExists(filepath.Join(i.SysfsRoot, "firmware/efi")) {
		return nil
	}

	i.Logger.Printf("creating UEFI boot entry for local disk")

	efivars := filepath.Join(i.SysfsRoot, "firmware/efi/efivars")
	if err := os.MkdirAll(efivars, 0o755); err != nil {
		i.Logger.Printf("WARNING: creating efivars mount point failed: %v", err)
	}

	if err := i.System.Mount("efivarfs", efivars, "efivarfs"); err != nil && !errors.Is(err, unix.EBUSY) {
		i.Logger.Printf("WARNING: efivarfs mount failed: %v", err)
	}

	for _, part := range i.partitionsForDisk(targetDisk) {
		var loader string
		if err := i.withMounted(part.Device, i.ESPMountPoint, []string{"vfat"}, func() error {
			loader = findEFILoader(i.ESPMountPoint)
			return nil
		}); err != nil {
			continue
		}

		if loader == "" {
			continue
		}

		if _, err := i.Runner.LookPath("efibootmgr"); err != nil {
			continue
		}

		if part.Number == "" {
			continue
		}

		if err := i.Runner.Run(ctx, "efibootmgr", "--create", "--disk", targetDisk, "--part", part.Number, "--loader", loader, "--label", "unbounded"); err != nil {
			i.Logger.Printf("WARNING: efibootmgr failed, PXE chainloader will be used as fallback")
		} else {
			i.Logger.Printf("UEFI boot entry created (%s on part %s)", loader, part.Number)
		}

		break
	}

	return nil
}

func findEFILoader(mountPoint string) string {
	candidates := []string{
		"/EFI/BOOT/BOOTX64.EFI",
		"/EFI/BOOT/BOOTAA64.EFI",
		"/EFI/ubuntu/shimx64.efi",
		"/EFI/ubuntu/shimaa64.efi",
	}

	for _, candidate := range candidates {
		if pathExists(filepath.Join(mountPoint, candidate)) {
			return strings.ReplaceAll(candidate, "/", "\\")
		}
	}

	return ""
}
