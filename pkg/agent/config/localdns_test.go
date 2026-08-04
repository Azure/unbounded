// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"strings"
	"testing"
)

func TestAgentLocalDNSConfigValidate(t *testing.T) {
	t.Parallel()

	positive := 1
	tests := []struct {
		name    string
		config  *AgentLocalDNSConfig
		nodeIP  string
		wantErr string
	}{
		{name: "disabled", config: &AgentLocalDNSConfig{}},
		{name: "defaults", config: &AgentLocalDNSConfig{Enabled: true}, nodeIP: "10.0.0.4"},
		{name: "explicit metrics", config: &AgentLocalDNSConfig{Enabled: true, MetricsAddress: "10.0.0.4:9253"}},
		{name: "missing metrics address", config: &AgentLocalDNSConfig{Enabled: true}, wantErr: "MetricsAddress"},
		{name: "duplicate listeners", config: &AgentLocalDNSConfig{Enabled: true, NodeListenerIP: "169.254.10.10", ClusterListenerIP: "169.254.10.10"}, nodeIP: "10.0.0.4", wantErr: "distinct"},
		{name: "invalid CPU", config: &AgentLocalDNSConfig{Enabled: true, CPULimitInMilliCores: new(int)}, nodeIP: "10.0.0.4", wantErr: "CPULimit"},
		{name: "valid resources", config: &AgentLocalDNSConfig{Enabled: true, CPULimitInMilliCores: &positive, MemoryLimitInMB: &positive}, nodeIP: "10.0.0.4"},
		{name: "duplicate plugin", config: &AgentLocalDNSConfig{Enabled: true, RequiredPlugins: []string{"hosts", "HOSTS"}}, nodeIP: "10.0.0.4", wantErr: "duplicated"},
		{name: "invalid template", config: &AgentLocalDNSConfig{Enabled: true, CorefileTemplate: "{{"}, nodeIP: "10.0.0.4", wantErr: "CorefileTemplate"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := test.config.Validate("10.0.0.10", test.nodeIP)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestAgentLocalDNSConfigDeepCopy(t *testing.T) {
	t.Parallel()

	cpu := 2000
	input := &AgentLocalDNSConfig{Enabled: true, CPULimitInMilliCores: &cpu, RequiredPlugins: []string{"hosts"}}
	copy := input.DeepCopy()
	*copy.CPULimitInMilliCores = 1000

	copy.RequiredPlugins[0] = "template"
	if *input.CPULimitInMilliCores != 2000 || input.RequiredPlugins[0] != "hosts" {
		t.Fatalf("DeepCopy() mutated input: %#v", input)
	}
}
