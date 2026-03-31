package goalstates

import "path/filepath"

type Containerd struct {
	SandboxImage      string
	ContainerdBinPath string
	RuncBinaryPath    string
	CNIBinDir         string
	CNIConfDir        string
	MetricsAddress    string
	NvidiaRuntime     NvidiaRuntime
}

type NvidiaRuntime struct {
	Enabled                    bool
	RuntimeClassName           string
	RuntimePath                string
	DisableSetAsDefaultRuntime bool
}

func DefaultContainerd() Containerd {
	return Containerd{
		SandboxImage:      SandboxImage,
		ContainerdBinPath: filepath.Join("/"+BinDir, "containerd"),
		RuncBinaryPath:    filepath.Join("/"+BinDir, "runc"),
		CNIBinDir:         CNIBinDir,
		CNIConfDir:        CNIConfigDir,
		MetricsAddress:    ContainerdMetricsAddress,
		NvidiaRuntime: NvidiaRuntime{
			Enabled:                    false,
			RuntimeClassName:           NvidiaRuntimeClassName,
			RuntimePath:                NvidiaContainerRuntimePath,
			DisableSetAsDefaultRuntime: false,
		},
	}
}
