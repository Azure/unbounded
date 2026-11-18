// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func selectorSoftOp() *v1alpha3.MachineOperation {
	op := publicSoftOp()
	op.Spec.MachineRef = ""
	op.Spec.MachineSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"rack": "a",
		},
	}
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{
		{
			MachineRef: "peer",
			Phase:      v1alpha3.OperationPhaseInProgress,
			BootID:     "peer-boot",
		},
	}

	return op
}

func selectorNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-1",
			Labels: map[string]string{
				"rack": "a",
			},
		},
	}
}

func TestHostSoftRebootSelectorAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*v1alpha3.MachineOperation)
		start  bool
	}{
		{
			name:  "peer in progress",
			start: true,
		},
		{
			name: "local pending",
			modify: func(op *v1alpha3.MachineOperation) {
				op.Status.Targets = append(op.Status.Targets, v1alpha3.MachineOperationTargetStatus{
					MachineRef: "machine-1",
					Phase:      v1alpha3.OperationPhasePending,
				})
			},
			start: true,
		},
		{
			name: "local started",
			modify: func(op *v1alpha3.MachineOperation) {
				op.Status.Targets = append(op.Status.Targets, v1alpha3.MachineOperationTargetStatus{
					MachineRef: "machine-1",
					Phase:      v1alpha3.OperationPhaseInProgress,
					BootID:     "recorded",
				})
			},
		},
		{
			name: "local complete",
			modify: func(op *v1alpha3.MachineOperation) {
				op.Status.Targets = append(op.Status.Targets, v1alpha3.MachineOperationTargetStatus{
					MachineRef: "machine-1",
					Phase:      v1alpha3.OperationPhaseComplete,
				})
			},
		},
		{
			name: "local failed",
			modify: func(op *v1alpha3.MachineOperation) {
				op.Status.Targets = append(op.Status.Targets, v1alpha3.MachineOperationTargetStatus{
					MachineRef: "machine-1",
					Phase:      v1alpha3.OperationPhaseFailed,
				})
			},
		},
		{
			name: "complete aggregate",
			modify: func(op *v1alpha3.MachineOperation) {
				op.Status.Phase = v1alpha3.OperationPhaseComplete
			},
		},
		{
			name: "failed aggregate",
			modify: func(op *v1alpha3.MachineOperation) {
				op.Status.Phase = v1alpha3.OperationPhaseFailed
			},
		},
		{
			name: "single reference in progress",
			modify: func(op *v1alpha3.MachineOperation) {
				op.Spec.MachineRef = "machine-1"
			},
		},
		{
			name: "foreign reference",
			modify: func(op *v1alpha3.MachineOperation) {
				op.Spec.MachineRef = "other"
			},
		},
		{
			name: "selector does not match",
			modify: func(op *v1alpha3.MachineOperation) {
				op.Spec.MachineSelector.MatchLabels["rack"] = "other"
			},
		},
		{
			name: "missing selector",
			modify: func(op *v1alpha3.MachineOperation) {
				op.Spec.MachineSelector = nil
			},
		},
		{
			name: "invalid selector",
			modify: func(op *v1alpha3.MachineOperation) {
				op.Spec.MachineSelector = &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "rack",
							Operator: "invalid",
						},
					},
				}
			},
		},
		{
			name: "wrong kind",
			modify: func(op *v1alpha3.MachineOperation) {
				op.Spec.OperationKind = v1alpha3.OperationNodeReboot
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := selectorSoftOp()
			if tc.modify != nil {
				tc.modify(op)
			}

			c := fakeMachineOperationClient(op, selectorNode())
			host := &fakeHostRebooter{bootID: "local-boot"}
			h := publicHandler(t, c, c, host, &atomic.Bool{})
			_, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
			require.NoError(t, err)

			if !tc.start {
				assert.Zero(t, host.calls)
				assert.Equal(t, op.Status, publicOperation(t, c).Status)

				return
			}

			require.Equal(t, 1, host.calls)
			started := publicOperation(t, c)
			require.Len(t, started.Status.Targets, 2)
			assert.Equal(t, "local-boot", started.Status.Targets[1].BootID)
			assert.Equal(t, v1alpha3.OperationPhaseInProgress, started.Status.Targets[1].Phase)

			// A fresh handler must also refuse the already-issued local target.
			h = publicHandler(t, c, c, host, &atomic.Bool{})
			_, err = h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
			require.NoError(t, err)
			assert.Equal(t, 1, host.calls)
			assert.Equal(t, started.Status, publicOperation(t, c).Status)
		})
	}
}

func TestHostSoftRebootSelectorStartRechecksPeerAndLabels(t *testing.T) {
	for _, tc := range []struct {
		name       string
		terminal   bool
		labelDrift bool
		start      bool
	}{
		{
			name:  "peer starts",
			start: true,
		},
		{
			name:     "peer finishes",
			terminal: true,
		},
		{
			name:       "labels change",
			labelDrift: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := selectorSoftOp()
			op.Status.Phase = v1alpha3.OperationPhasePending
			c := fakeMachineOperationClient(op, selectorNode())
			host := &fakeHostRebooter{bootID: "local-boot"}
			host.bootDuring = func() {
				latest := publicOperation(t, c)

				latest.Status.Phase = v1alpha3.OperationPhaseInProgress
				if tc.terminal {
					latest.Status.Phase = v1alpha3.OperationPhaseComplete
				}

				require.NoError(t, c.Status().Update(t.Context(), &latest))

				if tc.labelDrift {
					var node corev1.Node
					require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: "node-1"}, &node))
					node.Labels["rack"] = "other"
					require.NoError(t, c.Update(t.Context(), &node))
				}
			}
			h := publicHandler(t, c, c, host, &atomic.Bool{})

			_, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
			require.NoError(t, err)

			if tc.start {
				assert.Equal(t, 1, host.calls)
				require.Len(t, publicOperation(t, c).Status.Targets, 2)
			} else {
				assert.Zero(t, host.calls)
				require.Len(t, publicOperation(t, c).Status.Targets, 1)
			}
		})
	}
}

func TestHostSoftRebootSelectorStartRechecksLocalEvidence(t *testing.T) {
	op := selectorSoftOp()
	writes := 0
	c := fake.NewClientBuilder().WithScheme(fakeMachineOperationClient().Scheme()).
		WithStatusSubresource(&v1alpha3.MachineOperation{}).
		WithObjects(op, selectorNode()).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
			writes++

			var latest v1alpha3.MachineOperation
			if err := c.Get(ctx, client.ObjectKey{Name: "soft"}, &latest); err != nil {
				return err
			}

			latest.Status.Targets = append(latest.Status.Targets, v1alpha3.MachineOperationTargetStatus{
				MachineRef: "machine-1",
				Phase:      v1alpha3.OperationPhaseInProgress,
				BootID:     "concurrent",
			})
			if err := c.Status().Update(ctx, &latest); err != nil {
				return err
			}

			return apierrors.NewConflict(schema.GroupResource{Resource: "machineoperations"}, "soft", errors.New("local start raced"))
		},
	}).Build()
	host := &fakeHostRebooter{bootID: "local-boot"}
	h := publicHandler(t, c, c, host, &atomic.Bool{})

	_, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
	require.NoError(t, err)
	assert.Zero(t, host.calls)
	assert.Equal(t, 1, writes)
	assert.Equal(t, "concurrent", publicOperation(t, c).Status.Targets[1].BootID)
}

func TestHostSoftRebootSelectorWaitingPreservesPeerAggregate(t *testing.T) {
	soft := selectorSoftOp()
	soft.Status.Message = "peer is rebooting"
	blocker := publicSoftOp()
	blocker.Name = "oob"
	blocker.Spec.OperationKind = v1alpha3.OperationHostPowerOff
	c := fakeMachineOperationClient(soft, blocker, selectorNode())
	host := &fakeHostRebooter{bootID: "boot"}
	h := publicHandler(t, c, c, host, &atomic.Bool{})

	result, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
	require.NoError(t, err)
	assert.Equal(t, hostSoftRebootWaitInterval, result.RequeueAfter)
	assert.Zero(t, host.bootCalls)
	assert.Zero(t, host.calls)
	assert.Equal(t, soft.Status, publicOperation(t, c).Status)
}

func TestHostSoftRebootSelectorWaitingRechecksPeerStart(t *testing.T) {
	soft := selectorSoftOp()
	soft.Status.Phase = v1alpha3.OperationPhasePending
	blocker := publicSoftOp()
	blocker.Name = "oob"
	blocker.Spec.OperationKind = v1alpha3.OperationHostPowerOff
	writes := 0

	var peerStatus v1alpha3.MachineOperationStatus

	c := fake.NewClientBuilder().WithScheme(fakeMachineOperationClient().Scheme()).
		WithStatusSubresource(&v1alpha3.MachineOperation{}).
		WithObjects(soft, blocker, selectorNode()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}

			var latest v1alpha3.MachineOperation
			if err := c.Get(ctx, client.ObjectKey{Name: "soft"}, &latest); err != nil {
				return err
			}

			latest.Status.Phase = v1alpha3.OperationPhaseInProgress

			latest.Status.Message = "peer started"
			if err := c.Status().Update(ctx, &latest); err != nil {
				return err
			}

			peerStatus = latest.Status

			return nil
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}).Build()
	host := &fakeHostRebooter{bootID: "boot"}
	h := publicHandler(t, c, c, host, &atomic.Bool{})

	result, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
	require.NoError(t, err)
	assert.Equal(t, hostSoftRebootWaitInterval, result.RequeueAfter)
	assert.Zero(t, host.calls)
	assert.Zero(t, writes)
	assert.Equal(t, peerStatus, publicOperation(t, c).Status)
}

func TestHostSoftRebootSelectorBootReadFailure(t *testing.T) {
	soft := selectorSoftOp()
	c := fakeMachineOperationClient(soft, selectorNode())
	host := &fakeHostRebooter{bootErr: errors.New("boot read failed")}
	h := publicHandler(t, c, c, host, &atomic.Bool{})

	_, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
	require.NoError(t, err)
	status := publicOperation(t, c).Status
	assert.Equal(t, v1alpha3.OperationPhaseFailed, status.Phase)
	assert.Equal(t, "boot read failed", status.Message)
	assert.Equal(t, soft.Status.Targets, status.Targets)
	condition := apimeta.FindStatusCondition(status.Conditions, "Completed")
	require.NotNil(t, condition)
	assert.Equal(t, "ExecutionFailed", condition.Reason)
	assert.Zero(t, host.calls)
}

func TestHostSoftRebootSelectorBootFailureRechecksStatus(t *testing.T) {
	for _, phase := range []v1alpha3.OperationPhase{v1alpha3.OperationPhaseInProgress, v1alpha3.OperationPhaseComplete} {
		t.Run(string(phase), func(t *testing.T) {
			c := fakeMachineOperationClient(selectorSoftOp(), selectorNode())
			host := &fakeHostRebooter{bootErr: errors.New("boot read failed")}

			var concurrent v1alpha3.MachineOperationStatus

			host.bootDuring = func() {
				op := publicOperation(t, c)

				op.Status.Phase = phase
				if phase == v1alpha3.OperationPhaseInProgress {
					op.Status.Targets = append(op.Status.Targets, v1alpha3.MachineOperationTargetStatus{
						MachineRef: "machine-1",
						Phase:      v1alpha3.OperationPhaseInProgress,
						BootID:     "concurrent",
					})
				}

				require.NoError(t, c.Status().Update(t.Context(), &op))
				concurrent = publicOperation(t, c).Status
			}
			h := publicHandler(t, c, c, host, &atomic.Bool{})

			_, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
			require.NoError(t, err)
			assert.Equal(t, concurrent, publicOperation(t, c).Status)
			assert.Zero(t, host.calls)
		})
	}
}

func TestHostSoftRebootTerminalSelectorPublishesLocalEvidence(t *testing.T) {
	for _, aggregate := range []v1alpha3.OperationPhase{v1alpha3.OperationPhaseComplete, v1alpha3.OperationPhaseFailed} {
		for _, current := range []string{"new-boot", "local-boot"} {
			t.Run(string(aggregate)+"/"+current, func(t *testing.T) {
				op := selectorSoftOp()
				op.Status.Phase = aggregate
				op.Status.Message = "peer result"
				op.Status.ObservedMachineGeneration = 42
				completed := metav1.Now()
				op.Status.CompletedAt = &completed
				op.Status.Conditions = []metav1.Condition{
					{
						Type:               "Completed",
						Status:             metav1.ConditionTrue,
						Reason:             "PeerResult",
						Message:            "peer result",
						LastTransitionTime: completed,
					},
				}
				op.Status.Targets = append(op.Status.Targets, v1alpha3.MachineOperationTargetStatus{
					MachineRef:         "machine-1",
					Phase:              v1alpha3.OperationPhaseInProgress,
					BootID:             "local-boot",
					ObservedGeneration: 7,
				})
				writes := 0
				c := fake.NewClientBuilder().WithScheme(fakeMachineOperationClient().Scheme()).
					WithStatusSubresource(&v1alpha3.MachineOperation{}).
					WithObjects(op).WithInterceptorFuncs(interceptor.Funcs{
					SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
						writes++
						return c.SubResource(sub).Update(ctx, obj, opts...)
					},
				}).Build()
				h := publicHandler(t, c, c, &fakeHostRebooter{bootID: current}, &atomic.Bool{})
				before := publicOperation(t, c).Status

				require.NoError(t, h.PublishStartup(t.Context()))
				status := publicOperation(t, c).Status
				assert.Equal(t, aggregate, status.Phase)
				assert.Equal(t, before.Message, status.Message)
				assert.Equal(t, before.Conditions, status.Conditions)
				assert.Equal(t, before.CompletedAt, status.CompletedAt)
				assert.Equal(t, before.ObservedMachineGeneration, status.ObservedMachineGeneration)
				assert.Equal(t, before.StartedAt, status.StartedAt)
				assert.Equal(t, before.Targets[0], status.Targets[0])
				aggregateStatus := status.DeepCopy()
				aggregateStatus.Targets = before.Targets
				assert.Equal(t, before, *aggregateStatus)

				target := status.Targets[1]
				if current == "local-boot" {
					assert.Equal(t, v1alpha3.OperationPhaseFailed, target.Phase)
					assert.Equal(t, "host did not reboot after HostSoftReboot was issued", target.Message)
				} else {
					assert.Equal(t, v1alpha3.OperationPhaseComplete, target.Phase)
					assert.Equal(t, "HostSoftReboot completed", target.Message)
				}

				assert.Equal(t, "local-boot", target.BootID)
				assert.Equal(t, int64(7), target.ObservedGeneration)
				assert.NotNil(t, target.CompletedAt)
				require.NoError(t, h.PublishStartup(t.Context()))
				assert.Equal(t, 1, writes)
				assert.Equal(t, status, publicOperation(t, c).Status)
			})
		}
	}
}

func TestHostSoftRebootSelectorStartupRechecksEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changed bool
	}{
		{
			name: "terminal peer",
		},
		{
			name:    "new local evidence",
			changed: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := selectorSoftOp()
			op.Status.Targets = append(op.Status.Targets, v1alpha3.MachineOperationTargetStatus{
				MachineRef: "machine-1",
				Phase:      v1alpha3.OperationPhaseInProgress,
				BootID:     "old",
			})
			c := fakeMachineOperationClient(op)

			var peerStatus v1alpha3.MachineOperationStatus

			host := &fakeHostRebooter{bootID: "new"}
			host.bootDuring = func() {
				latest := publicOperation(t, c)
				latest.Status.Phase = v1alpha3.OperationPhaseComplete
				latest.Status.Message = "peer completed"
				completed := metav1.Now()

				latest.Status.CompletedAt = &completed
				if tc.changed {
					latest.Status.Targets[1].BootID = "newer-evidence"
				}

				require.NoError(t, c.Status().Update(t.Context(), &latest))
				peerStatus = publicOperation(t, c).Status
			}
			h := publicHandler(t, c, c, host, &atomic.Bool{})

			require.NoError(t, h.PublishStartup(t.Context()))

			status := publicOperation(t, c).Status
			if tc.changed {
				assert.Equal(t, peerStatus, status)
				return
			}

			assert.Equal(t, peerStatus.Phase, status.Phase)
			assert.Equal(t, peerStatus.Message, status.Message)
			assert.Equal(t, peerStatus.CompletedAt, status.CompletedAt)
			assert.Equal(t, v1alpha3.OperationPhaseComplete, status.Targets[1].Phase)
		})
	}
}

func TestHostSoftRebootSelectorCommandFailureAfterPeerPublication(t *testing.T) {
	c := fakeMachineOperationClient(selectorSoftOp(), selectorNode())
	host := &fakeHostRebooter{bootID: "local-boot"}

	var peerStatus v1alpha3.MachineOperationStatus

	host.reboot = func(ctx context.Context) error {
		op := publicOperation(t, c)
		op.Status.Phase = v1alpha3.OperationPhaseComplete
		op.Status.Message = "peer completed"
		completed := metav1.Now()
		op.Status.CompletedAt = &completed
		require.NoError(t, c.Status().Update(ctx, &op))
		peerStatus = publicOperation(t, c).Status

		return errors.New("command failed")
	}
	flag := &atomic.Bool{}
	h := publicHandler(t, c, c, host, flag)

	_, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
	require.NoError(t, err)
	status := publicOperation(t, c).Status
	assert.False(t, flag.Load())
	assert.Equal(t, 1, host.calls)
	assert.Equal(t, peerStatus.Phase, status.Phase)
	assert.Equal(t, peerStatus.Message, status.Message)
	assert.Equal(t, peerStatus.CompletedAt, status.CompletedAt)
	assert.Equal(t, peerStatus.Conditions, status.Conditions)
	assert.Equal(t, v1alpha3.OperationPhaseFailed, status.Targets[1].Phase)
	assert.Equal(t, "command failed", status.Targets[1].Message)
	assert.NotNil(t, status.Targets[1].CompletedAt)
}

func TestHostSoftRebootSingleReferenceFinishesOnlyIfStillNonterminal(t *testing.T) {
	op := publicSoftOp()
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{
		{
			MachineRef: "machine-1",
			Phase:      v1alpha3.OperationPhaseInProgress,
			BootID:     "old",
		},
	}
	c := fakeMachineOperationClient(op)
	host := &fakeHostRebooter{bootID: "new"}

	var terminal v1alpha3.MachineOperationStatus

	host.bootDuring = func() {
		latest := publicOperation(t, c)
		latest.Status.Phase = v1alpha3.OperationPhaseFailed
		latest.Status.Message = "already finished"
		require.NoError(t, c.Status().Update(t.Context(), &latest))
		terminal = publicOperation(t, c).Status
	}
	h := publicHandler(t, c, c, host, &atomic.Bool{})

	require.NoError(t, h.PublishStartup(t.Context()))
	assert.Equal(t, terminal, publicOperation(t, c).Status)
}

func TestHostSoftRebootTerminalSingleReferenceIsImmutable(t *testing.T) {
	for _, phase := range []v1alpha3.OperationPhase{v1alpha3.OperationPhaseComplete, v1alpha3.OperationPhaseFailed} {
		t.Run(string(phase), func(t *testing.T) {
			op := publicSoftOp()
			op.Status.Phase = phase
			op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{
				{
					MachineRef: "machine-1",
					Phase:      v1alpha3.OperationPhaseInProgress,
					BootID:     "old",
				},
			}
			c := fakeMachineOperationClient(op)
			host := &fakeHostRebooter{bootID: "new"}
			h := publicHandler(t, c, c, host, &atomic.Bool{})

			require.NoError(t, h.PublishStartup(t.Context()))
			assert.Zero(t, host.bootCalls)
			assert.Equal(t, op.Status, publicOperation(t, c).Status)
		})
	}
}
