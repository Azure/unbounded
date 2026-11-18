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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

type unreadableWriter struct{ client.Client }

func (unreadableWriter) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("cached Get used")
}

func (unreadableWriter) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("cached List used")
}

func TestHostSoftRebootPublicUsesReaderForStatusAndStartup(t *testing.T) {
	live := fakeMachineOperationClient(publicSoftOp())
	host := &fakeHostRebooter{bootID: "boot-1"}
	h := publicHandler(t, unreadableWriter{Client: live}, live, host, &atomic.Bool{})

	_, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
	require.NoError(t, err)
	require.Equal(t, 1, host.calls)

	host.bootID = "boot-2"

	require.NoError(t, h.PublishStartup(t.Context()))
	assert.Equal(t, v1alpha3.OperationPhaseComplete, publicOperation(t, live).Status.Phase)
}

func TestHostSoftRebootPublicConflictRevalidatesIdentity(t *testing.T) {
	writes := 0
	c := fake.NewClientBuilder().WithScheme(fakeMachineOperationClient().Scheme()).
		WithStatusSubresource(&v1alpha3.MachineOperation{}).
		WithObjects(publicSoftOp()).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
			writes++

			var latest v1alpha3.MachineOperation
			if err := c.Get(ctx, client.ObjectKey{Name: "soft"}, &latest); err != nil {
				return err
			}

			latest.Spec.MachineRef = "other"
			if err := c.Update(ctx, &latest); err != nil {
				return err
			}

			return apierrors.NewConflict(schema.GroupResource{Resource: "machineoperations"}, "soft", errors.New("target changed"))
		},
	}).Build()
	host := &fakeHostRebooter{bootID: "boot-1"}
	h := publicHandler(t, c, c, host, &atomic.Bool{})

	_, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
	require.NoError(t, err)
	assert.Equal(t, 1, writes)
	assert.Zero(t, host.calls)
	assert.Empty(t, publicOperation(t, c).Status.Phase)
}

func TestHostSoftRebootPublicFailedPublicationDoesNotReplay(t *testing.T) {
	writes := 0
	publishErr := errors.New("publication failed")
	c := fake.NewClientBuilder().WithScheme(fakeMachineOperationClient().Scheme()).
		WithStatusSubresource(&v1alpha3.MachineOperation{}).
		WithObjects(publicSoftOp()).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++
			if writes == 2 {
				return publishErr
			}

			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}).Build()
	flag := &atomic.Bool{}
	host := &fakeHostRebooter{
		bootID: "boot-1",
		reboot: func(context.Context) error {
			return errors.New("command failed")
		},
	}
	h := publicHandler(t, c, c, host, flag)

	_, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
	require.ErrorIs(t, err, publishErr)
	assert.False(t, flag.Load())
	assert.Equal(t, v1alpha3.OperationPhaseInProgress, publicOperation(t, c).Status.Phase)

	_, err = h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
	require.NoError(t, err)
	assert.Equal(t, 1, host.calls)
	assert.Equal(t, 2, writes)
}

func TestHostSoftRebootPublicStartupRejectsStaleEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		phase  v1alpha3.OperationPhase
		bootID string
	}{
		{
			name:   "finished target",
			phase:  v1alpha3.OperationPhaseComplete,
			bootID: "boot-1",
		},
		{
			name:   "changed boot ID",
			phase:  v1alpha3.OperationPhaseInProgress,
			bootID: "different",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stale := publicSoftOp()
			stale.Status.Phase = v1alpha3.OperationPhaseInProgress
			stale.Status.Targets = []v1alpha3.MachineOperationTargetStatus{
				{
					MachineRef: "machine-1",
					Phase:      v1alpha3.OperationPhaseInProgress,
					BootID:     "boot-1",
				},
			}
			fresh := stale.DeepCopy()
			fresh.Status.Targets[0].Phase = tc.phase
			fresh.Status.Targets[0].BootID = tc.bootID
			writes := 0
			c := fake.NewClientBuilder().WithScheme(fakeMachineOperationClient().Scheme()).
				WithStatusSubresource(&v1alpha3.MachineOperation{}).
				WithObjects(fresh).WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if err := c.List(ctx, list, opts...); err != nil {
						return err
					}

					list.(*v1alpha3.MachineOperationList).Items = []v1alpha3.MachineOperation{*stale.DeepCopy()}

					return nil
				},
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					writes++
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			}).Build()
			h := publicHandler(t, c, c, &fakeHostRebooter{bootID: "boot-2"}, &atomic.Bool{})

			require.NoError(t, h.PublishStartup(t.Context()))
			assert.Zero(t, writes)
			assert.Equal(t, fresh.Status, publicOperation(t, c).Status)
		})
	}
}

func TestHostSoftRebootPublicSelectorReadErrorDoesNotReboot(t *testing.T) {
	blocker := publicSoftOp()
	blocker.Name = "oob"
	blocker.Spec.OperationKind = v1alpha3.OperationHostPowerOff
	blocker.Spec.MachineRef = ""
	blocker.Spec.MachineSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"rack": "a"},
	}

	c := fake.NewClientBuilder().WithScheme(fakeMachineOperationClient().Scheme()).
		WithStatusSubresource(&v1alpha3.MachineOperation{}).
		WithObjects(publicSoftOp(), blocker).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == "node-1" {
				return errors.New("node unavailable")
			}

			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	host := &fakeHostRebooter{bootID: "boot-1"}
	h := publicHandler(t, c, c, host, &atomic.Bool{})

	_, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
	require.ErrorContains(t, err, "node unavailable")
	assert.Zero(t, host.calls)
	assert.Empty(t, publicOperation(t, c).Status.Phase)
}

func TestHostSoftRebootPublicCanceledUncertainWriteNeverReplays(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	writes := 0
	c := fake.NewClientBuilder().WithScheme(fakeMachineOperationClient().Scheme()).
		WithStatusSubresource(&v1alpha3.MachineOperation{}).
		WithObjects(publicSoftOp()).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			writes++

			if err := c.SubResource(sub).Update(ctx, obj, opts...); err != nil {
				return err
			}

			if writes == 1 {
				cancel()
				return errors.New("response lost")
			}

			return nil
		},
	}).Build()
	host := &fakeHostRebooter{bootID: "boot-1"}
	h := publicHandler(t, c, c, host, &atomic.Bool{})

	_, err := h.Reconcile(ctx, MachineOperation{Name: "soft"})
	require.ErrorContains(t, err, "response lost")
	_, err = h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
	require.NoError(t, err)
	assert.Zero(t, host.calls)
	assert.Equal(t, 1, writes)
	assert.Equal(t, v1alpha3.OperationPhaseInProgress, publicOperation(t, c).Status.Phase)

	require.NoError(t, h.PublishStartup(t.Context()))
	assert.Equal(t, v1alpha3.OperationPhaseFailed, publicOperation(t, c).Status.Phase)
	assert.Equal(t, 2, writes)
}

func TestHostSoftRebootPublicBootFailureDoesNotOverwriteConcurrentStart(t *testing.T) {
	for _, phase := range []v1alpha3.OperationPhase{v1alpha3.OperationPhaseInProgress, v1alpha3.OperationPhaseComplete} {
		t.Run(string(phase), func(t *testing.T) {
			c := fakeMachineOperationClient(publicSoftOp())
			host := &fakeHostRebooter{bootErr: errors.New("boot failed")}
			host.bootDuring = func() {
				op := publicOperation(t, c)
				op.Status.Phase = phase
				require.NoError(t, c.Status().Update(t.Context(), &op))
			}
			h := publicHandler(t, c, c, host, &atomic.Bool{})

			_, err := h.Reconcile(t.Context(), MachineOperation{Name: "soft"})
			require.NoError(t, err)
			assert.Equal(t, phase, publicOperation(t, c).Status.Phase)
			assert.Zero(t, host.calls)
		})
	}
}

func TestHostSoftRebootPublicStartupContinuesAfterWriteError(t *testing.T) {
	issued := func(name string) *v1alpha3.MachineOperation {
		op := publicSoftOp()
		op.Name = name
		op.Status.Phase = v1alpha3.OperationPhaseInProgress
		op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{
			{
				MachineRef: "machine-1",
				Phase:      v1alpha3.OperationPhaseInProgress,
				BootID:     "boot-1",
			},
		}

		return op
	}

	writeErr := errors.New("one operation failed")
	c := fake.NewClientBuilder().WithScheme(fakeMachineOperationClient().Scheme()).
		WithStatusSubresource(&v1alpha3.MachineOperation{}).
		WithObjects(issued("soft"), issued("second")).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if obj.GetName() == "soft" {
				return writeErr
			}

			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	}).Build()
	host := &fakeHostRebooter{bootID: "boot-2"}
	h := publicHandler(t, c, c, host, &atomic.Bool{})

	require.ErrorIs(t, h.PublishStartup(t.Context()), writeErr)
	assert.Equal(t, 1, host.bootCalls)
	assert.Equal(t, v1alpha3.OperationPhaseInProgress, publicOperation(t, c).Status.Phase)

	var second v1alpha3.MachineOperation
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: "second"}, &second))
	assert.Equal(t, v1alpha3.OperationPhaseComplete, second.Status.Phase)
}

func TestHostSoftRebootPublicStartupReadAndWriteErrors(t *testing.T) {
	for _, failure := range []string{"list", "boot", "write"} {
		t.Run(failure, func(t *testing.T) {
			op := publicSoftOp()
			op.Status.Phase = v1alpha3.OperationPhaseInProgress
			op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{
				{
					MachineRef: "machine-1",
					Phase:      v1alpha3.OperationPhaseInProgress,
					BootID:     "boot-1",
				},
			}
			c := fake.NewClientBuilder().WithScheme(fakeMachineOperationClient().Scheme()).
				WithStatusSubresource(&v1alpha3.MachineOperation{}).
				WithObjects(op).WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if failure == "list" {
						return errors.New("list failed")
					}

					return c.List(ctx, list, opts...)
				},
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if failure == "write" {
						return errors.New("write failed")
					}

					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			}).Build()

			host := &fakeHostRebooter{bootID: "boot-2"}
			if failure == "boot" {
				host.bootErr = errors.New("boot failed")
			}

			h := publicHandler(t, c, c, host, &atomic.Bool{})

			require.Error(t, h.PublishStartup(t.Context()))
			assert.Equal(t, op.Status, publicOperation(t, c).Status)
			assert.Zero(t, host.calls)
		})
	}
}
