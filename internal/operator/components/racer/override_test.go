// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/override"
)

func TestPlannedDeploymentOverrideValidation(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	for _, tc := range []struct {
		name      string
		field     string
		value     any
		wantError bool
	}{
		{name: "resources", field: "resources", value: map[string]any{"limits": map[string]any{"memory": "512Mi"}}},
		{name: "env", field: "env", value: []any{map[string]any{"name": "RACER_CLUSTER", "value": "other"}}, wantError: true},
		{name: "envFrom", field: "envFrom", value: []any{map[string]any{"configMapRef": map[string]any{"name": "other"}}}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := planPass(t, env)

			var deployment *unstructured.Unstructured

			for _, op := range plan.Operations {
				if op.Object.GetKind() == "Deployment" {
					require.True(t, op.Overridable)
					require.NotNil(t, op.ValidateOverride)
					deployment = op.Object
				} else {
					require.Nil(t, op.ValidateOverride)
				}
			}

			require.NotNil(t, deployment)
			original := deployment.DeepCopy()
			count := plan.Len()
			patch := map[string]any{}
			require.NoError(t, unstructured.SetNestedSlice(patch, []any{map[string]any{"name": "controller", tc.field: tc.value}}, "spec", "template", "spec", "containers"))
			entries := []override.SourcedEntry{{Source: override.Source{Key: "racer.yaml"}, Entry: override.Entry{Component: "racer", Kind: "Deployment", Patch: patch}}}
			require.NoError(t, override.ValidateErr(entries))
			report := override.Apply(plan, entries, nil)
			require.Equal(t, tc.wantError, report.Failed(), "%v", report.Err())
			require.Equal(t, original, deployment)

			if tc.wantError {
				require.Equal(t, count-1, plan.Len())
				require.Len(t, report.Withheld, 1)
				require.Equal(t, "racer", report.Withheld[0].Component)
				require.Equal(t, controllerName, report.Withheld[0].Ref.Name)
				require.ErrorContains(t, report.Err(), "racer.yaml")
				require.ErrorContains(t, report.Err(), "ConfigMap racer-config")
			} else {
				require.Equal(t, count, plan.Len())
				require.Empty(t, report.Withheld)

				for _, op := range plan.Operations {
					if op.Object.GetKind() == "Deployment" {
						containers, _, err := unstructured.NestedSlice(op.Object.Object, "spec", "template", "spec", "containers")
						require.NoError(t, err)
						memory, _, err := unstructured.NestedString(containers[0].(map[string]any), "resources", "limits", "memory")
						require.NoError(t, err)
						require.Equal(t, "512Mi", memory)
					}
				}
			}
		})
	}
}

func TestOperatorOwnershipDoesNotRenameAuthorityBinding(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	marker := &corev1.ConfigMap{}
	version := &corev1.ConfigMap{}
	credentials := &corev1.Secret{}

	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, markerName), marker))
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, versionName), version))
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, "racer-credentials"), credentials))

	for _, annotations := range []map[string]string{version.Annotations, credentials.Annotations} {
		require.Equal(t, string(marker.UID), annotations["racer.unbounded-cloud.io/installation-uid"])
		require.NotContains(t, annotations, runtimeInstallationAnnotation)
	}

	obj := component.ToUnstructured(&corev1.ConfigMap{})
	obj.SetUID("runtime-uid")
	obj.SetResourceVersion("1")
	obj.SetAnnotations(map[string]string{"racer.unbounded-cloud.io/manager": component.FieldOwner, "racer.unbounded-cloud.io/installation-uid": string(marker.UID)})
	require.ErrorContains(t, validateRuntimeOwner(obj, marker.UID), "refusing adoption")
	bindRuntime(obj, marker.UID)
	require.Equal(t, string(marker.UID), obj.GetAnnotations()["unbounded-cloud.io/racer-installation-uid"])
	require.Equal(t, component.FieldOwner, obj.GetAnnotations()["unbounded-cloud.io/racer-manager"])
	require.NoError(t, validateRuntimeOwner(obj, marker.UID))
}
