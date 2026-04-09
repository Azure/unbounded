// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package inventory

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// cpuInfoPath is the standard location of the kernel CPU info file.
const cpuInfoPath = "/proc/cpuinfo"

// CPUInfo holds information about the system's CPUs.
type CPUInfo struct {
	ModelName        string
	Architecture     string
	Flags            []string
	Microcode        string
	CoresPerCPU      string
	ThreadsPerCore   string
	AddressSizes     string
	SerialNumbers    []string
	LogicalCPUCount  int
	PhysicalCPUCount int
}

// cpuToRecord converts a CPUInfo into a DeviceRecord.
func cpuToRecord(c *CPUInfo, hostID string) DeviceRecord {
	name := c.ModelName
	if name == "" {
		name = "CPU"
	}

	serial := strings.Join(c.SerialNumbers, ",")
	if serial == "" {
		serial = "cpu-0"
	}

	return DeviceRecord{
		DeviceType:     DeviceTypeCPU,
		DeviceName:     name,
		HostIdentifier: hostID,
		SerialNumber:   serial,
		Attributes: mustMarshal(CPUInfo{
			Architecture:     c.Architecture,
			Flags:            c.Flags,
			Microcode:        c.Microcode,
			CoresPerCPU:      c.CoresPerCPU,
			ThreadsPerCore:   c.ThreadsPerCore,
			AddressSizes:     c.AddressSizes,
			SerialNumbers:    c.SerialNumbers,
			LogicalCPUCount:  c.LogicalCPUCount,
			PhysicalCPUCount: c.PhysicalCPUCount,
		}),
	}
}

// collectCpuInfo reads cpuinfo and returns CPU device records.
// cpuInfoPath overrides the default /proc/cpuinfo location when non-empty.
func collectCpuInfo(ctx context.Context, hostID string, debug bool, cpuInfoPath string) ([]DeviceRecord, error) {
	if cpuInfoPath == "" {
		return nil, fmt.Errorf("cpuInfoPath cannot be empty")
	}

	f, err := os.Open(cpuInfoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", cpuInfoPath, err)
	}

	defer func() {
		if cerr := f.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to close %s: %v\n", cpuInfoPath, cerr)
		}
	}()

	type rawCPU struct {
		modelName    string
		flags        []string
		microcode    string
		cpuCores     string
		siblings     string
		addressSizes string
		physicalID   string
	}

	var cpus []rawCPU

	current := rawCPU{}
	hasProcessor := false
	physicalIDs := make(map[string]struct{})

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()

		// Blank line separates logical CPU entries.
		if strings.TrimSpace(line) == "" {
			if hasProcessor {
				if current.physicalID != "" {
					physicalIDs[current.physicalID] = struct{}{}
				}

				cpus = append(cpus, current)
			}

			current = rawCPU{}
			hasProcessor = false

			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		switch key {
		case "processor":
			hasProcessor = true
		case "model name":
			current.modelName = val
		case "flags":
			current.flags = strings.Fields(val)
		case "microcode":
			current.microcode = val
		case "cpu cores":
			current.cpuCores = val
		case "siblings":
			current.siblings = val
		case "address sizes":
			current.addressSizes = val
		case "physical id":
			current.physicalID = val
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading /proc/cpuinfo: %w", err)
	}

	// Capture the last entry if the file doesn't end with a blank line.
	if hasProcessor {
		if current.physicalID != "" {
			physicalIDs[current.physicalID] = struct{}{}
		}

		cpus = append(cpus, current)
	}

	if len(cpus) == 0 {
		return nil, nil
	}

	// Use the first CPU's info as representative.
	first := cpus[0]
	arch := detectArchitecture()

	physCount := len(physicalIDs)
	if physCount == 0 {
		physCount = 1
	}

	threadsPerCore := computeThreadsPerCore(first.siblings, first.cpuCores)
	serials := collectCpuSerials(ctx)

	info := &CPUInfo{
		ModelName:        first.modelName,
		Architecture:     arch,
		Flags:            first.flags,
		Microcode:        first.microcode,
		CoresPerCPU:      first.cpuCores,
		ThreadsPerCore:   threadsPerCore,
		AddressSizes:     first.addressSizes,
		SerialNumbers:    serials,
		LogicalCPUCount:  len(cpus),
		PhysicalCPUCount: physCount,
	}

	if debug {
		printCpuInfo(info)
	}

	return []DeviceRecord{cpuToRecord(info, hostID)}, nil
}

// collectCpuSerials reads CPU serial numbers from dmidecode (SMBIOS).
// Returns nil if dmidecode is unavailable or no serials are populated.
func collectCpuSerials(ctx context.Context) []string {
	out, err := exec.CommandContext(ctx, "dmidecode", "-t", "processor").Output()
	if err != nil {
		return nil
	}

	var serials []string

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Serial Number:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Serial Number:"))
			if val != "" && !strings.EqualFold(val, "Not Specified") {
				serials = append(serials, val)
			}
		}
	}

	return serials
}

// computeThreadsPerCore derives threads per core from siblings and cores.
func computeThreadsPerCore(siblings, cores string) string {
	if siblings == "" || cores == "" {
		return "unknown"
	}

	var s, c int
	if _, err := fmt.Sscanf(siblings, "%d", &s); err != nil {
		return "unknown"
	}

	if _, err := fmt.Sscanf(cores, "%d", &c); err != nil || c == 0 {
		return "unknown"
	}

	return fmt.Sprintf("%d", s/c)
}

// detectArchitecture returns the machine architecture from the uname syscall
// (e.g. "x86_64", "aarch64").
func detectArchitecture() string {
	var utsname syscall.Utsname
	if err := syscall.Uname(&utsname); err != nil {
		return "unknown"
	}

	// Utsname.Machine is [65]int8; convert to string.
	var buf []byte

	for _, b := range utsname.Machine {
		if b == 0 {
			break
		}

		buf = append(buf, byte(b))
	}

	return string(buf)
}

// printCpuInfo prints the collected CPU information to stdout.
func printCpuInfo(cpu *CPUInfo) {
	fmt.Printf("Physical CPUs:   %d\n", cpu.PhysicalCPUCount)
	fmt.Printf("Logical CPUs:    %d\n", cpu.LogicalCPUCount)
	fmt.Printf("Cores per CPU:   %s\n", cpu.CoresPerCPU)
	fmt.Printf("Threads/Core:    %s\n", cpu.ThreadsPerCore)
	fmt.Printf("Model Name:      %s\n", cpu.ModelName)
	fmt.Printf("Architecture:    %s\n", cpu.Architecture)
	fmt.Printf("Microcode:       %s\n", cpu.Microcode)
	fmt.Printf("Address Sizes:   %s\n", cpu.AddressSizes)
	fmt.Printf("Serial Numbers:  %s\n", strings.Join(cpu.SerialNumbers, ", "))
	fmt.Printf("Flags:           %s\n", strings.Join(cpu.Flags, " "))
}
