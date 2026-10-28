// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator/component"
)

// renamedObjects are the objects a pre-rename operator applied.
func renamedObjects(ns string, annotations func() map[string]string) []client.Object {
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uuid.NewString()), Annotations: annotations()}
	}
	cluster := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, UID: types.UID(uuid.NewString()), Annotations: annotations()}
	}

	objects := []client.Object{
		&appsv1.DaemonSet{ObjectMeta: meta(renamedDataplane)},
		&corev1.ServiceAccount{ObjectMeta: meta(renamedDataplane)},
		&rbacv1.Role{ObjectMeta: meta(renamedDataplane)},
		&rbacv1.RoleBinding{ObjectMeta: meta(renamedDataplane)},
		&rbacv1.ClusterRole{ObjectMeta: cluster(renamedDataplane)},
		&rbacv1.ClusterRoleBinding{ObjectMeta: cluster(renamedDataplane)},
		&corev1.ConfigMap{ObjectMeta: meta(renamedConfig), Data: map[string]string{"RACER_SHARES": "7", "RACER_DATAPLANE_NVIDIA": "off"}},
	}

	for _, policy := range renamedPolicies {
		objects = append(objects,
			&admissionv1.ValidatingAdmissionPolicy{ObjectMeta: cluster(policy)},
			&admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: cluster(policy)})
	}

	return objects
}

func TestRenamedObjectsAreRemoved(t *testing.T) {
	ns := "custom-system"
	env := testEnv(t, append(renamedObjects(ns, managed), cacheObject("cache"))...)
	initialize(t, env)

	// Guards outlive the old DaemonSet and grants by at least one pass.
	for _, policy := range renamedPolicies {
		require.True(t, exists(t, env, &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: policy}}))
	}

	settle(t, env)

	for _, obj := range renamedObjects(ns, managed) {
		require.False(t, exists(t, env, obj), "%T %s survived", obj, obj.GetName())
	}

	// Administrator settings carry over, with renamed keys translated.
	config := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))
	require.Equal(t, "7", config.Data["RACER_SHARES"])
	require.Equal(t, "off", config.Data["RACER_NVIDIA"])
	require.NotContains(t, config.Data, "RACER_DATAPLANE_NVIDIA")

	require.True(t, exists(t, env, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: ns}}))
}

func TestRenamedCleanupRequeuesUntilDone(t *testing.T) {
	ns := "custom-system"
	env := testEnv(t, append(renamedObjects(ns, managed), cacheObject("cache"))...)
	initialize(t, env)

	// Old objects remain after the replacement is live: re-check soon.
	plan, result := planResult(t, env)
	require.True(t, result.Ready)
	require.Equal(t, pendingRequeue, result.RequeueAfter)
	persist(t, env, plan)

	settle(t, env)

	_, result = planResult(t, env)
	require.True(t, result.Ready)
	require.Equal(t, time.Hour, result.RequeueAfter)
}

func TestRenamedCleanupWaitsForNewDaemonSet(t *testing.T) {
	ns := "custom-system"
	env := testEnv(t, append(renamedObjects(ns, managed), cacheObject("cache"))...)

	// The first pass only creates the runtime state, so nothing old is touched.
	plan, result := planResult(t, env)
	require.Equal(t, reasonInitializing, result.Reason)

	for _, op := range plan.Operations {
		require.NotEqual(t, component.OpDelete, op.Kind, "%s deleted before the new DaemonSet is planned", op.Ref())
	}
}

func TestRenamedCleanupKeepsUnmanagedObjects(t *testing.T) {
	ns := "custom-system"
	unmanaged := func() map[string]string { return nil }
	env := testEnv(t, append(renamedObjects(ns, unmanaged), cacheObject("cache"))...)
	initialize(t, env)
	settle(t, env)

	for _, obj := range renamedObjects(ns, unmanaged) {
		require.True(t, exists(t, env, obj), "%T %s was removed", obj, obj.GetName())
	}

	// An unowned old ConfigMap does not seed the new one.
	config := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))
	require.NotEqual(t, "7", config.Data["RACER_SHARES"])
}

func TestRenamedCleanupKeepsUnboundLegacyStamp(t *testing.T) {
	ns := "custom-system"
	stale := func() map[string]string {
		return map[string]string{managerAnnotation: component.FieldOwner, legacyInstallationUID: "unbound-marker"}
	}
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: renamedDataplane, Namespace: ns, UID: "old", Annotations: stale()}}
	env := testEnv(t, ds, cacheObject("cache"))
	initialize(t, env)
	settle(t, env)

	require.True(t, exists(t, env, ds), "a stamp not bound to a marker is not proof of ownership")
}

func TestRenamedCleanupRequiresLiveReplacement(t *testing.T) {
	ns := "custom-system"
	now := metav1.Now()

	for _, tc := range []struct {
		name        string
		replacement *appsv1.DaemonSet
		deletes     bool
	}{
		{name: "missing"},
		{
			name:        "live and owned",
			replacement: &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: ns, UID: "new", Annotations: managed()}},
			deletes:     true,
		},
		{
			name: "terminating",
			replacement: &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
				Name: dataplaneName, Namespace: ns, UID: "new", Annotations: managed(),
				DeletionTimestamp: &now, Finalizers: []string{"test/hold"},
			}},
		},
		{
			name:        "not owned",
			replacement: &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: ns, UID: "new"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := renamedObjects(ns, managed)
			if tc.replacement != nil {
				objects = append(objects, tc.replacement)
			}

			env := testEnv(t, objects...)
			plan := &component.Plan{}

			pending, err := planRenamedCleanup(t.Context(), env, plan, true)
			require.NoError(t, err)
			require.True(t, pending)

			deletes := 0

			for _, op := range plan.Operations {
				if op.Kind == component.OpDelete {
					deletes++
				}
			}

			if tc.deletes {
				// The DaemonSet, ServiceAccount, RBAC, and ConfigMap.
				require.Equal(t, 7, deletes)
			} else {
				require.Zero(t, deletes, "pre-rename objects deleted without a live replacement")
			}
		})
	}
}

// renamedGrantDeletes returns the names of the pre-rename grants plan deletes.
func renamedGrantDeletes(plan *component.Plan) []string {
	var deleted []string

	for _, op := range plan.Operations {
		if op.Kind == component.OpDelete && op.Object.GetName() == renamedDataplane {
			deleted = append(deleted, op.Object.GetKind())
		}
	}

	return deleted
}

func TestRenamedContainment(t *testing.T) {
	ns := "custom-system"
	now := metav1.Now()

	without := func(kind, name string) func([]client.Object) []client.Object {
		return func(objects []client.Object) []client.Object {
			var kept []client.Object

			for _, obj := range objects {
				if obj.GetName() == name && objectKind(obj) == kind {
					continue
				}

				kept = append(kept, obj)
			}

			return kept
		}
	}

	for _, tc := range []struct {
		name    string
		mutate  func([]client.Object) []client.Object
		revokes bool
	}{
		{name: "guards intact", mutate: func(o []client.Object) []client.Object { return o }},
		{name: "policy deleted", mutate: without("ValidatingAdmissionPolicy", renamedPolicies[0]), revokes: true},
		{name: "binding deleted", mutate: without("ValidatingAdmissionPolicyBinding", renamedPolicies[1]), revokes: true},
		{
			name: "policy terminating",
			mutate: func(objects []client.Object) []client.Object {
				for _, obj := range objects {
					if p, ok := obj.(*admissionv1.ValidatingAdmissionPolicy); ok && p.Name == renamedPolicies[2] {
						p.DeletionTimestamp = &now
						p.Finalizers = []string{"test/hold"}
					}
				}

				return objects
			},
			revokes: true,
		},
		{
			name: "policy no longer managed",
			mutate: func(objects []client.Object) []client.Object {
				for _, obj := range objects {
					if p, ok := obj.(*admissionv1.ValidatingAdmissionPolicy); ok && p.Name == renamedPolicies[3] {
						p.Annotations = nil
					}
				}

				return objects
			},
			revokes: true,
		},
		{
			name: "no grants",
			mutate: func(objects []client.Object) []client.Object {
				objects = without("RoleBinding", renamedDataplane)(objects)
				objects = without("ClusterRoleBinding", renamedDataplane)(objects)

				return without("ValidatingAdmissionPolicy", renamedPolicies[0])(objects)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t, tc.mutate(renamedObjects(ns, managed))...)

			plan, err := planRenamedContainment(t.Context(), env)
			require.NoError(t, err)

			if !tc.revokes {
				require.Nil(t, plan)
				return
			}

			require.NotNil(t, plan)
			require.ElementsMatch(t, []string{"RoleBinding", "ClusterRoleBinding"}, renamedGrantDeletes(plan))
			require.Len(t, plan.Operations, 2, "only the grants are touched")
		})
	}
}

func TestRecordedStateUIDReadsRenamedObjects(t *testing.T) {
	ns := "custom-system"
	record := func(uid string) map[string]string {
		return map[string]string{managerAnnotation: component.FieldOwner, stateUIDAnnotation: uid}
	}
	now := metav1.Now()

	for label, tc := range map[string]struct {
		objects []client.Object
		want    string
		err     string
	}{
		"no records": {},
		"only old DaemonSet records": {
			objects: []client.Object{&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: renamedDataplane, Namespace: ns, UID: "ds", Annotations: record("state-1")}}},
			want:    "state-1",
		},
		"only old ServiceAccount records": {
			objects: []client.Object{&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: renamedDataplane, Namespace: ns, UID: "sa", Annotations: record("state-1")}}},
			want:    "state-1",
		},
		"old DaemonSet terminating": {
			objects: []client.Object{&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
				Name: renamedDataplane, Namespace: ns, UID: "ds", Annotations: record("state-1"),
				DeletionTimestamp: &now, Finalizers: []string{"example.com/hold"},
			}}},
			want: "state-1",
		},
		"old and new agree": {
			objects: []client.Object{
				&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: renamedDataplane, Namespace: ns, UID: "old", Annotations: record("state-1")}},
				&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: ns, UID: "new", Annotations: record("state-1")}},
			},
			want: "state-1",
		},
		"old and new conflict": {
			objects: []client.Object{
				&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: renamedDataplane, Namespace: ns, UID: "old", Annotations: record("state-1")}},
				&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: dataplaneName, Namespace: ns, UID: "new", Annotations: record("state-2")}},
			},
			err: "different racer-state UIDs",
		},
		"unmanaged old object ignored": {
			objects: []client.Object{&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
				Name: renamedDataplane, Namespace: ns, UID: "ds", Annotations: map[string]string{stateUIDAnnotation: "state-1"},
			}}},
		},
	} {
		t.Run(label, func(t *testing.T) {
			env := testEnv(t, tc.objects...)

			uid, err := recordedStateUID(t.Context(), env)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, uid)
		})
	}
}

func TestRenamedContainmentKeepsUnmanagedGrants(t *testing.T) {
	ns := "custom-system"
	objects := renamedObjects(ns, func() map[string]string { return nil })
	env := testEnv(t, objects[:6]...) // No guards at all.

	plan, err := planRenamedContainment(t.Context(), env)
	require.NoError(t, err)
	require.Nil(t, plan, "grants the operator does not own are not revoked")
}

func TestRenamedContainmentWithoutClusterCache(t *testing.T) {
	ns := "custom-system"
	objects := withoutRenamedPolicy(renamedObjects(ns, managed), renamedPolicies[0])
	env := testEnv(t, objects...)

	plan, result := planResult(t, env)
	require.Equal(t, reasonGuards, result.Reason)
	require.ElementsMatch(t, []string{"RoleBinding", "ClusterRoleBinding"}, renamedGrantDeletes(plan))

	for _, op := range plan.Operations {
		if op.Kind == component.OpDelete {
			require.Equal(t, renamedDataplane, op.Object.GetName())
			require.Contains(t, []string{"RoleBinding", "ClusterRoleBinding"}, op.Object.GetKind(), "the retained installation is not removed")
		}
	}

	// With the guards intact, the retained installation is left alone.
	env = testEnv(t, renamedObjects(ns, managed)...)
	plan, result = planResult(t, env)
	require.Empty(t, renamedGrantDeletes(plan))
	require.NotEqual(t, reasonGuards, result.Reason)
}

func TestRenamedContainmentWithClusterCache(t *testing.T) {
	ns := "custom-system"
	objects := append(withoutRenamedPolicy(renamedObjects(ns, managed), renamedPolicies[0]), cacheObject("cache"))
	env := testEnv(t, objects...)

	plan, result := planResult(t, env)
	require.Equal(t, reasonGuards, result.Reason)
	require.ElementsMatch(t, []string{"RoleBinding", "ClusterRoleBinding"}, renamedGrantDeletes(plan))
}

// withoutRenamedPolicy drops the pre-rename policy named policy.
func withoutRenamedPolicy(objects []client.Object, policy string) []client.Object {
	var kept []client.Object

	for _, obj := range objects {
		if p, ok := obj.(*admissionv1.ValidatingAdmissionPolicy); ok && p.Name == policy {
			continue
		}

		kept = append(kept, obj)
	}

	return kept
}

func objectKind(obj client.Object) string {
	switch obj.(type) {
	case *admissionv1.ValidatingAdmissionPolicy:
		return "ValidatingAdmissionPolicy"
	case *admissionv1.ValidatingAdmissionPolicyBinding:
		return "ValidatingAdmissionPolicyBinding"
	case *rbacv1.RoleBinding:
		return "RoleBinding"
	case *rbacv1.ClusterRoleBinding:
		return "ClusterRoleBinding"
	}

	return ""
}

func TestRenamedStateRecordFailsClosed(t *testing.T) {
	ns := "custom-system"
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name: renamedDataplane, Namespace: ns, UID: "ds",
		Annotations: map[string]string{managerAnnotation: component.FieldOwner, stateUIDAnnotation: "lost-state"},
	}}
	env := testEnv(t, ds, cacheObject("cache"))

	// racer-state is gone, so the first pass must not mint a new identity.
	_, _, err := (Component{}).Plan(t.Context(), env, nil)
	require.ErrorContains(t, err, "missing")
}

func TestTranslateRenamedKeys(t *testing.T) {
	for name, tc := range map[string]struct {
		in, want map[string]string
	}{
		"prefixed wins over unprefixed": {
			in:   map[string]string{"RACER_DATAPLANE_NVIDIA": "cdi", "RACER_NVIDIA": "off"},
			want: map[string]string{"RACER_NVIDIA": "cdi"},
		},
		"prefixed wins the other way": {
			in:   map[string]string{"RACER_DATAPLANE_NVIDIA": "off", "RACER_NVIDIA": "cdi"},
			want: map[string]string{"RACER_NVIDIA": "off"},
		},
		"unprefixed wins for keys the dataplane read": {
			in:   map[string]string{"RACER_DATAPLANE_SHARES": "3", "RACER_SHARES": "7"},
			want: map[string]string{"RACER_SHARES": "7"},
		},
		"no collision": {
			in:   map[string]string{"RACER_DATAPLANE_NVIDIA": "env", "RACER_SHARES": "7"},
			want: map[string]string{"RACER_NVIDIA": "env", "RACER_SHARES": "7"},
		},
		"empty": {in: nil, want: map[string]string{}},
	} {
		t.Run(name, func(t *testing.T) {
			// Map iteration order varies per range; repeat so an order
			// dependence cannot pass by luck.
			for range 100 {
				require.Equal(t, tc.want, translateRenamedKeys(tc.in))
			}
		})
	}
}
