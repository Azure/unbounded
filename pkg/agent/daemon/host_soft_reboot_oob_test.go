// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestHostSoftRebootPublicNodeOutOfBandPriority(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-1",
			Labels: map[string]string{
				"rack": "a",
			},
		},
	}

	selector := &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"rack": "a",
		},
	}
	for _, tc := range []struct {
		name     string
		kind     v1alpha3.OperationKind
		ref      string
		selector *metav1.LabelSelector
		node     *corev1.Node
		status   v1alpha3.MachineOperationStatus
		wait     bool
	}{
		{
			name: "explicit without node",
			kind: v1alpha3.OperationHostHardReboot,
			ref:  "machine-1",
			wait: true,
		},
		{
			name:     "selector reads node",
			kind:     v1alpha3.OperationHostPowerOff,
			selector: selector,
			node:     node,
			wait:     true,
		},
		{
			name:     "selector without node",
			kind:     v1alpha3.OperationHostPowerOff,
			selector: selector,
		},
		{
			name: "allocation excluded",
			kind: v1alpha3.OperationHostAllocate,
			ref:  "machine-1",
		},
		{
			name: "resolved foreign target overrides ref",
			kind: v1alpha3.OperationHostPowerOn,
			ref:  "machine-1",
			status: v1alpha3.MachineOperationStatus{
				Targets: []v1alpha3.MachineOperationTargetStatus{
					{
						MachineRef: "other",
						Phase:      v1alpha3.OperationPhaseInProgress,
					},
				},
			},
		},
		{
			name: "terminal with outstanding local media",
			kind: v1alpha3.OperationHostReplace,
			ref:  "machine-1",
			status: v1alpha3.MachineOperationStatus{
				Phase: v1alpha3.OperationPhaseFailed,
				Targets: []v1alpha3.MachineOperationTargetStatus{
					{
						MachineRef: "machine-1",
						Phase:      v1alpha3.OperationPhaseFailed,
						BootMedia: &v1alpha3.MachineOperationBootMediaStatus{
							Phase: "Installing",
						},
					},
				},
			},
			wait: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocker := publicSoftOp()
			blocker.Name = "oob"
			blocker.CreationTimestamp = metav1.NewTime(time.Unix(200, 0))
			blocker.Spec.OperationKind = tc.kind
			blocker.Spec.MachineRef = tc.ref
			blocker.Spec.MachineSelector = tc.selector
			blocker.Status = tc.status

			soft := publicSoftOp()
			soft.CreationTimestamp = metav1.NewTime(time.Unix(100, 0))

			objects := []client.Object{soft, blocker}
			if tc.node != nil {
				objects = append(objects, tc.node.DeepCopy())
			}

			c := fakeMachineOperationClient(objects...)
			host := &fakeHostRebooter{bootID: "boot-1"}
			h := publicHandler(t, c, c, host, &atomic.Bool{})

			result, err := h.Reconcile(t.Context(), nil, MachineOperation{Name: "soft"})
			require.NoError(t, err)

			if tc.wait {
				assert.Equal(t, hostSoftRebootWaitInterval, result.RequeueAfter)
				assert.Zero(t, host.calls)
				assert.Equal(t, "waiting for out-of-band host operation oob", publicOperation(t, c).Status.Message)
			} else {
				assert.Zero(t, result.RequeueAfter)
				assert.Equal(t, 1, host.calls)
			}
		})
	}
}
