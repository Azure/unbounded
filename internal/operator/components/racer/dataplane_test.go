// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/override"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func dataplaneOp(t *testing.T, plan *component.Plan) component.Operation {
	t.Helper()

	for _, op := range plan.Operations {
		if op.Object.GetKind() == "DaemonSet" && op.Object.GetName() == dataplaneName {
			return op
		}
	}

	t.Fatalf("plan has no DaemonSet %s:\n%s", dataplaneName, plan.Summary())

	return component.Operation{}
}

func liveDataplane(t *testing.T, env *component.Env) *appsv1.DaemonSet {
	t.Helper()

	ds := &appsv1.DaemonSet{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneName), ds))

	return ds
}

func TestClusterCacheDeploysDataplane(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)
	plan := planPass(t, env)

	op := dataplaneOp(t, plan)
	require.True(t, op.Overridable)
	require.NotNil(t, op.ValidateOverride)

	for _, dependency := range []string{trustName, dataplaneName, controllerName} {
		found := false

		for _, ref := range op.DependsOn {
			found = found || ref.Name == dependency
		}

		require.True(t, found, "DaemonSet must depend on %s", dependency)
	}

	persist(t, env, plan)

	account := &corev1.ServiceAccount{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneName), account))
	require.False(t, *account.AutomountServiceAccountToken)
	require.Equal(t, component.FieldOwner, account.Annotations[managerAnnotation])

	marker := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, markerName), marker))

	tuning := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), tuning))
	require.Equal(t, "4", tuning.Data["RACER_SHARES"])

	ds := liveDataplane(t, env)
	require.Equal(t, string(marker.UID), ds.Annotations[runtimeInstallationAnnotation])
	pod := ds.Spec.Template.Spec
	require.Equal(t, dataplaneName, pod.ServiceAccountName)
	require.False(t, pod.HostNetwork)
	require.Equal(t, component.ConfigMapPayloadHash(tuning), ds.Spec.Template.Annotations[dataplaneConfigHash])
	require.Len(t, pod.Containers, 1)
	container := pod.Containers[0]
	require.Equal(t, "mirror.test/unbounded/racer-dataplane:v1", container.Image)
	require.Equal(t, []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: dataplaneConfigName}}}}, container.EnvFrom)

	values := map[string]string{}
	for _, e := range container.Env {
		values[e.Name] = e.Value
	}

	require.Equal(t, marker.Data["cluster"], values["RACER_CLUSTER_ID"])
	require.Equal(t, "https://racer-controller.custom-system.svc:8443", values["RACER_CONTROL_ENDPOINT"])
	require.Equal(t, "[$(RACER_POD_IP)]:8082", values["RACER_PEER_LISTEN"])

	var bootstrap *corev1.Volume

	for i := range pod.Volumes {
		if pod.Volumes[i].Name == "bootstrap" {
			bootstrap = &pod.Volumes[i]
		}

		if pod.Volumes[i].Projected != nil {
			require.Equal(t, wire.TokenAudience, pod.Volumes[i].Projected.Sources[0].ServiceAccountToken.Audience)
		}
	}

	require.NotNil(t, bootstrap)
	require.Equal(t, trustName, bootstrap.ConfigMap.Name)

	// Steady state is idempotent.
	persist(t, env, planPass(t, env))
	require.Equal(t, ds.Spec, liveDataplane(t, env).Spec)
}

func TestDataplaneConfigEditsArePreservedAndRolledOut(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)
	persist(t, env, planPass(t, env))
	before := liveDataplane(t, env).Spec.Template.Annotations[dataplaneConfigHash]

	tuning := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), tuning))
	tuning.Data["RACER_SHARES"] = "8"
	tuning.Data["RACER_IO_WORKERS"] = "20"
	delete(tuning.Data, "RACER_SLAB_BYTES")
	require.NoError(t, env.Client.Update(t.Context(), tuning))

	persist(t, env, planPass(t, env))
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), tuning))
	require.Equal(t, "8", tuning.Data["RACER_SHARES"])
	require.Equal(t, "20", tuning.Data["RACER_IO_WORKERS"])
	require.NotContains(t, tuning.Data, "RACER_SLAB_BYTES")

	after := liveDataplane(t, env).Spec.Template.Annotations[dataplaneConfigHash]
	require.NotEqual(t, before, after)
	require.Equal(t, component.ConfigMapPayloadHash(tuning), after)
}

func TestDataplaneWorkloadInputsFromRacerConfig(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	cm := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, configName), cm))
	cm.Data["RACER_HOST_NETWORK"] = "true"
	cm.Data["RACER_DIAGNOSTICS_PORT"] = "9300"
	require.NoError(t, env.Client.Update(t.Context(), cm))
	persist(t, env, planPass(t, env))

	pod := liveDataplane(t, env).Spec.Template.Spec
	require.True(t, pod.HostNetwork)
	require.Equal(t, corev1.DNSClusterFirstWithHostNet, pod.DNSPolicy)
	require.EqualValues(t, 9300, pod.Containers[0].Ports[1].ContainerPort)

	for _, tc := range []struct{ key, value, want string }{
		{"RACER_POD_NETWORK_NODES", `["node-a"]`, "not supported"},
		{"RACER_HOST_NETWORK", "yes", "RACER_HOST_NETWORK"},
		{"RACER_DIAGNOSTICS_PORT", "80", "RACER_DIAGNOSTICS_PORT"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, configName), cm))
			previous, had := cm.Data[tc.key]
			cm.Data[tc.key] = tc.value
			require.NoError(t, env.Client.Update(t.Context(), cm))

			plan, _, err := (Component{}).Plan(t.Context(), env, nil)
			require.ErrorContains(t, err, tc.want)
			require.Nil(t, plan)

			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, configName), cm))

			if had {
				cm.Data[tc.key] = previous
			} else {
				delete(cm.Data, tc.key)
			}

			require.NoError(t, env.Client.Update(t.Context(), cm))
		})
	}
}

func TestUnmanagedDataplaneIsNotAdopted(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)
	persist(t, env, planPass(t, env))

	ds := liveDataplane(t, env)
	require.NoError(t, env.Client.Delete(t.Context(), ds))

	external := &appsv1.DaemonSet{}
	external.Name, external.Namespace = dataplaneName, env.Namespace
	require.NoError(t, env.Client.Create(t.Context(), external))

	plan, _, err := (Component{}).Plan(t.Context(), env, nil)
	require.ErrorContains(t, err, "refusing adoption")
	require.Nil(t, plan)

	actual := &appsv1.DaemonSet{}
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKeyFromObject(external), actual))
	require.Empty(t, actual.Annotations[managerAnnotation])
}

func TestDataplaneRetainedWhenLastCacheRemoved(t *testing.T) {
	cache := cacheObject("cache")
	env := testEnv(t, cache)
	initialize(t, env)
	persist(t, env, planPass(t, env))
	require.NoError(t, env.Client.Delete(t.Context(), cache))

	plan := planPass(t, env)
	for _, op := range plan.Operations {
		require.NotEqual(t, component.OpDelete, op.Kind, "%s", op.Ref())
		require.NotEqual(t, "DaemonSet", op.Object.GetKind())
	}

	persist(t, env, plan)
	liveDataplane(t, env)
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), &appsv1.Deployment{}))
}

func TestDataplaneOverrides(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	for _, tc := range []struct {
		name      string
		patch     map[string]any
		wantError string
	}{
		{name: "tuning", patch: map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{"benchmark": "yes"}},
			"spec": map[string]any{
				"updateStrategy": map[string]any{"rollingUpdate": map[string]any{"maxUnavailable": "50%"}},
				"template": map[string]any{"spec": map[string]any{
					"nodeSelector": map[string]any{"pool": "bench"},
					"containers": []any{map[string]any{
						"name":      "dataplane",
						"env":       []any{map[string]any{"name": "RACER_MAX_THREADS", "value": "8"}},
						"resources": map[string]any{"limits": map[string]any{"memory": "4Gi"}},
					}},
				}},
			},
		}},
		{name: "identity", wantError: "RACER_CONTROL_ENDPOINT", patch: map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "dataplane", "env": []any{map[string]any{"name": "RACER_CONTROL_ENDPOINT", "value": "https://evil:8443"}}}},
		}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := planPass(t, env)
			count := plan.Len()
			entries := []override.SourcedEntry{{Source: override.Source{Key: "racer.yaml"}, Entry: override.Entry{Component: "racer", Kind: "DaemonSet", Patch: tc.patch}}}
			require.NoError(t, override.ValidateErr(entries))
			report := override.Apply(plan, entries, nil)

			if tc.wantError != "" {
				require.True(t, report.Failed())
				require.ErrorContains(t, report.Err(), tc.wantError)
				require.Equal(t, count-1, plan.Len())
				require.Len(t, report.Withheld, 1)
				require.Equal(t, dataplaneName, report.Withheld[0].Ref.Name)

				return
			}

			require.False(t, report.Failed(), "%v", report.Err())
			require.Equal(t, count, plan.Len())

			obj := dataplaneOp(t, plan).Object
			selector, _, err := unstructured.NestedStringMap(obj.Object, "spec", "template", "spec", "nodeSelector")
			require.NoError(t, err)
			require.Equal(t, map[string]string{"pool": "bench"}, selector)
			require.Equal(t, "yes", obj.GetAnnotations()["benchmark"])

			persist(t, env, plan)

			ds := liveDataplane(t, env)
			require.Equal(t, "50%", ds.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable.String())
			require.Equal(t, "4Gi", ds.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().String())
			require.Contains(t, ds.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "RACER_MAX_THREADS", Value: "8"})
		})
	}
}
