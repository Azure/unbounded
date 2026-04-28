// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cloudinit

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestMain(m *testing.M) {
	ctrl.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))
	os.Exit(m.Run())
}

func TestTimeoutRunningCondition(t *testing.T) {
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-timeout"},
		Status: v1alpha3.MachineStatus{
			Conditions: []metav1.Condition{
				{
					Type:               v1alpha3.MachineConditionCloudInitDone,
					Status:             metav1.ConditionFalse,
					Reason:             "Running",
					Message:            "stage \"modules-config\" started",
					LastTransitionTime: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
				},
			},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	reconciler := &Reconciler{Client: fc}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "machine-timeout"}}

	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue after timeout, got RequeueAfter=%v", result.RequeueAfter)
	}

	var updated v1alpha3.Machine
	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineConditionCloudInitDone)
	if cond == nil {
		t.Fatal("expected CloudInitDone condition to exist")
	}

	if cond.Status != metav1.ConditionUnknown {
		t.Fatalf("expected condition status Unknown, got %s", cond.Status)
	}

	if cond.Reason != "TimedOut" {
		t.Fatalf("expected reason TimedOut, got %s", cond.Reason)
	}
}

func TestTimeoutExactBoundary(t *testing.T) {
	// A condition at exactly the timeout boundary should be timed out
	// (elapsed >= cloudInitTimeout triggers the timeout).
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-boundary"},
		Status: v1alpha3.MachineStatus{
			Conditions: []metav1.Condition{
				{
					Type:               v1alpha3.MachineConditionCloudInitDone,
					Status:             metav1.ConditionFalse,
					Reason:             "Running",
					Message:            "stage \"modules-config\" started",
					LastTransitionTime: metav1.NewTime(time.Now().Add(-cloudInitTimeout)),
				},
			},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	reconciler := &Reconciler{Client: fc}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "machine-boundary"}}

	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue at exact boundary, got RequeueAfter=%v", result.RequeueAfter)
	}

	var updated v1alpha3.Machine
	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineConditionCloudInitDone)
	if cond == nil {
		t.Fatal("expected CloudInitDone condition to exist")
	}

	if cond.Status != metav1.ConditionUnknown {
		t.Fatalf("expected condition status Unknown at boundary, got %s", cond.Status)
	}
}

func TestNotYetTimedOut(t *testing.T) {
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-not-expired"},
		Status: v1alpha3.MachineStatus{
			Conditions: []metav1.Condition{
				{
					Type:               v1alpha3.MachineConditionCloudInitDone,
					Status:             metav1.ConditionFalse,
					Reason:             "Running",
					Message:            "stage \"modules-config\" started",
					LastTransitionTime: metav1.NewTime(time.Now().Add(-2 * time.Minute)),
				},
			},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	reconciler := &Reconciler{Client: fc}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "machine-not-expired"}}

	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter == 0 {
		t.Fatal("expected RequeueAfter for not-yet-expired timeout")
	}

	if result.RequeueAfter > 4*time.Minute {
		t.Fatalf("expected RequeueAfter <= 4min, got %v", result.RequeueAfter)
	}

	var updated v1alpha3.Machine
	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineConditionCloudInitDone)
	if cond == nil {
		t.Fatal("expected CloudInitDone condition to still exist")
	}

	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected condition status False (unchanged), got %s", cond.Status)
	}

	if cond.Reason != "Running" {
		t.Fatalf("expected reason Running (unchanged), got %s", cond.Reason)
	}
}

func TestNoCondition(t *testing.T) {
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-no-cond"},
		Status:     v1alpha3.MachineStatus{},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	reconciler := &Reconciler{Client: fc}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "machine-no-cond"}}

	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue when no condition, got RequeueAfter=%v", result.RequeueAfter)
	}
}

func TestConditionAlreadyTrue(t *testing.T) {
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-true"},
		Status: v1alpha3.MachineStatus{
			Conditions: []metav1.Condition{
				{
					Type:               v1alpha3.MachineConditionCloudInitDone,
					Status:             metav1.ConditionTrue,
					Reason:             "Succeeded",
					Message:            "cloud-init completed successfully",
					LastTransitionTime: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
				},
			},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	reconciler := &Reconciler{Client: fc}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "machine-true"}}

	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue for True condition, got RequeueAfter=%v", result.RequeueAfter)
	}
}

func TestConditionAlreadyUnknown(t *testing.T) {
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-unknown"},
		Status: v1alpha3.MachineStatus{
			Conditions: []metav1.Condition{
				{
					Type:               v1alpha3.MachineConditionCloudInitDone,
					Status:             metav1.ConditionUnknown,
					Reason:             "TimedOut",
					Message:            "cloud-init did not complete within 5m0s",
					LastTransitionTime: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
				},
			},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	reconciler := &Reconciler{Client: fc}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "machine-unknown"}}

	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue for Unknown condition, got RequeueAfter=%v", result.RequeueAfter)
	}
}

func TestConditionFalseWithFailedReason(t *testing.T) {
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-failed"},
		Status: v1alpha3.MachineStatus{
			Conditions: []metav1.Condition{
				{
					Type:               v1alpha3.MachineConditionCloudInitDone,
					Status:             metav1.ConditionFalse,
					Reason:             "Failed",
					Message:            "stage \"modules-final\" failed",
					LastTransitionTime: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
				},
			},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	reconciler := &Reconciler{Client: fc}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "machine-failed"}}

	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue for Failed condition, got RequeueAfter=%v", result.RequeueAfter)
	}

	var updated v1alpha3.Machine
	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineConditionCloudInitDone)
	if cond == nil {
		t.Fatal("expected CloudInitDone condition to still exist")
	}

	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected condition status False (unchanged), got %s", cond.Status)
	}

	if cond.Reason != "Failed" {
		t.Fatalf("expected reason Failed (unchanged), got %s", cond.Reason)
	}
}

func TestZeroLastTransitionTime(t *testing.T) {
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-zero-time"},
		Status: v1alpha3.MachineStatus{
			Conditions: []metav1.Condition{
				{
					Type:   v1alpha3.MachineConditionCloudInitDone,
					Status: metav1.ConditionFalse,
					Reason: "Running",
				},
			},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	reconciler := &Reconciler{Client: fc}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "machine-zero-time"}}

	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != cloudInitTimeout {
		t.Fatalf("expected RequeueAfter=%v for zero LastTransitionTime, got %v",
			cloudInitTimeout, result.RequeueAfter)
	}

	// Verify the condition was NOT modified.
	var updated v1alpha3.Machine
	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineConditionCloudInitDone)
	if cond == nil {
		t.Fatal("expected CloudInitDone condition to still exist")
	}

	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected condition status False (unchanged), got %s", cond.Status)
	}
}

func TestUnexpectedReasonTimesOut(t *testing.T) {
	// Any non-Failed reason on a False condition should be subject to
	// timeout, not just "Running".
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-unexpected"},
		Status: v1alpha3.MachineStatus{
			Conditions: []metav1.Condition{
				{
					Type:               v1alpha3.MachineConditionCloudInitDone,
					Status:             metav1.ConditionFalse,
					Reason:             "SomethingElse",
					LastTransitionTime: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
				},
			},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	reconciler := &Reconciler{Client: fc}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "machine-unexpected"}}

	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue after timeout, got RequeueAfter=%v", result.RequeueAfter)
	}

	var updated v1alpha3.Machine
	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineConditionCloudInitDone)
	if cond == nil {
		t.Fatal("expected CloudInitDone condition to exist")
	}

	if cond.Status != metav1.ConditionUnknown {
		t.Fatalf("expected condition status Unknown, got %s", cond.Status)
	}

	if cond.Reason != "TimedOut" {
		t.Fatalf("expected reason TimedOut, got %s", cond.Reason)
	}
}

func TestMachineNotFound(t *testing.T) {
	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	reconciler := &Reconciler{Client: fc}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "nonexistent"}}

	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue for missing machine, got RequeueAfter=%v", result.RequeueAfter)
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	if err := v1alpha3.AddToScheme(s); err != nil {
		t.Fatal(err)
	}

	return s
}
