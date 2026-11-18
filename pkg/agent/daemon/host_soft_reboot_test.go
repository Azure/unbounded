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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

type nodeRebootTarget struct{}

func (nodeRebootTarget) Reference() string { return "machine-1" }
func (nodeRebootTarget) ObservedGeneration(context.Context, client.Reader) (int64, error) {
	return 0, nil
}

func (nodeRebootTarget) MatchSelector(ctx context.Context, r client.Reader, _ v1alpha3.OperationKind, selector labels.Selector) (bool, error) {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: "node-1"}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, err
	}

	return selector.Matches(labels.Set(node.Labels)), nil
}

type fakeHostRebooter struct {
	bootID     string
	bootErr    error
	bootCalls  int
	bootDuring func()
	reboot     func(context.Context) error
	calls      int
}

func (h *fakeHostRebooter) BootID() (string, error) {
	h.bootCalls++
	if h.bootDuring != nil {
		h.bootDuring()
	}

	return h.bootID, h.bootErr
}

func (h *fakeHostRebooter) RebootHost(ctx context.Context, _ *slog.Logger) error {
	h.calls++
	if h.reboot != nil {
		return h.reboot(ctx)
	}

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

	h, err := NewHostSoftRebootHandler(c, reader, slog.New(slog.NewTextHandler(io.Discard, nil)), nodeRebootTarget{}, host, flag)
	require.NoError(t, err)

	return h
}

func publicOperation(t *testing.T, r client.Reader) v1alpha3.MachineOperation {
	t.Helper()

	var op v1alpha3.MachineOperation
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "soft"}, &op))

	return op
}

func TestHostSoftRebootHandlerValidatesDependencies(t *testing.T) {
	c := fakeMachineOperationClient()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	host := &fakeHostRebooter{}

	flag := &atomic.Bool{}
	for _, tc := range []struct {
		name   string
		c      client.Client
		reader client.Reader
		log    *slog.Logger
		target HostSoftRebootTarget
		host   HostRebooter
		flag   *atomic.Bool
	}{
		{name: "client", reader: c, log: log, target: nodeRebootTarget{}, host: host, flag: flag},
		{name: "reader", c: c, log: log, target: nodeRebootTarget{}, host: host, flag: flag},
		{name: "logger", c: c, reader: c, target: nodeRebootTarget{}, host: host, flag: flag},
		{name: "target", c: c, reader: c, log: log, host: host, flag: flag},
		{name: "host", c: c, reader: c, log: log, target: nodeRebootTarget{}, flag: flag},
		{name: "typed nil host", c: c, reader: c, log: log, target: nodeRebootTarget{}, host: (*fakeHostRebooter)(nil), flag: flag},
		{name: "flag", c: c, reader: c, log: log, target: nodeRebootTarget{}, host: host},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewHostSoftRebootHandler(tc.c, tc.reader, tc.log, tc.target, tc.host, tc.flag)
			require.Error(t, err)
		})
	}
}

func TestHostSoftRebootPublicNodeIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ref      string
		selector *metav1.LabelSelector
		node     *corev1.Node
		want     bool
	}{
		{name: "explicit without node", ref: "machine-1", want: true},
		{name: "foreign ref", ref: "node-1"},
		{
			name: "selector matches node",
			selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"rack": "a"},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node-1",
					Labels: map[string]string{"rack": "a"},
				},
			},
			want: true,
		},
		{
			name: "selector without node",
			selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"rack": "a"},
			},
		},
		{
			name: "invalid selector",
			selector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "rack", Operator: "invalid"},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := publicSoftOp()
			op.Spec.MachineRef = tc.ref
			op.Spec.MachineSelector = tc.selector

			objects := []client.Object{op}
			if tc.node != nil {
				objects = append(objects, tc.node)
			}

			c := fakeMachineOperationClient(objects...)
			flag := &atomic.Bool{}
			host := &fakeHostRebooter{bootID: "boot-1"}
			host.reboot = func(ctx context.Context) error {
				started := publicOperation(t, c)
				assert.True(t, flag.Load())
				assert.Equal(t, v1alpha3.OperationPhaseInProgress, started.Status.Phase)
				require.Len(t, started.Status.Targets, 1)
				assert.Equal(t, "machine-1", started.Status.Targets[0].MachineRef)
				assert.Equal(t, "boot-1", started.Status.Targets[0].BootID)
				assert.Zero(t, started.Status.ObservedMachineGeneration)
				assert.Zero(t, started.Status.Targets[0].ObservedGeneration)

				return nil
			}
			h := publicHandler(t, c, c, host, flag)
			_, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
			require.NoError(t, err)
			assert.Equal(t, tc.want, host.calls == 1)
			assert.Equal(t, tc.want, flag.Load())
		})
	}
}

func TestHostSoftRebootPublicStartCAS(t *testing.T) {
	for _, tc := range []struct {
		name        string
		phase       v1alpha3.OperationPhase
		targetPhase v1alpha3.OperationPhase
		ref         string
	}{
		{name: "started aggregate", phase: v1alpha3.OperationPhaseInProgress},
		{name: "complete aggregate", phase: v1alpha3.OperationPhaseComplete},
		{name: "started target", targetPhase: v1alpha3.OperationPhaseInProgress},
		{name: "foreign identity", ref: "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := publicSoftOp()

			op.Status.Phase = tc.phase
			if tc.targetPhase != "" {
				op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{
					{MachineRef: "machine-1", Phase: tc.targetPhase, BootID: "original"},
				}
			}

			if tc.ref != "" {
				op.Spec.MachineRef = tc.ref
			}

			c := fakeMachineOperationClient(op)
			host := &fakeHostRebooter{bootID: "boot-1"}
			h := publicHandler(t, c, c, host, &atomic.Bool{})
			_, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
			require.NoError(t, err)
			assert.Zero(t, host.calls)
			assert.Equal(t, op.Status, publicOperation(t, c).Status)
		})
	}
}

func TestHostSoftRebootPublicUncertainWriteNeverReplays(t *testing.T) {
	writes := 0
	c := fake.NewClientBuilder().WithScheme(fakeMachineOperationClient().Scheme()).
		WithStatusSubresource(&v1alpha3.MachineOperation{}).
		WithObjects(publicSoftOp()).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++

			require.NoError(t, c.SubResource(sub).Update(ctx, obj, opts...))

			return errors.New("response lost")
		},
	}).Build()
	host := &fakeHostRebooter{bootID: "boot-1"}
	h := publicHandler(t, c, c, host, &atomic.Bool{})
	_, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
	require.ErrorContains(t, err, "response lost")
	_, err = h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
	require.NoError(t, err)
	assert.Zero(t, host.calls)
	assert.Equal(t, 1, writes)
	assert.Equal(t, v1alpha3.OperationPhaseInProgress, publicOperation(t, c).Status.Phase)
}

func TestHostSoftRebootPublicConflictReadsFreshStatus(t *testing.T) {
	writes := 0
	c := fake.NewClientBuilder().WithScheme(fakeMachineOperationClient().Scheme()).
		WithStatusSubresource(&v1alpha3.MachineOperation{}).
		WithObjects(publicSoftOp()).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++

			var latest v1alpha3.MachineOperation
			if err := c.Get(ctx, client.ObjectKey{Name: "soft"}, &latest); err != nil {
				return err
			}

			latest.Status.Phase = v1alpha3.OperationPhaseInProgress
			if err := c.Status().Update(ctx, &latest); err != nil {
				return err
			}

			return apierrors.NewConflict(schema.GroupResource{Resource: "machineoperations"}, "soft", errors.New("changed"))
		},
	}).Build()
	host := &fakeHostRebooter{bootID: "boot-1"}
	h := publicHandler(t, c, c, host, &atomic.Bool{})
	_, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
	require.NoError(t, err)
	assert.Zero(t, host.calls)
	assert.Equal(t, 1, writes)
}

func TestHostSoftRebootPublicCommandOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancel    bool
		wantPhase v1alpha3.OperationPhase
		wantFlag  bool
	}{
		{name: "failure", wantPhase: v1alpha3.OperationPhaseFailed},
		{name: "shutdown", cancel: true, wantPhase: v1alpha3.OperationPhaseInProgress, wantFlag: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeMachineOperationClient(publicSoftOp())

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			flag := &atomic.Bool{}
			host := &fakeHostRebooter{bootID: "boot-1", reboot: func(context.Context) error {
				if tc.cancel {
					cancel()
				}

				return errors.New("command failed")
			}}
			h := publicHandler(t, c, c, host, flag)
			_, err := h.Reconcile(ctx, MachineOperation{Name: "soft"})
			require.NoError(t, err)
			status := publicOperation(t, c).Status
			assert.Equal(t, tc.wantPhase, status.Phase)
			require.Len(t, status.Targets, 1)
			assert.Equal(t, tc.wantPhase, status.Targets[0].Phase)
			assert.Equal(t, tc.wantFlag, flag.Load())
		})
	}
}

func TestHostSoftRebootPublicInitialBootReadFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		host *fakeHostRebooter
	}{
		{name: "error", host: &fakeHostRebooter{bootErr: errors.New("missing boot ID")}},
		{name: "empty", host: &fakeHostRebooter{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeMachineOperationClient(publicSoftOp())
			h := publicHandler(t, c, c, tc.host, &atomic.Bool{})
			_, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
			require.NoError(t, err)
			status := publicOperation(t, c).Status
			assert.Equal(t, v1alpha3.OperationPhaseFailed, status.Phase)
			condition := apimeta.FindStatusCondition(status.Conditions, "Completed")
			require.NotNil(t, condition)
			assert.Equal(t, "ExecutionFailed", condition.Reason)
			assert.Zero(t, tc.host.calls)
		})
	}
}

func TestHostSoftRebootPublicStartupJudgment(t *testing.T) {
	for _, tc := range []struct {
		name, current string
		phase         v1alpha3.OperationPhase
		reason        string
	}{
		{name: "new boot", current: "boot-2", phase: v1alpha3.OperationPhaseComplete, reason: "Succeeded"},
		{name: "same boot", current: "boot-1", phase: v1alpha3.OperationPhaseFailed, reason: "RebootNotObserved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op := publicSoftOp()
			op.Status.Phase = v1alpha3.OperationPhaseInProgress
			op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{
				{
					MachineRef: "machine-1",
					Phase:      v1alpha3.OperationPhaseInProgress,
					BootID:     "boot-1",
				},
			}
			c := fakeMachineOperationClient(op)
			host := &fakeHostRebooter{bootID: tc.current}
			h := publicHandler(t, c, c, host, &atomic.Bool{})
			require.NoError(t, h.PublishStartup(t.Context()))
			status := publicOperation(t, c).Status
			assert.Equal(t, tc.phase, status.Phase)
			require.Len(t, status.Targets, 1)
			assert.Equal(t, tc.phase, status.Targets[0].Phase)
			assert.Equal(t, "boot-1", status.Targets[0].BootID)
			condition := apimeta.FindStatusCondition(status.Conditions, "Completed")
			require.NotNil(t, condition)
			assert.Equal(t, tc.reason, condition.Reason)
			require.NoError(t, h.PublishStartup(t.Context()))
			assert.Equal(t, status, publicOperation(t, c).Status)
			assert.Zero(t, host.calls)
		})
	}
}
