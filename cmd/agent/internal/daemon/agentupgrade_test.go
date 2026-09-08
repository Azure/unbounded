// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestParseAgentUpgradeRequest(t *testing.T) {
	t.Parallel()

	request, err := parseAgentUpgradeRequest(map[string]string{
		agentUpgradeDownloadURLParameter: " https://example.com/agent.tar.gz ",
		agentUpgradeSHA256Parameter:      " aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ",
	})
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/agent.tar.gz", request.downloadURL)
	assert.Equal(t, testAgentUpgradeSHA256, request.sha256)

	_, err = parseAgentUpgradeRequest(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), agentUpgradeDownloadURLParameter)

	request, err = parseAgentUpgradeRequest(map[string]string{
		agentUpgradeDownloadURLParameter: "http://example.com/agent.tar.gz",
	})
	require.NoError(t, err)
	assert.Empty(t, request.sha256)
}

func TestAgentUpgradeSignalOperatorRecordFailure(t *testing.T) {
	t.Parallel()

	signalPath := filepath.Join(t.TempDir(), "agent-upgrade-signal")
	signals := newAgentUpgradeSignalOperatorForPath(signalPath)

	require.NoError(t, signals.RecordPending("op-1", 7))
	require.NoError(t, signals.RecordFailure("rolled back"))

	data, err := os.ReadFile(signalPath)
	require.NoError(t, err)
	assert.JSONEq(t, `{"operationName":"op-1","observedMachineGeneration":7,"failureMessage":"rolled back"}`, string(data))
}

func TestAgentUpgradeSignalOperatorReadRejectsNonJSON(t *testing.T) {
	t.Parallel()

	signalPath := filepath.Join(t.TempDir(), "agent-upgrade-signal")
	signals := newAgentUpgradeSignalOperatorForPath(signalPath)
	require.NoError(t, os.WriteFile(signalPath, []byte("op-1\n"), 0o600))

	_, err := signals.Read()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode AgentUpgrade signal")
}

func TestAgentUpgradePathsInitialDaemonBinaryTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	paths := goalstates.AgentUpgradePaths{
		BinaryPath: filepath.Join(dir, "unbounded-agent"),
		BluePath:   filepath.Join(dir, "unbounded-agent-blue"),
		GreenPath:  filepath.Join(dir, "unbounded-agent-green"),
	}
	require.NoError(t, os.WriteFile(paths.BinaryPath, []byte("legacy"), 0o755))
	require.NoError(t, os.WriteFile(paths.BluePath, []byte("blue"), 0o755))
	require.NoError(t, os.WriteFile(paths.GreenPath, []byte("green"), 0o755))

	target, err := paths.InitialDaemonBinaryTarget()
	require.NoError(t, err)
	assert.Equal(t, paths.BluePath, target)

	require.NoError(t, os.Chmod(paths.BluePath, 0o644))
	target, err = paths.InitialDaemonBinaryTarget()
	require.NoError(t, err)
	assert.Equal(t, paths.GreenPath, target)

	require.NoError(t, os.Chmod(paths.GreenPath, 0o644))
	target, err = paths.InitialDaemonBinaryTarget()
	require.NoError(t, err)
	assert.Equal(t, paths.BinaryPath, target)

	require.NoError(t, os.Chmod(paths.BinaryPath, 0o644))
	_, err = paths.InitialDaemonBinaryTarget()
	require.Error(t, err)
}
