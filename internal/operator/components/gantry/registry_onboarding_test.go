// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/unbounded/internal/operator/component"
)

func TestRegistryOnboardingPatchesOnlyRequestedRuntimeUpstream(t *testing.T) {
	source := authSource("registry: example.azurecr.io")
	source.Data["config.yaml"] = "# preserve this comment\nmirror_listen: 0.0.0.0:5000\nupstream_registries:\n- name: ghcr.io\n  endpoint: https://ghcr.io\n"
	source.Data["other.yaml"] = "preserve"
	source.BinaryData = map[string][]byte{"keep": []byte("preserve")}
	env := testEnv(t, source)
	_, op, err := planConfig(t.Context(), env)
	require.NoError(t, err)
	require.NotNil(t, op)
	require.Equal(t, component.OpMergePatch, op.Kind)
	require.NotNil(t, op.Base)
	hash, err := ensureConfig(t, env)
	require.NoError(t, err)

	var got corev1.ConfigMap
	require.NoError(t, env.Client.Get(t.Context(), componentKey(configName), &got))
	require.Contains(t, got.Data["config.yaml"], "# preserve this comment")
	require.Contains(t, got.Data["config.yaml"], "ghcr.io")
	require.Contains(t, got.Data["config.yaml"], "example.azurecr.io")
	require.Equal(t, "preserve", got.Data["other.yaml"])
	require.Equal(t, source.BinaryData, got.BinaryData)
	require.Equal(t, source.Data["node-auth.yaml"], got.Data["node-auth.yaml"])
	require.Equal(t, component.ConfigMapPayloadHash(&got), hash)
	_, op, err = planConfig(t.Context(), env)
	require.NoError(t, err)
	require.Nil(t, op)
}

func TestRegistryOnboardingDoesNotOverwriteConcurrentAdministratorEdit(t *testing.T) {
	source := authSource("registry: example.azurecr.io")
	source.Data["config.yaml"] = "upstream_registries: []\n"
	env := testEnv(t, source)
	_, op, err := planConfig(t.Context(), env)
	require.NoError(t, err)

	var current corev1.ConfigMap
	require.NoError(t, env.Client.Get(t.Context(), componentKey(configName), &current))
	current.Data["config.yaml"] = "# concurrent administrator edit\nupstream_registries: []\n"
	require.NoError(t, env.Client.Update(t.Context(), &current))

	plan := component.NewPlan()
	plan.Add(*op)
	result, err := env.Execute(t.Context(), plan)
	require.NoError(t, err)
	require.NotEmpty(t, result.Deferred)

	var got corev1.ConfigMap
	require.NoError(t, env.Client.Get(t.Context(), componentKey(configName), &got))
	require.Equal(t, current.Data, got.Data)
}
