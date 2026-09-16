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

func TestRenderDaemonAsset(t *testing.T) {
	t.Parallel()

	renderedBytes, err := renderDaemonAsset("daemon-service", daemonServiceContent)
	require.NoError(t, err)

	rendered := string(renderedBytes)

	require.NotContains(t, rendered, "{{")
	assert.Contains(t, rendered, goalstates.DaemonRecoveryUnit)
	assert.Contains(t, rendered, goalstates.DaemonBinaryCurrentPath)

	renderedRecoveryBytes, err := renderDaemonAsset("daemon-recovery-script", daemonRecoveryScriptContent)
	require.NoError(t, err)

	renderedRecovery := string(renderedRecoveryBytes)
	require.NotContains(t, renderedRecovery, "{{")
	assert.Contains(t, renderedRecovery, goalstates.DaemonBinaryLastGoodPath)
	assert.Contains(t, renderedRecovery, goalstates.DaemonUnit)
	assert.Contains(t, renderedRecovery, goalstates.DaemonAgentUpgradeSignalPath)
	assert.Contains(t, renderedRecovery, "record-agent-upgrade-failure-signal")
}

func TestInstallBinaryStreamsAndReplacesAtomically(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source, target := filepath.Join(dir, "source"), filepath.Join(dir, "bin", "target")
	require.NoError(t, os.WriteFile(source, []byte("candidate"), 0o600))
	require.NoError(t, installBinary(source, target))
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "candidate", string(data))

	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	require.Error(t, installBinary(filepath.Join(dir, "missing"), target))
	data, err = os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "candidate", string(data))
}
