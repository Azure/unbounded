package goalstates

// RootFS defines the goal state of the machine root fs.
// This goal state produces a rootfs that is ready for running a Kubernetes node
// via systemd-nspawn from dir points to `.MachineDir`.
type RootFS struct {
	MachineDir          string
	NSpawnConfigFile    string // e.g. /etc/systemd/nspawn/node.nspawn
	ServiceOverrideFile string // e.g. /etc/systemd/system/systemd-nspawn@node.service.d/override.conf
	HostArch            string
	HostKernel          string // running kernel version from uname -r, e.g. "6.8.0-45-generic"
	ContainerdVersion   string
	RunCVersion         string
	CNIPluginVersion    string
	KubernetesVersion   string
	// TODO: declare GPU device & driver settings
}
