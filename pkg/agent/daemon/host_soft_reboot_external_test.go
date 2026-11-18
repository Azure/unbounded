// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon_test

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	daemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

type nodeTarget struct{}

func (nodeTarget) Reference() string { return "aks-machine-1" }

func (nodeTarget) ObservedGeneration(context.Context, client.Reader) (int64, error) { return 0, nil }

func (nodeTarget) MatchSelector(ctx context.Context, r client.Reader, _ v1alpha3.OperationKind, selector labels.Selector) (bool, error) {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: "node-1"}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, err
	}

	return selector.Matches(labels.Set(node.Labels)), nil
}

type fakeHost struct {
	bootID  string
	calls   int
	command func()
}

func (h *fakeHost) BootID() (string, error) { return h.bootID, nil }
func (h *fakeHost) RebootHost(context.Context, *slog.Logger) error {
	h.calls++
	h.command()

	return nil
}

func TestHostSoftRebootExternalNodeTarget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ref      string
		selector *metav1.LabelSelector
		node     *corev1.Node
		peer     bool
		want     bool
	}{
		{
			name: "explicit without node",
			ref:  "aks-machine-1",
			want: true,
		},
		{
			name: "selector",
			selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"rack": "a"},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "node-1",
					Generation: 9,
					Labels:     map[string]string{"rack": "a"},
				},
			},
			want: true,
		},
		{
			name: "selector with started peer",
			selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"rack": "a",
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-1",
					Labels: map[string]string{
						"rack": "a",
					},
				},
			},
			peer: true,
			want: true,
		},
		{
			name:     "empty selector without node",
			selector: &metav1.LabelSelector{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, v1alpha3.AddToScheme(scheme))

			op := &v1alpha3.MachineOperation{
				ObjectMeta: metav1.ObjectMeta{Name: "soft"},
				Spec: v1alpha3.MachineOperationSpec{
					MachineRef:      tc.ref,
					MachineSelector: tc.selector,
					OperationKind:   v1alpha3.OperationHostSoftReboot,
				},
			}

			if tc.peer {
				op.Status.Phase = v1alpha3.OperationPhaseInProgress
				op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{
					{
						MachineRef: "other-machine",
						Phase:      v1alpha3.OperationPhaseInProgress,
						BootID:     "peer-boot",
					},
				}
			}

			objects := []client.Object{op}
			if tc.node != nil {
				objects = append(objects, tc.node)
			}

			c := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&v1alpha3.MachineOperation{}).
				WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*v1alpha3.Machine); ok {
						t.Fatal("unexpected Machina Machine read")
					}

					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()
			flag := &atomic.Bool{}
			host := &fakeHost{bootID: "boot-1"}
			host.command = func() {
				var latest v1alpha3.MachineOperation
				require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: "soft"}, &latest))
				assert.True(t, flag.Load())
				assert.Equal(t, v1alpha3.OperationPhaseInProgress, latest.Status.Phase)

				targetIndex := 0
				if tc.peer {
					targetIndex = 1
				}

				require.Len(t, latest.Status.Targets, targetIndex+1)

				if tc.peer {
					assert.Equal(t, op.Status.Targets[0], latest.Status.Targets[0])
				}

				assert.Equal(t, "aks-machine-1", latest.Status.Targets[targetIndex].MachineRef)
				assert.Equal(t, "boot-1", latest.Status.Targets[targetIndex].BootID)
				assert.Zero(t, latest.Status.ObservedMachineGeneration)
				assert.Zero(t, latest.Status.Targets[targetIndex].ObservedGeneration)
			}
			h, err := daemon.NewHostSoftRebootHandler(c, c, slog.New(slog.NewTextHandler(io.Discard, nil)), nodeTarget{}, host, flag)
			require.NoError(t, err)

			_, err = h.Reconcile(t.Context(), daemon.MachineOperation{Name: "soft"})
			require.NoError(t, err)

			if !tc.want {
				assert.Zero(t, host.calls)
				return
			}

			assert.Equal(t, 1, host.calls)
			host.bootID = "boot-2"

			if tc.node != nil {
				require.NoError(t, c.Delete(t.Context(), tc.node))
			}

			require.NoError(t, h.PublishStartup(t.Context()))

			var latest v1alpha3.MachineOperation
			require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: "soft"}, &latest))
			assert.Equal(t, v1alpha3.OperationPhaseComplete, latest.Status.Phase)
		})
	}
}
