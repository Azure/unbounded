// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// SELinuxMode is the host SELinux state.
type SELinuxMode string

const (
	SELinuxDisabled   SELinuxMode = "disabled"
	SELinuxPermissive SELinuxMode = "permissive"
	SELinuxEnforcing  SELinuxMode = "enforcing"
)

// SELinuxLabel is the process label applied when SELinux is enforcing.
// PROVISIONAL: spc_t is the design's assumption for Azure Container Linux; it
// has not been verified on a node.
const SELinuxLabel = "system_u:system_r:spc_t:s0"

// SameMount is a host path that must resolve identically in the container
// (design invariant 3).
type SameMount struct {
	Path   string
	Shared bool // rshared propagation
}

// SamePathMounts are the paths shared with the host. PROVISIONAL: whether
// rshared propagation works through the Azure Container Linux root mount is
// unverified.
var SamePathMounts = []SameMount{
	{Path: "/run/containerd"},
	{Path: "/etc/containerd/certs.d"},
	{Path: "/var/lib/kubelet", Shared: true},
	{Path: "/opt/cni"},
	{Path: "/sys/fs/bpf"},
}

// ExtraBind is a host path bound into the container at a different path than
// the same-path contract covers, such as the staged offline image archives.
type ExtraBind struct {
	Source   string
	Target   string
	ReadOnly bool
}

// Host locations the nspawn machine also binds. They are local constants so
// this package does not import goalstates; a test pins them to goalstates.
const (
	// ImageArchiveDir is where staged offline container image archives are
	// visible inside the node, and the base of their host staging directory.
	ImageArchiveDir = "/var/lib/unbounded/container-images"
	// ImageArchiveHostDir is the host symlink to the current staging directory.
	ImageArchiveHostDir = ImageArchiveDir + "/current"
	// KernelModulesDir is the host's kernel modules directory, bound read-only
	// as the nspawn machine does so modprobe inside the node sees the host
	// kernel's modules. PROVISIONAL: Azure Container Linux may keep modules
	// elsewhere (for example under /usr/lib/modules); unverified.
	KernelModulesDir = "/lib/modules"
)

// DeviceInfo describes a host device node.
type DeviceInfo struct {
	Type  string // "c" or "b"
	Major int64
	Minor int64
	Mode  uint32
}

// BundleInput is everything config.json depends on, so generation is a pure
// function.
type BundleInput struct {
	RootfsDir string
	SELinux   SELinuxMode
	Grants    []isolation.Grant
	// Devices maps a granted device path to its node description.
	Devices map[string]DeviceInfo
	// ExtraBinds are bound after the same-path mounts and before grants.
	ExtraBinds []ExtraBind
	// RecursiveReadOnly makes read-only binds "rro" instead of "ro". With "ro"
	// a recursive bind keeps its submounts writable. The runtime (runc 1.1) and
	// the kernel (5.12, mount_setattr) must both support it, so the backend sets
	// this only after checking them.
	RecursiveReadOnly bool
}

// readOnlyOption is the mount option for a read-only bind.
func (in BundleInput) readOnlyOption() string {
	if in.RecursiveReadOnly {
		return "rro"
	}

	return "ro"
}

// cleanTarget returns the cleaned in-container path of a bind or device target
// and refuses the pseudo filesystems the container gets its own copy of: a
// bind over /proc or /sys would replace the container's view of the kernel.
// Cleaning first means "/run/containerd/../containerd" is the same path the
// conflict check sees.
func cleanTarget(what, p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%s target %q must be an absolute path", what, p)
	}

	c := filepath.Clean(p)
	for _, pseudo := range []string{"/proc", "/sys"} {
		if c == pseudo || strings.HasPrefix(c, pseudo+"/") {
			return "", fmt.Errorf("%s target %s is under /proc or /sys, which the container mounts itself", what, c)
		}
	}

	return c, nil
}

// The types below are the subset of the OCI runtime spec the backend emits.
// They are defined locally so no new module dependency is needed.

type spec struct {
	OCIVersion string  `json:"ociVersion"`
	Process    process `json:"process"`
	Root       root    `json:"root"`
	Mounts     []mount `json:"mounts"`
	Linux      linux   `json:"linux"`
}

type process struct {
	Terminal        bool     `json:"terminal"`
	User            user     `json:"user"`
	Args            []string `json:"args"`
	Env             []string `json:"env"`
	Cwd             string   `json:"cwd"`
	Capabilities    caps     `json:"capabilities"`
	NoNewPrivileges bool     `json:"noNewPrivileges"`
	SELinuxLabel    string   `json:"selinuxLabel,omitempty"`
}

type user struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

type caps struct {
	Bounding    []string `json:"bounding"`
	Effective   []string `json:"effective"`
	Permitted   []string `json:"permitted"`
	Inheritable []string `json:"inheritable"`
	Ambient     []string `json:"ambient"`
}

type root struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

type mount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"`
	Source      string   `json:"source,omitempty"`
	Options     []string `json:"options,omitempty"`
}

type linux struct {
	Namespaces        []namespace `json:"namespaces"`
	Devices           []device    `json:"devices,omitempty"`
	Resources         resources   `json:"resources"`
	RootfsPropagation string      `json:"rootfsPropagation"`
	MaskedPaths       []string    `json:"maskedPaths"`
	ReadonlyPaths     []string    `json:"readonlyPaths"`
}

type namespace struct {
	Type string `json:"type"`
}

type device struct {
	Path     string `json:"path"`
	Type     string `json:"type"`
	Major    int64  `json:"major"`
	Minor    int64  `json:"minor"`
	FileMode uint32 `json:"fileMode"`
	UID      int    `json:"uid"`
	GID      int    `json:"gid"`
}

// baselineDevices are the device nodes every node container gets beyond runc's
// defaults.
var baselineDevices = []device{
	{Path: "/dev/kmsg", Type: "c", Major: 1, Minor: 11, FileMode: 0o644},
}

type resources struct {
	Devices []deviceRule `json:"devices"`
}

type deviceRule struct {
	Allow  bool   `json:"allow"`
	Type   string `json:"type,omitempty"`
	Major  *int64 `json:"major,omitempty"`
	Minor  *int64 `json:"minor,omitempty"`
	Access string `json:"access,omitempty"`
}

// allCaps is the full capability set for a privileged container.
// PROVISIONAL: the list is the Linux capability set up to CAP_CHECKPOINT_RESTORE
// and should be trimmed once the node stack's needs are measured.
var allCaps = []string{
	"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_DAC_READ_SEARCH", "CAP_FOWNER", "CAP_FSETID",
	"CAP_KILL", "CAP_SETGID", "CAP_SETUID", "CAP_SETPCAP", "CAP_LINUX_IMMUTABLE",
	"CAP_NET_BIND_SERVICE", "CAP_NET_BROADCAST", "CAP_NET_ADMIN", "CAP_NET_RAW",
	"CAP_IPC_LOCK", "CAP_IPC_OWNER", "CAP_SYS_MODULE", "CAP_SYS_RAWIO", "CAP_SYS_CHROOT",
	"CAP_SYS_PTRACE", "CAP_SYS_PACCT", "CAP_SYS_ADMIN", "CAP_SYS_BOOT", "CAP_SYS_NICE",
	"CAP_SYS_RESOURCE", "CAP_SYS_TIME", "CAP_SYS_TTY_CONFIG", "CAP_MKNOD", "CAP_LEASE",
	"CAP_AUDIT_WRITE", "CAP_AUDIT_CONTROL", "CAP_SETFCAP", "CAP_MAC_OVERRIDE",
	"CAP_MAC_ADMIN", "CAP_SYSLOG", "CAP_WAKE_ALARM", "CAP_BLOCK_SUSPEND", "CAP_AUDIT_READ",
	"CAP_PERFMON", "CAP_BPF", "CAP_CHECKPOINT_RESTORE",
}

// RenderConfig returns the OCI runtime config.json for the input. The output
// is deterministic.
func RenderConfig(in BundleInput) ([]byte, error) {
	if !filepath.IsAbs(in.RootfsDir) {
		return nil, fmt.Errorf("rootfs dir %q must be absolute", in.RootfsDir)
	}

	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"container=oci",
	}

	// Baseline mounts for systemd as PID 1. PROVISIONAL: modeled on common
	// privileged systemd-in-OCI setups; not exercised on Azure Container Linux.
	mounts := []mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "rw"}},
		{Destination: "/sys/fs/cgroup", Type: "cgroup", Source: "cgroup", Options: []string{"nosuid", "noexec", "nodev", "rw"}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
		{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/run", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=755"}},
		{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=1777"}},
	}

	taken := map[string]bool{}
	for _, m := range mounts {
		taken[m.Destination] = true
	}

	for _, s := range SamePathMounts {
		opts := []string{"rbind", "rw"}

		if s.Shared {
			opts = append(opts, "rshared")
		}

		mounts = append(mounts, mount{Destination: s.Path, Type: "bind", Source: s.Path, Options: opts})
		taken[s.Path] = true
	}

	for _, e := range in.ExtraBinds {
		if !filepath.IsAbs(e.Source) || !filepath.IsAbs(e.Target) {
			return nil, fmt.Errorf("extra bind %q to %q must use absolute paths", e.Source, e.Target)
		}

		target, err := cleanTarget("extra bind", e.Target)
		if err != nil {
			return nil, err
		}

		if taken[target] {
			return nil, fmt.Errorf("extra bind target %s conflicts with an existing mount", target)
		}

		taken[target] = true

		mode := "rw"
		if e.ReadOnly {
			mode = in.readOnlyOption()
		}

		mounts = append(mounts, mount{Destination: target, Type: "bind", Source: e.Source, Options: []string{"rbind", mode}})
	}

	var libPaths []string

	// The kubelet opens /dev/kmsg to watch for OOM kills and exits when it is
	// missing, and runc's default /dev has no such node. It is created in every
	// bundle, with an explicit rule so it survives a tightened baseline.
	devices := make([]device, 0, len(baselineDevices))
	rules := []deviceRule{{Allow: true, Access: "rwm"}}

	for _, d := range baselineDevices {
		major, minor := d.Major, d.Minor
		devices = append(devices, d)
		rules = append(rules, deviceRule{Allow: true, Type: d.Type, Major: &major, Minor: &minor, Access: "rwm"})
		taken[d.Path] = true
	}

	for _, g := range in.Grants {
		if g.Kind == isolation.GrantDevice && g.Source == "" && g.DeviceClass != "" {
			// A device class selector names no node to expose. The container
			// already runs with every device allowed (allowAll above), so the
			// class needs no rule of its own.
			continue
		}

		if g.Source == "" || !filepath.IsAbs(g.Source) {
			return nil, fmt.Errorf("grant %q source %q must be an absolute path", g.Kind, g.Source)
		}

		target := g.Target
		if target == "" {
			target = g.Source
		}

		target, err := cleanTarget(fmt.Sprintf("grant %q", g.Kind), target)
		if err != nil {
			return nil, err
		}

		switch g.Kind {
		case isolation.GrantMount, isolation.GrantLibraryPath:
			if taken[target] {
				return nil, fmt.Errorf("grant target %s conflicts with an existing mount", target)
			}

			taken[target] = true

			mode := "rw"
			if g.ReadOnly || g.Kind == isolation.GrantLibraryPath {
				mode = in.readOnlyOption()
			}

			mounts = append(mounts, mount{Destination: target, Type: "bind", Source: g.Source, Options: []string{"rbind", mode}})

			if g.Kind == isolation.GrantLibraryPath {
				libPaths = append(libPaths, target)
			}
		case isolation.GrantDevice:
			info, ok := in.Devices[g.Source]
			if !ok {
				return nil, fmt.Errorf("device grant %s has no device information", g.Source)
			}

			if taken[target] {
				return nil, fmt.Errorf("grant target %s conflicts with an existing mount", target)
			}

			taken[target] = true

			major, minor := info.Major, info.Minor
			devices = append(devices, device{Path: target, Type: info.Type, Major: major, Minor: minor, FileMode: info.Mode})
			// Explicit rule so the grant survives a tightened baseline.
			rules = append(rules, deviceRule{Allow: true, Type: info.Type, Major: &major, Minor: &minor, Access: "rwm"})
		default:
			return nil, fmt.Errorf("unsupported grant kind %q", g.Kind)
		}
	}

	if len(libPaths) > 0 {
		env = append(env, "LD_LIBRARY_PATH="+strings.Join(libPaths, ":"))
	}

	p := process{
		User:         user{},
		Args:         []string{initPath},
		Env:          env,
		Cwd:          "/",
		Capabilities: caps{Bounding: allCaps, Effective: allCaps, Permitted: allCaps, Inheritable: allCaps, Ambient: []string{}},
	}
	if in.SELinux == SELinuxEnforcing {
		p.SELinuxLabel = SELinuxLabel
	}

	s := spec{
		OCIVersion: "1.0.2",
		Process:    p,
		Root:       root{Path: in.RootfsDir},
		Mounts:     mounts,
		Linux: linux{
			// Host network namespace (no network entry) and host UTS so the
			// hostname is the node name. PID 1 needs its own PID namespace.
			Namespaces:        []namespace{{Type: "pid"}, {Type: "mount"}, {Type: "ipc"}, {Type: "cgroup"}},
			Devices:           devices,
			Resources:         resources{Devices: rules},
			RootfsPropagation: "rslave",
			MaskedPaths:       []string{},
			ReadonlyPaths:     []string{},
		},
	}

	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}

	return append(b, '\n'), nil
}
