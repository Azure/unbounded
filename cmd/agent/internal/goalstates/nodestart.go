package goalstates

type NodeStart struct {
	MachineName string
	MachineDir  string // e.g. /var/lib/machines/node
	HostKernel  string // running kernel version from uname -r, e.g. "6.8.0-45-generic"
	Containerd  Containerd
	Kubelet     Kubelet
}
