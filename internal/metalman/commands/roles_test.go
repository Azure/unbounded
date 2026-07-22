// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package commands

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestMetalmanRoleComponentsAreIsolated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		role metalmanRole
		want roleComponents
	}{
		{
			name: "controller",
			role: metalmanRoleController,
			want: roleComponents{
				leaderElection: true,
				ociReconciler:  true,
				redfish:        true,
				machineOps:     true,
			},
		},
		{
			name: "server",
			role: metalmanRoleServer,
			want: roleComponents{
				ociReconciler: true,
				http:          true,
				attestation:   true,
				statusUpdates: true,
			},
		},
		{
			name: "edge",
			role: metalmanRoleEdge,
			want: roleComponents{
				ociReconciler: true,
				dhcp:          true,
				tftp:          true,
				http:          true,
				attestation:   true,
				statusUpdates: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := componentsForRole(tt.role); got != tt.want {
				t.Fatalf("componentsForRole(%q) = %#v, want %#v", tt.role, got, tt.want)
			}
		})
	}
}

func TestMetalmanRoleCommands(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		cmd  func() *cobra.Command
	}{
		{name: "controller", cmd: ControllerCmd},
		{name: "server", cmd: ServerCmd},
		{name: "edge", cmd: EdgeCmd},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.cmd().Name(); got != tt.name {
				t.Fatalf("command name = %q, want %q", got, tt.name)
			}
		})
	}
}
