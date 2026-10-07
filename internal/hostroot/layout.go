// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostroot

import "path/filepath"

// Base names of the unbounded agent's own host-side files.
const (
	BinaryName                = "unbounded-agent"
	BinaryBlueName            = "unbounded-agent-blue"
	BinaryGreenName           = "unbounded-agent-green"
	BinaryCurrentName         = "unbounded-agent-current"
	BinaryLastGoodName        = "unbounded-agent-last-good"
	NSpawnLifecycleName       = "unbounded-agent-nspawn-lifecycle"
	RecoveryScriptName        = "unbounded-agent-daemon-recovery.sh"
	LocalDNSNetworkHelperName = "unbounded-localdns-network"

	// The cloud-init variant and netboot write the install script under the
	// legacy root on every host, and older versions left the uninstall script
	// there, so teardown removes both from there.
	installScriptName   = "unbounded-agent-install.sh"
	uninstallScriptName = "unbounded-agent-uninstall.sh"

	// SeedFile is where install scripts place the agent binary, relative to
	// LegacyPath, for agents up to v0.8.0. Current agents do not use it, and
	// remove it from a host installed under Path.
	SeedFile = "bin/" + BinaryName
)

// Markers returns the files, relative to the root, whose presence under
// LegacyPath identifies an unbounded-agent installation from before Path; pass
// them to Migrate and Planned.
//
// They are the daemon's blue-green binary layout only, which every released
// agent since v0.1.4 creates when it installs. The plain binary is left out:
// install scripts seed it under the legacy root on fresh hosts too, so on its
// own it is not an installation. Neither are the installer scripts, which
// cloud-init writes on fresh hosts, or a helper left behind by an older reset.
func Markers() []string {
	return []string{
		filepath.Join("bin", BinaryBlueName),
		filepath.Join("bin", BinaryGreenName),
		filepath.Join("bin", BinaryCurrentName),
		filepath.Join("bin", BinaryLastGoodName),
	}
}

// Layout returns every file of the agent's own host-side layout, relative to
// the root. Moving a host installed by an older agent copies these from
// LegacyPath.
func Layout() []string {
	return append(
		Markers(),
		filepath.Join("bin", BinaryName),
		filepath.Join("bin", NSpawnLifecycleName),
		filepath.Join("bin", RecoveryScriptName),
		filepath.Join("libexec", LocalDNSNetworkHelperName),
	)
}

// LayoutUnder returns Layout under root.
func LayoutUnder(root string) []string {
	files := Layout()
	for i, rel := range files {
		files[i] = filepath.Join(root, rel)
	}

	return files
}

// OwnedFiles returns every host file outside the config directory that
// teardown removes: the agent's files under Path and under LegacyPath, and the
// installer scripts under LegacyPath. On a linked host the first set reaches
// the second through the link, and the second finds nothing.
//
// The legacy layout is swept on every host so teardown does not depend on the
// host having been migrated. Reset is what an operator runs when Migrate
// refuses, and it has to leave the host clean then too.
//
// Environment overrides are deliberately not applied. These are the paths the
// agent installs to as a matter of layout, and teardown needs to find them on a
// host whose environment no longer resembles the one that provisioned it.
func OwnedFiles() []string {
	return append(
		append(LayoutUnder(Path), LayoutUnder(LegacyPath)...),
		filepath.Join(LegacyPath, "bin", installScriptName),
		filepath.Join(LegacyPath, "bin", uninstallScriptName),
	)
}
