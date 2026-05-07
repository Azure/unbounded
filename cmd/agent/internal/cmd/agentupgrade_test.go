// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestRecordAgentUpgradeFailureSignalCommand(t *testing.T) {
	dir := t.TempDir()
	signalPath := filepath.Join(dir, "agent-upgrade-signal")
	t.Setenv(goalstates.EnvDaemonAgentUpgradeSignalPath, signalPath)
	require.NoError(t, os.WriteFile(signalPath, []byte(`{"operationName":"op-1"}`+"\n"), 0o600))

	cmd := newCmdRecordAgentUpgradeFailureSignal()
	cmd.SetArgs([]string{
		"--message", "rolled back to last good",
	})
	require.NoError(t, cmd.Execute())

	data, err := os.ReadFile(signalPath)
	require.NoError(t, err)
	assert.JSONEq(t, `{"operationName":"op-1","message":"rolled back to last good"}`, string(data))
}
