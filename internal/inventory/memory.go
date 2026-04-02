package inventory

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// memInfoPath is the standard location of the kernel memory info file.
const memInfoPath = "/proc/meminfo"

// MemoryInfo holds information about the system's memory.
type MemoryInfo struct {
	TotalBytes uint64
	DIMMs      []DIMMInfo
}

// DIMMInfo holds information about a single memory DIMM.
type DIMMInfo struct {
	Locator      string
	Size         string
	Speed        string
	SerialNumber string
}

// memoryToRecord converts a MemoryInfo into a DeviceRecord.
func memoryToRecord(m *MemoryInfo, hostID string) DeviceRecord {
	var dimms []DIMMInfo
	for _, d := range m.DIMMs {
		dimms = append(dimms, DIMMInfo(d))
	}

	return DeviceRecord{
		DeviceType:     DeviceTypeMemory,
		DeviceName:     "System Memory",
		HostIdentifier: hostID,
		SerialNumber:   "system-memory",
		Attributes: mustMarshal(MemoryInfo{
			TotalBytes: m.TotalBytes,
			DIMMs:      dimms,
		}),
	}
}

// collectMemoryInfo gathers system memory information and returns it as device records.
// memInfoPath overrides the default /proc/meminfo location when non-empty.
func collectMemoryInfo(ctx context.Context, hostID string, debug bool, memInfoPath string) ([]DeviceRecord, error) {
	if memInfoPath == "" {
		return nil, fmt.Errorf("memInfoPath cannot be empty")
	}

	total, err := readTotalMemory(memInfoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read total memory: %w", err)
	}

	dimms := collectDIMMInfo(ctx)

	info := &MemoryInfo{
		TotalBytes: total,
		DIMMs:      dimms,
	}

	if debug {
		printMemoryInfo(info)
	}

	return []DeviceRecord{memoryToRecord(info, hostID)}, nil
}

// readTotalMemory reads total system memory from the given meminfo path.
func readTotalMemory(memInfoPath string) (uint64, error) {
	data, err := os.ReadFile(memInfoPath)
	if err != nil {
		return 0, err
	}

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, fmt.Errorf("unexpected MemTotal format: %s", line)
			}

			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("failed to parse MemTotal: %w", err)
			}

			return kb * 1024, nil // convert kB to bytes
		}
	}

	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}

// collectDIMMInfo reads per-DIMM details from dmidecode (SMBIOS type 17).
// Returns nil if dmidecode is unavailable or requires elevated privileges.
func collectDIMMInfo(ctx context.Context) []DIMMInfo {
	out, err := exec.CommandContext(ctx, "dmidecode", "-t", "memory").Output()
	if err != nil {
		return nil
	}

	var (
		dimms   []DIMMInfo
		current DIMMInfo
	)

	inDevice := false

	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)

		// Each "Memory Device" block describes a DIMM slot.
		if trimmed == "Memory Device" {
			if inDevice {
				dimms = appendDIMM(dimms, current)
			}

			current = DIMMInfo{}
			inDevice = true

			continue
		}

		if !inDevice {
			continue
		}

		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		switch key {
		case "Size":
			current.Size = val
		case "Locator":
			current.Locator = val
		case "Serial Number":
			current.SerialNumber = val
		case "Speed":
			current.Speed = val
		}
	}

	// Capture the last device block.
	if inDevice {
		dimms = appendDIMM(dimms, current)
	}

	return dimms
}

// appendDIMM adds a DIMM to the list if it has a populated size (filters empty slots).
func appendDIMM(dimms []DIMMInfo, d DIMMInfo) []DIMMInfo {
	if d.Size == "" || strings.EqualFold(d.Size, "No Module Installed") {
		return dimms
	}

	return append(dimms, d)
}

// formatBytes returns a human-readable representation of bytes.
func formatBytes(b uint64) string {
	const (
		gib = 1024 * 1024 * 1024
		mib = 1024 * 1024
	)
	switch {
	case b >= gib:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(gib))
	case b >= mib:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(mib))
	default:
		return fmt.Sprintf("%d bytes", b)
	}
}

// printMemoryInfo prints the collected memory information to stdout.
func printMemoryInfo(mem *MemoryInfo) {
	fmt.Printf("Total Memory:    %s\n", formatBytes(mem.TotalBytes))

	if len(mem.DIMMs) > 0 {
		fmt.Printf("DIMMs Installed: %d\n", len(mem.DIMMs))

		for i, d := range mem.DIMMs {
			fmt.Printf("\n  DIMM %d:\n", i)
			fmt.Printf("    Locator:       %s\n", d.Locator)
			fmt.Printf("    Size:          %s\n", d.Size)
			fmt.Printf("    Speed:         %s\n", d.Speed)
			fmt.Printf("    Serial Number: %s\n", d.SerialNumber)
		}
	} else {
		fmt.Println("No DIMM information available")
	}
}
