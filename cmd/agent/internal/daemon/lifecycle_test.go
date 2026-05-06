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

	rendered := string(renderDaemonAsset(daemonServiceContent))

	require.NotContains(t, rendered, "{{")
	assert.Contains(t, rendered, goalstates.DaemonRecoveryUnit)
	assert.Contains(t, rendered, goalstates.DaemonBinaryCurrentPath)

	renderedRecovery := string(renderDaemonAsset(daemonRecoveryScriptContent))
	require.NotContains(t, renderedRecovery, "{{")
	assert.Contains(t, renderedRecovery, goalstates.DaemonBinaryLastGoodPath)
	assert.Contains(t, renderedRecovery, goalstates.DaemonUnit)
}
