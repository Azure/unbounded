package goalstates

type NodeStart struct {
	MachineName string
	MachineDir  string // e.g. /var/lib/machines/node
	Containerd  Containerd
	Kubelet     Kubelet

	// Nvidia holds NVIDIA GPU state discovered on the host. After the nspawn
	// boots, the setup-nvidia-libraries task uses LibMappings to create
	// symlinks inside the container's library path pointing into the
	// bind-mounted /run/host-nvidia/ directories.
	Nvidia NvidiaHost
}
