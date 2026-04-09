// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package lifecycle

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

	v1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
)

func TestMain(m *testing.M) {
	ctrl.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))
	os.Exit(m.Run())
}

func TestReimageTimeout(t *testing.T) {
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-timeout", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			Operations: &v1alpha3.OperationsSpec{
				RebootCounter:  1,
				ReimageCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Operations: &v1alpha3.OperationsStatus{RebootCounter: 1},
			Conditions: []metav1.Condition{
				{
					Type:               v1alpha3.MachineConditionReimaged,
					Status:             metav1.ConditionFalse,
					Reason:             "Pending",
					Message:            "image=test-image",
					LastTransitionTime: metav1.NewTime(time.Now().Add(-35 * time.Minute)),
				},
			},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-timeout", Namespace: "default"}}

	_, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated v1alpha3.Machine
	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	reimagedCond := meta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineConditionReimaged)
	if reimagedCond != nil {
		t.Fatalf("expected Reimaged condition to be removed, got %+v", reimagedCond)
	}

	if updated.Status.Operations.RebootCounter != 0 {
		t.Fatalf("expected rebootCounter=0 after timeout decrement, got %d", updated.Status.Operations.RebootCounter)
	}
}

func TestReimageTimeoutNotYetExpired(t *testing.T) {
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-not-expired", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			Operations: &v1alpha3.OperationsSpec{
				RebootCounter:  1,
				ReimageCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Operations: &v1alpha3.OperationsStatus{RebootCounter: 1},
			Conditions: []metav1.Condition{
				{
					Type:               v1alpha3.MachineConditionReimaged,
					Status:             metav1.ConditionFalse,
					Reason:             "Pending",
					Message:            "image=test-image",
					LastTransitionTime: metav1.NewTime(time.Now().Add(-20 * time.Minute)),
				},
			},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-not-expired", Namespace: "default"}}

	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter == 0 {
		t.Fatal("expected RequeueAfter for not-yet-expired reimage timeout")
	}

	if result.RequeueAfter > 11*time.Minute {
		t.Fatalf("expected RequeueAfter <= 11min, got %v", result.RequeueAfter)
	}

	var updated v1alpha3.Machine
	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	if updated.Status.Operations.RebootCounter != 1 {
		t.Fatalf("expected rebootCounter unchanged at 1, got %d", updated.Status.Operations.RebootCounter)
	}
}

func TestNoOpWithoutPendingReimage(t *testing.T) {
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-noop", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			Operations: &v1alpha3.OperationsSpec{
				RebootCounter:  1,
				ReimageCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Operations: &v1alpha3.OperationsStatus{
				RebootCounter:  1,
				ReimageCounter: 1,
			},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-noop", Namespace: "default"}}

	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue, got RequeueAfter=%v", result.RequeueAfter)
	}
}

func TestNoOpWhenReimageSucceeded(t *testing.T) {
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-succeeded", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			Operations: &v1alpha3.OperationsSpec{
				RebootCounter:  1,
				ReimageCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Operations: &v1alpha3.OperationsStatus{RebootCounter: 1},
			Conditions: []metav1.Condition{
				{
					Type:               v1alpha3.MachineConditionReimaged,
					Status:             metav1.ConditionTrue,
					Reason:             "Succeeded",
					LastTransitionTime: metav1.NewTime(time.Now().Add(-20 * time.Minute)),
				},
			},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-succeeded", Namespace: "default"}}

	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Fatalf("expected no requeue for Reimaged=True, got RequeueAfter=%v", result.RequeueAfter)
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
