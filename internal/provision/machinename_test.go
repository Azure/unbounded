// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestNormalizeDerivedMachineName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "already valid", input: "worker-1", want: "worker-1"},
		{name: "lowercases uppercase", input: "VMSS00001D", want: "vmss00001d"},
		{name: "trims whitespace", input: "  worker-1  ", want: "worker-1"},
		{name: "accepts fqdn", input: "vmss00001d.internal.cloudapp.net", want: "vmss00001d.internal.cloudapp.net"},
		{name: "lowercases fqdn", input: "Host01.Example.COM", want: "host01.example.com"},
		{name: "rejects underscore", input: "bad_name", wantErr: true},
		{name: "rejects empty", input: "", wantErr: true},
		{name: "rejects whitespace only", input: "   ", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeDerivedMachineName(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveMachineName(t *testing.T) {
	// Not parallel: subtests manipulate the AGENT_MACHINE_NAME env var.
	tests := []struct {
		name        string
		machineName string
		env         string
		wantName    string
		wantSource  string
		wantErr     bool
	}{
		{
			name:        "explicit config value kept",
			machineName: "machine-1",
			wantName:    "machine-1",
			wantSource:  "config",
		},
		{
			name:        "explicit config value trimmed",
			machineName: "  machine-1  ",
			wantName:    "machine-1",
			wantSource:  "config",
		},
		{
			name:        "explicit config value wins over env",
			machineName: "machine-1",
			env:         "env-machine",
			wantName:    "machine-1",
			wantSource:  "config",
		},
		{
			name:        "invalid explicit value errors",
			machineName: "Bad_Name",
			wantErr:     true,
		},
		{
			name:       "env override when config empty",
			env:        "vmss00001",
			wantName:   "vmss00001",
			wantSource: "env",
		},
		{
			name:       "env override lowercased",
			env:        "VMSS00001D",
			wantName:   "vmss00001d",
			wantSource: "env",
		},
		{
			name:    "invalid env errors",
			env:     "bad_env!",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(machineNameEnv, tt.env)

			cfg := &AgentConfig{MachineName: tt.machineName}

			source, err := ResolveMachineName(cfg)
			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantName, cfg.MachineName)
			assert.Equal(t, tt.wantSource, source)
		})
	}
}

func TestResolveMachineName_UsesHostHostname(t *testing.T) {
	t.Setenv(machineNameEnv, "")

	cfg := &AgentConfig{}

	hostname, err := os.Hostname()
	require.NoError(t, err)

	want, normErr := normalizeDerivedMachineName(hostname)

	source, err := ResolveMachineName(cfg)
	if normErr != nil {
		// Host hostname is not a valid node name in this environment.
		require.Error(t, err)
		return
	}

	require.NoError(t, err)
	assert.Equal(t, want, cfg.MachineName)
	assert.Equal(t, "hostname", source)
}

func TestResolveMachineName_ThenBackfillNodeName(t *testing.T) {
	// MachineName must be resolved before NodeName because BackfillNodeName
	// falls back to MachineName as its final option.
	t.Setenv(machineNameEnv, "kube-worker-7")

	cfg := &AgentConfig{}

	source, err := ResolveMachineName(cfg)
	require.NoError(t, err)
	require.Equal(t, "env", source)
	require.Equal(t, "kube-worker-7", cfg.MachineName)

	require.NoError(t, cfg.BackfillNodeName())

	// NodeName prefers a valid host hostname; otherwise it falls back to the
	// resolved MachineName.
	hostname, err := os.Hostname()
	require.NoError(t, err)

	wantNode := "kube-worker-7"
	if hn := strings.TrimSpace(hostname); len(validation.IsDNS1123Subdomain(hn)) == 0 {
		wantNode = hn
	}

	require.NotEmpty(t, cfg.NodeName)
	assert.Equal(t, wantNode, cfg.NodeName)
}
