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
	// BinDir is <root>/bin.
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
	return hostPathsUnder(hostroot.Planned(HostRootMarkers()...))
}

func hostPathsUnder(root string) HostPaths {
	binDir := filepath.Join(root, "bin")

	return HostPaths{
		BinDir:                binDir,
		NSpawnLifecycleBinary: filepath.Join(binDir, nspawnLifecycleName),
		DaemonRecoveryScript:  filepath.Join(binDir, daemonRecoveryScriptName),
		LocalDNSNetworkHelper: filepath.Join(root, "libexec", localDNSNetworkHelperName),
	}
}

// HostRootMarkers returns the files, relative to the host root, whose presence
// under hostroot.LegacyPath identifies an unbounded-agent installation from
// before the host root; pass them to hostroot.Migrate.
//
// They are the daemon's blue-green binary layout only, which every released
// agent since v0.1.4 creates when it installs. The plain binary is left out:
// install scripts seed it under the legacy root on fresh hosts too, for agents
// up to v0.8.0, so on its own it is not an installation. Neither are the
// installer scripts, which cloud-init writes on fresh hosts, or a helper left
// behind by an older reset.
func HostRootMarkers() []string {
	return []string{
		filepath.Join("bin", daemonBinaryBlueName),
		filepath.Join("bin", daemonBinaryGreenName),
		filepath.Join("bin", daemonBinaryCurrentName),
		filepath.Join("bin", daemonBinaryLastGoodName),
	}
}

// LegacySeedFile returns where install scripts seed the agent binary for
// agents up to v0.8.0, relative to hostroot.LegacyPath. Current agents do not
// use it, and remove it from a host installed under the host root.
func LegacySeedFile() string {
	return filepath.Join("bin", daemonBinaryName)
}

// HostLayout returns every file of the agent's own host-side layout, relative
// to the host root. Moving a host installed by an older agent copies these
// from the legacy root.
func HostLayout() []string {
	return append(
		HostRootMarkers(),
		filepath.Join("bin", daemonBinaryName),
		filepath.Join("bin", nspawnLifecycleName),
		filepath.Join("bin", daemonRecoveryScriptName),
		filepath.Join("libexec", localDNSNetworkHelperName),
	)
}

// LayoutUnder returns HostLayout under root.
func LayoutUnder(root string) []string {
	files := HostLayout()
	for i, rel := range files {
		files[i] = filepath.Join(root, rel)
	}

	return files
}

// Base names of the installer scripts. The cloud-init variant and netboot write
// the install script under the legacy root on every host, and older versions
// left the uninstall script there, so teardown removes both from there.
const (
	agentInstallScriptName   = "unbounded-agent-install.sh"
	agentUninstallScriptName = "unbounded-agent-uninstall.sh"
)

// OwnedHostFiles returns every host file outside the config directory that
// teardown removes: the agent's files under the host root and under the legacy
// root, and the installer scripts under the legacy root. On a linked host the
// first set reaches the second through the link, and the second finds nothing.
//
// The legacy layout is swept on every host so teardown does not depend on the
// host root having been migrated. Reset is what an operator runs when the
// migration refuses, and it has to leave the host clean then too.
//
// Environment overrides are deliberately not applied. These are the paths the
// agent installs to as a matter of layout, and teardown needs to find them on a
// host whose environment no longer resembles the one that provisioned it.
func OwnedHostFiles() []string {
	return append(
		append(LayoutUnder(hostroot.Path), LayoutUnder(hostroot.LegacyPath)...),
		filepath.Join(hostroot.LegacyPath, "bin", agentInstallScriptName),
		filepath.Join(hostroot.LegacyPath, "bin", agentUninstallScriptName),
	)
}
