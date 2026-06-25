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

// AMDHost aggregates AMD GPU host state discovered at agent startup.
type AMDHost struct {
	// GPUDevicePaths lists AMD GPU device paths discovered on the host
	// (e.g. /dev/kfd, /dev/dri/card*, /dev/dri/renderD*). When non-empty the
	// nspawn configuration will bind-mount these devices and grant cgroup access
	// so the AMD Kubernetes device plugin can detect GPUs inside the machine.
	GPUDevicePaths []string
}

// ResolveAMDHost probes the host for AMD GPU device nodes. Returns an empty
// struct when the host does not have AMD GPUs or the driver is not loaded.
func ResolveAMDHost() AMDHost {
	return AMDHost{GPUDevicePaths: discoverAMDDevices()}
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
			if strings.HasPrefix(name, "card") || strings.HasPrefix(name, "renderD") {
				devices = append(devices, filepath.Join(driPath, name))
			}
		}
	}

	slices.Sort(devices)

	return devices
}
