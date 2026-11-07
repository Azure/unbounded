// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"path/filepath"
	"slices"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// Grants returns the backend-neutral resource requests for the discovered
// NVIDIA state: device nodes, driver library directories and the helper binary
// directory. Each backend decides how to apply them.
func (n NvidiaHost) Grants() []isolation.Grant {
	var grants []isolation.Grant

	for _, path := range n.GPUDevicePaths {
		grants = append(grants, isolation.Grant{Kind: isolation.GrantDevice, Purpose: isolation.PurposeNvidiaDevice, Source: path})
	}

	for _, m := range n.LibDirMounts {
		grants = append(grants, libDirGrant(isolation.PurposeNvidiaLibDir, m))
	}

	for _, m := range n.I386LibDirMounts {
		grants = append(grants, libDirGrant(isolation.PurposeNvidiaI386LibDir, m))
	}

	if dir := n.HostBinDir(); dir != "" {
		grants = append(grants, isolation.Grant{
			Kind:     isolation.GrantMount,
			Purpose:  isolation.PurposeNvidiaHelperBin,
			Source:   dir,
			Target:   NvidiaHostBinDir,
			ReadOnly: true,
		})
	}

	return grants
}

func libDirGrant(purpose isolation.GrantPurpose, m isolation.LibDirMount) isolation.Grant {
	return isolation.Grant{
		Kind:     isolation.GrantMount,
		Purpose:  purpose,
		Source:   m.HostDir,
		Target:   m.ContainerDir,
		ReadOnly: true,
	}
}

// HostBinDir returns the host directory that holds the NVIDIA helper binaries,
// or the empty string when none was discovered.
func (n NvidiaHost) HostBinDir() string {
	for _, path := range []string{n.NvidiaSMIPath, n.NvidiaIMEXPath, n.NvidiaIMEXCtlPath} {
		if path != "" {
			return filepath.Dir(path)
		}
	}

	return ""
}

// Grants returns the backend-neutral resource requests for the discovered AMD
// state, skipping any device path in exclude (devices NVIDIA already owns).
func (a AMDHost) Grants(exclude []string) []isolation.Grant {
	var grants []isolation.Grant

	for _, path := range PathsExcluding(a.GPUDevicePaths, exclude) {
		grants = append(grants, isolation.Grant{Kind: isolation.GrantDevice, Purpose: isolation.PurposeAMDDevice, Source: path})
	}

	for _, path := range a.SysFSPaths {
		grants = append(grants, isolation.Grant{Kind: isolation.GrantMount, Purpose: isolation.PurposeAMDSysFS, Source: path, ReadOnly: true})
	}

	return grants
}

// Grants returns the backend-neutral resource requests for the host devices:
// one device grant per node path, then one per device group specifier, then
// one mount grant per device directory.
func (d HostDevices) Grants() []isolation.Grant {
	var grants []isolation.Grant

	for _, path := range d.Paths() {
		grants = append(grants, isolation.Grant{Kind: isolation.GrantDevice, Purpose: isolation.PurposeHostDevice, Source: path})
	}

	for _, class := range d.DeviceGroupSpecifiers() {
		grants = append(grants, isolation.Grant{Kind: isolation.GrantDevice, Purpose: isolation.PurposeHostDevice, DeviceClass: class})
	}

	return append(grants, AdditionalHostMountGrants(d.DeviceDirectories)...)
}

// AdditionalHostMountGrants converts configured host mounts to grants.
func AdditionalHostMountGrants(mounts []config.AdditionalHostMount) []isolation.Grant {
	grants := make([]isolation.Grant, 0, len(mounts))

	for _, m := range mounts {
		grants = append(grants, isolation.Grant{
			Kind:        isolation.GrantMount,
			Purpose:     isolation.PurposeHostMount,
			Source:      m.Source,
			Target:      m.Target,
			ReadOnly:    m.ReadOnly,
			DeviceAllow: slices.Clone(m.DeviceAllow),
		})
	}

	return grants
}

// Grants returns every device, mount and library request the machine needs, in
// the order host devices (with device directories), additional mounts, NVIDIA,
// AMD. AMD devices that
// NVIDIA already owns are not requested twice.
func (r *RootFS) Grants() []isolation.Grant {
	grants := r.HostDevices.Grants()
	grants = append(grants, AdditionalHostMountGrants(r.AdditionalHostMounts)...)
	grants = append(grants, r.Nvidia.Grants()...)

	return append(grants, r.AMD.Grants(r.Nvidia.GPUDevicePaths)...)
}

// PathsExcluding returns paths without the entries in excluded, preserving
// order. It returns paths unchanged when either input is empty.
func PathsExcluding(paths, excluded []string) []string {
	if len(paths) == 0 || len(excluded) == 0 {
		return paths
	}

	seen := make(map[string]bool, len(excluded))
	for _, p := range excluded {
		seen[p] = true
	}

	var out []string

	for _, p := range paths {
		if !seen[p] {
			out = append(out, p)
		}
	}

	return out
}
