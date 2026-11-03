// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/registryauth"
)

type audienceLiveReads struct {
	client.Reader
	gets, lists int
}

func (r *audienceLiveReads) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	r.gets++
	return r.Reader.Get(ctx, key, object, opts...)
}

func (r *audienceLiveReads) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.lists++
	return r.Reader.List(ctx, list, opts...)
}

func audienceNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: RegistryAuthNodeSelector()}}
}

func storeAudienceOperations(t *testing.T, env *component.Env, operations []component.Operation) {
	t.Helper()

	for _, op := range operations {
		object := op.Object.DeepCopy()
		switch op.Kind {
		case component.OpCreateIfAbsent:
			object.SetUID(types.UID("owned-" + object.GetKind() + "-" + object.GetName()))
			object.SetCreationTimestamp(metav1.Now())
			require.NoError(t, env.Client.Create(t.Context(), object))
		case component.OpApply:
			require.NoError(t, env.Client.Update(t.Context(), object))
		case component.OpDelete:
			require.NoError(t, env.Client.Delete(t.Context(), object))
		default:
			t.Fatalf("unexpected audience operation %s", op.Kind)
		}
	}
}

func audienceAssignments(t *testing.T, env *component.Env) map[string]string {
	t.Helper()

	var bindings rbacv1.ClusterRoleBindingList
	require.NoError(t, env.Client.List(t.Context(), &bindings))

	assignments := map[string]string{}

	for _, binding := range bindings.Items {
		if !registryAuthBindingName(binding.Name) {
			continue
		}

		require.LessOrEqual(t, len(binding.Subjects), registryAuthBindingSize)

		for _, subject := range binding.Subjects {
			require.Equal(t, rbacv1.UserKind, subject.Kind)
			require.Equal(t, rbacv1.GroupName, subject.APIGroup)
			require.Empty(t, subject.Namespace)
			require.NotContains(t, assignments, subject.Name)
			assignments[subject.Name] = binding.Name
		}
	}

	return assignments
}

func TestRegistryAuthTenThousandWorkersHaveStableBoundedGrants(t *testing.T) {
	objects := make([]client.Object, 0, 10002)
	for i := range 10000 {
		objects = append(objects, audienceNode(fmt.Sprintf("flex-%05d", i)))
	}

	native, windows := audienceNode("native"), audienceNode("windows")
	native.Labels[registryauth.FlexNodeLabel] = "VirtualMachineScaleSets"
	windows.Labels["kubernetes.io/os"] = "windows"
	objects = append(objects, native, windows)
	env := testEnv(t, objects...)
	live := &audienceLiveReads{Reader: env.Client}
	env.APIReader = live
	cfg := registryauth.Config{Mode: registryauth.IdentityBindingMode, Audience: "api://AKSIdentityBinding"}
	operations, err := planRegistryAuthAudience(t.Context(), env, cfg)
	require.NoError(t, err)
	require.Len(t, operations, 11, "one role and ten bounded bindings")
	storeAudienceOperations(t, env, operations)
	before := audienceAssignments(t, env)
	require.Len(t, before, 10000)
	require.NotContains(t, before, "system:node:native")
	require.NotContains(t, before, "system:node:windows")

	gets := live.gets
	operations, err = planRegistryAuthAudience(t.Context(), env, cfg)
	require.NoError(t, err)
	require.Empty(t, operations, "healthy RBAC must not be rewritten")
	require.Equal(t, gets, live.gets, "healthy RBAC must not need live ownership reads")
	require.Zero(t, live.lists, "all membership LISTs must be cache-only")

	removed := "flex-00017"
	require.NoError(t, env.Client.Delete(t.Context(), audienceNode(removed)))
	require.NoError(t, env.Client.Create(t.Context(), audienceNode("flex-new")))
	operations, err = planRegistryAuthAudience(t.Context(), env, cfg)
	require.NoError(t, err)
	require.Len(t, operations, 1, "a vacancy and replacement update only their binding")
	require.Equal(t, before["system:node:"+removed], operations[0].Object.GetName())
	storeAudienceOperations(t, env, operations)
	after := audienceAssignments(t, env)

	delete(before, "system:node:"+removed)

	for node, shard := range before {
		require.Equal(t, shard, after[node], "unaffected nodes must not be reassigned")
	}

	require.Len(t, after, 10000)

	require.NoError(t, env.Client.Create(t.Context(), audienceNode("flex-overflow")))
	operations, err = planRegistryAuthAudience(t.Context(), env, cfg)
	require.NoError(t, err)
	require.Len(t, operations, 1, "overflow creates one new binding without reshuffling")
	require.Equal(t, component.OpCreateIfAbsent, operations[0].Kind)
	storeAudienceOperations(t, env, operations)
	require.Len(t, audienceAssignments(t, env), 10001)
	require.Zero(t, live.lists)
}

func TestRegistryAuthLegacyBindingMigrationGrantsBeforeTruncation(t *testing.T) {
	legacy := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: registryAuthAudienceName, UID: "legacy", Labels: ownedAuthMetadata().Labels},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: registryAuthAudienceName},
	}
	objects := []client.Object{legacy}

	for i := range 1001 {
		name := fmt.Sprintf("flex-%04d", i)
		objects = append(objects, audienceNode(name))
		legacy.Subjects = append(legacy.Subjects, rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "system:node:" + name})
	}

	env := testEnv(t, objects...)
	operations, err := planRegistryAuthAudience(t.Context(), env, registryauth.Config{
		Mode: registryauth.IdentityBindingMode, Audience: "api://AKSIdentityBinding",
	})
	require.NoError(t, err)
	require.Len(t, operations, 3)
	require.Equal(t, component.OpCreateIfAbsent, operations[1].Kind)
	require.NotEqual(t, legacy.Name, operations[1].Object.GetName())
	require.Equal(t, legacy.Name, operations[2].Object.GetName())
	require.Equal(t, component.OpApply, operations[2].Kind)
	storeAudienceOperations(t, env, operations[:2])

	var unchanged rbacv1.ClusterRoleBinding
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: legacy.Name}, &unchanged))
	require.Len(t, unchanged.Subjects, 1001, "the old grant survives until the new shard exists")
	storeAudienceOperations(t, env, operations[2:])
	require.Len(t, audienceAssignments(t, env), 1001)
}

func TestRegistryAuthLastNodeRemovalClearsSubjects(t *testing.T) {
	node := audienceNode("last")
	env := testEnv(t, node)
	cfg := registryauth.Config{Mode: registryauth.IdentityBindingMode, Audience: "api://AKSIdentityBinding"}
	operations, err := planRegistryAuthAudience(t.Context(), env, cfg)
	require.NoError(t, err)
	storeAudienceOperations(t, env, operations)
	require.NoError(t, env.Client.Delete(t.Context(), node))
	operations, err = planRegistryAuthAudience(t.Context(), env, cfg)
	require.NoError(t, err)
	require.Len(t, operations, 1)
	require.Equal(t, []any{}, operations[0].Object.Object["subjects"], "empty subjects must be explicit for server-side apply")
	storeAudienceOperations(t, env, operations)
	require.Empty(t, audienceAssignments(t, env))
	operations, err = planRegistryAuthAudience(t.Context(), env, cfg)
	require.NoError(t, err)
	require.Empty(t, operations)
}

func TestRegistryAuthAudienceControllerCreatesConvergesAndRevokes(t *testing.T) {
	source := authSource("")
	env := testEnv(t, source, audienceNode("flex"))
	live := &audienceLiveReads{Reader: env.Client}
	r := &RegistryAuthAudienceReconciler{Client: env.Client, APIReader: live, Namespace: env.Namespace}
	result, err := r.Reconcile(t.Context(), ctrl.Request{})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Len(t, audienceAssignments(t, env), 1)

	reads := live.gets
	result, err = r.Reconcile(t.Context(), ctrl.Request{})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Equal(t, reads, live.gets)
	require.Zero(t, live.lists)
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(source), source))
	source.Data[registryauth.ConfigKey] = "mode: external"
	require.NoError(t, env.Client.Update(t.Context(), source))
	result, err = r.Reconcile(t.Context(), ctrl.Request{})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.Empty(t, audienceAssignments(t, env))

	var roles rbacv1.ClusterRoleList
	require.NoError(t, env.Client.List(t.Context(), &roles))
	require.Len(t, roles.Items, 1)
	require.Equal(t, registryauth.IdentityRoleName, roles.Items[0].Name)
}

func TestRegistryAuthIdentityUseRestoresAndRevokesWithoutHelpers(t *testing.T) {
	source := authSource("")
	env := testEnv(t, source)
	r := &RegistryAuthAudienceReconciler{Client: env.Client, APIReader: env.Client, Namespace: env.Namespace}
	_, err := r.Reconcile(t.Context(), ctrl.Request{})
	require.NoError(t, err)

	var binding rbacv1.ClusterRoleBinding

	key := client.ObjectKey{Name: registryauth.IdentityRoleName}
	require.NoError(t, env.Client.Get(t.Context(), key, &binding))
	require.Equal(t, "system:serviceaccounts", binding.Subjects[0].Name)
	require.NoError(t, env.Client.Delete(t.Context(), &binding))
	_, err = r.Reconcile(t.Context(), ctrl.Request{})
	require.NoError(t, err)
	require.NoError(t, env.Client.Get(t.Context(), key, &binding), "managed policy must restore a deleted identity-use grant")

	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(source), source))
	source.Data[registryauth.ConfigKey] = "mode: external"
	require.NoError(t, env.Client.Update(t.Context(), source))
	_, err = r.Reconcile(t.Context(), ctrl.Request{})
	require.NoError(t, err)
	require.Error(t, env.Client.Get(t.Context(), key, &binding), "revocation must not depend on helper objects")

	var helpers appsv1.DaemonSetList
	require.NoError(t, env.Client.List(t.Context(), &helpers))
	require.Empty(t, helpers.Items)
}

func TestRegistryAuthFindsShardsDespiteOwnershipLabelDrift(t *testing.T) {
	env := testEnv(t, audienceNode("flex"))
	cfg := registryauth.Config{Mode: registryauth.IdentityBindingMode, Audience: "api://AKSIdentityBinding"}
	operations, err := planRegistryAuthAudience(t.Context(), env, cfg)
	require.NoError(t, err)
	storeAudienceOperations(t, env, operations)

	var binding rbacv1.ClusterRoleBinding
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: registryAuthAudienceName}, &binding))
	binding.Name += "-0001"
	binding.ResourceVersion = ""
	binding.UID = ""
	delete(binding.Labels, "app.kubernetes.io/name")
	require.NoError(t, env.Client.Create(t.Context(), &binding))
	operations, err = planRegistryAuthAudience(t.Context(), env, cfg)
	require.ErrorContains(t, err, "unowned")
	require.Nil(t, operations)
}

func TestRegistryAuthAudienceRejectsRecreatedResourcesFromStaleCache(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(fmt.Sprint(owned), func(t *testing.T) {
			env := testEnv(t, audienceNode("flex"))
			cfg := registryauth.Config{Mode: registryauth.IdentityBindingMode, Audience: "api://AKSIdentityBinding"}
			operations, err := planRegistryAuthAudience(t.Context(), env, cfg)
			require.NoError(t, err)
			storeAudienceOperations(t, env, operations)

			var recreated rbacv1.ClusterRole
			require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: registryAuthAudienceName}, &recreated))
			recreated.UID = "recreated"

			recreated.ResourceVersion = ""
			if !owned {
				recreated.Labels["app.kubernetes.io/managed-by"] = "customer"
			}

			live := testEnv(t, &recreated)
			env.APIReader = live.Client
			cfg.Audience += "-changed"
			operations, err = planRegistryAuthAudience(t.Context(), env, cfg)
			require.Error(t, err)
			require.Nil(t, operations, "stale ownership must never become a forced adoption")
		})
	}
}

func TestRegistryAuthRBACEventsRepairPermissionDrift(t *testing.T) {
	for _, drift := range []string{"subjects", "role-ref", "rules", "aggregation"} {
		t.Run(drift, func(t *testing.T) {
			env := testEnv(t, audienceNode("flex"))
			cfg := registryauth.Config{Mode: registryauth.IdentityBindingMode, Audience: "api://AKSIdentityBinding"}
			operations, err := planRegistryAuthAudience(t.Context(), env, cfg)
			require.NoError(t, err)
			storeAudienceOperations(t, env, operations)

			var changed, before client.Object

			expected := component.OpApply

			switch drift {
			case "subjects", "role-ref":
				binding := &rbacv1.ClusterRoleBinding{}
				require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: registryAuthAudienceName}, binding))

				before = binding.DeepCopy()
				if drift == "subjects" {
					binding.Subjects = append(binding.Subjects, rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "system:nodes"})
				} else {
					binding.RoleRef.Name = "wrong-role"
					expected = component.OpDelete
				}

				changed = binding
			case "rules", "aggregation":
				role := &rbacv1.ClusterRole{}
				require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Name: registryAuthAudienceName}, role))

				before = role.DeepCopy()
				if drift == "rules" {
					role.Rules[0].Resources = []string{"*"}
				} else {
					role.AggregationRule = &rbacv1.AggregationRule{ClusterRoleSelectors: []metav1.LabelSelector{{}}}
					expected = component.OpDelete
				}

				changed = role
			}

			p := registryAuthRBACEvents()
			require.False(t, p.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: before}))
			require.True(t, p.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: changed}))
			require.NoError(t, env.Client.Update(t.Context(), changed))
			operations, err = planRegistryAuthAudience(t.Context(), env, cfg)
			require.NoError(t, err)
			require.Len(t, operations, 1)
			require.Equal(t, expected, operations[0].Kind)
			require.NotEmpty(t, operations[0].Object.GetUID())
			require.NotEmpty(t, operations[0].Object.GetResourceVersion())
		})
	}
}

func TestRegistryAuthMembershipEventsIgnoreStatusAndUnrelatedLabels(t *testing.T) {
	p := registryAuthNodeEvents()
	old := audienceNode("flex")
	newer := old.DeepCopy()
	newer.Labels["unrelated"] = "changed"
	newer.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	require.False(t, p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newer}))
	require.True(t, p.Create(event.CreateEvent{Object: old}))
	require.True(t, p.Delete(event.DeleteEvent{Object: old}))

	newer.Labels[registryauth.FlexNodeLabel] = "VirtualMachineScaleSets"
	require.True(t, p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newer}))
	newer = old.DeepCopy()
	now := metav1.Now()
	newer.DeletionTimestamp = &now
	require.True(t, p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newer}))
}

func TestRegistryAuthEventsCoalesceWithoutIdlePolling(t *testing.T) {
	q := workqueue.NewTypedRateLimitingQueue(workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](time.Millisecond, time.Second))
	defer q.ShutDown()

	h := registryAuthAudienceEvents()
	for i := range 10000 {
		h.Create(t.Context(), event.CreateEvent{Object: audienceNode(fmt.Sprintf("flex-%d", i))}, q)
	}

	require.Eventually(t, func() bool { return q.Len() == 1 }, time.Second, 5*time.Millisecond)

	key, shutdown := q.Get()
	require.False(t, shutdown)
	require.Equal(t, ctrl.Request{}, key)
	q.Done(key)
	time.Sleep(300 * time.Millisecond)
	require.Zero(t, q.Len(), "no timer should requeue a converged controller")
}

func TestRegistryAuthReadinessEventsIgnoreIntermediateRolloutProgress(t *testing.T) {
	env := testEnv(t)
	p := registryAuthWorkloadEvents(env, env.InNamespaceNamed(registryAuthName, daemonSetName))
	old := &appsv1.DaemonSet{ObjectMeta: ownedAuthMetadata()}
	old.Generation = 2
	old.Status = appsv1.DaemonSetStatus{ObservedGeneration: 2, DesiredNumberScheduled: 10000, UpdatedNumberScheduled: 9000, NumberAvailable: 9000}
	newer := old.DeepCopy()
	newer.Status.UpdatedNumberScheduled, newer.Status.NumberAvailable = 9999, 9999
	require.False(t, p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newer}))
	old = newer.DeepCopy()
	newer.Status.UpdatedNumberScheduled, newer.Status.NumberAvailable = 10000, 10000
	require.True(t, p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newer}))
	old = newer.DeepCopy()
	newer.Status.NumberAvailable--
	require.True(t, p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newer}))
}

func TestRegistryAuthMigratesLegacyHelperResourcesOnlyAfterRollout(t *testing.T) {
	env := testEnv(t, authSource(""))
	operations, _, err := planRegistryAuth(t.Context(), env)
	require.NoError(t, err)

	var ds appsv1.DaemonSet
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(operations[1].Object.Object, &ds))
	ds.Generation = 2
	ds.Status = appsv1.DaemonSetStatus{ObservedGeneration: 1}
	require.NoError(t, env.Client.Create(t.Context(), &ds))

	marker := &corev1.ConfigMap{ObjectMeta: ownedAuthMetadata()}
	require.NoError(t, env.Client.Create(t.Context(), marker))

	role := &rbacv1.Role{ObjectMeta: ownedAuthMetadata()}
	binding := &rbacv1.RoleBinding{ObjectMeta: ownedAuthMetadata()}
	lease := &coordinationv1.Lease{ObjectMeta: ownedAuthMetadata()}

	lease.Name = "gantry-acr-auth-restart"
	for _, object := range []client.Object{role, binding, lease} {
		require.NoError(t, env.Client.Create(t.Context(), object))
	}

	operations, _, err = planRegistryAuth(t.Context(), env)
	require.NoError(t, err)

	for _, op := range operations {
		require.NotEqual(t, component.OpDelete, op.Kind)
	}

	ds.Status.ObservedGeneration = 2
	require.NoError(t, env.Client.Status().Update(t.Context(), &ds))
	operations, result, err := planRegistryAuth(t.Context(), env)
	require.NoError(t, err)
	require.True(t, result.Ready)

	var cleanup []component.Operation

	for _, op := range operations {
		if op.Kind == component.OpDelete {
			cleanup = append(cleanup, op)
		}
	}

	require.Len(t, cleanup, 4)

	for _, op := range cleanup {
		require.Contains(t, []string{"RoleBinding", "Role", "Lease", "ConfigMap"}, op.Object.GetKind())
	}

	require.Equal(t, "ConfigMap", cleanup[3].Object.GetKind())
	require.Len(t, cleanup[3].DependsOn, 3)

	plan := component.NewPlan()
	plan.Add(operations...)
	resultExecuted, err := env.Execute(t.Context(), plan)
	require.NoError(t, err)
	require.Empty(t, resultExecuted.Failed())
}
