// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestNewMachinaMachineOperationReconcilerValidatesInputs(t *testing.T) {
	t.Parallel()

	c := fakeMachineOperationClient()

	if _, err := NewMachinaMachineOperationReconciler(nil, "machine-1", "node-1", MachineOperationHandlers{machinav1alpha3.OperationNodeReboot: noopMachineOperationTarget}); err == nil {
		t.Fatal("NewMachinaMachineOperationReconciler nil client error = nil")
	}

	if _, err := NewMachinaMachineOperationReconcilerWithReader(c, nil, "machine-1", "node-1", MachineOperationHandlers{machinav1alpha3.OperationNodeReboot: noopMachineOperationTarget}); err == nil {
		t.Fatal("NewMachinaMachineOperationReconcilerWithReader nil reader error = nil")
	}

	if _, err := NewMachinaMachineOperationReconciler(c, "", "node-1", MachineOperationHandlers{machinav1alpha3.OperationNodeReboot: noopMachineOperationTarget}); err == nil {
		t.Fatal("NewMachinaMachineOperationReconciler empty machine name error = nil")
	}

	if _, err := NewMachinaMachineOperationReconciler(c, "machine-1", "", MachineOperationHandlers{machinav1alpha3.OperationNodeReboot: noopMachineOperationTarget}); err == nil {
		t.Fatal("NewMachinaMachineOperationReconciler empty node name error = nil")
	}

	if _, err := NewMachinaMachineOperationReconciler(c, "machine-1", "node-1", nil); err == nil {
		t.Fatal("NewMachinaMachineOperationReconciler nil targets error = nil")
	}

	if _, err := NewMachinaMachineOperationReconciler(c, "machine-1", "node-1", MachineOperationHandlers{machinav1alpha3.OperationNodeReboot: nil}); err == nil {
		t.Fatal("NewMachinaMachineOperationReconciler nil target error = nil")
	}
}

func TestMachinaMachineOperationReconcilerDispatchesTarget(t *testing.T) {
	t.Parallel()

	op := &machinav1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: machinav1alpha3.MachineOperationSpec{
			OperationKind: machinav1alpha3.OperationNodeReboot,
		},
	}
	machine := &machinav1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "machine-1", Generation: 7}}
	target := &recordingMachineOperationTargetState{result: ctrl.Result{RequeueAfter: time.Second}}

	reconciler, err := NewMachinaMachineOperationReconciler(
		fakeMachineOperationClient(machine, op),
		"machine-1",
		"node-1",
		MachineOperationHandlers{machinav1alpha3.OperationNodeReboot: target.reconcile},
	)
	if err != nil {
		t.Fatalf("NewMachinaMachineOperationReconciler: %v", err)
	}

	result, err := reconciler.ReconcileMachineOperation(context.Background(), "op-1")
	if err != nil {
		t.Fatalf("ReconcileMachineOperation: %v", err)
	}

	if result.RequeueAfter == 0 {
		t.Fatalf("result = %v, want requeue", result)
	}

	if !target.called || target.operation.Name != "op-1" {
		t.Fatalf("operation = %#v", target.operation)
	}

	if target.operation.Kind != machinav1alpha3.OperationNodeReboot {
		t.Fatalf("kind = %v", target.operation.Kind)
	}
}

func TestMachinaMachineOperationReconcilerUsesUncachedTerminalState(t *testing.T) {
	t.Parallel()

	pending := &machinav1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec:       machinav1alpha3.MachineOperationSpec{OperationKind: machinav1alpha3.OperationNodeReboot},
	}
	terminal := pending.DeepCopy()
	terminal.Status.Phase = machinav1alpha3.OperationPhaseComplete
	target := &recordingMachineOperationTargetState{}

	reconciler, err := NewMachinaMachineOperationReconcilerWithReader(
		fakeMachineOperationClient(pending),
		fakeMachineOperationClient(terminal),
		"machine-1",
		"node-1",
		MachineOperationHandlers{machinav1alpha3.OperationNodeReboot: target.reconcile},
	)
	if err != nil {
		t.Fatalf("NewMachinaMachineOperationReconcilerWithReader: %v", err)
	}

	if _, err := reconciler.ReconcileMachineOperation(context.Background(), "op-1"); err != nil {
		t.Fatalf("ReconcileMachineOperation: %v", err)
	}

	if target.called {
		t.Fatal("terminal operation dispatched from stale cached state")
	}
}

func TestMachinaMachineOperationReconcilerSkipsTerminalAndIgnoresUnsupported(t *testing.T) {
	t.Parallel()

	terminal := &machinav1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "terminal"},
		Spec:       machinav1alpha3.MachineOperationSpec{OperationKind: machinav1alpha3.OperationNodeReboot},
		Status:     machinav1alpha3.MachineOperationStatus{Phase: machinav1alpha3.OperationPhaseComplete},
	}
	unsupported := &machinav1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "unsupported"},
		Spec:       machinav1alpha3.MachineOperationSpec{OperationKind: machinav1alpha3.OperationHostReboot},
	}
	target := &recordingMachineOperationTargetState{}

	reconciler, err := NewMachinaMachineOperationReconciler(
		fakeMachineOperationClient(terminal, unsupported),
		"machine-1",
		"node-1",
		MachineOperationHandlers{machinav1alpha3.OperationNodeReboot: target.reconcile},
	)
	if err != nil {
		t.Fatalf("NewMachinaMachineOperationReconciler: %v", err)
	}

	if _, err := reconciler.ReconcileMachineOperation(context.Background(), "terminal"); err != nil {
		t.Fatalf("ReconcileMachineOperation terminal: %v", err)
	}

	if _, err := reconciler.ReconcileMachineOperation(context.Background(), "unsupported"); err != nil {
		t.Fatalf("ReconcileMachineOperation unsupported: %v", err)
	}

	if target.called {
		t.Fatalf("operation = %#v, want nil", target.operation)
	}

	var updated machinav1alpha3.MachineOperation
	if err := reconciler.client.Get(context.Background(), client.ObjectKey{Name: "unsupported"}, &updated); err != nil {
		t.Fatalf("get unsupported MachineOperation: %v", err)
	}

	if updated.Status.Phase != "" {
		t.Fatalf("unsupported phase = %s, want empty", updated.Status.Phase)
	}
}

func TestMachinaMachineOperationReconcilerReturnsTargetError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("target failed")
	machine := &machinav1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "machine-1", Generation: 7}}
	op := &machinav1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec:       machinav1alpha3.MachineOperationSpec{OperationKind: machinav1alpha3.OperationNodeReboot},
	}

	reconciler, err := NewMachinaMachineOperationReconciler(
		fakeMachineOperationClient(machine, op),
		"machine-1",
		"node-1",
		MachineOperationHandlers{machinav1alpha3.OperationNodeReboot: (&recordingMachineOperationTargetState{err: wantErr}).reconcile},
	)
	if err != nil {
		t.Fatalf("NewMachinaMachineOperationReconciler: %v", err)
	}

	_, err = reconciler.ReconcileMachineOperation(context.Background(), "op-1")
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}

func TestMachinaMachineOperationReconcilerMapsMachineRef(t *testing.T) {
	t.Parallel()

	reconciler := newTestMachinaMachineOperationReconciler(t, fakeMachineOperationClient())
	op := &machinav1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: machinav1alpha3.MachineOperationSpec{
			MachineRef:    "machine-1",
			OperationKind: machinav1alpha3.OperationNodeReboot,
		},
	}

	requests := reconciler.mapMachineOperation(context.Background(), op)
	if len(requests) != 1 {
		t.Fatalf("requests = %#v, want one", requests)
	}

	req, ok := requests[0].machineOperationRequest()
	if !ok || req.Name != "op-1" {
		t.Fatalf("request = %#v", requests[0])
	}
}

func TestMachinaMachineOperationReconcilerMapsNodeSelector(t *testing.T) {
	t.Parallel()

	reconciler := newTestMachinaMachineOperationReconciler(t, fakeMachineOperationClient(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{"pool": "edge"}},
	}))
	op := &machinav1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: machinav1alpha3.MachineOperationSpec{
			MachineSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "edge"}},
			OperationKind:   machinav1alpha3.OperationNodeReboot,
		},
	}

	requests := reconciler.mapMachineOperation(context.Background(), op)
	if len(requests) != 1 {
		t.Fatalf("requests = %#v, want one", requests)
	}
}

func TestMachinaMachineOperationReconcilerMapsMachineSelectorFallback(t *testing.T) {
	t.Parallel()

	reconciler := newTestMachinaMachineOperationReconciler(t, fakeMachineOperationClient(&machinav1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-1", Labels: map[string]string{"pool": "edge"}},
	}))
	op := &machinav1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: machinav1alpha3.MachineOperationSpec{
			MachineSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"pool": "edge"}},
			OperationKind:   machinav1alpha3.OperationNodeReboot,
		},
	}

	requests := reconciler.mapMachineOperation(context.Background(), op)
	if len(requests) != 1 {
		t.Fatalf("requests = %#v, want one", requests)
	}
}

func TestMachinaMachineOperationReconcilerSkipsNonMatchingWatchObjects(t *testing.T) {
	t.Parallel()

	reconciler := newTestMachinaMachineOperationReconciler(t, fakeMachineOperationClient())
	tests := []client.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		&machinav1alpha3.MachineOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "other-machine"},
			Spec: machinav1alpha3.MachineOperationSpec{
				MachineRef:    "machine-2",
				OperationKind: machinav1alpha3.OperationNodeReboot,
			},
		},
		&machinav1alpha3.MachineOperation{
			ObjectMeta: metav1.ObjectMeta{Name: "complete"},
			Spec: machinav1alpha3.MachineOperationSpec{
				MachineRef:    "machine-1",
				OperationKind: machinav1alpha3.OperationNodeReboot,
			},
			Status: machinav1alpha3.MachineOperationStatus{Phase: machinav1alpha3.OperationPhaseComplete},
		},
	}

	for _, obj := range tests {
		if requests := reconciler.mapMachineOperation(context.Background(), obj); len(requests) != 0 {
			t.Fatalf("mapMachineOperation(%T) = %#v, want nil", obj, requests)
		}
	}
}

func TestMachinaMachineOperationReconcilerIgnoresUnsupportedMatchingOperation(t *testing.T) {
	t.Parallel()

	reconciler := newTestMachinaMachineOperationReconciler(t, fakeMachineOperationClient())
	op := &machinav1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "unsupported"},
		Spec: machinav1alpha3.MachineOperationSpec{
			MachineRef:    "machine-1",
			OperationKind: machinav1alpha3.OperationHostReboot,
		},
	}

	requests := reconciler.mapMachineOperation(context.Background(), op)
	if len(requests) != 0 {
		t.Fatalf("requests = %#v, want nil", requests)
	}
}

func newTestMachinaMachineOperationReconciler(t *testing.T, c client.Client) *MachinaMachineOperationReconciler {
	t.Helper()

	reconciler, err := NewMachinaMachineOperationReconciler(
		c,
		"machine-1",
		"node-1",
		MachineOperationHandlers{machinav1alpha3.OperationNodeReboot: noopMachineOperationTarget},
	)
	if err != nil {
		t.Fatalf("NewMachinaMachineOperationReconciler: %v", err)
	}

	return reconciler
}

func noopMachineOperationTarget(context.Context, MachineOperationStore[int64], MachineOperation) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

type recordingMachineOperationTargetState struct {
	operation MachineOperation
	store     MachineOperationStore[int64]
	called    bool
	result    ctrl.Result
	err       error
}

func (t *recordingMachineOperationTargetState) reconcile(
	_ context.Context,
	store MachineOperationStore[int64],
	op MachineOperation,
) (ctrl.Result, error) {
	t.called = true
	t.store = store
	t.operation = op

	return t.result, t.err
}

func fakeMachineOperationClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(machinav1alpha3.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
		WithObjects(objs...).
		Build()
}
