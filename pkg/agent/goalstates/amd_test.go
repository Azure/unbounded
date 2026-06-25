// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiscoverAMDDevices(t *testing.T) {
	t.Parallel()

	devDir := t.TempDir()
	driDir := filepath.Join(devDir, "dri")
	require.NoError(t, os.Mkdir(driDir, 0o755))

	for _, path := range []string{
		filepath.Join(devDir, "kfd"),
		filepath.Join(driDir, "card0"),
		filepath.Join(driDir, "renderD128"),
		filepath.Join(driDir, "by-path"),
	} {
		f, err := os.Create(path)
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}

	got := discoverAMDDevicesAt(filepath.Join(devDir, "kfd"), driDir)

	want := []string{
		filepath.Join(devDir, "dri", "card0"),
		filepath.Join(devDir, "dri", "renderD128"),
		filepath.Join(devDir, "kfd"),
	}
	require.Equal(t, want, got)
}

func TestDiscoverAMDDevices_NoKFD(t *testing.T) {
	t.Parallel()

	devDir := t.TempDir()
	driDir := filepath.Join(devDir, "dri")
	require.NoError(t, os.Mkdir(driDir, 0o755))

	f, err := os.Create(filepath.Join(driDir, "renderD128"))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	require.Nil(t, discoverAMDDevicesAt(filepath.Join(devDir, "kfd"), driDir))
}

func TestDiscoverAMDSysFSPaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	existing := filepath.Join(dir, "module", "amdgpu")
	missing := filepath.Join(dir, "class", "kfd")
	require.NoError(t, os.MkdirAll(existing, 0o755))

	require.Equal(t, []string{existing}, discoverAMDSysFSPaths([]string{existing, missing}))
}
