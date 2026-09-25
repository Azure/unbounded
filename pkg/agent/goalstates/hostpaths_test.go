// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHostPathsUnder(t *testing.T) {
	t.Parallel()

	paths := hostPathsUnder("/opt/unbounded")

	assert.Equal(t, HostPaths{
		Root:                  "/opt/unbounded",
		BinDir:                "/opt/unbounded/bin",
		LibexecDir:            "/opt/unbounded/libexec",
		NSpawnLifecycleBinary: "/opt/unbounded/bin/unbounded-agent-nspawn-lifecycle",
		DaemonRecoveryScript:  "/opt/unbounded/bin/unbounded-agent-daemon-recovery.sh",
		LocalDNSNetworkHelper: "/opt/unbounded/libexec/unbounded-localdns-network",
	}, paths)
}

// TestLegacyHostPathsMatchTheReleasedLayout pins the layout under the legacy
// root to the paths released agents used. A migrated host keeps those files,
// and the units and recovery script an older agent wrote name them.
func TestLegacyHostPathsMatchTheReleasedLayout(t *testing.T) {
	t.Parallel()

	legacy := LegacyHostPaths()

	assert.Equal(t, NSpawnLifecycleBinaryPath, legacy.NSpawnLifecycleBinary) //nolint:staticcheck // The released value is what is being pinned.
	assert.Equal(t, DaemonRecoveryScriptPath, legacy.DaemonRecoveryScript)   //nolint:staticcheck // The released value is what is being pinned.
	assert.Equal(t, "/usr/local/libexec/unbounded-localdns-network", legacy.LocalDNSNetworkHelper)

	for _, pinned := range []string{
		DaemonBinaryPath,         //nolint:staticcheck // The released value is what is being pinned.
		DaemonBinaryBluePath,     //nolint:staticcheck // The released value is what is being pinned.
		DaemonBinaryGreenPath,    //nolint:staticcheck // The released value is what is being pinned.
		DaemonBinaryCurrentPath,  //nolint:staticcheck // The released value is what is being pinned.
		DaemonBinaryLastGoodPath, //nolint:staticcheck // The released value is what is being pinned.
	} {
		rel, err := filepath.Rel("/usr/local", pinned)
		assert.NoError(t, err)
		assert.Contains(t, HostRootMarkers(), rel, "a released binary path must identify a legacy installation")
	}
}

// TestHostRootMarkersAreTheBinaryLayout keeps files that a fresh host also has
// under the legacy root out of the markers. Counting one would link a fresh
// host's root to the legacy root and install the agent there.
func TestHostRootMarkersAreTheBinaryLayout(t *testing.T) {
	t.Parallel()

	markers := HostRootMarkers()

	assert.Len(t, markers, 5)
	assert.NotContains(t, markers, filepath.Join("bin", agentInstallScriptName), "cloud-init writes it on fresh hosts")
	assert.NotContains(t, markers, filepath.Join("bin", nspawnLifecycleName), "an older reset can leave it behind")
}

func TestOwnedHostFilesUnder(t *testing.T) {
	t.Parallel()

	layout := func(root string) []string {
		return []string{
			root + "/bin/unbounded-agent",
			root + "/bin/unbounded-agent-blue",
			root + "/bin/unbounded-agent-green",
			root + "/bin/unbounded-agent-current",
			root + "/bin/unbounded-agent-last-good",
			root + "/bin/unbounded-agent-nspawn-lifecycle",
			root + "/bin/unbounded-agent-daemon-recovery.sh",
			root + "/libexec/unbounded-localdns-network",
		}
	}
	// Written under the legacy root by cloud-init and netboot on every host.
	scripts := []string{
		"/usr/local/bin/unbounded-agent-install.sh",
		"/usr/local/bin/unbounded-agent-uninstall.sh",
	}

	t.Run("new root sweeps the legacy layout too", func(t *testing.T) {
		t.Parallel()

		// Reset does not migrate, so on a host the migration refused the
		// installation is under the legacy root while the root is a real
		// directory.
		want := append(append(layout("/opt/unbounded"), layout("/usr/local")...), scripts...)
		assert.ElementsMatch(t, want, ownedHostFilesUnder("/opt/unbounded", "/usr/local"))
	})

	t.Run("migrated root is swept once", func(t *testing.T) {
		t.Parallel()

		want := append(layout("/usr/local"), scripts...)
		assert.ElementsMatch(t, want, ownedHostFilesUnder("/usr/local", "/usr/local"))
	})
}
