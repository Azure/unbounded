// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateRequiredHardware(t *testing.T) {
	t.Parallel()

	require.NoError(t, ValidateRequiredHardware(nil))
	require.NoError(t, ValidateRequiredHardware([]string{HardwareNvidiaGPU}))

	err := ValidateRequiredHardware([]string{HardwareNvidiaGPU, "AMDGPU", ""})
	require.ErrorContains(t, err, `"AMDGPU"`)
	require.ErrorContains(t, err, `""`)
}

func TestRequiresHardware(t *testing.T) {
	t.Parallel()

	require.False(t, RequiresHardware(nil, HardwareNvidiaGPU))
	require.True(t, RequiresHardware([]string{HardwareNvidiaGPU}, HardwareNvidiaGPU))
	require.False(t, RequiresHardware([]string{"Other"}, HardwareNvidiaGPU))
}

func TestRequiredHardwareJSON(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(AgentConfig{MachineName: "machine-1"})
	require.NoError(t, err)
	require.NotContains(t, string(data), "RequiredHardware")

	var cfg AgentConfig
	require.NoError(t, json.Unmarshal([]byte(`{"RequiredHardware":["NvidiaGPU"]}`), &cfg))
	require.Equal(t, []string{HardwareNvidiaGPU}, cfg.RequiredHardware)
}
