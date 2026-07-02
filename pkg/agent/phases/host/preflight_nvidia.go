// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const (
	checkNvidiaDriverName = "nvidia-driver"

	// NVIDIA's PCI-SIG vendor ID is 0x10de. PCI vendor IDs are stable
	// identifiers assigned by PCI-SIG; Linux exposes them via
	// /sys/bus/pci/devices/*/vendor. See the PCI ID database entry:
	// https://pci-ids.ucw.cz/read/PC/10de
	nvidiaVendorID      = "0x10de"
	pciDisplayClassPref = "0x03"
)

var (
	nvidiaRequiredModules = []string{"nvidia", "nvidia_modeset", "nvidia_uvm", "nvidia_drm"}
	nvidiaRequiredNodes   = []string{"nvidiactl", "nvidia-modeset", "nvidia-uvm"}
	nvidiaRequiredLibs    = []string{"libcuda.so.1", "libnvidia-ml.so.1"}
	nvidiaGPUDeviceRE     = regexp.MustCompile(`^nvidia[0-9]+$`)
)

type nvidiaDriverDeps struct {
	pciDevicesDir string
	devDir        string
	moduleDir     string
	readDir       func(string) ([]os.DirEntry, error)
	readFile      func(string) ([]byte, error)
	readLink      func(string) (string, error)
	stat          func(string) (fs.FileInfo, error)
	lookPath      func(string) (string, error)
	outputCmd     func(context.Context, string, ...string) ([]byte, error)
}

type nvidiaDriverChecker struct {
	log  *slog.Logger
	deps nvidiaDriverDeps
}

type nvidiaPCIDevice struct {
	path   string
	addr   string
	class  string
	driver string
}

func defaultNvidiaDriverDeps() nvidiaDriverDeps {
	return nvidiaDriverDeps{
		pciDevicesDir: "/sys/bus/pci/devices",
		devDir:        "/dev",
		moduleDir:     "/sys/module",
		readDir:       os.ReadDir,
		readFile:      os.ReadFile,
		readLink:      os.Readlink,
		stat:          os.Stat,
		lookPath:      exec.LookPath,
		outputCmd: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
	}
}

// CheckNvidiaDriver validates the host NVIDIA driver stack when NVIDIA GPU
// hardware is present. It is a no-op success on hosts without NVIDIA GPUs.
func CheckNvidiaDriver(log *slog.Logger) preflight.Checker {
	return checkNvidiaDriver(log, defaultNvidiaDriverDeps())
}

func checkNvidiaDriver(log *slog.Logger, deps nvidiaDriverDeps) preflight.Checker {
	return nvidiaDriverChecker{log: log, deps: deps}
}

func (c nvidiaDriverChecker) Name() string { return checkNvidiaDriverName }

func (c nvidiaDriverChecker) Check(ctx context.Context) []preflight.Result {
	devices := c.discoverPCIDevices()
	deviceNodes := c.discoverDeviceNodes()

	if len(devices) == 0 && len(deviceNodes.nvidia) == 0 {
		if deviceNodes.devReadErr != nil {
			return preflight.ResultsWarning(
				checkNvidiaDriverName,
				"NVIDIA device nodes",
				"NVIDIA device directory could not be inspected: %s",
				c.deps.devDir,
			)
		}

		return preflight.ResultsOK(checkNvidiaDriverName, "NVIDIA driver", "no NVIDIA GPU hardware detected")
	}

	var findings []preflight.Result

	findings = append(findings, c.checkPCIDriver(devices)...)
	findings = append(findings, c.checkKernelModules(ctx)...)
	findings = append(findings, c.checkDeviceNodes(deviceNodes)...)
	findings = append(findings, c.checkDriverLibraries(ctx)...)
	findings = append(findings, c.checkNvidiaSMI(ctx)...)
	findings = append(findings, c.checkServices(ctx)...)

	if len(findings) == 0 {
		return preflight.ResultsOK(checkNvidiaDriverName, "NVIDIA driver", "NVIDIA driver stack is available")
	}

	results := make([]preflight.Result, 0, len(findings)+1)
	if len(devices) > 0 {
		results = append(results, preflight.OK(
			checkNvidiaDriverName,
			"NVIDIA PCI hardware",
			fmt.Sprintf("NVIDIA GPU hardware detected: %d PCI device(s)", len(devices)),
		))
	}

	results = append(results, findings...)

	return results
}

func (c nvidiaDriverChecker) discoverPCIDevices() []nvidiaPCIDevice {
	entries, err := c.deps.readDir(c.deps.pciDevicesDir)
	if err != nil {
		c.log.Debug("NVIDIA PCI discovery failed", "path", c.deps.pciDevicesDir, "error", err)
		return nil
	}

	var devices []nvidiaPCIDevice

	for _, entry := range entries {
		addr := entry.Name()
		path := filepath.Join(c.deps.pciDevicesDir, addr)
		vendor := strings.TrimSpace(readString(c.deps.readFile, filepath.Join(path, "vendor")))
		class := strings.TrimSpace(readString(c.deps.readFile, filepath.Join(path, "class")))

		if !strings.EqualFold(vendor, nvidiaVendorID) || !strings.HasPrefix(strings.ToLower(class), pciDisplayClassPref) {
			continue
		}

		devices = append(devices, nvidiaPCIDevice{
			path:   path,
			addr:   addr,
			class:  class,
			driver: c.pciDriver(path),
		})
	}

	return devices
}

func (c nvidiaDriverChecker) pciDriver(devicePath string) string {
	target, err := c.deps.readLink(filepath.Join(devicePath, "driver"))
	if err != nil {
		return ""
	}

	return filepath.Base(filepath.Clean(target))
}

type nvidiaDeviceNodes struct {
	nvidia     []string
	caps       []string
	render     []string
	devReadErr error
}

func (c nvidiaDriverChecker) discoverDeviceNodes() nvidiaDeviceNodes {
	var nodes nvidiaDeviceNodes

	entries, err := c.deps.readDir(c.deps.devDir)
	if err != nil {
		nodes.devReadErr = err
	} else {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasPrefix(entry.Name(), "nvidia") {
				continue
			}

			nodes.nvidia = append(nodes.nvidia, entry.Name())
		}
	}

	capsDir := filepath.Join(c.deps.devDir, "nvidia-caps")
	if entries, err := c.deps.readDir(capsDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				nodes.caps = append(nodes.caps, entry.Name())
			}
		}
	}

	driDir := filepath.Join(c.deps.devDir, "dri")
	if entries, err := c.deps.readDir(driDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasPrefix(entry.Name(), "renderD") {
				nodes.render = append(nodes.render, entry.Name())
			}
		}
	}

	return nodes
}

func (c nvidiaDriverChecker) checkPCIDriver(devices []nvidiaPCIDevice) []preflight.Result {
	if len(devices) == 0 {
		return preflight.ResultsWarning(
			checkNvidiaDriverName,
			"NVIDIA PCI driver",
			"NVIDIA device nodes are present but no NVIDIA display or 3D PCI device was detected",
		)
	}

	bound := 0

	for _, device := range devices {
		c.log.Debug("checking NVIDIA PCI device", "addr", device.addr, "class", device.class, "driver", device.driver)

		if device.driver == "nvidia" {
			bound++
		}
	}

	if bound != len(devices) {
		return preflight.ResultsError(
			checkNvidiaDriverName,
			"NVIDIA PCI driver",
			"NVIDIA GPU hardware is present but only %d of %d device(s) are bound to the nvidia kernel driver",
			bound,
			len(devices),
		)
	}

	return nil
}

func (c nvidiaDriverChecker) checkKernelModules(ctx context.Context) []preflight.Result {
	var missing []string

	for _, module := range nvidiaRequiredModules {
		if _, err := c.deps.stat(filepath.Join(c.deps.moduleDir, module)); err != nil {
			missing = append(missing, module)
		}
	}

	var results []preflight.Result
	if len(missing) > 0 {
		results = append(results, preflight.Error(
			checkNvidiaDriverName,
			"NVIDIA kernel modules",
			"required NVIDIA kernel modules are not loaded: %s",
			strings.Join(missing, ", "),
		))
	}

	if _, err := c.deps.stat(filepath.Join(c.deps.moduleDir, "nvidia")); err == nil {
		if version, err := c.modinfoVersion(ctx); err != nil {
			results = append(results, preflight.Warning(
				checkNvidiaDriverName,
				"NVIDIA kernel modules",
				"NVIDIA kernel module version could not be determined",
			))
		} else {
			c.log.Debug("NVIDIA kernel module version", "version", version)
		}
	}

	return results
}

func (c nvidiaDriverChecker) modinfoVersion(ctx context.Context) (string, error) {
	modinfo, err := c.deps.lookPath("modinfo")
	if err != nil {
		return "", err
	}

	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	out, err := c.deps.outputCmd(checkCtx, modinfo, "-F", "version", "nvidia")
	if err != nil {
		return "", err
	}

	version := strings.TrimSpace(string(out))
	if version == "" {
		return "", errors.New("empty module version")
	}

	return version, nil
}

func (c nvidiaDriverChecker) checkDeviceNodes(nodes nvidiaDeviceNodes) []preflight.Result {
	if nodes.devReadErr != nil {
		return preflight.ResultsError(
			checkNvidiaDriverName,
			"NVIDIA device nodes",
			"NVIDIA device directory could not be inspected: %s",
			c.deps.devDir,
		)
	}

	var missing []string

	for _, node := range nvidiaRequiredNodes {
		if !slices.Contains(nodes.nvidia, node) {
			missing = append(missing, filepath.Join(c.deps.devDir, node))
		}
	}

	if !hasNvidiaGPUNode(nodes.nvidia) {
		missing = append(missing, filepath.Join(c.deps.devDir, "nvidia<N>"))
	}

	var results []preflight.Result
	if len(missing) > 0 {
		results = append(results, preflight.Error(
			checkNvidiaDriverName,
			"NVIDIA device nodes",
			"required NVIDIA device nodes are missing: %s",
			strings.Join(missing, ", "),
		))
	}

	if !slices.Contains(nodes.nvidia, "nvidia-uvm-tools") {
		results = append(results, preflight.Warning(
			checkNvidiaDriverName,
			"NVIDIA device nodes",
			"NVIDIA UVM tools device node is missing: %s",
			filepath.Join(c.deps.devDir, "nvidia-uvm-tools"),
		))
	}

	if len(nodes.caps) == 0 {
		results = append(results, preflight.Warning(
			checkNvidiaDriverName,
			"NVIDIA device nodes",
			"NVIDIA capability device nodes are missing",
		))
	}

	if len(nodes.render) == 0 {
		results = append(results, preflight.Warning(
			checkNvidiaDriverName,
			"NVIDIA device nodes",
			"DRI render device nodes are missing for NVIDIA GPUs",
		))
	}

	return results
}

func (c nvidiaDriverChecker) checkDriverLibraries(ctx context.Context) []preflight.Result {
	ldconfig, err := c.deps.lookPath("ldconfig")
	if err != nil {
		return preflight.ResultsError(
			checkNvidiaDriverName,
			"NVIDIA driver libraries",
			"ldconfig is required to discover NVIDIA driver libraries",
		)
	}

	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	out, err := c.deps.outputCmd(checkCtx, ldconfig, "-p")
	if err != nil {
		return preflight.ResultsError(
			checkNvidiaDriverName,
			"NVIDIA driver libraries",
			"NVIDIA driver libraries could not be discovered with ldconfig",
		)
	}

	libs := parseLdconfigLibraries(out, nvidiaLdconfigArchTag())

	var missing []string

	var missingPath []string

	for _, lib := range nvidiaRequiredLibs {
		path := libs[lib]
		if path == "" {
			missing = append(missing, lib)
			continue
		}

		if _, err := c.deps.stat(path); err != nil {
			missingPath = append(missingPath, fmt.Sprintf("%s at %s", lib, path))
		}
	}

	var results []preflight.Result
	if len(missing) > 0 {
		results = append(results, preflight.Error(
			checkNvidiaDriverName,
			"NVIDIA driver libraries",
			"required NVIDIA driver libraries are not discoverable: %s",
			strings.Join(missing, ", "),
		))
	}

	if len(missingPath) > 0 {
		results = append(results, preflight.Error(
			checkNvidiaDriverName,
			"NVIDIA driver libraries",
			"NVIDIA driver library paths from ldconfig are missing: %s",
			strings.Join(missingPath, ", "),
		))
	}

	return results
}

func (c nvidiaDriverChecker) checkNvidiaSMI(ctx context.Context) []preflight.Result {
	nvidiaSMI, err := c.deps.lookPath("nvidia-smi")
	if err != nil {
		return preflight.ResultsWarning(
			checkNvidiaDriverName,
			"NVIDIA diagnostic tooling",
			"nvidia-smi is not installed; skipping NVIDIA driver health query",
		)
	}

	checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	out, err := c.deps.outputCmd(checkCtx, nvidiaSMI, "-L")
	message := strings.TrimSpace(string(out))

	if err != nil {
		if message == "" {
			message = "nvidia-smi could not query NVIDIA GPUs"
		}

		return preflight.ResultsError(
			checkNvidiaDriverName,
			"NVIDIA diagnostic tooling",
			"%s",
			message,
		)
	}

	if message == "" {
		return preflight.ResultsError(
			checkNvidiaDriverName,
			"NVIDIA diagnostic tooling",
			"nvidia-smi did not report any NVIDIA GPUs",
		)
	}

	return nil
}

func (c nvidiaDriverChecker) checkServices(ctx context.Context) []preflight.Result {
	// TODO: Consider adding an NVIDIA Fabric Manager check for NVSwitch-based
	// systems once preflight can reliably identify SKUs that require it.
	systemctl, err := c.deps.lookPath("systemctl")
	if err != nil {
		return nil
	}

	if !c.systemdUnitExists(ctx, systemctl, "nvidia-persistence-mode.service") {
		return preflight.ResultsWarning(
			checkNvidiaDriverName,
			"NVIDIA persistence service",
			"NVIDIA persistence mode service is not installed",
		)
	}

	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	out, err := c.deps.outputCmd(checkCtx, systemctl, "is-active", "nvidia-persistence-mode.service")
	state := strings.TrimSpace(string(out))

	if err != nil || state != "active" {
		if state == "" {
			state = "unknown"
		}

		return preflight.ResultsWarning(
			checkNvidiaDriverName,
			"NVIDIA persistence service",
			"NVIDIA persistence mode service is not active: %s",
			state,
		)
	}

	return nil
}

func (c nvidiaDriverChecker) systemdUnitExists(ctx context.Context, systemctl, unit string) bool {
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	out, err := c.deps.outputCmd(checkCtx, systemctl, "list-unit-files", unit)

	return err == nil && strings.Contains(string(out), unit)
}

func parseLdconfigLibraries(out []byte, archTag string) map[string]string {
	libs := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(out))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.Contains(line, "=>") {
			continue
		}

		if archTag != "" && !strings.Contains(line, archTag) {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		name := fields[0]
		if _, ok := libs[name]; ok {
			continue
		}

		libs[name] = fields[len(fields)-1]
	}

	return libs
}

func nvidiaLdconfigArchTag() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86-64"
	case "arm64":
		return "aarch64"
	default:
		return ""
	}
}

func readString(readFile func(string) ([]byte, error), path string) string {
	data, err := readFile(path)
	if err != nil {
		return ""
	}

	return string(data)
}

func hasNvidiaGPUNode(nodes []string) bool {
	return slices.ContainsFunc(nodes, nvidiaGPUDeviceRE.MatchString)
}
