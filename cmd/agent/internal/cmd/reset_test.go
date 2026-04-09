// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded-kube/internal/provision"
)

func TestResolveMachineName_FlagTakesPriority(t *testing.T) {
	t.Parallel()

	name, err := resolveMachineName("flag-machine")
	require.NoError(t, err)
	assert.Equal(t, "flag-machine", name)
}

func TestResolveMachineName_FlagOverridesConfig(t *testing.T) {
	cfg := provision.AgentConfig{MachineName: "config-machine"}
	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, data, 0o644))
	t.Setenv(configFileEnv, path)

	name, err := resolveMachineName("flag-machine")
	require.NoError(t, err)
	assert.Equal(t, "flag-machine", name)
}

func TestResolveMachineName_FromConfigFile(t *testing.T) {
	cfg := provision.AgentConfig{MachineName: "config-machine"}
	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, data, 0o644))
	t.Setenv(configFileEnv, path)

	name, err := resolveMachineName("")
	require.NoError(t, err)
	assert.Equal(t, "config-machine", name)
}

func TestResolveMachineName_ConfigFileMissing(t *testing.T) {
	t.Setenv(configFileEnv, "/tmp/nonexistent-"+t.Name()+".json")

	_, err := resolveMachineName("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading config for machine name")
}

func TestResolveMachineName_ConfigFileEmptyMachineName(t *testing.T) {
	cfg := provision.AgentConfig{MachineName: ""}
	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, data, 0o644))
	t.Setenv(configFileEnv, path)

	_, err = resolveMachineName("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "machine name is required")
}

func TestResolveMachineName_NoFlagNoConfig(t *testing.T) {
	t.Setenv(configFileEnv, "")

	_, err := resolveMachineName("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "machine name is required")
	assert.Contains(t, err.Error(), configFileEnv)
}

func TestNewCmdReset_IsRegistered(t *testing.T) {
	t.Parallel()

	cmdCtx := &CommandContext{LogFormat: "text"}
	cmd := newCmdReset(cmdCtx)

	assert.Equal(t, "reset", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	assert.NotEmpty(t, cmd.Long)

	flag := cmd.Flags().Lookup("machine-name")
	require.NotNil(t, flag)
	assert.Equal(t, "", flag.DefValue)
}
