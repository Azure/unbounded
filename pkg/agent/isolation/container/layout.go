// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// Defaults for the host layout. The agent downloads runc into an agent-owned
// directory under /opt/unbounded (rootfs.InstallHostRuntime, run by the
// containerops composition). PROVISIONAL: that the node image allows executing
// from there is unverified on Azure Container Linux.
const (
	// DefaultBase holds one directory per slot (the unpacked rootfs).
	DefaultBase = "/var/lib/unbounded/containers"
	// DefaultRuntimePath is the runc executable. Only runc is supported.
	DefaultRuntimePath = "/opt/unbounded/runtime/runc"
	// SystemdDir is where the agent writes the supervising unit.
	SystemdDir = "/etc/systemd/system"
	// initPath is PID 1 inside the container.
	initPath = "/usr/lib/systemd/systemd"

	slotKube1 = "kube1"
	slotKube2 = "kube2"
)

// ValidSlot reports whether slot is one of the blue/green slots.
func ValidSlot(slot string) error {
	if slot != slotKube1 && slot != slotKube2 {
		return fmt.Errorf("invalid container slot %q (want %s or %s)", slot, slotKube1, slotKube2)
	}

	return nil
}

// ContainerID is the OCI container id for a slot.
func ContainerID(slot string) string { return "unbounded-" + slot }

// UnitName is the agent-owned supervising unit for a slot.
func UnitName(slot string) string { return "unbounded-node-" + slot + ".service" }

// Layout is the on-disk arrangement for container slots.
type Layout struct {
	// Base is the directory holding the slots. Empty means DefaultBase.
	Base string
}

func (l Layout) base() string {
	if l.Base == "" {
		return DefaultBase
	}

	return l.Base
}

// RootfsDir is the unpacked rootfs of a slot, which is what the NodeRoot maps.
func (l Layout) RootfsDir(slot string) string { return filepath.Join(l.base(), slot) }

// BundleDir is the OCI bundle directory holding config.json. It is kept apart
// from the rootfs so the rootfs stays a pure image tree. "bundles" is not a
// valid slot so the two never collide.
func (l Layout) BundleDir(slot string) string { return filepath.Join(l.base(), "bundles", slot) }

// ConfigPath is the generated OCI runtime config.
func (l Layout) ConfigPath(slot string) string {
	return filepath.Join(l.BundleDir(slot), "config.json")
}

// SpecPath records the NodeSpec so the lifecycle pre-start hook can
// regenerate config.json without the original caller.
func (l Layout) SpecPath(slot string) string {
	return filepath.Join(l.BundleDir(slot), "nodespec.json")
}

// Root is the NodeRoot of a container slot. Most in-node paths resolve below
// the slot's unpacked rootfs, but the same-path contract (design invariant 3)
// bind-mounts host directories over some of them, for example /var/lib/kubelet
// and /opt/cni. A file the agent writes below the rootfs at one of those paths
// would be hidden by the mount, so Path maps them to the host directory itself,
// which is what the container sees there.
type Root struct {
	SlotName string
	// Dir is the unpacked rootfs.
	Dir string
	// HostRoot prefixes the same-path host directories. Empty is the real host;
	// tests use a temporary tree.
	HostRoot string
}

// Slot implements isolation.NodeRoot.
func (r Root) Slot() string { return r.SlotName }

// Path implements isolation.NodeRoot.
func (r Root) Path(inNode string) string {
	clean := filepath.Clean("/" + inNode)

	for _, s := range SamePathMounts {
		if clean == s.Path || strings.HasPrefix(clean, s.Path+"/") {
			return filepath.Join(r.HostRoot, clean)
		}
	}

	return filepath.Join(r.Dir, clean)
}

var _ isolation.NodeRoot = Root{}
