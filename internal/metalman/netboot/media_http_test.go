// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// Released history yields only to an install that is actually going to run:
// a held operation has not started and may not for a long time, and a
// selector operation's machineRef operation is the one that installs, not the
// parent that lists the machine as a target.
func TestNonMediaSuccessor(t *testing.T) {
	t.Parallel()

	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-1"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{Image: "ghcr.io/test/host:v1"},
		},
	}

	replace := func(name string) v1alpha3.MachineOperation {
		return v1alpha3.MachineOperation{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1alpha3.MachineOperationSpec{
				OperationKind: v1alpha3.OperationHostReplace,
				MachineRef:    machine.Name,
			},
		}
	}

	tests := []struct {
		name string
		ops  []v1alpha3.MachineOperation
		want bool
	}{
		{
			name: "no operations",
			want: false,
		},
		{
			name: "HostReplace for the machine",
			ops:  []v1alpha3.MachineOperation{replace("op-replace")},
			want: true,
		},
		{
			name: "held HostReplace for the machine",
			ops: func() []v1alpha3.MachineOperation {
				op := replace("op-held")
				op.Spec.Suspend = true

				return []v1alpha3.MachineOperation{op}
			}(),
			want: false,
		},
		{
			name: "selector HostReplace listing the machine as a target",
			ops: func() []v1alpha3.MachineOperation {
				op := replace("op-selector")
				op.Spec.MachineRef = ""
				op.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"unbounded-cloud.io/site": "rack-a"}}
				op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
					MachineRef: machine.Name,
					Phase:      v1alpha3.OperationPhaseInProgress,
				}}

				return []v1alpha3.MachineOperation{op}
			}(),
			want: false,
		},
		{
			name: "terminal HostReplace for the machine",
			ops: func() []v1alpha3.MachineOperation {
				op := replace("op-terminal")
				op.Status.Phase = v1alpha3.OperationPhaseFailed

				return []v1alpha3.MachineOperation{op}
			}(),
			want: false,
		},
		{
			name: "HostReplace for another machine",
			ops: func() []v1alpha3.MachineOperation {
				op := replace("op-other-machine")
				op.Spec.MachineRef = "other-machine"

				return []v1alpha3.MachineOperation{op}
			}(),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, nonMediaSuccessor(machine, tt.ops))
		})
	}
}
