// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import "path/filepath"

// Containerd describes the containerd configuration goal state.
type Containerd struct {
	SandboxImage      string
	ContainerdBinPath string
	RuncBinaryPath    string
	CNIBinDir         string
	CNIConfDir        string
	MetricsAddress    string
	NvidiaRuntime     NvidiaRuntime
}

// ResolveContainerd returns the containerd configuration goal state.
func ResolveContainerd(sandboxImage string) Containerd {
	return ResolveContainerdForNVIDIACapability(sandboxImage, len(discoverNVIDIADevices()) > 0)
}

// ResolveContainerdForNVIDIACapability resolves containerd without probing GPU
// hardware again. Lifecycle callers use this to preserve the capability chosen
// when the machine was provisioned.
func ResolveContainerdForNVIDIACapability(sandboxImage string, nvidiaRequired bool) Containerd {
	if sandboxImage == "" {
		sandboxImage = SandboxImage
	}

	return Containerd{
		SandboxImage:      sandboxImage,
		ContainerdBinPath: filepath.Join("/"+BinDir, "containerd"),
		RuncBinaryPath:    filepath.Join("/"+BinDir, "runc"),
		CNIBinDir:         CNIBinDir,
		CNIConfDir:        CNIConfigDir,
		MetricsAddress:    ContainerdMetricsAddress,
		NvidiaRuntime:     resolveNvidiaRuntime(nvidiaRequired),
	}
}
