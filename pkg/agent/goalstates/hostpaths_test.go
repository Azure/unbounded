// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestHostPathsUnder pins the layout, including under the legacy root, where
// the units and recovery script a released agent wrote name these paths.
func TestHostPathsUnder(t *testing.T) {
	t.Parallel()

	for _, root := range []string{"/opt/unbounded", "/usr/local"} {
		assert.Equal(t, HostPaths{
			BinDir:                root + "/bin",
			NSpawnLifecycleBinary: root + "/bin/unbounded-agent-nspawn-lifecycle",
			DaemonRecoveryScript:  root + "/bin/unbounded-agent-daemon-recovery.sh",
			LocalDNSNetworkHelper: root + "/libexec/unbounded-localdns-network",
		}, hostPathsUnder(root))
	}
}

// TestHostRootMarkersAreTheBinaryLayout keeps files that a fresh host also has
// under the legacy root out of the markers: the plain binary install scripts
// seed, the install script cloud-init writes, and a helper an older reset can
// leave behind. Counting one would link a fresh host's root to the legacy root
// and install the agent there.
func TestHostRootMarkersAreTheBinaryLayout(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{
		"bin/unbounded-agent-blue",
		"bin/unbounded-agent-green",
		"bin/unbounded-agent-current",
		"bin/unbounded-agent-last-good",
	}, HostRootMarkers())
}

func TestOwnedHostFiles(t *testing.T) {
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
	// Reset does not migrate, so on a host the migration refused the
	// installation is under the legacy root while the root is a real
	// directory. The installer scripts are written under the legacy root by
	// cloud-init and netboot on every host.
	want := append(append(layout("/opt/unbounded"), layout("/usr/local")...),
		"/usr/local/bin/unbounded-agent-install.sh",
		"/usr/local/bin/unbounded-agent-uninstall.sh",
	)
	assert.ElementsMatch(t, want, OwnedHostFiles())
}
