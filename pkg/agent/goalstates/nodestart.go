// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"context"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

type NodeStart struct {
	// MachineName is the local systemd-nspawn machine name (e.g. "kube1").
	// Used by nspawn service management.
	MachineName string

	// KubeMachineName is the Kubernetes Machine CR name (e.g. "agent-e2e").
	// This is the name that appears in the cluster and may differ from
	// the local nspawn machine name.
	KubeMachineName string

	// NodeName is the Kubernetes Node name used by kubelet and host-side daemon watches.
	NodeName string

	// MachineDir is the nspawn backend's root directory. It is an intentional
	// nspawn-only goal state field; phases reach it through Root().
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

	// NvidiaToolkit locates the NVIDIA Container Toolkit tools on the host. It
	// is set only for isolation mode none; the zero value is the default paths.
	NvidiaToolkit NvidiaToolkit

	// RootOverride replaces the nspawn machine root for backends that do not
	// run the node stack in a machine, such as the none backend. Nil means the
	// nspawn root.
	RootOverride isolation.NodeRoot

	// ExecOverride replaces nspawn.Exec for the same backends. Nil means
	// commands run inside the nspawn machine. A non-nil value also marks the
	// node stack as host native, so phases skip nspawn-only files, unless
	// InContainer is also set.
	ExecOverride func(ctx context.Context, log *slog.Logger, args ...string) (string, error)

	// InContainer marks a node stack that runs in its own root filesystem
	// inside an OCI container (the container backend). Its overrides replace
	// the nspawn machine like the none backend's, but the node stack still has
	// a private root, so phases keep writing the rootfs files and use the
	// in-node archive path. PROVISIONAL: see the container backend.
	InContainer bool
}

// HostNative reports whether the node stack runs directly on the host, so
// phases that write nspawn-only files or masquerade as the machine skip them.
// A node stack in an OCI container has its own root and is not host native.
func (n *NodeStart) HostNative() bool { return n.RootOverride != nil && !n.InContainer }

// NSpawnMachine reports whether the node stack runs in an nspawn machine, which
// is the case when no backend replaced the machine root.
func (n *NodeStart) NSpawnMachine() bool { return n.RootOverride == nil }

type Gantry struct {
	Disabled bool
}
