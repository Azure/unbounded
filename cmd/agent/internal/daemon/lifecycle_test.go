// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
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
