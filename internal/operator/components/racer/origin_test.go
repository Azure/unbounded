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
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/event"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racer/workload"
)

const testContainerURL = "https://Account.blob.core.windows.net/models"

func originCache(name string, credential *racerv1.AzureCredential) *racerv1.ClusterCache {
	cache := cacheObject(name)
	cache.Spec.Origin = &racerv1.ObjectOrigin{
		Azure: &racerv1.AzureOrigin{ContainerURL: testContainerURL, Credential: credential},
	}

	return cache
}

func liveOrigin(t *testing.T, env *component.Env, cache string) (*appsv1.DaemonSet, *corev1.ServiceAccount) {
	t.Helper()

	ds := &appsv1.DaemonSet{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, originName(cache)), ds))

	sa := &corev1.ServiceAccount{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, originName(cache)), sa))

	return ds, sa
}

func requireNoOrigin(t *testing.T, env *component.Env, cache string) {
	t.Helper()
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, originName(cache)), &appsv1.DaemonSet{})))
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), objectKey(env, originName(cache)), &corev1.ServiceAccount{})))
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

func TestOriginCredentialRendering(t *testing.T) {
	for _, tc := range []struct {
		name          string
		credential    *racerv1.AzureCredential
		mode          string
		env           []corev1.EnvVar
		envFrom       []corev1.EnvFromSource
		saAnnotations map[string]string
		podLabel      bool
	}{
		{name: "anonymous", mode: "anonymous"},
		{name: "default", credential: &racerv1.AzureCredential{Default: &racerv1.AzureDefaultCredential{}}, mode: "default"},
		{
			name: "default-client-id", credential: &racerv1.AzureCredential{Default: &racerv1.AzureDefaultCredential{ClientID: "client-a"}}, mode: "default",
			env: []corev1.EnvVar{{Name: "AZURE_CLIENT_ID", Value: "client-a"}},
		},
		{
			name: "default-secret", credential: &racerv1.AzureCredential{Default: &racerv1.AzureDefaultCredential{SecretRef: &racerv1.SecretReference{Name: "azure-sp"}}}, mode: "default",
			envFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "azure-sp"}}}},
		},
		{
			name: "workload-identity", credential: &racerv1.AzureCredential{WorkloadIdentity: &racerv1.AzureWorkloadIdentityCredential{ClientID: "client-b", TenantID: "tenant-b"}}, mode: "default",
			saAnnotations: map[string]string{workloadIdentityClientID: "client-b", workloadIdentityTenantID: "tenant-b"}, podLabel: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := originCache("cache", tc.credential)
			cache.Spec.Origin.MetadataTTL = &metav1.Duration{Duration: 0}
			cache.Spec.Origin.RequestTimeout = &metav1.Duration{Duration: 90 * time.Second}
			cache.Spec.Origin.MaxConcurrentRequests = ptr.To(int32(32))
			env := testEnv(t, cache)
			initialize(t, env)

			plan, result := planResult(t, env)
			require.True(t, result.Ready, "%+v", result)

			for _, op := range plan.Operations {
				if strings.HasPrefix(op.Object.GetName(), originPrefix) {
					require.False(t, op.Overridable)
					require.Equal(t, "racer-origin/cache", op.FailureDomain)
				}
			}

			persist(t, env, plan)

			ds, sa := liveOrigin(t, env, "cache")
			owner := []metav1.OwnerReference{{APIVersion: "racer.unbounded-cloud.io/v1alpha1", Kind: "ClusterCache", Name: "cache", UID: cache.UID, Controller: ptr.To(true)}}
			require.Equal(t, owner, ds.OwnerReferences)
			require.Equal(t, owner, sa.OwnerReferences)
			require.Equal(t, component.FieldOwner, ds.Annotations[managerAnnotation])
			require.NotEmpty(t, ds.Annotations[runtimeInstallationAnnotation])
			require.Equal(t, ds.Annotations[runtimeInstallationAnnotation], sa.Annotations[runtimeInstallationAnnotation])
			require.False(t, *sa.AutomountServiceAccountToken)
			require.Equal(t, originAppName, ds.Labels["app.kubernetes.io/component"])
			require.Equal(t, originAppName, sa.Labels["app.kubernetes.io/component"])

			for key, value := range tc.saAnnotations {
				require.Equal(t, value, sa.Annotations[key])
			}

			if tc.saAnnotations == nil {
				require.NotContains(t, sa.Annotations, workloadIdentityClientID)
				require.NotContains(t, sa.Annotations, workloadIdentityTenantID)
			}

			tpl := ds.Spec.Template
			require.Equal(t, originCacheValue("cache"), tpl.Labels[originCacheLabel])
			require.Equal(t, ds.Spec.Selector.MatchLabels[originCacheLabel], tpl.Labels[originCacheLabel])
			require.NotContains(t, tpl.Annotations, "racer.unbounded-cloud.io/origin-identity")
			require.NotEmpty(t, tpl.Annotations[originConfigHash])

			if tc.podLabel {
				require.Equal(t, "true", tpl.Labels[workloadIdentityUse])
			} else {
				require.NotContains(t, tpl.Labels, workloadIdentityUse)
			}

			pod := tpl.Spec
			require.Equal(t, originName("cache"), pod.ServiceAccountName)
			require.False(t, *pod.AutomountServiceAccountToken)
			require.EqualValues(t, 5, *pod.TerminationGracePeriodSeconds)
			require.EqualValues(t, 65532, *pod.SecurityContext.RunAsUser)
			require.EqualValues(t, 65532, *pod.SecurityContext.RunAsGroup)
			require.True(t, *pod.SecurityContext.RunAsNonRoot)
			require.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, pod.SecurityContext.SeccompProfile.Type)
			require.Len(t, pod.Containers, 1)

			c := pod.Containers[0]
			require.Equal(t, "mirror.test/unbounded/racer-object:v1", c.Image)
			require.Equal(t, []string{
				"origin", "--cache=cache", "--cache-uid=" + string(cache.UID), "--provider=azure",
				"--container-url=" + testContainerURL, "--azure-credential=" + tc.mode,
				"--metadata-ttl=0s", "--request-timeout=1m30s", "--max-concurrent-requests=32",
			}, c.Args)
			require.Equal(t, tc.env, c.Env)
			require.Equal(t, tc.envFrom, c.EnvFrom)
			require.False(t, *c.SecurityContext.AllowPrivilegeEscalation)
			require.True(t, *c.SecurityContext.ReadOnlyRootFilesystem)
			require.Equal(t, []corev1.Capability{"ALL"}, c.SecurityContext.Capabilities.Drop)
			require.Equal(t, "/run/racer/cache/origin", c.VolumeMounts[0].MountPath)
			require.Equal(t, "/run/racer/cache/origin", pod.Volumes[0].HostPath.Path)
			require.Equal(t, corev1.HostPathDirectory, *pod.Volumes[0].HostPath.Type)
			require.Equal(t, "100%", ds.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable.String())

			// Steady state is idempotent.
			persist(t, env, planPass(t, env))
			after, _ := liveOrigin(t, env, "cache")
			require.Equal(t, ds.Spec, after.Spec)
		})
	}
}

func TestOriginCacheUIDBindsRecreatedCache(t *testing.T) {
	old := originCache("cache", nil)
	recreated := originCache("cache", nil)
	require.NotEqual(t, old.UID, recreated.UID)

	oldSpec, err := renderOrigin(old)
	require.NoError(t, err)
	newSpec, err := renderOrigin(recreated)
	require.NoError(t, err)
	require.Contains(t, oldSpec.args, "--cache-uid="+string(old.UID))
	require.Contains(t, newSpec.args, "--cache-uid="+string(recreated.UID))

	oldHash, err := oldSpec.hash()
	require.NoError(t, err)
	newHash, err := newSpec.hash()
	require.NoError(t, err)
	require.NotEqual(t, oldHash, newHash)

	old.UID = ""
	_, err = renderOrigin(old)
	require.ErrorContains(t, err, "metadata.uid is required")
}

func TestOriginOptionalFlagsOmittedWhenUnset(t *testing.T) {
	spec, err := renderOrigin(originCache("cache", nil))
	require.NoError(t, err)

	for _, arg := range spec.args {
		require.NotContains(t, arg, "--metadata-ttl")
		require.NotContains(t, arg, "--request-timeout")
		require.NotContains(t, arg, "--max-concurrent-requests")
	}
}

func TestOriginConfigHashTracksCredentialIdentity(t *testing.T) {
	wi := func(client, tenant string) string {
		cache := originCache("cache", &racerv1.AzureCredential{WorkloadIdentity: &racerv1.AzureWorkloadIdentityCredential{ClientID: client, TenantID: tenant}})
		cache.UID = "00000000-0000-4000-8000-000000000001"
		spec, err := renderOrigin(cache)
		require.NoError(t, err)
		require.Empty(t, spec.env)

		hash, err := spec.hash()
		require.NoError(t, err)

		return hash
	}

	base := wi("a", "t")
	require.NotEqual(t, base, wi("b", "t"))
	require.NotEqual(t, base, wi("a", "u"))
	require.NotEqual(t, base, wi("a", ""))
	require.Equal(t, base, wi("a", "t"))
}

func TestOriginSwitchFromWorkloadIdentityToDefault(t *testing.T) {
	env := testEnv(t, originCache("cache", &racerv1.AzureCredential{WorkloadIdentity: &racerv1.AzureWorkloadIdentityCredential{ClientID: "c", TenantID: "t"}}))
	initialize(t, env)
	persist(t, env, planPass(t, env))

	ds, sa := liveOrigin(t, env, "cache")
	require.Equal(t, "c", sa.Annotations[workloadIdentityClientID])
	before := ds.Spec.Template.Annotations[originConfigHash]

	updateCache(t, env, "cache", func(c *racerv1.ClusterCache) {
		c.Spec.Origin.Azure.Credential = &racerv1.AzureCredential{Default: &racerv1.AzureDefaultCredential{}}
	})
	persist(t, env, planPass(t, env))

	ds, sa = liveOrigin(t, env, "cache")
	require.NotContains(t, sa.Annotations, workloadIdentityClientID)
	require.NotContains(t, sa.Annotations, workloadIdentityTenantID)
	require.NotContains(t, ds.Spec.Template.Labels, workloadIdentityUse)
	require.NotEqual(t, before, ds.Spec.Template.Annotations[originConfigHash])
}

func TestOriginMultipleCachesAndPruning(t *testing.T) {
	env := testEnv(t, originCache("a", nil), originCache("b", nil), cacheObject("plain"))
	initialize(t, env)
	persist(t, env, planPass(t, env))

	liveOrigin(t, env, "a")
	liveOrigin(t, env, "b")
	requireNoOrigin(t, env, "plain")

	a, _ := liveOrigin(t, env, "a")
	b, _ := liveOrigin(t, env, "b")
	require.NotEqual(t, a.Spec.Selector.MatchLabels[originCacheLabel], b.Spec.Selector.MatchLabels[originCacheLabel])

	// An origin of another installation is never pruned.
	foreign := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: originName("gone"), Namespace: env.Namespace, Labels: originLabels(),
			Annotations: map[string]string{managerAnnotation: component.FieldOwner, runtimeInstallationAnnotation: "other"},
		},
	}
	require.NoError(t, env.Client.Create(t.Context(), foreign))

	updateCache(t, env, "a", func(c *racerv1.ClusterCache) { c.Spec.Origin = nil })

	plan := planPass(t, env)

	deleted := map[string]bool{}

	for _, op := range plan.Operations {
		if op.Kind == component.OpDelete {
			deleted[op.Object.GetKind()+"/"+op.Object.GetName()] = true
		}
	}

	require.Equal(t, map[string]bool{"DaemonSet/" + originName("a"): true, "ServiceAccount/" + originName("a"): true}, deleted)
	persist(t, env, plan)

	requireNoOrigin(t, env, "a")
	liveOrigin(t, env, "b")
	liveDataplane(t, env)
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, originName("gone")), &appsv1.DaemonSet{}))
}

func TestOriginPrunedWhileCacheTerminates(t *testing.T) {
	cache := originCache("held", nil)
	cache.Finalizers = []string{"test/hold"}

	env := testEnv(t, cache, cacheObject("other"))
	initialize(t, env)
	persist(t, env, planPass(t, env))
	liveOrigin(t, env, "held")

	held := &racerv1.ClusterCache{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(&component.Env{}, "held"), held))
	require.NoError(t, env.Client.Delete(t.Context(), held))
	require.NoError(t, env.Client.Get(t.Context(), objectKey(&component.Env{}, "held"), held))
	require.NotNil(t, held.DeletionTimestamp)

	persist(t, env, planPass(t, env))
	requireNoOrigin(t, env, "held")
	liveDataplane(t, env)
}

func TestOriginInvalidCacheIsIsolated(t *testing.T) {
	env := testEnv(t, originCache("bad", nil), originCache("good", nil))
	initialize(t, env)
	persist(t, env, planPass(t, env))

	bad, _ := liveOrigin(t, env, "bad")

	// The fake client does not run CEL, modeling a cache that bypassed admission.
	updateCache(t, env, "bad", func(c *racerv1.ClusterCache) { c.Spec.Origin.Azure.ContainerURL = "https://user@host/models" })
	updateCache(t, env, "good", func(c *racerv1.ClusterCache) { c.Spec.Origin.MetadataTTL = &metav1.Duration{Duration: 7 * time.Second} })

	plan, result := planResult(t, env)
	require.False(t, result.Ready)
	require.Equal(t, "OriginInvalid", result.Reason)
	require.Contains(t, result.Message, "bad (")
	require.NotContains(t, result.Message, "good")

	for _, op := range plan.Operations {
		require.NotEqual(t, originName("bad"), op.Object.GetName(), "%s", op.Ref())
	}

	dataplaneOp(t, plan)
	persist(t, env, plan)

	kept, _ := liveOrigin(t, env, "bad")
	require.Equal(t, bad.Spec, kept.Spec)

	good, _ := liveOrigin(t, env, "good")
	require.Contains(t, good.Spec.Template.Spec.Containers[0].Args, "--metadata-ttl=7s")
}

func containerURL(value string) func(*racerv1.ObjectOrigin) {
	return func(o *racerv1.ObjectOrigin) { o.Azure.ContainerURL = value }
}

func TestRenderOriginRejectsInvalidSpecs(t *testing.T) {
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
		{"default-both", func(o *racerv1.ObjectOrigin) {
			o.Azure.Credential = &racerv1.AzureCredential{Default: &racerv1.AzureDefaultCredential{ClientID: "c", SecretRef: &racerv1.SecretReference{Name: "s"}}}
		}, "mutually exclusive"},
		{"secret-slash", secretRefName("bad/name"), "secretRef.name"},
		{"secret-dots", secretRefName(".."), "secretRef.name"},
		{"secret-upper", secretRefName("Azure"), "secretRef.name"},
		{"wi-no-client", func(o *racerv1.ObjectOrigin) {
			o.Azure.Credential = &racerv1.AzureCredential{WorkloadIdentity: &racerv1.AzureWorkloadIdentityCredential{}}
		}, "clientID is required"},
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
			_, err := renderOrigin(cache)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func secretRefName(name string) func(*racerv1.ObjectOrigin) {
	return func(o *racerv1.ObjectOrigin) {
		o.Azure.Credential = &racerv1.AzureCredential{Default: &racerv1.AzureDefaultCredential{SecretRef: &racerv1.SecretReference{Name: name}}}
	}
}

func TestRenderOriginAcceptsValidSecretNames(t *testing.T) {
	for _, name := range []string{"azure", "azure-creds", "a.b-c.d"} {
		cache := originCache("cache", nil)
		secretRefName(name)(cache.Spec.Origin)

		spec, err := renderOrigin(cache)
		require.NoError(t, err, name)
		require.Equal(t, name, spec.envFrom[0].SecretRef.Name)
	}
}

func TestOriginMalformedSecretRefIsIsolated(t *testing.T) {
	env := testEnv(t, originCache("bad", nil), originCache("good", nil))
	initialize(t, env)
	persist(t, env, planPass(t, env))

	// The fake client does not enforce the CRD pattern, modeling a cache that bypassed admission.
	updateCache(t, env, "bad", func(c *racerv1.ClusterCache) { secretRefName("bad/name")(c.Spec.Origin) })
	updateCache(t, env, "good", func(c *racerv1.ClusterCache) { c.Spec.Origin.MetadataTTL = &metav1.Duration{Duration: 7 * time.Second} })

	plan, result := planResult(t, env)
	require.Equal(t, "OriginInvalid", result.Reason)
	require.Contains(t, result.Message, "bad (")
	require.NotContains(t, result.Message, "good")

	dataplaneOp(t, plan)
	persist(t, env, plan)

	good, _ := liveOrigin(t, env, "good")
	require.Contains(t, good.Spec.Template.Spec.Containers[0].Args, "--metadata-ttl=7s")
}

func TestUnmanagedOriginIsNotAdopted(t *testing.T) {
	for _, kind := range []string{"DaemonSet", "ServiceAccount"} {
		t.Run(kind, func(t *testing.T) {
			env := testEnv(t, originCache("taken", nil), originCache("free", nil))
			meta := metav1.ObjectMeta{Name: originName("taken"), Namespace: env.Namespace}

			if kind == "DaemonSet" {
				require.NoError(t, env.Client.Create(t.Context(), &appsv1.DaemonSet{ObjectMeta: meta}))
			} else {
				require.NoError(t, env.Client.Create(t.Context(), &corev1.ServiceAccount{ObjectMeta: meta}))
			}

			initialize(t, env)

			plan, result := planResult(t, env)
			require.Equal(t, "OriginInvalid", result.Reason)
			require.Contains(t, result.Message, "taken (")
			require.Contains(t, result.Message, "refusing adoption")

			for _, op := range plan.Operations {
				require.NotEqual(t, originName("taken"), op.Object.GetName(), "%s", op.Ref())
			}

			persist(t, env, plan)
			liveOrigin(t, env, "free")
		})
	}
}

func TestOriginPlacementMirrorsDataplane(t *testing.T) {
	env := testEnv(t, originCache("cache", nil))
	initialize(t, env)
	persist(t, env, planPass(t, env))

	ds, _ := liveOrigin(t, env, "cache")
	require.Equal(t, workload.DataplaneAffinity(), ds.Spec.Template.Spec.Affinity)

	dataplane := liveDataplane(t, env)
	dataplane.Spec.Template.Spec.NodeSelector = map[string]string{"pool": "gpu"}
	dataplane.Spec.Template.Spec.Tolerations = []corev1.Toleration{{Key: "gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
	dataplane.Spec.Template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}}}},
	}}}
	nodeAffinity := dataplane.Spec.Template.Spec.Affinity.NodeAffinity.DeepCopy()
	dataplaneTerm := corev1.PodAffinityTerm{
		TopologyKey:   corev1.LabelHostname,
		LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": dataplaneName}},
	}
	dataplane.Spec.Template.Spec.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{dataplaneTerm}}
	dataplane.Spec.Template.Spec.Affinity.PodAffinity = &corev1.PodAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{dataplaneTerm}}
	require.NoError(t, env.Client.Update(t.Context(), dataplane))

	plan := planPass(t, env)

	for _, op := range plan.Operations {
		if op.Object.GetName() == originName("cache") && op.Object.GetKind() == "DaemonSet" {
			obj := &appsv1.DaemonSet{}
			require.NoError(t, env.Scheme.Convert(op.Object, obj, nil))
			require.Equal(t, dataplane.Spec.Template.Spec.NodeSelector, obj.Spec.Template.Spec.NodeSelector)
			require.Equal(t, dataplane.Spec.Template.Spec.Tolerations, obj.Spec.Template.Spec.Tolerations)
			require.Equal(t, &corev1.Affinity{NodeAffinity: nodeAffinity}, obj.Spec.Template.Spec.Affinity)
			require.Nil(t, obj.Spec.Template.Spec.Affinity.PodAffinity)
			require.Nil(t, obj.Spec.Template.Spec.Affinity.PodAntiAffinity)

			return
		}
	}

	t.Fatalf("no origin DaemonSet in plan:\n%s", plan.Summary())
}

func TestOriginMirrorsDataplanePullSecrets(t *testing.T) {
	env := testEnv(t, originCache("cache", nil))
	initialize(t, env)
	persist(t, env, planPass(t, env))

	ds, _ := liveOrigin(t, env, "cache")
	require.Empty(t, ds.Spec.Template.Spec.ImagePullSecrets)

	for _, secrets := range [][]corev1.LocalObjectReference{
		{{Name: "registry-a"}},
		{{Name: "registry-b"}, {Name: "registry-c"}},
		nil,
	} {
		dataplane := liveDataplane(t, env)
		dataplane.Spec.Template.Spec.ImagePullSecrets = secrets
		require.NoError(t, env.Client.Update(t.Context(), dataplane))
		persist(t, env, planPass(t, env))

		ds, _ := liveOrigin(t, env, "cache")
		if len(secrets) == 0 {
			require.Empty(t, ds.Spec.Template.Spec.ImagePullSecrets)
		} else {
			require.Equal(t, secrets, ds.Spec.Template.Spec.ImagePullSecrets)
		}
	}
}

func TestOriginPullSecretsFallBackToDataplaneServiceAccount(t *testing.T) {
	env := testEnv(t, originCache("cache", nil))
	initialize(t, env)
	persist(t, env, planPass(t, env))

	account := &corev1.ServiceAccount{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneName), account))
	account.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "sa-registry"}}
	require.NoError(t, env.Client.Update(t.Context(), account))
	persist(t, env, planPass(t, env))

	ds, _ := liveOrigin(t, env, "cache")
	require.Equal(t, []corev1.LocalObjectReference{{Name: "sa-registry"}}, ds.Spec.Template.Spec.ImagePullSecrets)

	// Explicit template secrets take precedence over the ServiceAccount.
	dataplane := liveDataplane(t, env)
	dataplane.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "template-registry"}}
	require.NoError(t, env.Client.Update(t.Context(), dataplane))
	persist(t, env, planPass(t, env))

	ds, _ = liveOrigin(t, env, "cache")
	require.Equal(t, []corev1.LocalObjectReference{{Name: "template-registry"}}, ds.Spec.Template.Spec.ImagePullSecrets)
}

func TestOriginPlacementMissingServiceAccount(t *testing.T) {
	env := testEnv(t, originCache("cache", nil))
	initialize(t, env)
	persist(t, env, planPass(t, env))

	dataplane := liveDataplane(t, env)
	dataplane.Spec.Template.Spec.ServiceAccountName = "absent"
	require.NoError(t, env.Client.Update(t.Context(), dataplane))

	mirror, found, err := originPlacement(t.Context(), env)
	require.NoError(t, err)
	require.True(t, found)
	require.Empty(t, mirror.pullSecrets)
}

func TestOriginLongDottedCacheName(t *testing.T) {
	name := strings.Repeat("a.", 40) + "bc"
	require.Len(t, name, 82)

	env := testEnv(t, originCache(name, nil))
	initialize(t, env)
	persist(t, env, planPass(t, env))

	ds, _ := liveOrigin(t, env, name)
	require.Empty(t, validation.IsDNS1123Subdomain(ds.Name))
	require.Empty(t, validation.IsValidLabelValue(ds.Spec.Selector.MatchLabels[originCacheLabel]))
	require.Equal(t, "/run/racer/"+name+"/origin", ds.Spec.Template.Spec.Volumes[0].HostPath.Path)
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
		{originName("cache"), env.Namespace, true},
		{originPrefix, env.Namespace, false},
		{originName("cache"), "other", false},
		{"unrelated", env.Namespace, false},
	} {
		ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: tc.namespace}}
		require.Equal(t, tc.want, predicate.Create(event.CreateEvent{Object: ds}), "%s/%s", tc.namespace, tc.name)
		require.Equal(t, tc.want, match(&corev1.ServiceAccount{ObjectMeta: ds.ObjectMeta}), "%s/%s", tc.namespace, tc.name)
	}
}

func planHasObject(plan *component.Plan, kind, name string) bool {
	for _, op := range plan.Operations {
		if op.Object.GetKind() == kind && op.Object.GetName() == name {
			return true
		}
	}

	return false
}

func TestOriginWaitsForDataplaneOnInitialInstall(t *testing.T) {
	env := testEnv(t, originCache("cache", nil))

	var plan *component.Plan

	for range 9 {
		var result component.Result

		plan, result = planResult(t, env)
		if planHasObject(plan, "DaemonSet", dataplaneName) {
			require.Equal(t, "OriginPending", result.Reason)
			break
		}

		persist(t, env, plan)
	}

	require.True(t, planHasObject(plan, "DaemonSet", dataplaneName), "dataplane never planned")
	require.False(t, planHasObject(plan, "DaemonSet", originName("cache")))
	require.False(t, planHasObject(plan, "ServiceAccount", originName("cache")))
	persist(t, env, plan)
	requireNoOrigin(t, env, "cache")

	// Simulate restrictive placement overrides applied to the dataplane.
	dataplane := liveDataplane(t, env)
	dataplane.Spec.Template.Spec.NodeSelector = map[string]string{"pool": "gpu"}
	dataplane.Spec.Template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}}}},
	}}}
	require.NoError(t, env.Client.Update(t.Context(), dataplane))

	plan, result := planResult(t, env)
	require.NotEqual(t, "OriginPending", result.Reason)
	persist(t, env, plan)

	ds, _ := liveOrigin(t, env, "cache")
	require.Equal(t, map[string]string{"pool": "gpu"}, ds.Spec.Template.Spec.NodeSelector)
	require.Equal(t, dataplane.Spec.Template.Spec.Affinity.NodeAffinity, ds.Spec.Template.Spec.Affinity.NodeAffinity)
}

func TestOriginPruneProceedsWhileDataplaneWithheld(t *testing.T) {
	env := testEnv(t, originCache("keep", nil), originCache("stale", nil))
	initialize(t, env)
	persist(t, env, planPass(t, env))
	liveOrigin(t, env, "keep")
	liveOrigin(t, env, "stale")

	updateCache(t, env, "stale", func(cache *racerv1.ClusterCache) { cache.Spec.Origin = nil })
	require.NoError(t, env.Client.Delete(t.Context(), liveDataplane(t, env)))

	plan, result := planResult(t, env)
	require.Equal(t, "OriginPending", result.Reason)
	require.False(t, planHasObject(plan, "DaemonSet", originName("keep")))

	// Execute only the origin operations, as if the dataplane were withheld.
	filtered := &component.Plan{}

	for _, op := range plan.Operations {
		if strings.HasPrefix(op.Object.GetName(), originPrefix) {
			filtered.Add(op)
		}
	}

	persist(t, env, filtered)
	requireNoOrigin(t, env, "stale")
	liveOrigin(t, env, "keep")
}

func TestOriginCredentialSecretRotationRollsPods(t *testing.T) {
	cache := originCache("cache", &racerv1.AzureCredential{Default: &racerv1.AzureDefaultCredential{SecretRef: &racerv1.SecretReference{Name: "creds"}}})
	env := testEnv(t, cache)
	initialize(t, env)
	persist(t, env, planPass(t, env))

	hash := func() string {
		ds, _ := liveOrigin(t, env, "cache")
		return ds.Spec.Template.Annotations[originConfigHash]
	}

	missing := hash()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: env.Namespace},
		Data:       map[string][]byte{"AZURE_CLIENT_SECRET": []byte("one")},
	}
	require.NoError(t, env.Client.Create(t.Context(), secret))
	persist(t, env, planPass(t, env))

	first := hash()
	require.NotEqual(t, missing, first, "creating the Secret rolls the Pods")

	persist(t, env, planPass(t, env))
	require.Equal(t, first, hash(), "unchanged Secret keeps the hash")

	secret.Data["AZURE_CLIENT_SECRET"] = []byte("two")
	require.NoError(t, env.Client.Update(t.Context(), secret))
	persist(t, env, planPass(t, env))

	rotated := hash()
	require.NotEqual(t, first, rotated, "rotating the Secret rolls the Pods")
	require.NotContains(t, rotated, "two")

	require.NoError(t, env.Client.Delete(t.Context(), secret))
	persist(t, env, planPass(t, env))
	require.Equal(t, missing, hash())
}

func TestSecretWatchedMatchesReferencedCredentials(t *testing.T) {
	cache := originCache("cache", &racerv1.AzureCredential{Default: &racerv1.AzureDefaultCredential{SecretRef: &racerv1.SecretReference{Name: "creds"}}})
	env := testEnv(t, cache, originCache("anon", nil))
	match := secretWatched(env)

	secret := func(ns, name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	}

	require.True(t, match(secret(env.Namespace, tlsName)))
	require.True(t, match(secret(env.Namespace, "creds")))
	require.False(t, match(secret("other", "creds")), "other namespace")
	require.False(t, match(secret(env.Namespace, "unrelated")))
}

// kubeletExpand mirrors kubelet's $(VAR) expansion of container args: "$$"
// becomes "$", and "$(NAME)" becomes the value of a defined variable.
func kubeletExpand(in string, env map[string]string) string {
	var out strings.Builder

	for i := 0; i < len(in); i++ {
		if in[i] != '$' || i+1 >= len(in) {
			out.WriteByte(in[i])
			continue
		}

		switch in[i+1] {
		case '$':
			out.WriteByte('$')
			i++
		case '(':
			end := strings.IndexByte(in[i+2:], ')')
			if end < 0 {
				out.WriteByte(in[i])
				continue
			}

			name := in[i+2 : i+2+end]
			if v, ok := env[name]; ok {
				out.WriteString(v)
			} else {
				out.WriteString(in[i : i+3+end])
			}

			i += 2 + end
		default:
			out.WriteByte(in[i])
		}
	}

	return out.String()
}

func TestOriginEscapesContainerURLVariableReferences(t *testing.T) {
	const url = "https://$(STORAGE_HOST)/models"

	cache := originCache("cache", nil)
	cache.Spec.Origin.Azure.ContainerURL = url

	_, err := renderOrigin(cache)
	require.ErrorContains(t, err, "spec.origin.azure.containerURL")

	// Validation rejects the host, but the arg is still escaped as defense in
	// depth: after kubelet expansion it must stay literal.
	env := map[string]string{"STORAGE_HOST": "other.blob.core.windows.net"}
	require.Equal(t, "--container-url="+url, kubeletExpand("--container-url="+escapeKubeArg(url), env))
	require.Equal(t, "$$(A)$$$$", escapeKubeArg("$(A)$$"))
	require.Equal(t, "$(A)$$", kubeletExpand(escapeKubeArg("$(A)$$"), map[string]string{"A": "x"}))

	spec, err := renderOrigin(originCache("cache", nil))
	require.NoError(t, err)
	require.Contains(t, spec.args, "--container-url="+testContainerURL)
}
