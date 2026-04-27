// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import "testing"

func TestAlternateMachine(t *testing.T) {
	tests := []struct {
		current  string
		expected string
	}{
		{NSpawnMachineKube1, NSpawnMachineKube2},
		{NSpawnMachineKube2, NSpawnMachineKube1},
	}

	for _, tt := range tests {
		got := AlternateMachine(tt.current)
		if got != tt.expected {
			t.Errorf("AlternateMachine(%q) = %q, want %q", tt.current, got, tt.expected)
		}
	}
}

func TestAppliedConfigPath(t *testing.T) {
	tests := []struct {
		machine  string
		expected string
	}{
		{NSpawnMachineKube1, "/etc/unbounded/agent/kube1-applied-config.json"},
		{NSpawnMachineKube2, "/etc/unbounded/agent/kube2-applied-config.json"},
	}

	for _, tt := range tests {
		got := AppliedConfigPath(tt.machine)
		if got != tt.expected {
			t.Errorf("AppliedConfigPath(%q) = %q, want %q", tt.machine, got, tt.expected)
		}
	}
}
