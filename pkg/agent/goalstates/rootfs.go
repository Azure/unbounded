// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import "github.com/Azure/unbounded/pkg/agent/config"

// RootFS defines the goal state of the machine root fs.
// This goal state produces a rootfs that is ready for running a Kubernetes node
// via systemd-nspawn from dir points to `.MachineDir`.
type RootFS struct {
	MachineDir             string
	NSpawnConfigFile       string // e.g. /etc/systemd/nspawn/node.nspawn
	ServiceOverrideFile    string // e.g. /etc/systemd/system/systemd-nspawn@node.service.d/override.conf
	LifecycleStateFile     string // durable pre-start to post-start resolved-state handoff
	ConfigRegenerationFile string // host systemd pre-start unit
	HostArch               string
	HostKernel             string // running kernel version from uname -r, e.g. "6.8.0-45-generic"
	Hostname               string // host hostname, written into the rootfs so the nspawn container inherits it
	ContainerdVersion      string
	RunCVersion            string
	CNIPluginVersion       string
	KubernetesVersion      string
	LocalDNS               LocalDNS

	// Downloads optionally overrides the download sources for binaries
	// the agent installs into the nspawn rootfs (kubelet, containerd,
	// runc, CNI plugins, crictl). Nil means upstream defaults apply.
	Downloads *DownloadOverrides

	// OCIImage is an OCI registry reference, local OCI layout, or HTTPS URL
	// to a tarred OCI image layout used to bootstrap the machine rootfs. The
	// single tagged image reference in an HTTPS archive is selected automatically.
	// The image must use OCI media types and include a platform manifest
	// matching the host architecture.
	OCIImage string

	// NVIDIARequired is the provisioned GPU capability. It remains stable across
	// lifecycle discovery so transient hardware absence cannot turn a GPU node
	// into a CPU node, and newly appearing hardware cannot enable a CPU node.
	NVIDIARequired bool

	// Nvidia holds NVIDIA GPU state discovered on the host: device paths,
	// driver library mappings, and bind-mount specifications for the nspawn
	// container. Empty on non-GPU hosts.
	Nvidia NvidiaHost

	// AMD holds AMD GPU state discovered on the host. Empty on non-AMD GPU hosts.
	AMD AMDHost

	// HostDevices holds host device nodes to be bind-mounted into the
	// nspawn container, grouped by category (KVM, network virtualization,
	// block storage, InfiniBand HCA). Device nodes are discovered at agent
	// startup. Empty on hosts without any supported devices.
	HostDevices HostDevices

	// AdditionalHostMounts holds configured non-device host paths to be
	// bind-mounted into the nspawn container.
	AdditionalHostMounts []config.AdditionalHostMount
}
