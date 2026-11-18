// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package containerops

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTeardownPreservesHostCopiesOfSlotPrivateData(t *testing.T) {
	f, td, _ := teardownFixture(t)
	paths := []string{
		"/etc/cni/net.d/10-host.conf",
		"/var/lib/cni/networks/host-network/10.0.0.2",
		"/var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db",
	}

	for _, path := range paths {
		hostPath := f.hostPath(path)
		require.NoError(t, os.MkdirAll(filepath.Dir(hostPath), 0o755))
		require.NoError(t, os.WriteFile(hostPath, []byte("host-owned state"), 0o600))

		for _, slot := range []string{"kube1", "kube2"} {
			root, err := f.node(slot, false).Backend().NodeRoot(slot)
			require.NoError(t, err)

			slotPath := root.Path(path)
			require.Equal(t, filepath.Join(f.rootfs(slot), path), slotPath)
			require.NotEqual(t, hostPath, slotPath)
			require.NoError(t, os.MkdirAll(filepath.Dir(slotPath), 0o755))
			require.NoError(t, os.WriteFile(slotPath, []byte(slot+" private state"), 0o600))
		}
	}

	require.NoError(t, td.Stop().Do(t.Context()))
	require.NoError(t, td.RemoveConfig().Do(t.Context()))

	for range 2 {
		require.NoError(t, td.Destroy().Do(t.Context()))

		for _, path := range paths {
			data, err := os.ReadFile(f.hostPath(path))
			require.NoError(t, err)
			require.Equal(t, "host-owned state", string(data))

			for _, slot := range []string{"kube1", "kube2"} {
				require.NoFileExists(t, filepath.Join(f.rootfs(slot), path))
			}
		}

		require.NoDirExists(t, f.rootfs("kube1"))
		require.NoDirExists(t, f.rootfs("kube2"))
	}
}
