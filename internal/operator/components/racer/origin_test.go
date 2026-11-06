// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
)

const testContainerURL = "https://Account.blob.core.windows.net/models"

func originCache(name string, credential *racerv1.AzureCredential) *racerv1.ClusterCache {
	cache := cacheObject(name)
	cache.Spec.Origin = &racerv1.ObjectOrigin{
		Azure: &racerv1.AzureOrigin{ContainerURL: testContainerURL, Credential: credential},
	}

	return cache
}

// leftoverOrigin creates the DaemonSet and ServiceAccount an earlier operator
// version ran for a cache origin. manager is the managerAnnotation value.
func leftoverOrigin(t *testing.T, env *component.Env, cache, manager string) {
	t.Helper()

	meta := func() metav1.ObjectMeta {
		return metav1.ObjectMeta{
			Name: originPrefix + cache, Namespace: env.Namespace,
			Labels:      map[string]string{"app.kubernetes.io/name": originAppName, "app.kubernetes.io/component": originAppName},
			Annotations: map[string]string{managerAnnotation: manager},
		}
	}

	require.NoError(t, env.Client.Create(t.Context(), &appsv1.DaemonSet{ObjectMeta: meta()}))
	require.NoError(t, env.Client.Create(t.Context(), &corev1.ServiceAccount{ObjectMeta: meta()}))
}

func originExists(t *testing.T, env *component.Env, cache string) (ds, sa bool) {
	t.Helper()

	exists := func(obj client.Object) bool {
		err := env.Client.Get(t.Context(), objectKey(env, originPrefix+cache), obj)
		if apierrors.IsNotFound(err) {
			return false
		}

		require.NoError(t, err)

		return true
	}

	return exists(&appsv1.DaemonSet{}), exists(&corev1.ServiceAccount{})
}

func updateCache(t *testing.T, env *component.Env, name string, mutate func(*racerv1.ClusterCache)) {
	t.Helper()

	cache := &racerv1.ClusterCache{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(&component.Env{}, name), cache))
	mutate(cache)
	require.NoError(t, env.Client.Update(t.Context(), cache))
}

func planResult(t *testing.T, env *component.Env) (*component.Plan, component.Result) {
	t.Helper()

	plan, result, err := (Component{}).Plan(t.Context(), env, nil)
	require.NoError(t, err)

	return plan, result
}

func TestOriginCachesNeedNoOriginWorkloads(t *testing.T) {
	credentials := []*racerv1.AzureCredential{
		nil,
		{Default: &racerv1.AzureDefaultCredential{}},
		{Default: &racerv1.AzureDefaultCredential{ClientID: "00000000-0000-4000-8000-000000000001"}},
		{WorkloadIdentity: &racerv1.AzureWorkloadIdentityCredential{ClientID: "00000000-0000-4000-8000-000000000001"}},
	}

	caches := []client.Object{cacheObject("plain")}
	for i, credential := range credentials {
		caches = append(caches, originCache("cache"+string(rune('a'+i)), credential))
	}

	env := testEnv(t, caches...)
	initialize(t, env)

	plan, result := planResult(t, env)
	require.True(t, result.Ready, "%s: %s", result.Reason, result.Message)

	for _, op := range plan.Operations {
		require.False(t, strings.HasPrefix(op.Object.GetName(), originPrefix), "%s", op.Ref())
	}

	dataplaneOp(t, plan)
}

func TestLeftoverOriginsArePruned(t *testing.T) {
	env := testEnv(t, originCache("a", nil), cacheObject("plain"))
	initialize(t, env)

	leftoverOrigin(t, env, "a", component.FieldOwner)
	leftoverOrigin(t, env, "gone", component.FieldOwner)
	// An origin the operator does not manage is never pruned.
	leftoverOrigin(t, env, "foreign", "someone-else")

	plan := planPass(t, env)

	deleted := map[string]bool{}

	for _, op := range plan.Operations {
		if op.Kind == component.OpDelete {
			deleted[op.Object.GetKind()+"/"+op.Object.GetName()] = true
		}
	}

	require.Equal(t, map[string]bool{
		"DaemonSet/" + originPrefix + "a": true, "ServiceAccount/" + originPrefix + "a": true,
		"DaemonSet/" + originPrefix + "gone": true, "ServiceAccount/" + originPrefix + "gone": true,
	}, deleted)
	persist(t, env, plan)

	for _, cache := range []string{"a", "gone"} {
		ds, sa := originExists(t, env, cache)
		require.False(t, ds || sa, cache)
	}

	ds, sa := originExists(t, env, "foreign")
	require.True(t, ds && sa)
	liveDataplane(t, env)
}

func deletedOrigins(plan *component.Plan) []string {
	var deleted []string

	for _, op := range plan.Operations {
		if op.Kind == component.OpDelete && strings.HasPrefix(op.Object.GetName(), originPrefix) {
			deleted = append(deleted, op.Object.GetKind()+"/"+op.Object.GetName())
		}
	}

	return deleted
}

func setDataplaneStatus(t *testing.T, env *component.Env, status appsv1.DaemonSetStatus) {
	t.Helper()

	ds := liveDataplane(t, env)
	ds.Status = status
	require.NoError(t, env.Client.Status().Update(t.Context(), ds))
}

func TestLeftoverOriginsWaitForDataplaneRollout(t *testing.T) {
	env := testEnv(t, originCache("a", nil))
	initialize(t, env)
	leftoverOrigin(t, env, "a", component.FieldOwner)
	leftoverOrigin(t, env, "gone", component.FieldOwner)

	// Some nodes still run the old dataplane: keep the origins and requeue.
	setDataplaneStatus(t, env, appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, UpdatedNumberScheduled: 1, NumberAvailable: 3})
	plan, result := planResult(t, env)
	require.Empty(t, deletedOrigins(plan))
	require.True(t, result.Ready)
	require.Equal(t, pendingRequeue, result.RequeueAfter)
	require.Contains(t, result.Message, "legacy origin")
	persist(t, env, plan)

	for _, cache := range []string{"a", "gone"} {
		ds, sa := originExists(t, env, cache)
		require.True(t, ds && sa, cache)
	}

	// Updated but not yet available still waits.
	setDataplaneStatus(t, env, appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, UpdatedNumberScheduled: 3, NumberAvailable: 2, NumberUnavailable: 1})
	require.Empty(t, deletedOrigins(planPass(t, env)))

	// A live template without the native-origin marker is an old dataplane.
	ds := liveDataplane(t, env)
	delete(ds.Spec.Template.Annotations, nativeOriginAnnotation)
	require.NoError(t, env.Client.Update(t.Context(), ds))
	setDataplaneStatus(t, env, appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, UpdatedNumberScheduled: 3, NumberAvailable: 3})
	done, err := nativeOriginRolledOut(t.Context(), env)
	require.NoError(t, err)
	require.False(t, done)

	// Restore the template and complete the rollout: the origins go.
	persist(t, env, planPass(t, env))
	ds = liveDataplane(t, env)
	setDataplaneStatus(t, env, appsv1.DaemonSetStatus{ObservedGeneration: ds.Generation, DesiredNumberScheduled: 3, UpdatedNumberScheduled: 3, NumberAvailable: 3})
	plan, result = planResult(t, env)
	require.Len(t, deletedOrigins(plan), 4)
	require.Equal(t, time.Hour, result.RequeueAfter)
}

func TestLeftoverOriginsWaitForDataplane(t *testing.T) {
	env := testEnv(t, originCache("a", nil))
	initialize(t, env)
	leftoverOrigin(t, env, "a", component.FieldOwner)
	require.NoError(t, env.Client.Delete(t.Context(), liveDataplane(t, env)))

	// With no dataplane DaemonSet yet, it is planned and the origins wait.
	plan, result := planResult(t, env)
	dataplaneOp(t, plan)
	require.Empty(t, deletedOrigins(plan))
	require.Equal(t, pendingRequeue, result.RequeueAfter)
}

func TestOriginInvalidCacheIsReported(t *testing.T) {
	env := testEnv(t, originCache("bad", nil), originCache("good", nil))
	initialize(t, env)
	persist(t, env, planPass(t, env))

	// The fake client does not run CEL, modeling a cache that bypassed admission.
	updateCache(t, env, "bad", func(c *racerv1.ClusterCache) { c.Spec.Origin.Azure.ContainerURL = "https://user@host/models" })
	updateCache(t, env, "good", func(c *racerv1.ClusterCache) { c.Spec.Origin.MetadataTTL = &metav1.Duration{Duration: 7 * time.Second} })

	plan, result := planResult(t, env)
	require.False(t, result.Ready)
	require.Equal(t, "OriginInvalid", result.Reason)
	require.Contains(t, result.Message, "bad (")
	require.NotContains(t, result.Message, "good")

	// An invalid cache never withholds the dataplane.
	dataplaneOp(t, plan)
}

func TestOriginTerminatingCacheIsNotReported(t *testing.T) {
	cache := originCache("held", nil)
	cache.Finalizers = []string{"test/hold"}
	cache.Spec.Origin.Azure.ContainerURL = "https://user@host/models"

	env := testEnv(t, cache)
	initialize(t, env)

	_, result := planResult(t, env)
	require.Equal(t, "OriginInvalid", result.Reason)

	held := &racerv1.ClusterCache{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(&component.Env{}, "held"), held))
	require.NoError(t, env.Client.Delete(t.Context(), held))

	_, result = planResult(t, env)
	require.NotEqual(t, "OriginInvalid", result.Reason)
}

func containerURL(value string) func(*racerv1.ObjectOrigin) {
	return func(o *racerv1.ObjectOrigin) { o.Azure.ContainerURL = value }
}

func TestValidateOriginRejectsInvalidSpecs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*racerv1.ObjectOrigin)
		want   string
	}{
		{"no-azure", func(o *racerv1.ObjectOrigin) { o.Azure = nil }, "azure is required"},
		{"empty-credential", func(o *racerv1.ObjectOrigin) { o.Azure.Credential = &racerv1.AzureCredential{} }, "exactly one"},
		{"both-credentials", func(o *racerv1.ObjectOrigin) {
			o.Azure.Credential = &racerv1.AzureCredential{Default: &racerv1.AzureDefaultCredential{}, WorkloadIdentity: &racerv1.AzureWorkloadIdentityCredential{ClientID: "c"}}
		}, "exactly one"},
		{"wi-no-client", func(o *racerv1.ObjectOrigin) {
			o.Azure.Credential = &racerv1.AzureCredential{WorkloadIdentity: &racerv1.AzureWorkloadIdentityCredential{}}
		}, "clientID is required"},
		{"default-secret-ref", func(o *racerv1.ObjectOrigin) {
			o.Azure.Credential = &racerv1.AzureCredential{Default: &racerv1.AzureDefaultCredential{SecretRef: &racerv1.SecretReference{Name: "s"}}}
		}, "secretRef is no longer supported"},
		{"default-http", func(o *racerv1.ObjectOrigin) {
			o.Azure.ContainerURL = "http://127.0.0.1:10000/devstoreaccount1/models"
			o.Azure.Credential = &racerv1.AzureCredential{Default: &racerv1.AzureDefaultCredential{}}
		}, "containerURL"},
		{"empty-container-url", containerURL(""), "spec.origin.azure.containerURL"},
		{"account-only", containerURL("https://account.blob.core.windows.net"), "spec.origin.azure.containerURL"},
		{"trailing-slash", containerURL("https://account.blob.core.windows.net/models/"), "spec.origin.azure.containerURL"},
		{"blob-path", containerURL("https://account.blob.core.windows.net/account/models/blob"), "spec.origin.azure.containerURL"},
		{"bad-container", containerURL("https://account.blob.core.windows.net/a_b"), "spec.origin.azure.containerURL"},
		{"long-container", containerURL("https://account.blob.core.windows.net/" + strings.Repeat("b", 64)), "spec.origin.azure.containerURL"},
		{"negative-ttl", func(o *racerv1.ObjectOrigin) { o.MetadataTTL = &metav1.Duration{Duration: -time.Second} }, "metadataTTL"},
		{"zero-timeout", func(o *racerv1.ObjectOrigin) { o.RequestTimeout = &metav1.Duration{} }, "requestTimeout"},
		{"zero-requests", func(o *racerv1.ObjectOrigin) { o.MaxConcurrentRequests = ptr.To(int32(0)) }, "maxConcurrentRequests"},
		{"too-many-requests", func(o *racerv1.ObjectOrigin) { o.MaxConcurrentRequests = ptr.To(int32(65537)) }, "maxConcurrentRequests"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := originCache("cache", nil)
			tc.mutate(cache.Spec.Origin)
			require.ErrorContains(t, validateOrigin(cache), tc.want)
		})
	}
}

func TestValidateOriginAcceptsValidSpecs(t *testing.T) {
	for name, mutate := range map[string]func(*racerv1.ObjectOrigin){
		"anonymous":      func(*racerv1.ObjectOrigin) {},
		"anonymous-http": containerURL("http://127.0.0.1:10000/devstoreaccount1/models"),
		"default": func(o *racerv1.ObjectOrigin) {
			o.Azure.Credential = &racerv1.AzureCredential{Default: &racerv1.AzureDefaultCredential{}}
		},
		"workload-identity": func(o *racerv1.ObjectOrigin) {
			o.Azure.Credential = &racerv1.AzureCredential{WorkloadIdentity: &racerv1.AzureWorkloadIdentityCredential{ClientID: "c", TenantID: "t"}}
		},
		"tuning": func(o *racerv1.ObjectOrigin) {
			o.MetadataTTL = &metav1.Duration{}
			o.RequestTimeout = &metav1.Duration{Duration: time.Second}
			o.MaxConcurrentRequests = ptr.To(int32(65536))
		},
	} {
		t.Run(name, func(t *testing.T) {
			cache := originCache("cache", nil)
			mutate(cache.Spec.Origin)
			require.NoError(t, validateOrigin(cache))
		})
	}
}

func TestOriginWatchPredicates(t *testing.T) {
	env := testEnv(t)
	match := runtimeWorkload(env)
	predicate := env.ManagedWorkloadPredicate(match)

	for _, tc := range []struct {
		name, namespace string
		want            bool
	}{
		{dataplaneName, env.Namespace, true},
		{originPrefix + "cache", env.Namespace, true},
		{originPrefix, env.Namespace, false},
		{originPrefix + "cache", "other", false},
		{"unrelated", env.Namespace, false},
	} {
		ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: tc.namespace}}
		require.Equal(t, tc.want, predicate.Create(event.CreateEvent{Object: ds}), "%s/%s", tc.namespace, tc.name)
		require.Equal(t, tc.want, match(&corev1.ServiceAccount{ObjectMeta: ds.ObjectMeta}), "%s/%s", tc.namespace, tc.name)
	}
}

func TestSecretWatchedMatchesOnlyState(t *testing.T) {
	env := testEnv(t, originCache("cache", nil))
	watched := secretWatched(env)

	require.True(t, watched(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: stateName, Namespace: env.Namespace}}))
	require.False(t, watched(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: env.Namespace}}))
	require.False(t, watched(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: stateName, Namespace: "other"}}))
}

func planHasObject(plan *component.Plan, kind, name string) bool {
	for _, op := range plan.Operations {
		if op.Object.GetKind() == kind && op.Object.GetName() == name {
			return true
		}
	}

	return false
}

// dataplanePod creates a Pod owned by the live dataplane DaemonSet.
func dataplanePod(t *testing.T, env *component.Env, name string, native, ready bool) {
	t.Helper()

	ds := liveDataplane(t, env)
	if ds.UID == "" {
		ds.UID = types.UID("dataplane-uid")
		require.NoError(t, env.Client.Update(t.Context(), ds))
		ds = liveDataplane(t, env)
	}

	annotations := map[string]string{}
	if native {
		annotations[nativeOriginAnnotation] = "true"
	}

	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       ds.Namespace,
			Labels:          ds.Spec.Selector.MatchLabels,
			Annotations:     annotations,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ds, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))},
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
	require.NoError(t, env.Client.Create(t.Context(), pod))
}

func TestLeftoverOriginsWaitForSurgeRollout(t *testing.T) {
	env := testEnv(t, originCache("a", nil))
	initialize(t, env)
	leftoverOrigin(t, env, "a", component.FieldOwner)

	// A surge rollout: the old Pod is still ready and serving while its
	// replacement is not, yet the counters all equal the desired count.
	dataplanePod(t, env, "old", false, true)
	dataplanePod(t, env, "new", true, false)

	ds := liveDataplane(t, env)
	setDataplaneStatus(t, env, appsv1.DaemonSetStatus{ObservedGeneration: ds.Generation, DesiredNumberScheduled: 1, UpdatedNumberScheduled: 1, NumberAvailable: 1})

	plan, result := planResult(t, env)
	require.Empty(t, deletedOrigins(plan))
	require.Equal(t, pendingRequeue, result.RequeueAfter)

	// The old Pod goes but the replacement is still unready: keep waiting.
	require.NoError(t, env.Client.Delete(t.Context(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: ds.Namespace}}))

	plan, _ = planResult(t, env)
	require.Empty(t, deletedOrigins(plan))

	// Only ready native-origin Pods remain: the origins go.
	require.NoError(t, env.Client.Delete(t.Context(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "new", Namespace: ds.Namespace}}))
	dataplanePod(t, env, "new2", true, true)

	plan, _ = planResult(t, env)
	require.Len(t, deletedOrigins(plan), 2)
}
