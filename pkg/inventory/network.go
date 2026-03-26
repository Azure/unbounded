package inventory

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// NICInfo holds information about a single network interface.
type NICInfo struct {
	Name      string
	MACAddr   string
	IPAddrs   []string
	Driver    string
	Firmware  string
	LinkSpeed string
	MTU       string
}

// nicToRecord converts a NICInfo into a DeviceRecord.
func nicToRecord(n *NICInfo, hostID string) DeviceRecord {
	serial := n.MACAddr
	if serial == "" {
		serial = "nic-" + n.Name
	}

	return DeviceRecord{
		DeviceType:     DeviceTypeNIC,
		DeviceName:     n.Name,
		HostIdentifier: hostID,
		SerialNumber:   serial,
		Attributes: mustMarshal(NICInfo{
			MACAddr:   n.MACAddr,
			IPAddrs:   n.IPAddrs,
			Driver:    n.Driver,
			Firmware:  n.Firmware,
			LinkSpeed: n.LinkSpeed,
			MTU:       n.MTU,
		}),
	}
}

// collectNICInfo discovers physical network interfaces and returns them as device records.
func collectNICInfo(ctx context.Context, hostID string, debug bool) ([]DeviceRecord, error) {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return nil, fmt.Errorf("failed to read /sys/class/net: %w", err)
	}

	var nics []NICInfo

	for _, e := range entries {
		name := e.Name()

		// Skip loopback and virtual devices.
		if name == "lo" || !isPhysicalNIC(name) {
			continue
		}

		base := filepath.Join("/sys/class/net", name)

		nic := NICInfo{
			Name:      name,
			MACAddr:   readSysfsField(filepath.Join(base, "address")),
			LinkSpeed: readNICLinkSpeed(base),
			MTU:       readSysfsField(filepath.Join(base, "mtu")),
			Driver:    readNICDriver(base),
			Firmware:  readNICFirmware(ctx, name),
			IPAddrs:   readNICAddresses(name),
		}

		nics = append(nics, nic)
	}

	if debug {
		printNICInfo(nics)
	}

	var records []DeviceRecord
	for i := range nics {
		records = append(records, nicToRecord(&nics[i], hostID))
	}

	return records, nil
}

// isPhysicalNIC returns true if the interface appears to be a physical NIC
// rather than a virtual device (bridge, veth, tun, etc.).
func isPhysicalNIC(name string) bool {
	devicePath := filepath.Join("/sys/class/net", name, "device")
	_, err := os.Stat(devicePath)

	return err == nil
}

// readNICDriver reads the driver name for a network interface from sysfs.
func readNICDriver(base string) string {
	link, err := os.Readlink(filepath.Join(base, "device", "driver"))
	if err != nil {
		return ""
	}

	return filepath.Base(link)
}

// readNICLinkSpeed reads the negotiated link speed (e.g. "1000") from sysfs.
// The value is in Mbps. Returns "" if the link is down or speed is unavailable.
func readNICLinkSpeed(base string) string {
	speed := readSysfsField(filepath.Join(base, "speed"))
	// Kernel returns "-1" or empty when the link is down or speed is unknown.
	if speed == "" || speed == "-1" {
		return ""
	}

	return speed + " Mbps"
}

// readNICAddresses returns the IP addresses assigned to the named interface.
func readNICAddresses(name string) []string {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}

	var ips []string

	for _, a := range addrs {
		// a.String() returns "ip/mask" (CIDR notation).
		ip := strings.Split(a.String(), "/")[0]
		ips = append(ips, ip)
	}

	return ips
}

// readNICFirmware reads the firmware version of a NIC using ethtool.
func readNICFirmware(ctx context.Context, name string) string {
	out, err := exec.CommandContext(ctx, "ethtool", "-i", name).Output()
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "firmware-version:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "firmware-version:"))
		}
	}

	return ""
}

// printNICInfo prints the collected NIC information to stdout.
func printNICInfo(nics []NICInfo) {
	fmt.Printf("NICs Found:      %d\n", len(nics))

	for i, n := range nics {
		fmt.Printf("\n  NIC %d (%s):\n", i, n.Name)
		fmt.Printf("    MAC Address:   %s\n", n.MACAddr)

		if len(n.IPAddrs) > 0 {
			fmt.Printf("    IP Addresses:  %s\n", strings.Join(n.IPAddrs, ", "))
		}

		if n.Driver != "" {
			fmt.Printf("    Driver:        %s\n", n.Driver)
		}

		if n.Firmware != "" {
			fmt.Printf("    Firmware:      %s\n", n.Firmware)
		}

		if n.LinkSpeed != "" {
			fmt.Printf("    Link Speed:    %s\n", n.LinkSpeed)
		}

		if n.MTU != "" {
			fmt.Printf("    MTU:           %s\n", n.MTU)
		}
	}
}
