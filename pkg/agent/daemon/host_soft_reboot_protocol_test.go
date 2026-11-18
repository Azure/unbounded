// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

type rebootTestTarget struct {
	labels map[string]string
}

func (rebootTestTarget) Reference() string { return "machine-1" }

func (rebootTestTarget) ObservedGeneration(context.Context, client.Reader) (int64, error) {
	return 0, nil
}

func (t rebootTestTarget) MatchSelector(_ context.Context, _ client.Reader, _ v1alpha3.OperationKind, selector labels.Selector) (bool, error) {
	return selector.Matches(labels.Set(t.labels)), nil
}

type fakeHostRebooter struct {
	bootID    string
	bootErr   error
	bootCalls int
}

func (h *fakeHostRebooter) BootID() (string, error) {
	h.bootCalls++

	return h.bootID, h.bootErr
}

func (*fakeHostRebooter) RebootHost(context.Context, *slog.Logger) error {
	return nil
}

func publicSoftOp() *v1alpha3.MachineOperation {
	return &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "soft"},
		Spec: v1alpha3.MachineOperationSpec{
			OperationKind: v1alpha3.OperationHostSoftReboot,
			MachineRef:    "machine-1",
		},
	}
}

func publicHandler(t *testing.T, c client.Client, reader client.Reader, host *fakeHostRebooter, flag *atomic.Bool) *HostSoftRebootHandler {
	t.Helper()

	h, err := NewHostSoftRebootHandler(c, reader, slog.New(slog.NewTextHandler(io.Discard, nil)), rebootTestTarget{}, host, flag)
	require.NoError(t, err)

	return h
}

func publicOperation(t *testing.T, r client.Reader) v1alpha3.MachineOperation {
	t.Helper()

	var op v1alpha3.MachineOperation
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "soft"}, &op))

	return op
}

func outOfBandOperation(name, ref string, kind v1alpha3.OperationKind) *v1alpha3.MachineOperation {
	op := publicSoftOp()
	op.Name = name
	op.Spec.MachineRef = ref
	op.Spec.OperationKind = kind

	return op
}

func TestUnfinishedOutOfBandHostOperation_ReturnsOldest(t *testing.T) {
	newer := outOfBandOperation("op-a-newer", "machine-1", v1alpha3.OperationHostPowerOn)
	newer.CreationTimestamp = metav1.NewTime(time.Unix(300, 0))
	older := outOfBandOperation("op-z-older", "machine-1", v1alpha3.OperationHostPowerOff)
	older.CreationTimestamp = metav1.NewTime(time.Unix(100, 0))
	tieB := outOfBandOperation("op-tie-b", "machine-1", v1alpha3.OperationHostHardReboot)
	tieB.CreationTimestamp = metav1.NewTime(time.Unix(50, 0))
	tieA := outOfBandOperation("op-tie-a", "machine-1", v1alpha3.OperationHostHardReboot)
	tieA.CreationTimestamp = metav1.NewTime(time.Unix(50, 0))

	c := fakeMachineOperationClient(newer, older)
	h := publicHandler(t, c, c, &fakeHostRebooter{}, &atomic.Bool{})
	blocker, err := h.unfinishedOutOfBandHostOperation(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "op-z-older", blocker)

	c = fakeMachineOperationClient(newer, older, tieB, tieA)
	h = publicHandler(t, c, c, &fakeHostRebooter{}, &atomic.Bool{})
	blocker, err = h.unfinishedOutOfBandHostOperation(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "op-tie-a", blocker, "operations created at the same time are ordered by name")
}

func TestIsOutOfBandHostOperation(t *testing.T) {
	tests := map[v1alpha3.OperationKind]bool{
		v1alpha3.OperationHostHardReboot: true,
		v1alpha3.OperationHostPowerOff:   true,
		v1alpha3.OperationHostPowerOn:    true,
		v1alpha3.OperationHostReplace:    true,
		v1alpha3.OperationHostSoftReboot: false,
		v1alpha3.OperationNodeReboot:     false,
		v1alpha3.OperationAgentUpgrade:   false,
		v1alpha3.OperationAgentReset:     false,
		v1alpha3.OperationHostAllocate:   false,
		v1alpha3.OperationHostRelease:    false,
	}

	for kind, want := range tests {
		assert.Equal(t, want, isOutOfBandHostOperation(kind), kind)
	}
}

func TestUnfinishedOutOfBandHostOperation_IgnoresAllocationOperations(t *testing.T) {
	for _, kind := range []v1alpha3.OperationKind{v1alpha3.OperationHostAllocate, v1alpha3.OperationHostRelease} {
		t.Run(string(kind), func(t *testing.T) {
			allocation := outOfBandOperation("op-allocation", "machine-1", kind)
			allocation.Spec.AllocationRef = &v1alpha3.UIDReference{
				Name: "alloc-1",
				UID:  "alloc-1-uid",
			}
			allocation.CreationTimestamp = metav1.NewTime(time.Unix(100, 0))

			c := fakeMachineOperationClient(allocation)
			h := publicHandler(t, c, c, &fakeHostRebooter{}, &atomic.Bool{})
			blocker, err := h.unfinishedOutOfBandHostOperation(context.Background())
			require.NoError(t, err)
			assert.Empty(t, blocker)

			powerOff := outOfBandOperation("op-power-off", "machine-1", v1alpha3.OperationHostPowerOff)
			powerOff.CreationTimestamp = metav1.NewTime(time.Unix(200, 0))

			c = fakeMachineOperationClient(allocation, powerOff)
			h = publicHandler(t, c, c, &fakeHostRebooter{}, &atomic.Bool{})
			blocker, err = h.unfinishedOutOfBandHostOperation(context.Background())
			require.NoError(t, err)
			assert.Equal(t, "op-power-off", blocker)
		})
	}
}

func TestHostOperationUnfinishedOn(t *testing.T) {
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "machine-1",
			Labels: map[string]string{
				"unbounded-cloud.io/site": "lab",
				"rack":                    "a",
			},
		},
	}
	selectorOperation := func(selector *metav1.LabelSelector) *v1alpha3.MachineOperation {
		op := outOfBandOperation("op", "", v1alpha3.OperationHostHardReboot)
		op.Spec.MachineSelector = selector

		return op
	}
	withTargets := func(op *v1alpha3.MachineOperation, targets ...v1alpha3.MachineOperationTargetStatus) *v1alpha3.MachineOperation {
		op.Status.Phase = v1alpha3.OperationPhaseInProgress
		op.Status.Targets = targets

		return op
	}
	siteSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{"unbounded-cloud.io/site": "lab"},
	}
	failedReplace := func(targets ...v1alpha3.MachineOperationTargetStatus) *v1alpha3.MachineOperation {
		op := outOfBandOperation("op", "machine-1", v1alpha3.OperationHostReplace)
		op.Status.Phase = v1alpha3.OperationPhaseFailed
		op.Status.Targets = targets

		return op
	}

	completed := outOfBandOperation("op", "machine-1", v1alpha3.OperationHostHardReboot)
	completed.Status.Phase = v1alpha3.OperationPhaseComplete

	tests := []struct {
		name string
		op   *v1alpha3.MachineOperation
		want bool
	}{
		{
			name: "machineRef pending",
			op:   outOfBandOperation("op", "machine-1", v1alpha3.OperationHostHardReboot),
			want: true,
		},
		{
			name: "machineRef other machine",
			op:   outOfBandOperation("op", "other-machine", v1alpha3.OperationHostHardReboot),
			want: false,
		},
		{
			name: "operation complete",
			op:   completed,
			want: false,
		},
		{
			name: "selector matches",
			op:   selectorOperation(siteSelector),
			want: true,
		},
		{
			name: "selector does not match",
			op: selectorOperation(&metav1.LabelSelector{
				MatchLabels: map[string]string{"rack": "b"},
			}),
			want: false,
		},
		{
			name: "invalid selector",
			op: selectorOperation(&metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "rack",
						Operator: "Bogus",
					},
				},
			}),
			want: false,
		},
		{
			name: "no machineRef or selector",
			op:   outOfBandOperation("op", "", v1alpha3.OperationHostHardReboot),
			want: false,
		},
		{
			name: "target for machine in progress",
			op: withTargets(selectorOperation(siteSelector), v1alpha3.MachineOperationTargetStatus{
				MachineRef: "machine-1",
				Phase:      v1alpha3.OperationPhaseInProgress,
			}),
			want: true,
		},
		{
			name: "target for machine pending",
			op: withTargets(selectorOperation(siteSelector), v1alpha3.MachineOperationTargetStatus{
				MachineRef: "machine-1",
			}),
			want: true,
		},
		{
			name: "target for machine finished while others run",
			op: withTargets(selectorOperation(siteSelector),
				v1alpha3.MachineOperationTargetStatus{
					MachineRef: "machine-1",
					Phase:      v1alpha3.OperationPhaseComplete,
				},
				v1alpha3.MachineOperationTargetStatus{
					MachineRef: "other-machine",
					Phase:      v1alpha3.OperationPhaseInProgress,
				},
			),
			want: false,
		},
		{
			name: "target for machine failed",
			op: withTargets(outOfBandOperation("op", "machine-1", v1alpha3.OperationHostReplace), v1alpha3.MachineOperationTargetStatus{
				MachineRef: "machine-1",
				Phase:      v1alpha3.OperationPhaseFailed,
			}),
			want: false,
		},
		{
			name: "failed replace with unreleased media",
			op: failedReplace(v1alpha3.MachineOperationTargetStatus{
				MachineRef: "machine-1",
				Phase:      v1alpha3.OperationPhaseFailed,
				BootMedia:  &v1alpha3.MachineOperationBootMediaStatus{Phase: "Installing"},
			}),
			want: true,
		},
		{
			name: "failed replace with released media and confirmed boot",
			op: failedReplace(v1alpha3.MachineOperationTargetStatus{
				MachineRef: "machine-1",
				Phase:      v1alpha3.OperationPhaseFailed,
				BootMedia: &v1alpha3.MachineOperationBootMediaStatus{
					ImageWritten: true,
					Cleaned:      true,
					Released:     true,
				},
				Conditions: []metav1.Condition{
					{
						Type:   v1alpha3.MachineOperationConditionCloudInitDone,
						Status: metav1.ConditionTrue,
					},
				},
			}),
			want: false,
		},
		{
			name: "terminal replace with released media and unconfirmed boot",
			op: failedReplace(v1alpha3.MachineOperationTargetStatus{
				MachineRef: "machine-1",
				Phase:      v1alpha3.OperationPhaseFailed,
				BootMedia: &v1alpha3.MachineOperationBootMediaStatus{
					ImageWritten: true,
					Cleaned:      true,
					Released:     true,
				},
			}),
			want: true,
		},
		{
			name: "outstanding media on another machine",
			op: failedReplace(v1alpha3.MachineOperationTargetStatus{
				MachineRef: "other-machine",
				Phase:      v1alpha3.OperationPhaseFailed,
				BootMedia:  &v1alpha3.MachineOperationBootMediaStatus{Phase: "Installing"},
			}),
			want: false,
		},
		{
			name: "targets resolved without machine",
			op: withTargets(selectorOperation(siteSelector), v1alpha3.MachineOperationTargetStatus{
				MachineRef: "other-machine",
				Phase:      v1alpha3.OperationPhaseInProgress,
			}),
			want: false,
		},
	}

	c := fakeMachineOperationClient()
	h := publicHandler(t, c, c, &fakeHostRebooter{}, &atomic.Bool{})
	h.target = rebootTestTarget{labels: machine.Labels}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := h.hostOperationUnfinishedOn(context.Background(), tt.op)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMarkHostSoftRebootWaiting_LeavesStartedOperation(t *testing.T) {
	started := publicSoftOp()
	started.Status.Phase = v1alpha3.OperationPhaseInProgress
	started.Status.Message = hostSoftRebootMessage
	started.Status.Targets = []v1alpha3.MachineOperationTargetStatus{
		{
			MachineRef:         "machine-1",
			Phase:              v1alpha3.OperationPhaseInProgress,
			BootID:             "boot-1",
			ObservedGeneration: 4,
		},
	}
	c := fakeMachineOperationClient(publicSoftOp())
	h := publicHandler(t, c, c, &fakeHostRebooter{}, &atomic.Bool{})
	latest := publicOperation(t, c)
	latest.Status = started.Status
	require.NoError(t, c.Status().Update(context.Background(), &latest))

	require.NoError(t, h.markWaiting(context.Background(), "soft", "waiting for out-of-band host operation op-oob"))
	unchanged := publicOperation(t, c)
	assert.Equal(t, v1alpha3.OperationPhaseInProgress, unchanged.Status.Phase)
	assert.Equal(t, hostSoftRebootMessage, unchanged.Status.Message)
}

func TestMarkHostSoftRebootWaiting_IgnoresDeletedOperation(t *testing.T) {
	op := publicSoftOp()
	c := fakeMachineOperationClient(op)
	h := publicHandler(t, c, c, &fakeHostRebooter{}, &atomic.Bool{})
	require.NoError(t, c.Delete(context.Background(), op))
	require.NoError(t, h.markWaiting(context.Background(), "soft", "waiting"))
}

func TestPublishHostOSReboots_SameBootFails(t *testing.T) {
	op := publicSoftOp()
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{
		{
			MachineRef:         "machine-1",
			Phase:              v1alpha3.OperationPhaseInProgress,
			BootID:             "boot-1",
			ObservedGeneration: 4,
		},
	}
	c := fakeMachineOperationClient(op)
	h := publicHandler(t, c, c, &fakeHostRebooter{bootID: "boot-1"}, &atomic.Bool{})
	require.NoError(t, h.PublishStartup(context.Background()))

	updated := publicOperation(t, c)
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	condition := apimeta.FindStatusCondition(updated.Status.Conditions, "Completed")
	require.NotNil(t, condition)
	assert.Equal(t, "RebootNotObserved", condition.Reason)
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Targets[0].Phase)
	assert.Equal(t, int64(4), updated.Status.Targets[0].ObservedGeneration)
}

func TestPublishHostOSReboots_IgnoresOtherOperations(t *testing.T) {
	issued := func(name, ref, bootID string) *v1alpha3.MachineOperation {
		op := publicSoftOp()
		op.Name = name
		op.Spec.MachineRef = ref
		op.Status.Phase = v1alpha3.OperationPhaseInProgress
		op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{
			{
				MachineRef:         ref,
				Phase:              v1alpha3.OperationPhaseInProgress,
				BootID:             bootID,
				ObservedGeneration: 4,
			},
		}

		return op
	}
	otherMachine := issued("op-other-machine", "other-machine", "boot-1")
	noBootID := issued("op-no-boot-id", "machine-1", "")
	otherKind := issued("op-other-kind", "machine-1", "boot-1")
	otherKind.Spec.OperationKind = v1alpha3.OperationAgentUpgrade
	c := fakeMachineOperationClient(otherMachine, noBootID, otherKind)
	host := &fakeHostRebooter{bootID: "boot-2"}
	h := publicHandler(t, c, c, host, &atomic.Bool{})

	require.NoError(t, h.PublishStartup(context.Background()))
	assert.Zero(t, host.bootCalls)

	for _, name := range []string{"op-other-machine", "op-no-boot-id", "op-other-kind"} {
		var updated v1alpha3.MachineOperation
		require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: name}, &updated))
		assert.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase, name)
	}
}

func TestPublishHostOSReboots_LeavesInProgressWhenBootIDUnavailable(t *testing.T) {
	op := publicSoftOp()
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{
		{
			MachineRef:         "machine-1",
			Phase:              v1alpha3.OperationPhaseInProgress,
			BootID:             "boot-1",
			ObservedGeneration: 4,
		},
	}
	c := fakeMachineOperationClient(op)
	host := &fakeHostRebooter{bootErr: errors.New("boot ID unavailable")}
	h := publicHandler(t, c, c, host, &atomic.Bool{})

	require.Error(t, h.PublishStartup(context.Background()))
	updated := publicOperation(t, c)
	assert.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	assert.Equal(t, op.Status, updated.Status)
	assert.Equal(t, 1, host.bootCalls)
}
