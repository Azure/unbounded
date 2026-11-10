// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nspawn

import (
	"bytes"
	"embed"
	"fmt"
	"slices"
	"text/template"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

//go:embed assets/nspawn.conf assets/config-regeneration.service assets/service-override.conf
var assets embed.FS

// Templates holds the nspawn settings file, the service override drop-in and
// the config regeneration unit, by their asset file names.
var Templates = template.Must(
	template.New("nspawn").ParseFS(assets, "assets/nspawn.conf", "assets/config-regeneration.service", "assets/service-override.conf"),
)

// Bind describes one Bind= or BindReadOnly= entry of the .nspawn file.
type Bind struct {
	// Source is the host path passed to Bind=/BindReadOnly=.
	Source string
	// Target is the optional container path. Empty means "bind at Source".
	Target string
	// ReadOnly controls whether BindReadOnly= is used instead of Bind=.
	ReadOnly bool
}

// DeviceAllow describes one DeviceAllow= entry of the service override.
type DeviceAllow struct {
	// Specifier is the systemd DeviceAllow selector (path or char-* class).
	Specifier string
	// Access is the device permission mode, usually "rwm".
	Access string
}

// DeviceTarget pairs the bind that makes a device visible with the cgroup rule
// that makes it usable.
type DeviceTarget struct {
	// Bind controls visibility of a host path inside nspawn.
	Bind Bind
	// Allow controls cgroup device access for the corresponding target.
	Allow DeviceAllow
}

// TemplateData holds the data passed to the nspawn.conf, service-override.conf
// and config-regeneration.service templates. Using a struct (rather than
// map[string]any) lets the templates call helper methods directly.
type TemplateData struct {
	// MachineName is the nspawn machine name (e.g. "kube1"). Used by the
	// service drop-in for the ExecStartPre `machinectl terminate` cleanup.
	MachineName                  string
	BPFFSMountPath               string
	ContainerImageArchiveDir     string
	ContainerImageArchiveHostDir string
	HostDevicePaths              []string
	HostDeviceGroupSpecifiers    []string
	// MountDeviceAllow holds DeviceAllow entries (paths or device groups)
	// declared by host mounts, such as whole device directories.
	MountDeviceAllow     []string
	AdditionalHostMounts []config.AdditionalHostMount
	// NvidiaDeviceTargets contains render-ready Bind+DeviceAllow pairs.
	NvidiaDeviceTargets    []DeviceTarget
	NvidiaLibDirMounts     []isolation.LibDirMount
	NvidiaI386LibDirMounts []isolation.LibDirMount
	NvidiaBinDir           string
	AMDGPUDevicePaths      []string
	AMDSysFSPaths          []string
	ConfigRegenerationUnit string
	// PreStartCommand and PostStartCommand are the lifecycle hooks' commands.
	PreStartCommand  string
	PostStartCommand string
	// PreStartRefusedExitCode is the pre-start exit code that must not be
	// restarted. It is set only for the agent's lifecycle helper, whose exit
	// codes the agent controls; zero renders no directive.
	PreStartRefusedExitCode int
}

// ApplyGrants fills the device, mount and library fields of d from backend
// neutral grants. This is how the nspawn backend applies grants: mounts become
// Bind= and BindReadOnly= lines and devices become Bind= plus DeviceAllow=
// lines. A mount's DeviceAllow entries become DeviceAllow= lines. The order of
// grants within a purpose is preserved.
func (d *TemplateData) ApplyGrants(grants []isolation.Grant) {
	for _, g := range grants {
		switch g.Purpose {
		case isolation.PurposeHostDevice:
			if g.Source != "" {
				d.HostDevicePaths = append(d.HostDevicePaths, g.Source)
			}

			if g.DeviceClass != "" {
				d.HostDeviceGroupSpecifiers = append(d.HostDeviceGroupSpecifiers, g.DeviceClass)
			}
		case isolation.PurposeHostMount:
			d.AdditionalHostMounts = append(d.AdditionalHostMounts, config.AdditionalHostMount{
				Source: g.Source, Target: g.Target, ReadOnly: g.ReadOnly, DeviceAllow: slices.Clone(g.DeviceAllow),
			})
		case isolation.PurposeNvidiaDevice:
			d.NvidiaDeviceTargets = append(d.NvidiaDeviceTargets, NvidiaDeviceTargets([]string{g.Source})...)
		case isolation.PurposeNvidiaLibDir:
			d.NvidiaLibDirMounts = append(d.NvidiaLibDirMounts, libDirMount(len(d.NvidiaLibDirMounts), g))
		case isolation.PurposeNvidiaI386LibDir:
			d.NvidiaI386LibDirMounts = append(d.NvidiaI386LibDirMounts, libDirMount(len(d.NvidiaI386LibDirMounts), g))
		case isolation.PurposeNvidiaHelperBin:
			d.NvidiaBinDir = g.Source
		case isolation.PurposeAMDDevice:
			d.AMDGPUDevicePaths = append(d.AMDGPUDevicePaths, g.Source)
		case isolation.PurposeAMDSysFS:
			d.AMDSysFSPaths = append(d.AMDSysFSPaths, g.Source)
		}
	}

	d.MountDeviceAllow = MountDeviceAllowEntries(d.AdditionalHostMounts)
}

// MountDeviceAllowEntries returns the de-duplicated, sorted DeviceAllow entries
// declared by mounts.
func MountDeviceAllowEntries(mounts []config.AdditionalHostMount) []string {
	var entries []string

	for _, m := range mounts {
		entries = append(entries, m.DeviceAllow...)
	}

	slices.Sort(entries)

	return slices.Compact(entries)
}

func libDirMount(index int, g isolation.Grant) isolation.LibDirMount {
	return isolation.LibDirMount{Index: index, HostDir: g.Source, ContainerDir: g.Target}
}

// NvidiaDeviceTargets returns the bind and DeviceAllow pair for each NVIDIA
// device path.
func NvidiaDeviceTargets(paths []string) []DeviceTarget {
	targets := make([]DeviceTarget, 0, len(paths))

	for _, path := range paths {
		target := DeviceTarget{
			Bind: Bind{Source: path},
			Allow: DeviceAllow{
				Specifier: path,
				Access:    "rwm",
			},
		}

		// The caps paths are directories that must be bind-mounted so dynamic
		// NVIDIA capability and channel device nodes are visible inside nspawn.
		// DeviceAllow operates on character device nodes and classes, so use
		// the kernel device-class names to grant current and future nodes access.
		switch path {
		case "/dev/nvidia-caps":
			target.Allow.Specifier = "char-nvidia-caps"
		case "/dev/nvidia-caps-imex-channels":
			target.Allow.Specifier = "char-nvidia-caps-imex-channels"
		}

		targets = append(targets, target)
	}

	return targets
}

// Rendered is the host configuration the backend writes for one machine.
type Rendered struct {
	// Settings is the .nspawn file.
	Settings []byte
	// ServiceOverride is the systemd-nspawn@<machine>.service drop-in.
	ServiceOverride []byte
	// ConfigRegenerationUnit is the unit that regenerates machine config.
	ConfigRegenerationUnit []byte
}

// Render executes the three templates for data.
func Render(data TemplateData) (Rendered, error) {
	var r Rendered

	for _, item := range []struct {
		name string
		dst  *[]byte
		what string
	}{
		{"nspawn.conf", &r.Settings, "nspawn config"},
		{"service-override.conf", &r.ServiceOverride, "service override"},
		{"config-regeneration.service", &r.ConfigRegenerationUnit, "config regeneration unit"},
	} {
		var buf bytes.Buffer
		if err := Templates.ExecuteTemplate(&buf, item.name, data); err != nil {
			return Rendered{}, fmt.Errorf("render %s template: %w", item.what, err)
		}

		*item.dst = buf.Bytes()
	}

	return r, nil
}
