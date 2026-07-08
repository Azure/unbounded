// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netbootinit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (i *Installer) selectTargetDisk(ctx context.Context, configured string) (string, error) {
	if configured == "" {
		i.Logger.Printf("waiting for block devices")
		i.logDisks()

		var disk string

		if err := retry(ctx, 30, time.Second, "find block device", i.Sleep, i.Logger, func() error {
			selected, ok := i.findLargestDisk()
			if !ok {
				return errors.New("no target disk found")
			}

			disk = selected

			return nil
		}); err != nil {
			return "", errors.New("no target disk found")
		}

		i.Logger.Printf("WARNING: target disk was not specified, selected largest disk")

		configured = disk
	} else if !pathExists(configured) {
		return "", fmt.Errorf("target disk %s does not exist", configured)
	}

	resolved, err := filepath.EvalSymlinks(configured)
	if err == nil {
		configured = resolved
	}

	if _, err := i.targetDiskSysfs(configured); err != nil {
		return "", err
	}

	return configured, nil
}

func (i *Installer) logDisks() {
	i.Logger.Printf("candidate disks:")

	for _, disk := range i.candidateDisks() {
		size := strings.TrimSpace(readFileString(filepath.Join(disk.SysfsPath, "size")))
		model := strings.TrimSpace(readFileString(filepath.Join(disk.SysfsPath, "device/model")))
		serial := strings.TrimSpace(readFileString(filepath.Join(disk.SysfsPath, "device/serial")))
		removable := strings.TrimSpace(readFileString(filepath.Join(disk.SysfsPath, "removable")))
		i.Logger.Printf("  /dev/%s sectors=%s model=%s serial=%s removable=%s", disk.Name, defaultString(size, "0"), defaultString(model, "unknown"), defaultString(serial, "unknown"), defaultString(removable, "unknown"))
	}
}

type diskCandidate struct {
	Name      string
	SysfsPath string
	Sectors   uint64
}

func (i *Installer) candidateDisks() []diskCandidate {
	patterns := []string{
		filepath.Join(i.SysfsRoot, "block/sd*"),
		filepath.Join(i.SysfsRoot, "block/nvme*n*"),
		filepath.Join(i.SysfsRoot, "block/vd*"),
	}

	var disks []diskCandidate

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}

		for _, match := range matches {
			if !isWholeDiskSysfs(match) {
				continue
			}

			sectors, err := strconv.ParseUint(strings.TrimSpace(readFileString(filepath.Join(match, "size"))), 10, 64)
			if err != nil {
				sectors = 0
			}

			disks = append(disks, diskCandidate{Name: filepath.Base(match), SysfsPath: match, Sectors: sectors})
		}
	}

	return disks
}

func isWholeDiskSysfs(path string) bool {
	return !pathExists(filepath.Join(path, "partition"))
}

func (i *Installer) findLargestDisk() (string, bool) {
	var selected diskCandidate
	for _, disk := range i.candidateDisks() {
		if disk.Sectors > selected.Sectors {
			selected = disk
		}
	}

	if selected.Name == "" {
		return "", false
	}

	return "/dev/" + selected.Name, true
}

func (i *Installer) targetDiskSysfs(targetDisk string) (string, error) {
	base := filepath.Base(targetDisk)

	sysdisk := filepath.Join(i.SysfsRoot, "class/block", base)
	if resolved, err := filepath.EvalSymlinks(sysdisk); err == nil {
		sysdisk = resolved
	} else {
		sysdisk = filepath.Join(i.SysfsRoot, "block", base)
	}

	if !pathExists(sysdisk) {
		return "", fmt.Errorf("target disk %s has no sysfs device", targetDisk)
	}

	if pathExists(filepath.Join(sysdisk, "partition")) {
		return "", fmt.Errorf("target disk %s is a partition, expected whole disk", targetDisk)
	}

	return sysdisk, nil
}

type partition struct {
	Device string
	Number string
}

func (i *Installer) partitionsForDisk(targetDisk string) []partition {
	sysdisk, err := i.targetDiskSysfs(targetDisk)
	if err != nil {
		return nil
	}

	entries, err := os.ReadDir(sysdisk)
	if err != nil {
		return nil
	}

	parts := make([]partition, 0, len(entries))
	for _, entry := range entries {
		number := strings.TrimSpace(readFileString(filepath.Join(sysdisk, entry.Name(), "partition")))
		if number != "" {
			parts = append(parts, partition{Device: "/dev/" + entry.Name(), Number: number})
		}
	}

	return parts
}
