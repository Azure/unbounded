// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestLoadAppliedConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "applied-config.json")
	checksumPath := configPath + ".sha256"
	want := provision.AgentConfig{
		MachineName: "machine-1",
		NodeName:    "node-1",
	}

	data, err := json.Marshal(&want)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, data, 0o600))
	require.NoError(t, os.WriteFile(checksumPath, []byte(goalstates.ComputeChecksum(data)+"\n"), 0o600))

	got, ok, err := loadAppliedConfig(testLogger(), configPath, checksumPath)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want.MachineName, got.MachineName)
	require.Equal(t, want.NodeName, got.NodeName)
}

func TestLoadAppliedConfigMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	got, ok, err := loadAppliedConfig(
		testLogger(),
		filepath.Join(dir, "missing.json"),
		filepath.Join(dir, "missing.json.sha256"),
	)

	require.NoError(t, err)
	require.False(t, ok)
	require.Nil(t, got)
}

func TestLoadAppliedConfigChecksumMismatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "applied-config.json")
	checksumPath := configPath + ".sha256"
	require.NoError(t, os.WriteFile(configPath, []byte(`{"MachineName":"machine-1"}`), 0o600))
	require.NoError(t, os.WriteFile(checksumPath, []byte(goalstates.ComputeChecksum([]byte("different"))), 0o600))

	got, ok, err := loadAppliedConfig(testLogger(), configPath, checksumPath)
	require.ErrorIs(t, err, goalstates.ErrChecksumMismatch)
	require.False(t, ok)
	require.Nil(t, got)
}
