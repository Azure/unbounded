// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestMachineOperationReconciler_CompletesSupportedOperation(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	machine.Generation = 3
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ignored", Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseComplete, updated.Status.Phase)
	require.NotNil(t, updated.Status.StartedAt)
	require.NotNil(t, updated.Status.CompletedAt)
	require.Equal(t, machine.Generation, updated.Status.ObservedMachineGeneration)
	require.Equal(t, []string{"HostReboot:machine-1:azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/machine-1"}, provider.calls)

	cond := apimeta.FindStatusCondition(updated.Status.Conditions, OperationConditionCompleted)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
}

func TestMachineOperationReconciler_DoesNotReexecuteInProgressOperation(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	op.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
	op.Status.StartedAt = ptrTo(fixedOperationNow())
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, provider.calls)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	require.Nil(t, updated.Status.CompletedAt)
}

func TestMachineOperationReconciler_UnsupportedOperationIsIgnored(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	machine := newExternalMachine("machine-1", unboundedv1alpha3.ExternalProviderAzureVM)
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationNodeReboot)
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, provider.calls)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Empty(t, updated.Status.Phase)
}

func TestMachineOperationReconciler_SelectorOperationIsIgnored(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	op := newMachineOperation("op-1", "", unboundedv1alpha3.OperationHostReboot)
	op.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"rack": "a"}}
	provider := &recordingProvider{provider: unboundedv1alpha3.ExternalProviderAzureVM, supported: map[unboundedv1alpha3.OperationKind]bool{unboundedv1alpha3.OperationHostReboot: true}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(op).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{provider}, Now: fixedOperationNow}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, provider.calls)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Empty(t, updated.Status.Phase)
}

func TestMachineOperationReconciler_FailsMissingMachine(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	op := newMachineOperation("op-1", "missing", unboundedv1alpha3.OperationHostReboot)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(op).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Providers: []Provider{&recordingProvider{}}, Now: fixedOperationNow}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "not found")
}

func TestMachineOperationReconciler_DeletesExpiredTerminalOperation(t *testing.T) {
	t.Parallel()

	s := newOperationTestScheme(t)
	completedAt := metav1.NewTime(time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC))
	ttl := int32(30)
	op := newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot)
	op.Spec.TTLSecondsAfterFinished = &ttl
	op.Status.Phase = unboundedv1alpha3.OperationPhaseComplete
	op.Status.CompletedAt = &completedAt

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(op).WithStatusSubresource(op).Build()
	reconciler := &MachineOperationReconciler{Client: c, Now: func() metav1.Time {
		return metav1.NewTime(completedAt.Add(31 * time.Second))
	}}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "op-1"}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	var updated unboundedv1alpha3.MachineOperation

	err = c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated)
	require.Error(t, err)
}

func TestShouldReconcileOperation(t *testing.T) {
	t.Parallel()

	completedAt := metav1.Now()
	ttl := int32(30)

	tests := []struct {
		name string
		op   *unboundedv1alpha3.MachineOperation
		want bool
	}{
		{
			name: "non terminal",
			op:   newMachineOperation("op-1", "machine-1", unboundedv1alpha3.OperationHostReboot),
			want: true,
		},
		{
			name: "terminal without ttl",
			op: &unboundedv1alpha3.MachineOperation{
				Status: unboundedv1alpha3.MachineOperationStatus{Phase: unboundedv1alpha3.OperationPhaseComplete},
			},
			want: false,
		},
		{
			name: "terminal with ttl and completion time",
			op: &unboundedv1alpha3.MachineOperation{
				Spec: unboundedv1alpha3.MachineOperationSpec{TTLSecondsAfterFinished: &ttl},
				Status: unboundedv1alpha3.MachineOperationStatus{
					Phase:       unboundedv1alpha3.OperationPhaseComplete,
					CompletedAt: &completedAt,
				},
			},
			want: true,
		},
		{
			name: "terminal with ttl but no completion time",
			op: &unboundedv1alpha3.MachineOperation{
				Spec:   unboundedv1alpha3.MachineOperationSpec{TTLSecondsAfterFinished: &ttl},
				Status: unboundedv1alpha3.MachineOperationStatus{Phase: unboundedv1alpha3.OperationPhaseComplete},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, shouldReconcileOperation(tt.op))
		})
	}
}

type recordingProvider struct {
	provider  string
	supported map[unboundedv1alpha3.OperationKind]bool
	calls     []string
	err       error
}

func (p *recordingProvider) Name() string {
	return p.provider
}

func (p *recordingProvider) Supports(operation unboundedv1alpha3.OperationKind) bool {
	return p.supported[operation]
}

func (p *recordingProvider) Execute(_ context.Context, request OperationRequest) error {
	p.calls = append(p.calls, fmt.Sprintf("%s:%s:%s", request.Operation, request.Machine.Name, request.ProviderID))
	return p.err
}

func newOperationTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))

	return s
}

func newMachineOperation(name, machineRef string, operation unboundedv1alpha3.OperationKind) *unboundedv1alpha3.MachineOperation {
	return &unboundedv1alpha3.MachineOperation{
		ObjectMeta: metav1ObjectMeta(name),
		Spec: unboundedv1alpha3.MachineOperationSpec{
			MachineRef:    machineRef,
			OperationKind: operation,
		},
	}
}

func newExternalMachine(name, provider string) *unboundedv1alpha3.Machine {
	return &unboundedv1alpha3.Machine{
		ObjectMeta: metav1ObjectMeta(name),
		Spec: unboundedv1alpha3.MachineSpec{
			Provider:   provider,
			ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/" + name,
		},
	}
}

func fixedOperationNow() metav1.Time {
	return metav1.NewTime(time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC))
}

func ptrTo[T any](value T) *T {
	return &value
}

func metav1ObjectMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name}
}
