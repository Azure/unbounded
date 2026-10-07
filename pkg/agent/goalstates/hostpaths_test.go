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

	for root, paths := range map[string]HostPaths{
		"/opt/unbounded": hostPathsUnder("/opt/unbounded"),
		"/usr/local":     LegacyHostPaths(),
	} {
		assert.Equal(t, HostPaths{
			Root:                  root,
			BinDir:                root + "/bin",
			NSpawnLifecycleBinary: root + "/bin/unbounded-agent-nspawn-lifecycle",
			DaemonRecoveryScript:  root + "/bin/unbounded-agent-daemon-recovery.sh",
			LocalDNSNetworkHelper: root + "/libexec/unbounded-localdns-network",
		}, paths)
	}
}
