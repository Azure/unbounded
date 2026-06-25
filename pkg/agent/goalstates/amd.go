// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const amdKFDDevicePath = "/dev/kfd"

var amdSysFSPaths = []string{
	"/sys/module/amdgpu",
	"/sys/class/kfd",
	"/sys/class/drm",
	"/sys/devices",
}

// AMDHost aggregates AMD GPU host state discovered at agent startup.
type AMDHost struct {
	// GPUDevicePaths lists AMD GPU device paths discovered on the host
	// (e.g. /dev/kfd, /dev/dri/card*, /dev/dri/renderD*). When non-empty the
	// nspawn configuration will bind-mount these devices and grant cgroup access
	// so the AMD Kubernetes device plugin can detect GPUs inside the machine.
	GPUDevicePaths []string

	// SysFSPaths lists host sysfs paths the AMD Kubernetes device plugin reads
	// to discover AMD GPUs and KFD topology inside the nspawn machine.
	SysFSPaths []string
}

// ResolveAMDHost probes the host for AMD GPU device nodes. Returns an empty
// struct when the host does not have AMD GPUs or the driver is not loaded.
func ResolveAMDHost() AMDHost {
	devices := discoverAMDDevices()
	if len(devices) == 0 {
		return AMDHost{}
	}

	return AMDHost{
		GPUDevicePaths: devices,
		SysFSPaths:     discoverAMDSysFSPaths(amdSysFSPaths),
	}
}

// discoverAMDDevices scans for AMD GPU device nodes and returns them as a
// sorted slice. AMD ROCm workloads and the AMD Kubernetes device plugin require
// /dev/kfd plus the corresponding DRM card/render nodes under /dev/dri.
func discoverAMDDevices() []string {
	return discoverAMDDevicesAt(amdKFDDevicePath, driDir)
}

func discoverAMDDevicesAt(kfdPath, driPath string) []string {
	if _, err := os.Stat(kfdPath); err != nil {
		return nil
	}

	devices := []string{kfdPath}

	driEntries, err := os.ReadDir(driPath)
	if err == nil {
		for _, e := range driEntries {
			if e.IsDir() {
				continue
			}

			name := e.Name()
			// TODO: Narrow DRM discovery to AMD-owned nodes by checking
			// /sys/class/drm/<name>/device/vendor == 0x1002. Today we expose all DRM
			// card/render nodes when /dev/kfd exists so the AMD device plugin can
			// discover GPUs in nspawn. That is broader than necessary on mixed-GPU
			// hosts, but avoids accidentally missing non-standard AMD layouts until
			// we validate sysfs filtering behavior across target hardware.
			if strings.HasPrefix(name, "card") || strings.HasPrefix(name, "renderD") {
				devices = append(devices, filepath.Join(driPath, name))
			}
		}
	}

	slices.Sort(devices)

	return devices
}

func discoverAMDSysFSPaths(paths []string) []string {
	var existing []string

	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			continue
		}

		existing = append(existing, path)
	}

	return existing
}
