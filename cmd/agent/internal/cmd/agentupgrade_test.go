// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordAgentUpgradeFailureSignalCommand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	operationPath := filepath.Join(dir, "agent-upgrade-operation")
	failurePath := filepath.Join(dir, "agent-upgrade-failure")
	require.NoError(t, os.WriteFile(operationPath, []byte(`{"operationName":"op-1"}`+"\n"), 0o600))

	cmd := newCmdRecordAgentUpgradeFailureSignal()
	cmd.SetArgs([]string{
		"--operation-path", operationPath,
		"--failure-path", failurePath,
		"--message", "rolled back to last good",
	})
	require.NoError(t, cmd.Execute())

	data, err := os.ReadFile(failurePath)
	require.NoError(t, err)
	assert.JSONEq(t, `{"operationName":"op-1","message":"rolled back to last good"}`, string(data))
	assert.NoFileExists(t, operationPath)
}
