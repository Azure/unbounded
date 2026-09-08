// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureDaemonConfigAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	require.NoError(t, os.WriteFile(path, []byte("{\"log-level\":\"warn\",\"iptables\":true}\n"), 0o600))

	require.NoError(t, ensureDaemonConfigAt(path))

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"log-level":"warn","iptables":false}`, string(content))
}

func TestEnsureDaemonConfigAtCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")

	require.NoError(t, ensureDaemonConfigAt(path))

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"iptables":false}`, string(content))
}

func TestEnsureDaemonConfigAtRejectsInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	require.NoError(t, os.WriteFile(path, []byte("invalid"), 0o600))

	err := ensureDaemonConfigAt(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing")
}
