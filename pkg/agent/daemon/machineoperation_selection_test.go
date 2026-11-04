// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestMachineRefSelectable(t *testing.T) {
	t.Parallel()

	var probe client.ListOptions

	c := interceptor.NewClient(selectingMachineOperationClient(), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			probe.ApplyOptions(opts)
			return c.List(ctx, list, opts...)
		},
	})

	selectable, err := MachineRefSelectable(context.Background(), c, "machine-1")
	require.NoError(t, err)
	assert.True(t, selectable)

	require.NotNil(t, probe.FieldSelector)
	assert.Equal(t, "spec.machineRef=machine-1", probe.FieldSelector.String())
	assert.Equal(t, int64(1), probe.Limit, "the probe must not download every operation")
}

func TestMachineRefSelectableFalseWhenAPIServerRejectsSelector(t *testing.T) {
	t.Parallel()

	// An API server before Kubernetes 1.31, or one serving a CRD without the
	// selectable field, rejects the selector as a bad request.
	c := interceptor.NewClient(selectingMachineOperationClient(), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return apierrors.NewBadRequest(`unable to parse requirement: field label not supported: spec.machineRef`)
		},
	})

	selectable, err := MachineRefSelectable(context.Background(), c, "machine-1")
	require.NoError(t, err)
	assert.False(t, selectable)
}

func TestMachineRefSelectableReturnsOtherErrors(t *testing.T) {
	t.Parallel()

	listErr := apierrors.NewServiceUnavailable("etcd unavailable")
	c := interceptor.NewClient(selectingMachineOperationClient(), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return listErr
		},
	})

	selectable, err := MachineRefSelectable(context.Background(), c, "machine-1")
	require.ErrorIs(t, err, listErr)
	assert.False(t, selectable)
}

func TestListMachineOperationsSelectsOnAPIServer(t *testing.T) {
	t.Parallel()

	var selectors []string

	c := interceptor.NewClient(selectingMachineOperationClient(machineOperationsForSelection()...), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			var o client.ListOptions
			o.ApplyOptions(opts)

			if o.FieldSelector == nil {
				return errors.New("listed every MachineOperation")
			}

			selectors = append(selectors, o.FieldSelector.String())

			return c.List(ctx, list, opts...)
		},
	})

	ops, err := ListMachineOperations(context.Background(), c, "machine-1", true)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"own", "selector"}, machineOperationNames(ops))
	assert.ElementsMatch(t, []string{"spec.machineRef=machine-1", "spec.machineRef="}, selectors)
}

func TestListMachineOperationsFiltersWhenNotSelectable(t *testing.T) {
	t.Parallel()

	// Without an index the fake client, like an API server that cannot select
	// by spec.machineRef, fails any list that selects by it.
	c := fakeMachineOperationClient(machineOperationsForSelection()...)

	ops, err := ListMachineOperations(context.Background(), c, "machine-1", false)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"own", "selector"}, machineOperationNames(ops))
}

func TestListMachineOperationsReturnsListError(t *testing.T) {
	t.Parallel()

	listErr := errors.New("list failed")
	c := interceptor.NewClient(selectingMachineOperationClient(), interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			return listErr
		},
	})

	for _, selectable := range []bool{true, false} {
		_, err := ListMachineOperations(context.Background(), c, "machine-1", selectable)
		require.ErrorIs(t, err, listErr, "selectable=%t", selectable)
	}
}

func TestListMachineOperationsRequiresMachineName(t *testing.T) {
	t.Parallel()

	// An empty name would select only selector operations, or, unfiltered,
	// keep operations for every Machine.
	c := selectingMachineOperationClient(machineOperationsForSelection()...)

	for _, selectable := range []bool{true, false} {
		_, err := ListMachineOperations(context.Background(), c, "", selectable)
		require.Error(t, err, "selectable=%t", selectable)
	}
}

func TestAddMachineOperationCachesRequiresMachineName(t *testing.T) {
	t.Parallel()

	_, err := AddMachineOperationCaches(nil, "")
	require.Error(t, err)
}

// machineOperationsForSelection returns operations for machine-1, for
// another Machine, and for Machines selected by label.
func machineOperationsForSelection() []client.Object {
	return []client.Object{
		&machinav1alpha3.MachineOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "own"},
			Spec: machinav1alpha3.MachineOperationSpec{
				MachineRef:    "machine-1",
				OperationKind: machinav1alpha3.OperationNodeReboot,
			},
		},
		&machinav1alpha3.MachineOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "other"},
			Spec: machinav1alpha3.MachineOperationSpec{
				MachineRef:    "machine-2",
				OperationKind: machinav1alpha3.OperationNodeReboot,
			},
		},
		&machinav1alpha3.MachineOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "selector"},
			Spec: machinav1alpha3.MachineOperationSpec{
				MachineSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "a"}},
				OperationKind:   machinav1alpha3.OperationNodeReboot,
			},
		},
	}
}

// selectingMachineOperationClient returns a fake client that, like an API
// server serving the current MachineOperation CRD, selects MachineOperations
// by spec.machineRef.
func selectingMachineOperationClient(objs ...client.Object) client.WithWatch {
	return fake.NewClientBuilder().
		WithScheme(fakeMachineOperationClient().Scheme()).
		WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
		WithIndex(&machinav1alpha3.MachineOperation{}, machinav1alpha3.MachineOperationMachineRefField, func(obj client.Object) []string {
			return []string{obj.(*machinav1alpha3.MachineOperation).Spec.MachineRef}
		}).
		WithObjects(objs...).
		Build()
}

func machineOperationNames(ops []machinav1alpha3.MachineOperation) []string {
	names := make([]string, 0, len(ops))
	for i := range ops {
		names = append(names, ops[i].Name)
	}

	return names
}
