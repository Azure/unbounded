// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package inventory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ChassisInfo holds information about the system chassis / motherboard.
type ChassisInfo struct {
	Manufacturer string
	Model        string
	SerialNumber string
	Hostname     string
	BIOSVersion  string
	IsVirtual    bool
}

// chassisToRecord converts a ChassisInfo into a DeviceRecord.
func chassisToRecord(c *ChassisInfo, hostID string) DeviceRecord {
	name := c.Model
	if name == "" {
		name = "System"
	}

	serial := c.SerialNumber
	if serial == "" {
		serial = "chassis-0"
	}

	return DeviceRecord{
		DeviceType:     DeviceTypeChassis,
		DeviceName:     name,
		HostIdentifier: hostID,
		SerialNumber:   serial,
		Attributes: mustMarshal(ChassisInfo{
			Manufacturer: c.Manufacturer,
			Model:        c.Model,
			Hostname:     c.Hostname,
			BIOSVersion:  c.BIOSVersion,
			IsVirtual:    c.IsVirtual,
		}),
	}
}

// collectChassisInfo reads system/motherboard identity from DMI sysfs entries
// and returns it as a slice of DeviceRecord plus the derived host identifier.
// When DMI sysfs is unavailable it still returns a record populated with
// whatever fallback identifier can be found (product_uuid or machine-id).
func collectChassisInfo(_ context.Context, debug bool) ([]DeviceRecord, string, error) {
	dmiBase := "/sys/class/dmi/id"

	info := &ChassisInfo{}

	var collectErr error

	if _, err := os.Stat(dmiBase); err != nil {
		collectErr = fmt.Errorf("DMI sysfs not available: %w", err)
	} else {
		info.Manufacturer = readDMIField(dmiBase, "sys_vendor", "board_vendor", "chassis_vendor")
		info.Model = readDMIField(dmiBase, "product_name", "board_name")
		info.SerialNumber = readDMIField(dmiBase, "product_serial", "board_serial", "chassis_serial")
		info.BIOSVersion = readDMIField(dmiBase, "bios_version")
	}

	info.IsVirtual = detectVirtual(dmiBase)

	hostname, err := os.Hostname()
	if err == nil {
		info.Hostname = hostname
	}

	// If no usable DMI serial was found, fall back to the SMBIOS product
	// UUID or the systemd machine-id as a stable host identifier.
	if info.SerialNumber == "" {
		info.SerialNumber = fallbackHostIdentifier(dmiBase)
	}

	if debug {
		printChassisInfo(info)
	}

	hostID := info.SerialNumber

	return []DeviceRecord{chassisToRecord(info, hostID)}, hostID, collectErr
}

// fallbackHostIdentifier returns a stable unique identifier for the host when
// no DMI serial number is available. It tries the SMBIOS product_uuid first,
// then falls back to /etc/machine-id.
func fallbackHostIdentifier(dmiBase string) string {
	if uuid := readSysfsField(filepath.Join(dmiBase, "product_uuid")); uuid != "" && !isDMIPlaceholder(uuid) {
		return uuid
	}

	if mid := readSysfsField("/etc/machine-id"); mid != "" {
		return mid
	}

	return ""
}

// readDMIField tries each candidate file under dmiBase in order and returns the
// first non-empty, non-placeholder value.
func readDMIField(dmiBase string, candidates ...string) string {
	for _, name := range candidates {
		val := readSysfsField(filepath.Join(dmiBase, name))
		if val != "" && !isDMIPlaceholder(val) {
			return val
		}
	}

	return ""
}

// isDMIPlaceholder returns true for common SMBIOS filler values.
func isDMIPlaceholder(s string) bool {
	lower := strings.ToLower(s)
	switch lower {
	case "to be filled by o.e.m.", "default string", "not specified", "not available", "none", "n/a", "unknown":
		return true
	}

	return false
}

// detectVirtual returns true when the host appears to be a virtual machine
// rather than bare-metal hardware. It checks the SMBIOS chassis_type code,
// well-known hypervisor vendor/product strings, and the kernel release string
// (for environments like WSL that lack DMI data).
func detectVirtual(dmiBase string) bool {
	// WSL does not expose DMI sysfs, but the kernel release string contains
	// "microsoft" (e.g. "5.15.153.1-microsoft-standard-WSL2").
	if rel := readSysfsField("/proc/sys/kernel/osrelease"); strings.Contains(strings.ToLower(rel), "microsoft") {
		return true
	}

	// SMBIOS chassis type 1 == "Other" is used by many hypervisors (QEMU,
	// cloud providers). Not conclusive on its own, but combined with vendor
	// checks it is a strong signal.
	if ct := readSysfsField(filepath.Join(dmiBase, "chassis_type")); ct == "1" {
		return true
	}

	// Check sys_vendor and product_name for common hypervisor identifiers.
	for _, field := range []string{"sys_vendor", "product_name", "board_vendor"} {
		val := strings.ToLower(readSysfsField(filepath.Join(dmiBase, field)))
		for _, keyword := range []string{
			"qemu", "kvm", "vmware", "virtualbox", "vbox",
			"hyper-v", "microsoft corporation", "xen",
			"parallels", "bhyve", "openstack", "digitalocean",
			"google compute engine", "amazon ec2",
		} {
			if strings.Contains(val, keyword) {
				return true
			}
		}
	}

	return false
}

// printChassisInfo prints the collected chassis information to stdout.
func printChassisInfo(c *ChassisInfo) {
	fmt.Println("Chassis / System:")

	if c.Manufacturer != "" {
		fmt.Printf("  Manufacturer:  %s\n", c.Manufacturer)
	}

	if c.Model != "" {
		fmt.Printf("  Model:         %s\n", c.Model)
	}

	if c.SerialNumber != "" {
		fmt.Printf("  Serial Number: %s\n", c.SerialNumber)
	}

	if c.Hostname != "" {
		fmt.Printf("  Hostname:      %s\n", c.Hostname)
	}

	if c.BIOSVersion != "" {
		fmt.Printf("  BIOS Version:  %s\n", c.BIOSVersion)
	}

	fmt.Printf("  Virtual:       %t\n", c.IsVirtual)
}
