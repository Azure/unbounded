// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"path/filepath"

	"github.com/Azure/unbounded/internal/hostroot"
)

// HostPaths is the host-side layout of the agent's own files under a host
// root; see the hostroot package.
//
// These are paths on the host. Files inside the nspawn machine are always
// resolved relative to the machine directory.
type HostPaths struct {
	// Root is the host root these paths are under.
	Root string
	// BinDir is <Root>/bin.
	BinDir string
	// NSpawnLifecycleBinary is the rollback-stable helper invoked by the
	// generated nspawn hook units.
	NSpawnLifecycleBinary string
	// DaemonRecoveryScript is executed by the daemon recovery unit.
	DaemonRecoveryScript string
	// LocalDNSNetworkHelper backs unbounded-localdns-network.service.
	LocalDNSNetworkHelper string
}

// ResolveHostPaths returns the agent's host-side layout on this host.
func ResolveHostPaths() HostPaths {
	return hostPathsUnder(hostroot.Resolve())
}

// PlannedHostPaths returns the layout ResolveHostPaths will return once the
// host root is migrated, without migrating it. It is for code that must not
// change the host, such as preflight.
func PlannedHostPaths() HostPaths {
	return hostPathsUnder(hostroot.Planned(hostroot.Markers()...))
}

// LegacyHostPaths returns the layout under hostroot.LegacyPath, where releases
// that predate the host root installed it.
func LegacyHostPaths() HostPaths {
	return hostPathsUnder(hostroot.LegacyPath)
}

func hostPathsUnder(root string) HostPaths {
	binDir := filepath.Join(root, "bin")

	return HostPaths{
		Root:                  root,
		BinDir:                binDir,
		NSpawnLifecycleBinary: filepath.Join(binDir, hostroot.NSpawnLifecycleName),
		DaemonRecoveryScript:  filepath.Join(binDir, hostroot.RecoveryScriptName),
		LocalDNSNetworkHelper: filepath.Join(root, "libexec", hostroot.LocalDNSNetworkHelperName),
	}
}
