// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

// Base names of the agent's own host-side files, joined with the resolved host
// root.
const (
	daemonBinaryName          = "unbounded-agent"
	daemonBinaryBlueName      = "unbounded-agent-blue"
	daemonBinaryGreenName     = "unbounded-agent-green"
	daemonBinaryCurrentName   = "unbounded-agent-current"
	daemonBinaryLastGoodName  = "unbounded-agent-last-good"
	nspawnLifecycleName       = "unbounded-agent-nspawn-lifecycle"
	daemonRecoveryScriptName  = "unbounded-agent-daemon-recovery.sh"
	localDNSNetworkHelperName = "unbounded-localdns-network"
)

// HostPaths is the host-side layout of the agent's own files under the
// resolved host root; see the hostroot package.
//
// These are paths on the host. Files inside the nspawn machine are always
// resolved relative to the machine directory.
type HostPaths struct {
	// Root is the resolved host root.
	Root string
	// BinDir is <Root>/bin.
	BinDir string
	// LibexecDir is <Root>/libexec.
	LibexecDir string

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
	return hostPathsUnder(hostroot.Planned(HostRootMarkers()...))
}

// LegacyHostPaths returns the layout under hostroot.LegacyPath, for the few
// checks that have to find an installation that has not been migrated yet.
func LegacyHostPaths() HostPaths {
	return hostPathsUnder(hostroot.LegacyPath)
}

func hostPathsUnder(root string) HostPaths {
	binDir := filepath.Join(root, "bin")
	libexecDir := filepath.Join(root, "libexec")

	return HostPaths{
		Root:                  root,
		BinDir:                binDir,
		LibexecDir:            libexecDir,
		NSpawnLifecycleBinary: filepath.Join(binDir, nspawnLifecycleName),
		DaemonRecoveryScript:  filepath.Join(binDir, daemonRecoveryScriptName),
		LocalDNSNetworkHelper: filepath.Join(libexecDir, localDNSNetworkHelperName),
	}
}

// HostRootMarkers returns the files, relative to the host root, whose presence
// under hostroot.LegacyPath identifies an unbounded-agent installation from
// before the host root; pass them to hostroot.Migrate.
//
// They are the daemon's binary layout only. The installer scripts are written
// under the legacy root on fresh hosts too, and a helper left behind by an
// older reset is not an installation.
func HostRootMarkers() []string {
	return []string{
		filepath.Join("bin", daemonBinaryName),
		filepath.Join("bin", daemonBinaryBlueName),
		filepath.Join("bin", daemonBinaryGreenName),
		filepath.Join("bin", daemonBinaryCurrentName),
		filepath.Join("bin", daemonBinaryLastGoodName),
	}
}

// Base names of the installer scripts. The cloud-init variant and netboot write
// the install script under the legacy root on every host, and older versions
// left the uninstall script there, so teardown removes both from there.
const (
	agentInstallScriptName   = "unbounded-agent-install.sh"
	agentUninstallScriptName = "unbounded-agent-uninstall.sh"
)

// OwnedHostFiles returns every host file outside the config directory that
// teardown removes: the agent's files under the host root and, when that is not
// the legacy root, under the legacy root as well, and the installer scripts
// under the legacy root.
//
// The legacy layout is swept on every host so teardown does not depend on the
// host root having been migrated. Reset is what an operator runs when the
// migration refuses, and it has to leave the host clean then too.
//
// The existing-deployment preflight deliberately checks only a subset: the
// daemon units and the recovery script. The install script and Ignition both
// put the agent binary in place before preflight runs, so a preflight that
// checked this whole list would refuse every fresh host.
//
// Environment overrides are deliberately not applied. These are the paths the
// agent installs to as a matter of layout, and teardown needs to find them on a
// host whose environment no longer resembles the one that provisioned it.
func OwnedHostFiles() []string {
	return ownedHostFilesUnder(hostroot.Resolve(), hostroot.LegacyPath)
}

func ownedHostFilesUnder(root, legacy string) []string {
	files := layoutFilesUnder(root)
	if root != legacy {
		files = append(files, layoutFilesUnder(legacy)...)
	}

	return append(files,
		filepath.Join(legacy, "bin", agentInstallScriptName),
		filepath.Join(legacy, "bin", agentUninstallScriptName),
	)
}

func layoutFilesUnder(root string) []string {
	paths := hostPathsUnder(root)

	return []string{
		filepath.Join(paths.BinDir, daemonBinaryName),
		filepath.Join(paths.BinDir, daemonBinaryBlueName),
		filepath.Join(paths.BinDir, daemonBinaryGreenName),
		filepath.Join(paths.BinDir, daemonBinaryCurrentName),
		filepath.Join(paths.BinDir, daemonBinaryLastGoodName),
		paths.NSpawnLifecycleBinary,
		paths.DaemonRecoveryScript,
		paths.LocalDNSNetworkHelper,
	}
}
