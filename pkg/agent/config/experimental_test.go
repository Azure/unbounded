// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func validIsolationConfig(mode string, experimental *ExperimentalConfig) AgentConfig {
	return AgentConfig{
		MachineName:  "m",
		NodeName:     "n",
		Cluster:      AgentClusterConfig{ClusterDNS: "10.0.0.10"},
		Isolation:    mode,
		Experimental: experimental,
	}
}

func TestValidateRefusesContainerWithoutTheOptIn(t *testing.T) {
	t.Parallel()

	for name, experimental := range map[string]*ExperimentalConfig{
		"no experimental section": nil,
		"opt-in false":            {ContainerIsolation: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := validIsolationConfig("container", experimental)

			err := cfg.Validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), "experimental")
			require.Contains(t, err.Error(), "Experimental.ContainerIsolation")

			// The mode itself is still a known mode.
			mode, err := cfg.IsolationMode()
			require.NoError(t, err)
			require.Equal(t, isolation.ModeContainer, mode)
		})
	}
}

func TestValidateAcceptsContainerWithTheOptIn(t *testing.T) {
	t.Parallel()

	cfg := validIsolationConfig("container", &ExperimentalConfig{ContainerIsolation: true})
	if err := cfg.Validate(); err != nil && strings.Contains(err.Error(), "Isolation") {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestTheOptInDoesNotChangeOtherModes(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"", "nspawn", "none"} {
		for _, experimental := range []*ExperimentalConfig{nil, {ContainerIsolation: true}} {
			cfg := validIsolationConfig(mode, experimental)
			if err := cfg.Validate(); err != nil && strings.Contains(err.Error(), "Isolation") {
				t.Fatalf("mode %q: Validate() = %v", mode, err)
			}
		}
	}
}

func TestCheckModeEnabled(t *testing.T) {
	t.Parallel()

	off := AgentConfig{}
	on := AgentConfig{Experimental: &ExperimentalConfig{ContainerIsolation: true}}

	for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeNone} {
		require.NoError(t, off.CheckModeEnabled(mode), mode)
		require.NoError(t, on.CheckModeEnabled(mode), mode)
	}

	require.Error(t, off.CheckModeEnabled(isolation.ModeContainer))
	require.NoError(t, on.CheckModeEnabled(isolation.ModeContainer))
	require.False(t, off.ContainerIsolationEnabled())
	require.True(t, on.ContainerIsolationEnabled())
}

func TestExperimentalJSON(t *testing.T) {
	t.Parallel()

	// Unset is omitted, so configs that never opt in are unchanged.
	data, err := json.Marshal(AgentConfig{MachineName: "m"})
	require.NoError(t, err)
	require.NotContains(t, string(data), "Experimental")

	var cfg AgentConfig
	require.NoError(t, json.Unmarshal([]byte(`{"Isolation":"container","Experimental":{"ContainerIsolation":true}}`), &cfg))
	require.True(t, cfg.ContainerIsolationEnabled())

	// Clone copies the section rather than sharing it.
	clone := cfg.DeepCopy()
	clone.Experimental.ContainerIsolation = false

	require.True(t, cfg.ContainerIsolationEnabled())
}
