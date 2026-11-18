// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon_test

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	daemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

type externalNodeTarget struct{}

func (externalNodeTarget) Reference() string { return "machine-1" }
func (externalNodeTarget) ObservedGeneration(context.Context, client.Reader) (int64, error) {
	return 0, nil
}

func (externalNodeTarget) MatchSelector(ctx context.Context, r client.Reader, _ v1alpha3.OperationKind, selector labels.Selector) (bool, error) {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: "node-1"}, &node); err != nil {
		return false, err
	}

	return selector.Matches(labels.Set(node.Labels)), nil
}

type externalHost struct {
	bootID string
	calls  int
}

func (h *externalHost) BootID() (string, error) { return h.bootID, nil }
func (h *externalHost) RebootHost(context.Context, *slog.Logger) error {
	h.calls++
	return nil
}

func TestHostSoftRebootExternalNodeSmoke(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha3.AddToScheme(scheme))

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			Labels: map[string]string{"rack": "a"},
		},
	}
	op := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "soft"},
		Spec: v1alpha3.MachineOperationSpec{
			OperationKind: v1alpha3.OperationHostSoftReboot,
			MachineSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"rack": "a"},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1alpha3.MachineOperation{}).
		WithObjects(node, op).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*v1alpha3.Machine); ok {
				t.Fatal("unexpected Machina Machine read")
			}

			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	host := &externalHost{bootID: "boot-1"}
	flag := &atomic.Bool{}
	h, err := daemon.NewHostSoftRebootHandler(c, c, slog.New(slog.NewTextHandler(io.Discard, nil)), externalNodeTarget{}, host, flag)
	require.NoError(t, err)
	r, err := daemon.NewMachinaMachineOperationReconciler(c, "machine-1", "node-1", daemon.MachineOperationHandlers{v1alpha3.OperationHostSoftReboot: h.Reconcile})
	require.NoError(t, err)

	_, err = r.ReconcileMachineOperation(t.Context(), "soft")
	require.NoError(t, err)
	require.Equal(t, 1, host.calls)
	require.True(t, flag.Load())

	var started v1alpha3.MachineOperation
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: "soft"}, &started))
	require.Len(t, started.Status.Targets, 1)
	require.Equal(t, "machine-1", started.Status.Targets[0].MachineRef)
	require.Equal(t, "boot-1", started.Status.Targets[0].BootID)
	require.Zero(t, started.Status.ObservedMachineGeneration)
	require.Zero(t, started.Status.Targets[0].ObservedGeneration)

	host.bootID = "boot-2"

	require.NoError(t, h.PublishStartup(t.Context()))
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: "soft"}, &started))
	require.Equal(t, v1alpha3.OperationPhaseComplete, started.Status.Phase)
	require.Equal(t, v1alpha3.OperationPhaseComplete, started.Status.Targets[0].Phase)
}
