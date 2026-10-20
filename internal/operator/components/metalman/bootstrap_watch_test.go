// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package metalman

import (
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Azure/unbounded/internal/operator/component"
)

func TestBootstrapComponentProvidesWatches(t *testing.T) {
	if _, ok := NewBootstrap().(component.WatchProvider); !ok {
		t.Fatal("bootstrap component does not implement component.WatchProvider; nothing would trigger the zero-Site plan")
	}
}

// deliver feeds ev through the watch registered for obj's type, the same way
// the controller would: predicate first, then the singleton handler.
func deliver(t *testing.T, env *component.Env, ev string, obj, old client.Object) int {
	t.Helper()

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[ctrlreconcile.Request]())
	defer queue.ShutDown()

	handler := env.RequestSingleton()

	var found bool

	for _, w := range bootstrapWatches(env) {
		if sameType(w.object, obj) {
			found = true

			switch ev {
			case "create":
				e := event.CreateEvent{Object: obj}
				if w.predicate.Create(e) {
					handler.Create(t.Context(), e, queue)
				}
			case "delete":
				e := event.DeleteEvent{Object: obj}
				if w.predicate.Delete(e) {
					handler.Delete(t.Context(), e, queue)
				}
			case "update":
				e := event.UpdateEvent{ObjectOld: old, ObjectNew: obj}
				if w.predicate.Update(e) {
					handler.Update(t.Context(), e, queue)
				}
			}
		}
	}

	if !found {
		t.Fatalf("no watch registered for %T", obj)
	}

	n := queue.Len()
	if n > 0 {
		req, _ := queue.Get()
		if req.Name != component.SingletonRequestName {
			t.Fatalf("request = %q, want singleton", req.Name)
		}
	}

	return n
}

func sameType(a, b client.Object) bool {
	switch a.(type) {
	case *corev1.ServiceAccount:
		_, ok := b.(*corev1.ServiceAccount)
		return ok
	case *rbacv1.ClusterRoleBinding:
		_, ok := b.(*rbacv1.ClusterRoleBinding)
		return ok
	case *admissionregistrationv1.ValidatingAdmissionPolicy:
		_, ok := b.(*admissionregistrationv1.ValidatingAdmissionPolicy)
		return ok
	case *admissionregistrationv1.ValidatingAdmissionPolicyBinding:
		_, ok := b.(*admissionregistrationv1.ValidatingAdmissionPolicyBinding)
		return ok
	}

	return false
}

func TestBootstrapWatchesEnqueueSingleton(t *testing.T) {
	env := &component.Env{Namespace: "unbounded-system"}
	meta := func(ns, name string, gen int64) metav1.ObjectMeta {
		return metav1.ObjectMeta{Namespace: ns, Name: name, Generation: gen}
	}

	cases := []struct {
		name string
		ev   string
		obj  client.Object
		old  client.Object
		want int
	}{
		{"bootstrap SA create", "create", &corev1.ServiceAccount{ObjectMeta: meta("unbounded-system", BootstrapServiceAccountName, 0)}, nil, 1},
		{"bootstrap SA other namespace", "create", &corev1.ServiceAccount{ObjectMeta: meta("default", BootstrapServiceAccountName, 0)}, nil, 0},
		{"unrelated SA", "create", &corev1.ServiceAccount{ObjectMeta: meta("unbounded-system", "other", 0)}, nil, 0},
		{"bootstrap binding create", "create", &rbacv1.ClusterRoleBinding{ObjectMeta: meta("", BootstrapMachineBindingName, 0)}, nil, 1},
		{"stale auto-approve binding create", "create", &rbacv1.ClusterRoleBinding{ObjectMeta: meta("", StaleAutoApproveBindingName, 0)}, nil, 1},
		{"bootstrap binding update", "update", &rbacv1.ClusterRoleBinding{ObjectMeta: meta("", BootstrapMachineBindingName, 0)}, &rbacv1.ClusterRoleBinding{ObjectMeta: meta("", BootstrapMachineBindingName, 0)}, 1},
		{"unrelated binding", "create", &rbacv1.ClusterRoleBinding{ObjectMeta: meta("", "cluster-admin", 0)}, nil, 0},
		{"policy delete", "delete", &admissionregistrationv1.ValidatingAdmissionPolicy{ObjectMeta: meta("", BootstrapRestrictionPolicyName, 1)}, nil, 1},
		{"policy spec drift", "update", &admissionregistrationv1.ValidatingAdmissionPolicy{ObjectMeta: meta("", BootstrapRestrictionPolicyName, 2)}, &admissionregistrationv1.ValidatingAdmissionPolicy{ObjectMeta: meta("", BootstrapRestrictionPolicyName, 1)}, 1},
		{"policy status only", "update", &admissionregistrationv1.ValidatingAdmissionPolicy{ObjectMeta: meta("", BootstrapRestrictionPolicyName, 1)}, &admissionregistrationv1.ValidatingAdmissionPolicy{ObjectMeta: meta("", BootstrapRestrictionPolicyName, 1)}, 0},
		{"unrelated policy", "delete", &admissionregistrationv1.ValidatingAdmissionPolicy{ObjectMeta: meta("", "other", 1)}, nil, 0},
		{"policy binding delete", "delete", &admissionregistrationv1.ValidatingAdmissionPolicyBinding{ObjectMeta: meta("", BootstrapRestrictionPolicyName, 1)}, nil, 1},
		{"policy binding drift", "update", &admissionregistrationv1.ValidatingAdmissionPolicyBinding{ObjectMeta: meta("", BootstrapRestrictionPolicyName, 3)}, &admissionregistrationv1.ValidatingAdmissionPolicyBinding{ObjectMeta: meta("", BootstrapRestrictionPolicyName, 2)}, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deliver(t, env, tc.ev, tc.obj, tc.old); got != tc.want {
				t.Fatalf("enqueued %d requests, want %d", got, tc.want)
			}
		})
	}
}

func TestBootstrapRestrictionPolicyNameMatchesManifest(t *testing.T) {
	for _, kind := range []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
		supportObject(t, kind, BootstrapRestrictionPolicyName)
	}
}
