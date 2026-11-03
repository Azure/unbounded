// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package isolation defines where the node stack (containerd, kubelet, CNI and
// CoreDNS) runs on a host. The agent and its daemon always run on the host; a
// Backend decides only where the node stack lives and how it is operated.
//
// Phases receive a NodeRoot and call Path and Backend.Exec. They must not
// reference machine directories, machinectl or systemd-run --machine directly.
package isolation

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// Mode names an isolation mode.
type Mode string

const (
	// ModeNSpawn runs the node stack in a systemd-nspawn machine. It is the
	// default, and the mode of every host that has no recorded mode.
	ModeNSpawn Mode = "nspawn"
	// ModeNone runs the node stack directly on the host.
	ModeNone Mode = "none"
	// ModeContainer runs the node stack in a privileged OCI container.
	ModeContainer Mode = "container"
)

// DefaultMode is the mode used when none is configured or recorded.
const DefaultMode = ModeNSpawn

// ParseMode validates s and returns the Mode it names. The empty string is the
// default mode. Unrecognized values are rejected rather than ignored so an
// older agent never silently runs a newer mode as nspawn.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case "":
		return DefaultMode, nil
	case ModeNSpawn, ModeNone, ModeContainer:
		return m, nil
	default:
		return "", fmt.Errorf("unsupported isolation mode %q (supported: %s, %s, %s)",
			s, ModeNSpawn, ModeNone, ModeContainer)
	}
}

// Requirements declares what a backend needs from the host. Preflight checks it
// before anything is mutated.
type Requirements struct {
	// HostPackages are packages that must be present on the host.
	HostPackages []string
	// HostCommands are executables that must be resolvable on the host.
	HostCommands []string
	// WritablePaths are host paths that must be writable.
	WritablePaths []string
	// After lists host-global units the node stack must start after.
	After []string
}

// NodeSpec describes the node a backend prepares.
type NodeSpec struct {
	// Slot is the blue/green slot, for example kube1 or kube2.
	Slot string
	// Version is the Kubernetes version the node runs. It is informational:
	// no backend selects behavior from it (the container backend records it in
	// the bundle's node spec file).
	Version string
	// Grants are device, mount and library requests the node stack needs.
	Grants []Grant
}

// GrantKind is the type of a Grant.
type GrantKind string

const (
	// GrantMount exposes a host path inside the node stack.
	GrantMount GrantKind = "mount"
	// GrantDevice exposes a host device inside the node stack.
	GrantDevice GrantKind = "device"
	// GrantLibraryPath adds a directory to the node stack's library search path.
	GrantLibraryPath GrantKind = "library-path"
)

// GrantPurpose says what a Grant is for. Backends use it to place a grant in
// the right section of their own configuration and to keep a stable rendering
// order. It carries no host paths.
type GrantPurpose string

const (
	// PurposeHostDevice is a discovered or configured host device node, or a
	// systemd device group specifier, such as /dev/kvm or char-input.
	PurposeHostDevice GrantPurpose = "host-device"
	// PurposeHostMount is a configured non-device host path.
	PurposeHostMount GrantPurpose = "host-mount"
	// PurposeNvidiaDevice is an NVIDIA GPU device node or directory.
	PurposeNvidiaDevice GrantPurpose = "nvidia-device"
	// PurposeNvidiaLibDir is a host directory of NVIDIA driver libraries.
	PurposeNvidiaLibDir GrantPurpose = "nvidia-lib-dir"
	// PurposeNvidiaI386LibDir is a host directory of 32-bit NVIDIA libraries.
	PurposeNvidiaI386LibDir GrantPurpose = "nvidia-i386-lib-dir"
	// PurposeNvidiaHelperBin is the host directory of NVIDIA helper binaries
	// that node-start copies into the node.
	PurposeNvidiaHelperBin GrantPurpose = "nvidia-helper-bin"
	// PurposeAMDDevice is an AMD GPU device node.
	PurposeAMDDevice GrantPurpose = "amd-device"
	// PurposeAMDSysFS is a host sysfs path the AMD device plugin reads.
	PurposeAMDSysFS GrantPurpose = "amd-sysfs"
)

// LibDirMount is a host library directory made visible at ContainerDir. Index
// orders the mounts and maps libraries to their directory.
type LibDirMount struct {
	Index        int
	HostDir      string
	ContainerDir string
}

// Grant is a backend-neutral request for host resources. Each backend applies
// it in its own way, and a backend with nothing to apply ignores it.
type Grant struct {
	Kind    GrantKind
	Purpose GrantPurpose
	// Source is the host path (or device node). It is empty for a GrantDevice
	// that only names a device class and has no node to expose.
	Source string
	// Target is the in-node path. Empty means the same as Source.
	Target   string
	ReadOnly bool
	// DeviceClass is a cgroup device class selector such as char-input, for a
	// GrantDevice that has no Source.
	DeviceClass string
}

// ExecSpec is a command run in the node stack.
type ExecSpec struct {
	// Args is the command and its arguments.
	Args []string
}

// ExecResult is the outcome of a successful Exec.
type ExecResult struct {
	// Stdout is the captured standard output with trailing newlines trimmed.
	Stdout string
}

// LogOptions selects which logs Logs returns.
type LogOptions struct {
	// Unit is the systemd unit inside the node stack. Empty means all units.
	Unit string
	// Follow keeps the stream open for new entries.
	Follow bool
	// Lines limits output to the last N entries when positive.
	Lines int
}

// Status is the observed state of a node root.
type Status struct {
	// Running reports whether the node stack is up.
	Running bool
	// Detail is a human readable explanation.
	Detail string
}

// NodeRoot resolves paths into the node stack's filesystem view.
type NodeRoot interface {
	Slot() string
	// Path maps an in-node absolute path to its path as seen by the agent. For
	// ModeNone this is the identity.
	Path(inNode string) string
}

// DirRoot is a NodeRoot whose filesystem is a directory on the host. The nspawn
// backend uses it with the unpacked rootfs directory; an empty Dir is the
// identity mapping. Callers that only need to name a slot (the container
// backend resolves paths with its own container.Root) use it as a slot carrier.
type DirRoot struct {
	SlotName string
	Dir      string
}

// Slot implements NodeRoot.
func (r DirRoot) Slot() string { return r.SlotName }

// Path implements NodeRoot.
func (r DirRoot) Path(inNode string) string {
	if r.Dir == "" {
		return inNode
	}

	return filepath.Join(r.Dir, strings.TrimPrefix(inNode, "/"))
}

// Backend owns where the node stack runs.
type Backend interface {
	Mode() Mode

	// Requirements declares host requirements checked by preflight before
	// anything is mutated, and ordering dependencies on host-global units.
	Requirements() Requirements

	// Prepare materializes a node root for a slot: it unpacks or installs
	// artifacts and renders backend configuration.
	Prepare(ctx context.Context, n NodeSpec) (NodeRoot, error)

	Start(ctx context.Context, r NodeRoot) error
	Stop(ctx context.Context, r NodeRoot) error
	// Restart restarts the node stack only, never the host (soft reboot).
	Restart(ctx context.Context, r NodeRoot) error
	// Destroy removes the node root and backend configuration for a slot.
	Destroy(ctx context.Context, r NodeRoot) error

	Status(ctx context.Context, r NodeRoot) (Status, error)

	// Exec runs a command in the node stack.
	Exec(ctx context.Context, r NodeRoot, cmd ExecSpec) (ExecResult, error)
	// Logs streams logs for a unit in the node stack.
	Logs(ctx context.Context, r NodeRoot, opts LogOptions) (io.ReadCloser, error)
}
