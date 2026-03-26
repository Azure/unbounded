package inventory

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GPUInfo holds information about a single GPU.
type GPUInfo struct {
	Name          string
	PCIAddress    string
	Vendor        string
	Model         string
	SerialNumber  string
	VRAM          string
	Firmware      string
	Driver        string
	DriverVersion string
	NUMANode      string
}

// gpuToRecord converts a GPUInfo into a DeviceRecord.
func gpuToRecord(g *GPUInfo, hostID string) DeviceRecord {
	serial := g.SerialNumber
	if serial == "" {
		serial = "gpu-" + g.PCIAddress
	}

	return DeviceRecord{
		DeviceType:     DeviceTypeGPU,
		DeviceName:     g.Name,
		HostIdentifier: hostID,
		SerialNumber:   serial,
		Attributes: mustMarshal(GPUInfo{
			PCIAddress:    g.PCIAddress,
			Vendor:        g.Vendor,
			Model:         g.Model,
			VRAM:          g.VRAM,
			Firmware:      g.Firmware,
			Driver:        g.Driver,
			DriverVersion: g.DriverVersion,
			NUMANode:      g.NUMANode,
		}),
	}
}

// collectGPUInfo detects GPUs by scanning the PCI bus for display/3D controller
// class devices and returns them as device records.
func collectGPUInfo(ctx context.Context, hostID string, debug bool) ([]DeviceRecord, error) {
	gpus, err := scanPCIForGPUs()
	if err != nil {
		return nil, err
	}

	// Enrich all GPUs with lspci data (model name, VRAM, etc.).
	if lspciData, err := queryLspci(ctx); err == nil {
		mergeLspciInfo(gpus, lspciData)
	}

	// Enrich NVIDIA GPUs with nvidia-smi if available.
	if nvidiaGPUs, err := queryNvidiaSMI(ctx); err == nil {
		mergeNvidiaInfo(gpus, nvidiaGPUs)
	}

	// Fill in firmware and driver version from sysfs/modinfo for any GPU
	// still missing them after vendor-specific enrichment.
	for i := range gpus {
		enrichFromSysfs(ctx, &gpus[i])
	}

	if debug {
		printGPUInfo(gpus)
	}

	var records []DeviceRecord
	for i := range gpus {
		records = append(records, gpuToRecord(&gpus[i], hostID))
	}

	return records, nil
}

// scanPCIForGPUs walks /sys/bus/pci/devices looking for VGA-compatible (0x0300)
// and 3D controller (0x0302) class devices.
func scanPCIForGPUs() ([]GPUInfo, error) {
	pciBase := "/sys/bus/pci/devices"

	entries, err := os.ReadDir(pciBase)
	if err != nil {
		return nil, fmt.Errorf("cannot read PCI devices: %w", err)
	}

	var gpus []GPUInfo

	for _, e := range entries {
		devPath := filepath.Join(pciBase, e.Name())
		class := readSysfsField(filepath.Join(devPath, "class"))

		// PCI class 0x030000 = VGA, 0x030200 = 3D controller.
		if !strings.HasPrefix(class, "0x0300") && !strings.HasPrefix(class, "0x0302") {
			continue
		}

		vendor := readSysfsField(filepath.Join(devPath, "vendor"))
		device := readSysfsField(filepath.Join(devPath, "device"))
		numaNode := readSysfsField(filepath.Join(devPath, "numa_node"))

		driver := ""

		driverLink, err := filepath.EvalSymlinks(filepath.Join(devPath, "driver"))
		if err == nil {
			driver = filepath.Base(driverLink)
		}

		gpu := GPUInfo{
			PCIAddress: e.Name(),
			Vendor:     pciVendorName(vendor),
			Model:      fmt.Sprintf("%s:%s", vendor, device),
			Driver:     driver,
			NUMANode:   numaNode,
		}
		gpus = append(gpus, gpu)
	}

	return gpus, nil
}

// nvidiaSMIInfo holds fields parsed from nvidia-smi CSV output.
type nvidiaSMIInfo struct {
	PCIAddress string
	Name       string
	Serial     string
	VRAM       string
	Firmware   string
	DriverVer  string
}

// queryNvidiaSMI runs nvidia-smi to get detailed GPU info.
func queryNvidiaSMI(ctx context.Context) ([]nvidiaSMIInfo, error) {
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=pci.bus_id,name,serial,memory.total,vbios_version,driver_version",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, err
	}

	var gpus []nvidiaSMIInfo

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), ", ", 6)
		if len(fields) < 6 {
			continue
		}

		gpus = append(gpus, nvidiaSMIInfo{
			PCIAddress: normalizePCIAddr(strings.TrimSpace(fields[0])),
			Name:       strings.TrimSpace(fields[1]),
			Serial:     strings.TrimSpace(fields[2]),
			VRAM:       strings.TrimSpace(fields[3]) + " MiB",
			Firmware:   strings.TrimSpace(fields[4]),
			DriverVer:  strings.TrimSpace(fields[5]),
		})
	}

	return gpus, scanner.Err()
}

// mergeNvidiaInfo enriches PCI-scanned GPUInfo entries with nvidia-smi data
// matched by PCI address.
func mergeNvidiaInfo(gpus []GPUInfo, nv []nvidiaSMIInfo) {
	byAddr := make(map[string]*nvidiaSMIInfo, len(nv))
	for i := range nv {
		byAddr[nv[i].PCIAddress] = &nv[i]
	}

	for i := range gpus {
		info, ok := byAddr[gpus[i].PCIAddress]
		if !ok {
			continue
		}

		if info.Name != "" {
			gpus[i].Name = info.Name
			gpus[i].Model = info.Name
		}

		if info.Serial != "" && info.Serial != "[N/A]" {
			gpus[i].SerialNumber = info.Serial
		}

		if info.VRAM != "" {
			gpus[i].VRAM = info.VRAM
		}

		if info.Firmware != "" {
			gpus[i].Firmware = info.Firmware
		}

		if info.DriverVer != "" {
			gpus[i].DriverVersion = info.DriverVer
		}
	}
}

// normalizePCIAddr converts nvidia-smi's PCI address format
// (e.g. "00000000:07:00.0") to the sysfs format ("0000:07:00.0").
func normalizePCIAddr(addr string) string {
	addr = strings.ToLower(addr)
	// nvidia-smi uses full domain:bus:dev.fn with 8-char domain; sysfs uses 4-char.
	parts := strings.SplitN(addr, ":", 2)
	if len(parts) == 2 && len(parts[0]) == 8 {
		return parts[0][4:] + ":" + parts[1]
	}

	return addr
}

// pciVendorName returns a human-readable name for common GPU PCI vendor IDs.
func pciVendorName(vendorID string) string {
	switch strings.ToLower(vendorID) {
	case "0x10de":
		return "NVIDIA"
	case "0x1002":
		return "AMD"
	case "0x8086":
		return "Intel"
	case "0x102b":
		return "Matrox"
	case "0x1a03":
		return "ASPEED"
	case "0x1da3":
		return "Habana Labs"
	default:
		return vendorID
	}
}

// lspciGPU holds per-device info parsed from lspci output.
type lspciGPU struct {
	PCIAddress string
	Name       string
}

// queryLspci runs `lspci -mm -D` and returns display/3D devices with their
// human-readable names.
func queryLspci(ctx context.Context) ([]lspciGPU, error) {
	out, err := exec.CommandContext(ctx, "lspci", "-mm", "-D").Output()
	if err != nil {
		return nil, err
	}

	var gpus []lspciGPU

	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}

		fields := parseLspciMM(line)
		if len(fields) < 4 {
			continue
		}
		// Class is field[1]; look for VGA/3D/Display.
		classLower := strings.ToLower(fields[1])
		if !strings.Contains(classLower, "vga") &&
			!strings.Contains(classLower, "3d") &&
			!strings.Contains(classLower, "display") {
			continue
		}
		// fields[0]=slot, [1]=class, [2]=vendor, [3]=device
		addr := fields[0]

		name := fields[3]
		if fields[2] != "" {
			name = fields[2] + " " + name
		}

		gpus = append(gpus, lspciGPU{PCIAddress: addr, Name: strings.TrimSpace(name)})
	}

	return gpus, nil
}

// parseLspciMM parses a single line of `lspci -mm` output, handling quoted fields.
func parseLspciMM(line string) []string {
	var fields []string

	for line != "" {
		line = strings.TrimLeft(line, " \t")
		if line == "" {
			break
		}

		if line[0] == '"' {
			end := strings.Index(line[1:], "\"")
			if end < 0 {
				fields = append(fields, line[1:])
				break
			}

			fields = append(fields, line[1:end+1])
			line = line[end+2:]
		} else {
			idx := strings.IndexAny(line, " \t")
			if idx < 0 {
				fields = append(fields, line)
				break
			}

			fields = append(fields, line[:idx])
			line = line[idx:]
		}
	}

	return fields
}

// mergeLspciInfo enriches PCI-scanned GPUs with friendly names from lspci.
func mergeLspciInfo(gpus []GPUInfo, lspci []lspciGPU) {
	byAddr := make(map[string]string, len(lspci))
	for _, l := range lspci {
		byAddr[l.PCIAddress] = l.Name
	}

	for i := range gpus {
		name, ok := byAddr[gpus[i].PCIAddress]
		if ok && name != "" {
			gpus[i].Name = name
			gpus[i].Model = name
		}
	}
}

// enrichFromSysfs fills in firmware version and driver version from sysfs and
// modinfo for GPUs that weren't enriched by vendor-specific tools.
func enrichFromSysfs(ctx context.Context, g *GPUInfo) {
	devPath := filepath.Join("/sys/bus/pci/devices", g.PCIAddress)

	// Try reading firmware from the DRM subsystem.
	if g.Firmware == "" {
		g.Firmware = readDRMFirmware(devPath)
	}

	// Try getting driver version via modinfo.
	if g.DriverVersion == "" && g.Driver != "" {
		if out, err := exec.CommandContext(ctx, "modinfo", "-F", "version", g.Driver).Output(); err == nil {
			if v := strings.TrimSpace(string(out)); v != "" {
				g.DriverVersion = v
			}
		}
	}

	// Try reading VRAM from the DRM memory info.
	if g.VRAM == "" {
		g.VRAM = readDRMVRAM(devPath)
	}
}

// readDRMFirmware reads the firmware version from the DRM subsystem if available.
func readDRMFirmware(devPath string) string {
	drmPath := filepath.Join(devPath, "drm")

	entries, err := os.ReadDir(drmPath)
	if err != nil {
		return ""
	}

	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "card") {
			continue
		}
		// Some drivers expose firmware under /sys/class/drm/cardN/device/fw_version
		fw := readSysfsField(filepath.Join("/sys/class/drm", e.Name(), "device", "fw_version"))
		if fw != "" {
			return fw
		}
	}

	return ""
}

// readDRMVRAM reads video memory size from DRM sysfs if available.
func readDRMVRAM(devPath string) string {
	// resource file line 6 (BAR 0) often represents VRAM; also try
	// /sys/class/drm/cardN/device/mem_info_vram_total (amdgpu).
	drmPath := filepath.Join(devPath, "drm")

	entries, err := os.ReadDir(drmPath)
	if err != nil {
		return ""
	}

	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "card") {
			continue
		}

		vram := readSysfsField(filepath.Join("/sys/class/drm", e.Name(), "device", "mem_info_vram_total"))
		if vram != "" {
			// Convert bytes to MiB if it looks like a number.
			var bytes uint64
			if _, err := fmt.Sscanf(vram, "%d", &bytes); err == nil && bytes > 0 {
				return fmt.Sprintf("%d MiB", bytes/1024/1024)
			}

			return vram
		}
	}

	return ""
}

// printGPUInfo prints the collected GPU information to stdout.
func printGPUInfo(gpus []GPUInfo) {
	fmt.Printf("GPUs Found:      %d\n", len(gpus))

	for i, g := range gpus {
		name := g.Name
		if name == "" {
			name = g.Model
		}

		fmt.Printf("\n  GPU %d (%s):\n", i, g.PCIAddress)
		fmt.Printf("    Vendor:        %s\n", g.Vendor)
		fmt.Printf("    Model:         %s\n", name)

		if g.SerialNumber != "" {
			fmt.Printf("    Serial:        %s\n", g.SerialNumber)
		}

		if g.VRAM != "" {
			fmt.Printf("    VRAM:          %s\n", g.VRAM)
		}

		if g.Firmware != "" {
			fmt.Printf("    Firmware:      %s\n", g.Firmware)
		}

		if g.Driver != "" {
			fmt.Printf("    Driver:        %s\n", g.Driver)
		}

		if g.DriverVersion != "" {
			fmt.Printf("    Driver Ver:    %s\n", g.DriverVersion)
		}

		if g.NUMANode != "" && g.NUMANode != "-1" {
			fmt.Printf("    NUMA Node:     %s\n", g.NUMANode)
		}
	}
}
