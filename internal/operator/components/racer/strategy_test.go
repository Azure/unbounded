// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/override"
	racercore "github.com/Azure/unbounded/internal/racer"
)

func strategyPlan(t *testing.T, env *component.Env) *component.Plan {
	t.Helper()

	cfg := occupancyConfig(env.Namespace)
	cfg.HostNetwork = true
	cfg.PodNetworkNodes = []string{"pod-node"}
	sets, _, err := migrationPlan(t.Context(), env, cfg)
	require.NoError(t, err)
	require.Len(t, sets, 2)

	plan := component.NewPlan()
	for _, ds := range sets {
		plan.Add(component.Operation{Kind: component.OpApply, Object: component.ToUnstructured(ds), Component: "racer", Overridable: true})
	}

	return plan
}

func strategyEntries(t *testing.T, name, strategy string) []override.SourcedEntry {
	t.Helper()

	entries, problems, err := override.Parse(map[string]string{"strategy.yaml": `apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
  - component: racer
    kind: DaemonSet
` + name + `    patch:
      spec:
        updateStrategy: ` + strategy + "\n"})
	require.NoError(t, err)
	require.Empty(t, problems)

	if name == "" {
		require.ErrorContains(t, override.ValidateErr(entries), "name is required for Racer DaemonSet overrides")
	} else {
		require.NoError(t, override.ValidateErr(entries))
	}

	return entries
}

func TestDaemonSetOnDeleteOverride(t *testing.T) {
	for _, name := range []string{"", "    name: racer-dataplane\n"} {
		t.Run(name, func(t *testing.T) {
			env := testEnv(t)
			plan := strategyPlan(t, env)
			host, pod := plan.Operations[0].Object.DeepCopy(), plan.Operations[1].Object.DeepCopy()

			entries := strategyEntries(t, name, "{type: OnDelete}")
			if name == "" {
				// Rejected entries must never reach Apply, whose input is validated.
				require.Equal(t, host, plan.Operations[0].Object)
				require.Equal(t, pod, plan.Operations[1].Object)

				return
			}

			report := override.Apply(plan, entries, nil)
			require.NoError(t, report.Err())
			require.Len(t, report.Workloads, 1)
			require.Empty(t, report.Withheld)

			want := host.DeepCopy()
			require.NoError(t, unstructured.SetNestedField(want.Object, "OnDelete", "spec", "updateStrategy", "type"))
			unstructured.RemoveNestedField(want.Object, "spec", "updateStrategy", "rollingUpdate")
			want.SetAnnotations(plan.Operations[0].Object.GetAnnotations())
			require.Equal(t, want, plan.Operations[0].Object, "only strategy and override annotations may change; preserve placement guards")
			require.Equal(t, pod, plan.Operations[1].Object, "host override must not touch podnet")
			require.Equal(t, "racer-dataplane", report.Workloads[0].Ref.Name)
			// Rendering again without overrides restores the original rolling strategy.
			require.Equal(t, host, strategyPlan(t, env).Operations[0].Object)
		})
	}

	t.Run("named-podnet-rolling", func(t *testing.T) {
		plan := strategyPlan(t, testEnv(t))
		host := plan.Operations[0].Object.DeepCopy()
		entries := strategyEntries(t, "    name: racer-dataplane-podnet\n", "{type: RollingUpdate, rollingUpdate: {maxUnavailable: '100%', maxSurge: 0}}")
		report := override.Apply(plan, entries, nil)
		require.NoError(t, report.Err())
		require.Len(t, report.Workloads, 1)
		require.Equal(t, racercore.PodNetworkDaemonSetName, report.Workloads[0].Ref.Name)
		require.Equal(t, host, plan.Operations[0].Object)
		strategy, _, err := unstructured.NestedMap(plan.Operations[1].Object.Object, "spec", "updateStrategy")
		require.NoError(t, err)
		require.Equal(t, map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxUnavailable": "100%", "maxSurge": int64(0)}}, strategy)
	})

	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "split-contributors", true: "reverse-contributors"}[reverse], func(t *testing.T) {
			plan := strategyPlan(t, testEnv(t))
			pod := plan.Operations[1].Object.DeepCopy()
			a := strategyEntries(t, "    name: racer-dataplane\n", "{type: OnDelete}")
			b := strategyEntries(t, "    name: racer-dataplane\n", "{rollingUpdate: {maxUnavailable: 1}}")

			b[0].Source.Key = "rolling.yaml"
			if reverse {
				a, b = b, a
			}

			report := override.Apply(plan, append(a, b...), nil)
			require.ErrorContains(t, report.Err(), "rollingUpdate")
			require.Len(t, report.Withheld, 1)
			require.Len(t, plan.Operations, 1)
			require.Equal(t, pod, plan.Operations[0].Object)
		})
	}
}

func TestEnvtestDaemonSetOnDeleteOverride(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for local API strategy validation")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets}
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	env := testEnv(t)
	env.Client, err = client.New(rc, client.Options{Scheme: env.Scheme})
	require.NoError(t, err)

	env.APIReader = env.Client
	require.NoError(t, env.Client.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: env.Namespace}}))
	plan := strategyPlan(t, env)
	persist(t, env, plan)
	key := objectKey(env, dataplaneName)
	before := &appsv1.DaemonSet{}
	require.NoError(t, env.Client.Get(t.Context(), key, before))
	// Kubernetes currently tolerates the pre-fix payload. The operator's
	// supported OnDelete contract is stricter: no ignored rolling settings.
	// Dry-run leaves the real rolling object unchanged.
	bad := plan.Operations[0].Object.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(bad.Object, "OnDelete", "spec", "updateStrategy", "type"))
	err = env.Client.Apply(t.Context(), client.ApplyConfigurationFromUnstructured(bad), client.FieldOwner(component.FieldOwner), client.ForceOwnership, client.DryRunAll)
	require.NoError(t, err)
	// RollingUpdate admission checks must still run: normalization must not
	// erase an invalid user rolling block to make admission succeed.
	invalidRolling := strategyPlan(t, env)
	rollingEntries := strategyEntries(t, "    name: racer-dataplane\n", "{type: RollingUpdate, rollingUpdate: {maxUnavailable: 1, maxSurge: 1}}")
	require.NoError(t, override.Apply(invalidRolling, rollingEntries, nil).Err())
	err = env.Client.Apply(t.Context(), client.ApplyConfigurationFromUnstructured(invalidRolling.Operations[0].Object), client.FieldOwner(component.FieldOwner), client.ForceOwnership, client.DryRunAll)
	require.True(t, apierrors.IsInvalid(err), "expected RollingUpdate admission rejection, got %v", err)
	entries := strategyEntries(t, "    name: racer-dataplane\n", "{type: OnDelete}")
	require.NoError(t, override.Apply(plan, entries, nil).Err())
	persist(t, env, plan)

	after := &appsv1.DaemonSet{}
	require.NoError(t, env.Client.Get(t.Context(), key, after))
	require.Equal(t, appsv1.OnDeleteDaemonSetStrategyType, after.Spec.UpdateStrategy.Type)
	require.Nil(t, after.Spec.UpdateStrategy.RollingUpdate, "SSA must remove the operator-owned rolling block")
	require.Equal(t, before.UID, after.UID)
	require.Equal(t, before.Spec.Template, after.Spec.Template)
	version := after.ResourceVersion

	persist(t, env, plan)
	require.NoError(t, env.Client.Get(t.Context(), key, after))
	require.Equal(t, version, after.ResourceVersion, "normalization must be idempotent")
	persist(t, env, strategyPlan(t, env))
	require.NoError(t, env.Client.Get(t.Context(), key, after))
	require.Equal(t, before.Spec.UpdateStrategy, after.Spec.UpdateStrategy, "removing override restores RollingUpdate")
}
