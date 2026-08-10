// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"fmt"
	"strings"
)

const (
	// ConfigDir is the host-level configuration directory for unbounded-kube.
	ConfigDir = "/etc/unbounded/kube"

	// AgentConfigDir is the host-level configuration directory for the
	// unbounded-agent. Applied config files are persisted here.
	AgentConfigDir = "/etc/unbounded/agent"

	SystemdNSpawnDir = "/etc/systemd/nspawn"
	SystemdSystemDir = "/etc/systemd/system"
	BPFFSMountDir    = "/run/bpffs"

	// DaemonUnit is the systemd unit name for the unbounded-agent daemon.
	DaemonUnit = "unbounded-agent-daemon.service"

	// DaemonRecoveryUnit is the systemd recovery unit for the agent daemon.
	DaemonRecoveryUnit = "unbounded-agent-daemon-recovery.service"

	DaemonBinaryPath             = "/usr/local/bin/unbounded-agent"
	DaemonBinaryBluePath         = "/usr/local/bin/unbounded-agent-blue"
	DaemonBinaryGreenPath        = "/usr/local/bin/unbounded-agent-green"
	DaemonBinaryCurrentPath      = "/usr/local/bin/unbounded-agent-current"
	DaemonBinaryLastGoodPath     = "/usr/local/bin/unbounded-agent-last-good"
	DaemonRecoveryScriptPath     = "/usr/local/bin/unbounded-agent-daemon-recovery.sh"
	DaemonAgentUpgradeSignalPath = AgentConfigDir + "/agent-upgrade-signal"
	DaemonAgentUpgradeLockPath   = "/run/unbounded-agent-upgrade.lock"

	EnvDaemonBinary                 = "UNBOUNDED_AGENT_DAEMON_BINARY"
	EnvDaemonBinaryBlue             = "UNBOUNDED_AGENT_DAEMON_BINARY_BLUE"
	EnvDaemonBinaryGreen            = "UNBOUNDED_AGENT_DAEMON_BINARY_GREEN"
	EnvDaemonBinaryCurrent          = "UNBOUNDED_AGENT_DAEMON_BINARY_CURRENT"
	EnvDaemonBinaryLastGood         = "UNBOUNDED_AGENT_DAEMON_BINARY_LAST_GOOD"
	EnvDaemonAgentUpgradeSignalPath = "UNBOUNDED_AGENT_DAEMON_AGENT_UPGRADE_SIGNAL_PATH"
)

// NSpawn machine names used for alternating in-place upgrades.
//
//   - NSpawnMachineKube1 is the initial (default) machine name.
//   - NSpawnMachineKube2 will be used for the next upgrade cycle.
//
// The pattern alternates between the two so an in-place upgrade can bring
// up the new machine before tearing down the old one:
//
//	kube1  ← initial
//	kube2  ← after operation 1
//	kube1  ← after operation 2
//	…
const (
	NSpawnMachineKube1 = "kube1"
	NSpawnMachineKube2 = "kube2"
)

// AlternateMachine returns the other machine name in the pair.
// kube1 -> kube2, kube2 -> kube1.
func AlternateMachine(current string) string {
	if current == NSpawnMachineKube1 {
		return NSpawnMachineKube2
	}

	return NSpawnMachineKube1
}

// BPFFSMountPath returns the host-side bpffs mount path for an nspawn machine.
func BPFFSMountPath(machineName string) string {
	return fmt.Sprintf("%s/%s", BPFFSMountDir, machineName)
}

// AppliedConfigPath returns the path to the applied config file for the
// given nspawn machine name, e.g. /etc/unbounded/agent/kube1-applied-config.json.
func AppliedConfigPath(machineName string) string {
	return fmt.Sprintf("%s/%s-applied-config.json", AgentConfigDir, machineName)
}

// ContainerImageArchivePath returns the path inside the nspawn machine where a
// preloaded container image archive is staged before importing into containerd.
func ContainerImageArchivePath(index int) string {
	return fmt.Sprintf("%s/image-%d.tar", ContainerImageArchiveDir, index)
}

func KubeProxyImage(kubernetesVersion string) string {
	kubernetesVersion = strings.TrimSpace(kubernetesVersion)
	if kubernetesVersion == "" {
		kubernetesVersion = "latest"
	} else if !strings.HasPrefix(kubernetesVersion, "v") {
		kubernetesVersion = "v" + kubernetesVersion
	}

	return KubeProxyImageRepository + ":" + kubernetesVersion
}

const (
	ContainerdVersion = "2.1.8"
	RunCVersion       = "1.5.0"
	CNIPluginVersion  = "1.5.1"

	ContainerdMetricsAddress = "0.0.0.0:10257"
	SandboxImage             = "mcr.microsoft.com/oss/v2/kubernetes/pause:3.9"
	KubeProxyImageRepository = "mcr.microsoft.com/oss/v2/kubernetes/kube-proxy"

	// OfflineArtifactArchiveHostDir stores HTTPS offline artifact archives
	// after download and extraction.
	OfflineArtifactArchiveHostDir = "/var/lib/unbounded/offline-artifacts"

	// ContainerImageArchiveDir is the path inside the nspawn machine where
	// staged container image archives are mounted.
	ContainerImageArchiveDir = "/var/lib/unbounded/container-images"
	// ContainerImageArchiveHostDir is the stable host-side symlink bind-mounted
	// read-only at ContainerImageArchiveDir inside nspawn machines.
	ContainerImageArchiveHostDir = ContainerImageArchiveDir + "/current"
	// ContainerImageArchiveHostSourceDir stores source-specific archive staging
	// directories pointed to by ContainerImageArchiveHostDir.
	ContainerImageArchiveHostSourceDir = ContainerImageArchiveDir

	CNIBinDir    = "/opt/cni/bin"
	CNIConfigDir = "/etc/cni/net.d"

	// BinDir is the standard binary directory relative to the machine root.
	// Use filepath.Join(machineDir, BinDir) for host-side rootfs paths, or
	// "/"+BinDir for absolute paths inside the running machine container.
	BinDir = "usr/local/bin"

	// NvidiaHostLibDir is the base directory inside the nspawn container where
	// host NVIDIA library directories are bind-mounted read-only. Each unique
	// host directory gets a numbered subdirectory (e.g. /run/host-nvidia/0/).
	NvidiaHostLibDir = "/run/host-nvidia"
	// NvidiaHostI386LibDir is the base directory for optional i386 NVIDIA
	// library bind mounts.
	NvidiaHostI386LibDir = "/run/host-nvidia-i386"
	// NvidiaHostBinDir exposes the host directory containing NVIDIA helper binaries.
	NvidiaHostBinDir = "/run/host-nvidia-bin"
	// NvidiaDriverDir is the driver-root layout used by NVIDIA tooling and
	// device plugins inside the nspawn machine.
	NvidiaDriverDir = "/run/nvidia/driver"

	NvidiaContainerRuntimePath = "/usr/bin/nvidia-container-runtime"
	NvidiaRuntimeClassName     = "nvidia"
	NvidiaCTKPath              = "/usr/bin/nvidia-ctk"

	// CDISpecDir is the directory where CDI specifications are stored inside the
	// nspawn machine. containerd reads specs from this directory when CDI is enabled.
	CDISpecDir = "/etc/cdi"

	// CDISpecFile is the path to the NVIDIA CDI specification file inside the
	// nspawn machine. This is generated by nvidia-ctk cdi generate.
	CDISpecFile = "/etc/cdi/nvidia.yaml"

	// Default OCI images for the nspawn rootfs when no image is explicitly
	// configured or set via AGENT_OCI_IMAGE.
	DefaultOCIImage                  = "ghcr.io/azure/agent-ubuntu2404:v20260619"
	DefaultNvidiaOCIImage            = "ghcr.io/azure/agent-ubuntu2404-nvidia:v20260619"
	DefaultUbuntu2604OCIImage        = "ghcr.io/azure/agent-ubuntu2604:v20260619"
	DefaultUbuntu2604NvidiaOCIImage  = "ghcr.io/azure/agent-ubuntu2604-nvidia:v20260619"
	DefaultAzureLinux3OCIImage       = "ghcr.io/azure/agent-azlinux3:v20260619"
	DefaultAzureLinux3NvidiaOCIImage = "ghcr.io/azure/agent-azlinux3-nvidia:v20260626"

	SystemdUnitContainerd      = "containerd.service"
	ContainerdConfigPath       = "/etc/containerd/config.toml"
	ContainerdConfDropInDir    = "/etc/containerd/conf.d"
	ContainerdCertsDir         = "/etc/containerd/certs.d"
	ContainerdDefaultHostsDir  = ContainerdCertsDir + "/_default"
	ContainerdDefaultHostsPath = ContainerdDefaultHostsDir + "/hosts.toml"

	SystemdUnitKubelet             = "kubelet.service"
	KubeletConfigurationPath       = "/var/lib/kubelet/config.yaml"
	KubeletKubeconfigPath          = "/var/lib/kubelet/kubeconfig"
	KubeletBootstrapKubeconfigPath = "/var/lib/kubelet/bootstrap-kubeconfig"
	KubeletPKIDir                  = "/etc/kubernetes/pki"
	KubeletAPIServerCACertPath     = "/etc/kubernetes/pki/apiserver-client-ca.crt"
	KubeletServiceDropInDir        = "/etc/systemd/system/kubelet.service.d"
	KubeletStaticPodManifestsDir   = "/etc/kubernetes/manifests"
	LocalDNSResolvConfPath         = "/etc/unbounded/localdns/resolv.conf"
)
