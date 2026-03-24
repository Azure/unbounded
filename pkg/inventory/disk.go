package inventory

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DiskType represents the type of storage device.
type DiskType string

const (
	DiskTypeNVMe    DiskType = "nvme"
	DiskTypeSSD     DiskType = "ssd"
	DiskTypeHDD     DiskType = "hdd"
	DiskTypeUnknown DiskType = "unknown"
)

// DiskAttributes contains disk-specific fields stored in DeviceRecord.Attributes.
type DiskAttributes struct {
	Type          string `json:"type"`
	SizeBytes     uint64 `json:"size_bytes"`
	SerialNumber  string `json:"serial_number,omitempty"`
	Firmware      string `json:"firmware,omitempty"`
	BlockSize     string `json:"block_size,omitempty"`
	Speed         string `json:"speed,omitempty"`
	Driver        string `json:"driver,omitempty"`
	DriverVersion string `json:"driver_version,omitempty"`
}

// DiskToRecord converts a DiskInfo into a DeviceRecord.
func DiskToRecord(d *DiskInfo, hostID string) DeviceRecord {
	serial := d.SerialNumber
	if serial == "" {
		serial = "disk-" + d.Name
	}
	return DeviceRecord{
		DeviceType:     DeviceTypeDisk,
		DeviceName:     d.Name,
		HostIdentifier: hostID,
		SerialNumber:   serial,
		Attributes: mustMarshal(DiskAttributes{
			Type:          string(d.Type),
			SizeBytes:     d.SizeBytes,
			SerialNumber:  d.SerialNumber,
			Firmware:      d.Firmware,
			BlockSize:     d.BlockSize,
			Speed:         d.Speed,
			Driver:        d.Driver,
			DriverVersion: d.DriverVersion,
		}),
	}
}

// DiskInfo holds information about a single storage device.
type DiskInfo struct {
	Name          string
	Type          DiskType
	SizeBytes     uint64
	SerialNumber  string
	Firmware      string
	BlockSize     string
	Speed         string
	Driver        string
	DriverVersion string
}

// collectDiskInfo discovers block devices and returns info for each disk.
func collectDiskInfo() ([]DiskInfo, error) {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil, fmt.Errorf("failed to read /sys/block: %w", err)
	}

	var disks []DiskInfo
	for _, e := range entries {
		name := e.Name()

		// Skip virtual/loop/ram devices.
		if strings.HasPrefix(name, "loop") ||
			strings.HasPrefix(name, "ram") ||
			strings.HasPrefix(name, "dm-") ||
			strings.HasPrefix(name, "zram") {
			continue
		}

		disk := DiskInfo{Name: name}
		base := filepath.Join("/sys/block", name)

		disk.SizeBytes = readBlockDeviceSize(base)
		disk.Type = detectDiskType(name, base)
		disk.BlockSize = readSysfsField(filepath.Join(base, "queue", "physical_block_size"))
		disk.Speed = readLinkSpeed(name, base)
		disk.SerialNumber = readDiskSerial(name)
		disk.Firmware = readDiskFirmware(name)
		disk.Driver, disk.DriverVersion = readDiskDriver(name, base)

		disks = append(disks, disk)
	}

	return disks, nil
}

// readBlockDeviceSize reads the device size in bytes from sysfs.
func readBlockDeviceSize(base string) uint64 {
	sizeStr := readSysfsField(filepath.Join(base, "size"))
	if sizeStr == "" {
		return 0
	}
	var sectors uint64
	if _, err := fmt.Sscanf(sizeStr, "%d", &sectors); err != nil {
		return 0
	}
	// Sectors are always 512 bytes in sysfs.
	return sectors * 512
}

// detectDiskType determines whether a device is NVMe, SSD, or HDD.
func detectDiskType(name, base string) DiskType {
	// Check by device name prefix.
	if strings.HasPrefix(name, "nvme") {
		return DiskTypeNVMe
	}

	// Check if the device path or subsystem indicates NVMe
	// (handles NVMe-oF or compatibility layers that expose as sd*).
	devicePath, err := filepath.EvalSymlinks(filepath.Join(base, "device"))
	if err == nil && strings.Contains(devicePath, "/nvme") {
		return DiskTypeNVMe
	}
	subsystem, err := filepath.EvalSymlinks(filepath.Join(base, "device", "subsystem"))
	if err == nil && strings.HasSuffix(subsystem, "/nvme") {
		return DiskTypeNVMe
	}

	rotational := readSysfsField(filepath.Join(base, "queue", "rotational"))

	// rotational=0 is reliably SSD.
	if strings.TrimSpace(rotational) == "0" {
		return DiskTypeSSD
	}

	// rotational=1 is unreliable — many SSDs and virtual disks report 1.
	// Cross-check with TRIM/discard support: if the device supports discard
	// it is almost certainly flash-based (SSD), not a spinning disk.
	if strings.TrimSpace(rotational) == "1" {
		discardGran := readSysfsField(filepath.Join(base, "queue", "discard_granularity"))
		if discardGran != "" && discardGran != "0" {
			return DiskTypeSSD
		}
		return DiskTypeHDD
	}

	return DiskTypeUnknown
}

// readLinkSpeed reads the negotiated link speed for the device.
func readLinkSpeed(name, base string) string {
	// NVMe: check the PCIe link speed.
	if strings.HasPrefix(name, "nvme") {
		deviceLink, err := os.Readlink(filepath.Join(base, "device"))
		if err == nil {
			pciDir := filepath.Join(base, "device", deviceLink)
			speed := readSysfsField(filepath.Join(pciDir, "current_link_speed"))
			if speed != "" {
				return speed
			}
		}
		// Try the parent PCIe device.
		speed := readSysfsField(filepath.Join(base, "device", "device", "current_link_speed"))
		if speed != "" {
			return speed
		}
	}

	// SATA/SAS: try to find negotiated speed from the host adapter.
	linkPath := filepath.Join(base, "device", "ata_link")
	if entries, err := os.ReadDir(linkPath); err == nil {
		for _, e := range entries {
			speed := readSysfsField(filepath.Join(linkPath, e.Name(), "sata_spd"))
			if speed != "" && speed != "<unknown>" {
				return speed
			}
		}
	}

	return ""
}

// readDiskSerial reads the disk serial number from sysfs or udevadm.
func readDiskSerial(name string) string {
	// Try direct sysfs path first (works for many SCSI/SATA disks).
	serial := readSysfsField(filepath.Join("/sys/block", name, "device", "serial"))
	if serial != "" {
		return serial
	}

	// NVMe exposes serial under the nvme controller.
	if strings.HasPrefix(name, "nvme") {
		// e.g., nvme0n1 → controller is nvme0.
		ctrl := strings.TrimRight(name, "0123456789")
		if strings.HasSuffix(ctrl, "n") {
			ctrl = strings.TrimSuffix(ctrl, "n")
		}
		serial = readSysfsField(filepath.Join("/sys/class/nvme", ctrl, "serial"))
		if serial != "" {
			return serial
		}
	}

	return ""
}

// readDiskFirmware reads the firmware revision from sysfs.
func readDiskFirmware(name string) string {
	// SCSI/SATA devices.
	fw := readSysfsField(filepath.Join("/sys/block", name, "device", "firmware_rev"))
	if fw != "" {
		return fw
	}
	fw = readSysfsField(filepath.Join("/sys/block", name, "device", "rev"))
	if fw != "" {
		return fw
	}

	// NVMe: firmware is under the controller.
	if strings.HasPrefix(name, "nvme") {
		ctrl := strings.TrimRight(name, "0123456789")
		if strings.HasSuffix(ctrl, "n") {
			ctrl = strings.TrimSuffix(ctrl, "n")
		}
		fw = readSysfsField(filepath.Join("/sys/class/nvme", ctrl, "firmware_rev"))
		if fw != "" {
			return fw
		}
	}

	return ""
}

// readSysfsField reads and trims a single sysfs file. Returns "" on any error.
func readSysfsField(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readDiskDriver determines the driver and its version for a block device.
func readDiskDriver(name, base string) (string, string) {
	var driverName string

	// Direct device driver symlink (works for NVMe and some others).
	if link, err := os.Readlink(filepath.Join(base, "device", "driver")); err == nil {
		driverName = filepath.Base(link)
	}

	// For SCSI devices the direct driver is "sd" — find the HBA driver instead.
	if driverName == "sd" || driverName == "" {
		if hostDriver := readScsiHostDriver(base); hostDriver != "" {
			driverName = hostDriver
		}
	}

	if driverName == "" {
		return "", ""
	}

	version := readDriverVersion(driverName)
	return driverName, version
}

// readScsiHostDriver finds the HBA/transport driver for a SCSI device.
func readScsiHostDriver(base string) string {
	// Resolve the device path and extract the SCSI host name.
	devicePath, err := filepath.EvalSymlinks(filepath.Join(base, "device"))
	if err != nil {
		return ""
	}

	// Walk path components to find "hostN".
	parts := strings.Split(devicePath, "/")
	var hostName string
	for _, p := range parts {
		if strings.HasPrefix(p, "host") {
			hostName = p
			break
		}
	}
	if hostName == "" {
		return ""
	}

	// proc_name gives the driver name for this SCSI host.
	return readSysfsField(filepath.Join("/sys/class/scsi_host", hostName, "proc_name"))
}

// readDriverVersion gets the version of a kernel module via modinfo.
func readDriverVersion(driverName string) string {
	out, err := exec.Command("modinfo", "-F", "version", driverName).Output()
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(out))
	if v != "" {
		return v
	}

	// Some built-in modules don't have a version field; try vermagic.
	out, err = exec.Command("modinfo", "-F", "vermagic", driverName).Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) > 0 {
		return fields[0] + " (kernel)"
	}
	return ""
}

// printDiskInfo prints the collected disk information to stdout.
func printDiskInfo(disks []DiskInfo) {
	fmt.Printf("Disks Found:     %d\n", len(disks))
	for i, d := range disks {
		fmt.Printf("\n  Disk %d (%s):\n", i, d.Name)
		fmt.Printf("    Type:          %s\n", d.Type)
		fmt.Printf("    Size:          %s\n", formatBytes(d.SizeBytes))
		if d.SerialNumber != "" {
			fmt.Printf("    Serial Number: %s\n", d.SerialNumber)
		}
		if d.Firmware != "" {
			fmt.Printf("    Firmware:      %s\n", d.Firmware)
		}
		fmt.Printf("    Block Size:    %s\n", d.BlockSize)
		if d.Speed != "" {
			fmt.Printf("    Speed:         %s\n", d.Speed)
		}
		if d.Driver != "" {
			fmt.Printf("    Driver:        %s\n", d.Driver)
		}
		if d.DriverVersion != "" {
			fmt.Printf("    Driver Ver:    %s\n", d.DriverVersion)
		}
	}
}
