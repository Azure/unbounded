// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"

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
