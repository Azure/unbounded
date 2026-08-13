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

// ContainerdOptions controls containerd goal-state resolution.
type ContainerdOptions struct {
	SandboxImage   string
	NvidiaRequired bool
}

// ResolveContainerd returns the containerd configuration goal state.
func ResolveContainerd(opts ContainerdOptions) Containerd {
	if opts.SandboxImage == "" {
		opts.SandboxImage = SandboxImage
	}

	return Containerd{
		SandboxImage:      opts.SandboxImage,
		ContainerdBinPath: filepath.Join("/"+BinDir, "containerd"),
		RuncBinaryPath:    filepath.Join("/"+BinDir, "runc"),
		CNIBinDir:         CNIBinDir,
		CNIConfDir:        CNIConfigDir,
		MetricsAddress:    ContainerdMetricsAddress,
		NvidiaRuntime:     resolveNvidiaRuntime(opts.NvidiaRequired),
	}
}
