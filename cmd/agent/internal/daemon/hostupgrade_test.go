// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestHostDaemonActivationServicePreflightRejectsMachineOperationSignal(t *testing.T) {
	dir := t.TempDir()
	signalPath := filepath.Join(dir, "agent-upgrade-signal")
	require.NoError(t, os.WriteFile(signalPath, []byte(`{"operationName":"op-1"}`), 0o600))

	service := NewHostDaemonActivationService(discardLogger(), goalstates.AgentUpgradePaths{
		SignalPath: signalPath,
	})
	_, err := service.Preflight(context.Background(), filepath.Join(dir, "unbounded-agent-current"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MachineOperation signal exists")
}
