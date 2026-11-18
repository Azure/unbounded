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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

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
