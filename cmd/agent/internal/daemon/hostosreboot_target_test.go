// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestHostSoftRebootTargetSelectorSources(t *testing.T) {
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "machine-1",
			Generation: 7,
			Labels:     map[string]string{"source": "machine"},
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			Labels: map[string]string{"source": "node"},
		},
	}
	target := hostSoftRebootTarget{machineName: "machine-1", nodeName: "node-1"}

	for _, tc := range []struct {
		name    string
		kind    v1alpha3.OperationKind
		objects []client.Object
		label   string
		want    bool
	}{
		{
			name:    "soft uses node",
			kind:    v1alpha3.OperationHostSoftReboot,
			objects: []client.Object{machine, node},
			label:   "node",
			want:    true,
		},
		{
			name:    "soft does not use machine when node exists",
			kind:    v1alpha3.OperationHostSoftReboot,
			objects: []client.Object{machine, node},
			label:   "machine",
		},
		{
			name:    "soft falls back to machine",
			kind:    v1alpha3.OperationHostSoftReboot,
			objects: []client.Object{machine},
			label:   "machine",
			want:    true,
		},
		{
			name:    "out of band uses machine",
			kind:    v1alpha3.OperationHostPowerOff,
			objects: []client.Object{machine, node},
			label:   "machine",
			want:    true,
		},
		{
			name:    "out of band does not use node",
			kind:    v1alpha3.OperationHostPowerOff,
			objects: []client.Object{machine, node},
			label:   "node",
		},
		{
			name:    "missing machine",
			kind:    v1alpha3.OperationHostPowerOff,
			objects: []client.Object{node},
			label:   "node",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeStatusClient(tc.objects...)
			matches, err := target.MatchSelector(context.Background(), c, tc.kind, labels.SelectorFromSet(labels.Set{"source": tc.label}))
			require.NoError(t, err)
			assert.Equal(t, tc.want, matches)
		})
	}

	generation, err := target.ObservedGeneration(context.Background(), fakeStatusClient(machine))
	require.NoError(t, err)
	assert.Equal(t, int64(7), generation)
}

func TestHostSoftRebootRequiresNativeMachineBeforeBootRead(t *testing.T) {
	op := &fakeNodeOperator{bootIDErr: assert.AnError}
	c := fakeStatusClient(hostSoftRebootOperation("op-1"))
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(t.Context(), "op-1")
	require.Error(t, err)
	assert.Zero(t, op.rebootCalls)
	assert.Empty(t, getMachineOperation(t, c, "op-1").Status.Phase)
}
