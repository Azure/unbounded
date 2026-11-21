// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// newMachineOperationListClient returns a fake client that records the options
// of every MachineOperation list, and a function that returns what it recorded.
// A non-nil listErr replaces the list result.
func newMachineOperationListClient(t *testing.T, listErr error, objs ...client.Object) (client.Client, func() []client.ListOptions) {
	t.Helper()

	scheme := runtime.NewScheme()
	utilruntime.Must(machinav1alpha3.AddToScheme(scheme))

	var recorded []client.ListOptions

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
		WithIndex(
			&machinav1alpha3.MachineOperation{},
			machinav1alpha3.MachineOperationMachineRefField,
			func(obj client.Object) []string {
				return []string{obj.(*machinav1alpha3.MachineOperation).Spec.MachineRef}
			},
		).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*machinav1alpha3.MachineOperationList); !ok {
					return fmt.Errorf("unexpected list type %T", list)
				}

				var options client.ListOptions

				options.ApplyOptions(opts)
				recorded = append(recorded, options)

				if listErr != nil {
					return listErr
				}

				return c.List(ctx, list, opts...)
			},
		}).
		Build()

	return c, func() []client.ListOptions { return recorded }
}

func newTestHostSoftRebootHandler(t *testing.T, c client.Client, reader client.Reader, machineRefSelectable bool) *HostSoftRebootHandler {
	t.Helper()

	h, err := NewHostSoftRebootHandler(
		c,
		reader,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		rebootTestTarget{},
		&fakeHostRebooter{},
		&atomic.Bool{},
		machineRefSelectable,
	)
	require.NoError(t, err)

	return h
}

func newMachineOperation(name, machineRef string, kind machinav1alpha3.OperationKind) *machinav1alpha3.MachineOperation {
	return &machinav1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: machinav1alpha3.MachineOperationSpec{
			MachineRef:    machineRef,
			OperationKind: kind,
		},
	}
}

func newSelectorMachineOperation(name string) *machinav1alpha3.MachineOperation {
	return &machinav1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: machinav1alpha3.MachineOperationSpec{
			MachineSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"site": "site-a"}},
			OperationKind:   machinav1alpha3.OperationHostPowerOff,
		},
	}
}

func ownMachineOperation() *machinav1alpha3.MachineOperation {
	return newMachineOperation("op-own", "machine-1", machinav1alpha3.OperationHostPowerOff)
}

func otherMachineOperation() *machinav1alpha3.MachineOperation {
	return newMachineOperation("op-other", "machine-2", machinav1alpha3.OperationHostPowerOff)
}

func machineOperationNames(ops []machinav1alpha3.MachineOperation) []string {
	names := make([]string, len(ops))
	for i := range ops {
		names[i] = ops[i].Name
	}

	return names
}

func TestMachineRefSelectable(t *testing.T) {
	t.Parallel()

	t.Run("selectable", func(t *testing.T) {
		t.Parallel()

		c, recorded := newMachineOperationListClient(t, nil)

		selectable, err := MachineRefSelectable(t.Context(), c, "machine-1")
		require.NoError(t, err)
		assert.True(t, selectable)

		options := recorded()
		require.Len(t, options, 1)
		assert.Equal(t, int64(1), options[0].Limit)
		require.NotNil(t, options[0].FieldSelector)
		assert.Equal(t, "spec.machineRef=machine-1", options[0].FieldSelector.String())
	})

	t.Run("bad request", func(t *testing.T) {
		t.Parallel()

		c, _ := newMachineOperationListClient(t, apierrors.NewBadRequest("field label not supported"))

		selectable, err := MachineRefSelectable(t.Context(), c, "machine-1")
		require.NoError(t, err)
		assert.False(t, selectable)
	})

	t.Run("other error", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("apiserver unavailable")
		c, _ := newMachineOperationListClient(t, wantErr)

		selectable, err := MachineRefSelectable(t.Context(), c, "machine-1")
		assert.False(t, selectable)
		require.ErrorIs(t, err, wantErr)
	})
}

// Both live MachineOperation lists carry the field selector only when the API
// server supports it. The selector is what keeps the traffic to one agent
// independent of the size of the fleet.
func TestHostSoftRebootLiveListsUseMachineRefSelection(t *testing.T) {
	t.Parallel()

	lists := map[string]func(context.Context, *HostSoftRebootHandler) error{
		"unfinished out-of-band host operation": func(ctx context.Context, h *HostSoftRebootHandler) error {
			_, err := h.unfinishedOutOfBandHostOperation(ctx)
			return err
		},
		"publish startup": func(ctx context.Context, h *HostSoftRebootHandler) error {
			return h.PublishStartup(ctx)
		},
	}

	for name, list := range lists {
		for _, selectable := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/selectable=%t", name, selectable), func(t *testing.T) {
				t.Parallel()

				c, recorded := newMachineOperationListClient(t, nil, ownMachineOperation(), otherMachineOperation(), newSelectorMachineOperation("op-selector"))
				h := newTestHostSoftRebootHandler(t, c, c, selectable)

				require.NoError(t, list(t.Context(), h))

				options := recorded()
				require.Len(t, options, 1)

				if selectable {
					require.NotNil(t, options[0].FieldSelector)
					assert.Equal(t, "spec.machineRef=machine-1", options[0].FieldSelector.String())
				} else {
					assert.Nil(t, options[0].FieldSelector)
				}
			})
		}
	}
}

// With selection supported, an operation for another machine never reaches the
// agent. In fallback mode it does, and the existing client-side check ignores
// it.
func TestHostSoftRebootListsSeeOnlyThisMachinesOperationsWhenSelectable(t *testing.T) {
	t.Parallel()

	t.Run("selectable", func(t *testing.T) {
		t.Parallel()

		c, _ := newMachineOperationListClient(t, nil, ownMachineOperation(), otherMachineOperation(), newSelectorMachineOperation("op-selector"))
		h := newTestHostSoftRebootHandler(t, c, c, true)

		ops, err := h.listMachineOperations(t.Context(), c)
		require.NoError(t, err)
		assert.Equal(t, []string{"op-own"}, machineOperationNames(ops))

		blocker, err := h.unfinishedOutOfBandHostOperation(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "op-own", blocker)
	})

	t.Run("selectable never sees an operation for another machine", func(t *testing.T) {
		t.Parallel()

		c, _ := newMachineOperationListClient(t, nil, otherMachineOperation(), newSelectorMachineOperation("op-selector"))
		h := newTestHostSoftRebootHandler(t, c, c, true)

		ops, err := h.listMachineOperations(t.Context(), c)
		require.NoError(t, err)
		assert.Empty(t, ops)

		blocker, err := h.unfinishedOutOfBandHostOperation(t.Context())
		require.NoError(t, err)
		assert.Empty(t, blocker)
	})

	t.Run("fallback sees every operation", func(t *testing.T) {
		t.Parallel()

		c, _ := newMachineOperationListClient(t, nil, ownMachineOperation(), otherMachineOperation(), newSelectorMachineOperation("op-selector"))
		h := newTestHostSoftRebootHandler(t, c, c, false)

		ops, err := h.listMachineOperations(t.Context(), c)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"op-own", "op-other", "op-selector"}, machineOperationNames(ops))

		blocker, err := h.unfinishedOutOfBandHostOperation(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "op-own", blocker)
	})

	t.Run("fallback sees an operation for another machine and ignores it", func(t *testing.T) {
		t.Parallel()

		c, _ := newMachineOperationListClient(t, nil, otherMachineOperation(), newSelectorMachineOperation("op-selector"))
		h := newTestHostSoftRebootHandler(t, c, c, false)

		ops, err := h.listMachineOperations(t.Context(), c)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"op-other", "op-selector"}, machineOperationNames(ops))

		blocker, err := h.unfinishedOutOfBandHostOperation(t.Context())
		require.NoError(t, err)
		assert.Empty(t, blocker)
	})
}
