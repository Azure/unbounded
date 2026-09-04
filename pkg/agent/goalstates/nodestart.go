// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

type NodeStart struct {
	// MachineName is the local systemd-nspawn machine name (e.g. "kube1").
	// Used by machinectl commands and nspawn service management.
	MachineName string

	// KubeMachineName is the Kubernetes Machine CR name (e.g. "agent-e2e").
	// This is the name that appears in the cluster and may differ from
	// the local nspawn machine name.
	KubeMachineName string

	// NodeName is the Kubernetes Node name used by kubelet and host-side daemon watches.
	NodeName string

	MachineDir string // e.g. /var/lib/machines/node
	Containerd Containerd
	Gantry     Gantry
	Kubelet    Kubelet
	LocalDNS   LocalDNS

	// Nvidia holds NVIDIA GPU state discovered on the host. After the nspawn
	// boots, the setup-nvidia-libraries task uses LibMappings to create
	// symlinks inside the container's library path pointing into the
	// bind-mounted /run/host-nvidia/ directories.
	Nvidia NvidiaHost

	// HostPaths is the resolved host-side layout of the agent's own files.
	HostPaths HostPaths
}

type Gantry struct {
	Disabled bool
}
