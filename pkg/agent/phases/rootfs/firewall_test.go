// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestDisableImageFirewallMasksFirewallUnits(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	unitDir := filepath.Join(machineDir, "etc/systemd/system")
	require.NoError(t, os.MkdirAll(unitDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(unitDir, "iptables.service"), []byte("old"), 0o644))

	task := DisableImageFirewall(&goalstates.RootFS{MachineDir: machineDir})
	require.Equal(t, "disable-image-firewall", task.Name())
	require.NoError(t, task.Do(context.Background()))

	for _, unit := range rootfsFirewallUnits {
		target, err := os.Readlink(filepath.Join(unitDir, unit))
		require.NoError(t, err)
		require.Equal(t, "/dev/null", target)
	}
}
