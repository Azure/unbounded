// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestRenderDaemonAsset(t *testing.T) {
	t.Parallel()

	renderedBytes, err := renderDaemonAssetForPathsDefault("daemon-service", daemonServiceContent)
	require.NoError(t, err)

	rendered := string(renderedBytes)

	require.NotContains(t, rendered, "{{")
	assert.Contains(t, rendered, goalstates.DaemonRecoveryUnit)
	assert.Contains(t, rendered, defaultTestPaths().CurrentPath)

	renderedRecoveryBytes, err := renderDaemonAssetForPathsDefault("daemon-recovery-script", daemonRecoveryScriptContent)
	require.NoError(t, err)

	renderedRecovery := string(renderedRecoveryBytes)
	require.NotContains(t, renderedRecovery, "{{")
	assert.Contains(t, renderedRecovery, defaultTestPaths().LastGoodPath)
	assert.Contains(t, renderedRecovery, goalstates.DaemonUnit)
	assert.Contains(t, renderedRecovery, goalstates.DaemonAgentUpgradeSignalPath)
	assert.Contains(t, renderedRecovery, "record-agent-upgrade-failure-signal")
}

// defaultTestPaths returns the agent binary layout for the default prefix.
func defaultTestPaths() goalstates.AgentUpgradePaths {
	paths, err := goalstates.ResolvedAgentUpgradePaths("")
	if err != nil {
		panic(err)
	}

	return paths
}

// renderDaemonAssetForPathsDefault renders a daemon asset using the default
// installation prefix.
func renderDaemonAssetForPathsDefault(name string, content []byte) ([]byte, error) {
	return renderDaemonAssetForPaths(name, content, defaultTestPaths())
}
