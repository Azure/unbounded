// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/Azure/unbounded/internal/operator/component"
)

// Use the production planner and executor against real SSA, defaulting, and
// immutable-field validation. No manager runs here, so each pass is deterministic.
func TestEnvtestDataplaneApply(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API-server workload execution")
	}

	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{"../../../../deploy/racer/crd"}, ErrorIfCRDPathMissing: true,
	}
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	env := testEnv(t)
	env.Client, err = client.New(rc, client.Options{Scheme: env.Scheme})
	require.NoError(t, err)

	env.APIReader = env.Client
	require.NoError(t, env.Client.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: env.Namespace}}))
	require.NoError(t, env.Client.Create(t.Context(), cache("cache")))
	initialize(t, env)

	ds := &appsv1.DaemonSet{}
	key := objectKey(env, dataplaneName)
	persist(t, env, planPass(t, env))
	require.NoError(t, env.Client.Get(t.Context(), key, ds))
	uid := ds.UID
	selector := ds.Spec.Selector.DeepCopy()
	want := ds.Spec.DeepCopy()
	require.True(t, slices.ContainsFunc(ds.ManagedFields, func(entry metav1.ManagedFieldsEntry) bool {
		return entry.Manager == component.FieldOwner && entry.Operation == metav1.ManagedFieldsOperationApply
	}))

	for _, scenario := range []string{"upgrade", "repair", "missing-label"} {
		t.Run(scenario, func(t *testing.T) {
			container := &ds.Spec.Template.Spec.Containers[0]

			switch scenario {
			case "upgrade":
				container.ReadinessProbe = nil
				container.Env = slices.DeleteFunc(container.Env, func(env corev1.EnvVar) bool {
					return env.Name == "RACER_POD_IP" || env.Name == "RACER_DIAGNOSTICS_LISTEN"
				})
				container.Ports = container.Ports[:1]
				ds.Spec.MinReadySeconds = 0
			case "repair":
				container.Image = "example.test/drift:v0"

				container.ReadinessProbe.HTTPGet.Path = "/healthz"
				for i := range container.Env {
					if container.Env[i].Name == "RACER_PEER_LISTEN" {
						container.Env[i].Value = "0.0.0.0:7443"
					}
				}
			case "missing-label":
				delete(ds.Labels, "app.kubernetes.io/name")
			}

			ds.Annotations = map[string]string{"admin": "preserve"}
			ds.Spec.Template.Annotations["rollout"] = "preserve"
			require.NoError(t, env.Client.Update(t.Context(), ds, client.FieldOwner("administrator")))
			persist(t, env, planPass(t, env))
			require.NoError(t, env.Client.Get(t.Context(), key, ds))
			require.Equal(t, uid, ds.UID)
			require.Equal(t, selector, ds.Spec.Selector)
			require.Equal(t, want.Template.Spec, ds.Spec.Template.Spec)
			require.Equal(t, want.MinReadySeconds, ds.Spec.MinReadySeconds)
			require.Equal(t, want.UpdateStrategy, ds.Spec.UpdateStrategy)
			require.Equal(t, "racer-dataplane", ds.Labels["app.kubernetes.io/name"])
			require.Equal(t, dataplaneName, ds.Labels["app.kubernetes.io/instance"])
			require.Equal(t, "preserve", ds.Annotations["admin"])
			require.Equal(t, "preserve", ds.Spec.Template.Annotations["rollout"])
			version := ds.ResourceVersion

			persist(t, env, planPass(t, env))
			require.NoError(t, env.Client.Get(t.Context(), key, ds))
			require.Equal(t, version, ds.ResourceVersion, "API defaults must not cause repeated writes")
		})
	}

	t.Run("delete-and-recreate", func(t *testing.T) {
		ds.Finalizers = []string{"test.unbounded-cloud.io/hold"}
		require.NoError(t, env.Client.Update(t.Context(), ds))
		require.NoError(t, env.Client.Delete(t.Context(), ds))
		// Live occupancy cannot be safely classified while its workload is
		// deleting. Planning must fail closed until deletion completes.
		_, _, err := (Component{}).Plan(t.Context(), env, nil)
		require.ErrorContains(t, err, "dataplane workload racer-dataplane is deleting")
		require.NoError(t, env.Client.Get(t.Context(), key, ds))
		require.False(t, ds.DeletionTimestamp.IsZero())
		require.Equal(t, uid, ds.UID)
		ds.Finalizers = nil
		require.NoError(t, env.Client.Update(t.Context(), ds))
		persist(t, env, planPass(t, env))
		require.NoError(t, env.Client.Get(t.Context(), key, ds))
		require.NotEqual(t, uid, ds.UID)
		require.True(t, ds.DeletionTimestamp.IsZero())
	})

	t.Run("immutable-selector", func(t *testing.T) {
		require.NoError(t, env.Client.Delete(t.Context(), ds))
		foreign := ds.DeepCopy()
		foreign.ResourceVersion, foreign.UID, foreign.ManagedFields = "", "", nil
		foreign.Spec.Selector.MatchLabels = map[string]string{"foreign": "selector"}
		foreign.Spec.Template.Labels["foreign"] = "selector"
		require.NoError(t, env.Client.Create(t.Context(), foreign))
		result, err := env.Execute(t.Context(), planPass(t, env))
		require.NoError(t, err)
		require.True(t, apierrors.IsInvalid(result.Err()), "%v", result.Err())
		require.Len(t, result.Failed(), 1)
		require.Equal(t, dataplaneName, result.Failed()[0].Ref.Name)
		require.Empty(t, result.Deferred)
		require.NoError(t, env.Client.Get(t.Context(), key, ds))
		require.Equal(t, foreign.UID, ds.UID)
		require.Equal(t, foreign.Spec.Selector, ds.Spec.Selector)
	})
}
