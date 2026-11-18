// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestNativeHostSoftRebootUsesMachineSnapshot(t *testing.T) {
	for _, blocker := range []bool{false, true} {
		name := "generation"
		if blocker {
			name = "labels"
		}

		t.Run(name, func(t *testing.T) {
			machine := &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-machine",
					Generation: 4,
					Labels:     map[string]string{"rack": "a"},
				},
			}
			objects := []client.Object{machine, hostSoftRebootOperation("soft")}

			if blocker {
				oob := outOfBandOperation("oob", "", v1alpha3.OperationHostPowerOff)
				oob.Spec.MachineSelector = &metav1.LabelSelector{
					MatchLabels: map[string]string{"rack": "a"},
				}
				objects = append(objects, oob)
			}

			machineReads := 0
			c := fake.NewClientBuilder().WithScheme(fakeScheme()).
				WithStatusSubresource(&v1alpha3.MachineOperation{}).
				WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*v1alpha3.Machine); ok {
						machineReads++
					}

					return c.Get(ctx, key, obj, opts...)
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					var latest v1alpha3.Machine
					if err := c.Get(ctx, client.ObjectKey{Name: "test-machine"}, &latest); err != nil {
						return err
					}

					latest.Generation = 9

					latest.Labels["rack"] = "b"
					if err := c.Update(ctx, &latest); err != nil {
						return err
					}

					return c.List(ctx, list, opts...)
				},
			}).Build()
			host := &fakeNodeOperator{bootID: "boot-1"}
			r := newTestMachinaMachineOperationReconciler(t, c, host)

			result, err := r.ReconcileMachineOperation(context.Background(), "soft")
			require.NoError(t, err)
			require.Equal(t, 1, machineReads)

			status := getMachineOperation(t, c, "soft").Status
			if blocker {
				require.Equal(t, 10*time.Second, result.RequeueAfter)
				require.Zero(t, host.rebootCalls)
				require.Equal(t, "waiting for out-of-band host operation oob", status.Message)
			} else {
				require.Equal(t, 1, host.rebootCalls)
				require.Equal(t, int64(4), status.ObservedMachineGeneration)
				require.Len(t, status.Targets, 1)
				require.Equal(t, int64(4), status.Targets[0].ObservedGeneration)
			}
		})
	}
}
