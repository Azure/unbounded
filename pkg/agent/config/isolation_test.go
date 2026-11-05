// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestAgentConfigValidateIsolation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		isolation string
		want      isolation.Mode
		wantErr   bool
	}{
		{"", isolation.ModeNSpawn, false},
		{"nspawn", isolation.ModeNSpawn, false},
		{"none", isolation.ModeNone, false},
		// Modes this agent has no backend for are refused, not run as nspawn.
		{"microvm", "", true},
	}

	for _, test := range tests {
		t.Run(test.isolation, func(t *testing.T) {
			t.Parallel()

			cfg := AgentConfig{
				MachineName: "m",
				NodeName:    "n",
				Cluster:     AgentClusterConfig{ClusterDNS: "10.0.0.10"},
				Isolation:   test.isolation,
			}

			err := cfg.Validate()
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "Isolation") {
					t.Fatalf("Validate() = %v, want Isolation error", err)
				}

				if _, err := cfg.IsolationMode(); err == nil {
					t.Fatal("IsolationMode() should fail")
				}

				return
			}

			if err != nil && strings.Contains(err.Error(), "Isolation") {
				t.Fatalf("Validate() = %v", err)
			}

			got, err := cfg.IsolationMode()
			if err != nil || got != test.want {
				t.Fatalf("IsolationMode() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestAgentConfigIsolationJSONCompat(t *testing.T) {
	t.Parallel()

	// A config written before the field existed decodes to the default mode.
	var old AgentConfig
	if err := json.Unmarshal([]byte(`{"MachineName":"m"}`), &old); err != nil {
		t.Fatal(err)
	}

	if mode, err := old.IsolationMode(); err != nil || mode != isolation.ModeNSpawn {
		t.Fatalf("legacy config mode = %q, %v", mode, err)
	}

	// The default is omitted so existing generated output is unchanged.
	data, err := json.Marshal(AgentConfig{MachineName: "m"})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(data), "Isolation") {
		t.Fatalf("default config must not emit Isolation: %s", data)
	}
}
