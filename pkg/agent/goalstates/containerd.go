// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/config"
)

// Containerd describes the containerd configuration goal state.
type Containerd struct {
	SandboxImage      string
	ContainerdBinPath string
	RuncBinaryPath    string
	CNIBinDir         string
	CNIConfDir        string
	MetricsAddress    string
	NvidiaRuntime     NvidiaRuntime
	RegistryMirrors   []config.ContainerdRegistryMirror
}

// ResolveContainerd returns the containerd configuration goal state. Registry
// mirrors are taken from the agent config when present.
func ResolveContainerd(cfg *config.AgentConfig) Containerd {
	return Containerd{
		SandboxImage:      SandboxImage,
		ContainerdBinPath: filepath.Join("/"+BinDir, "containerd"),
		RuncBinaryPath:    filepath.Join("/"+BinDir, "runc"),
		CNIBinDir:         CNIBinDir,
		CNIConfDir:        CNIConfigDir,
		MetricsAddress:    ContainerdMetricsAddress,
		NvidiaRuntime:     resolveNvidiaRuntime(),
		RegistryMirrors:   cfg.CRI.Containerd.RegistryMirrors,
	}
}
