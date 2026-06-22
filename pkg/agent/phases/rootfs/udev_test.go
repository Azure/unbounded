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

func TestDisableUdevMasksContainerUnits(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	task := DisableUdev(&goalstates.RootFS{MachineDir: machineDir})

	require.NoError(t, task.Do(context.Background()))

	for _, unit := range []string{
		"systemd-udevd.service",
		"systemd-udevd-control.socket",
		"systemd-udevd-kernel.socket",
	} {
		link := filepath.Join(machineDir, "etc/systemd/system", unit)
		target, err := os.Readlink(link)
		require.NoError(t, err)
		require.Equal(t, "/dev/null", target)
	}
}
