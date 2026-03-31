package goalstates

const (
	SystemdNSpawnDir = "/etc/systemd/nspawn"
	SystemdSystemDir = "/etc/systemd/system"
)

const (
	ContainerdVersion = "2.0.4"
	RunCVersion       = "1.1.12"
	CNIPluginVersion  = "1.5.1"

	ContainerdMetricsAddress = "0.0.0.0:10257"
	SandboxImage             = "mcr.microsoft.com/oss/kubernetes/pause:3.9"

	CNIBinDir    = "/opt/cni/bin"
	CNIConfigDir = "/etc/cni/net.d"

	// BinDir is the standard binary directory relative to the machine root.
	// Use filepath.Join(machineDir, BinDir) for host-side rootfs paths, or
	// "/"+BinDir for absolute paths inside the running machine container.
	BinDir = "usr/local/bin"

	NvidiaContainerRuntimePath = "/usr/bin/nvidia-container-runtime"
	NvidiaRuntimeClassName     = "nvidia"

	SystemdUnitContainerd   = "containerd.service"
	ContainerdConfigPath    = "/etc/containerd/config.toml"
	ContainerdConfDropInDir = "/etc/containerd/conf.d"

	SystemdUnitKubelet             = "kubelet.service"
	KubeletKubeconfigPath          = "/var/lib/kubelet/kubeconfig"
	KubeletBootstrapKubeconfigPath = "/var/lib/kubelet/bootstrap-kubeconfig"
	KubeletPKIDir                  = "/etc/kubernetes/pki"
	KubeletAPIServerCACertPath     = "/etc/kubernetes/pki/apiserver-client-ca.crt"
	KubeletServiceDropInDir        = "/etc/systemd/system/kubelet.service.d"
	KubeletStaticPodManifestsDir   = "/etc/kubernetes/manifests"
)
