// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/unbounded/internal/operator/component"
)

func TestMigrateRetiredSettingsDropsPeerTCPNodelay(t *testing.T) {
	in := map[string]string{"RACER_PEER_TCP_NODELAY": "false", "RACER_SHARES": "4"}

	out, changed := migrateRetiredSettings(in)
	require.True(t, changed)
	require.Equal(t, map[string]string{"RACER_SHARES": "4"}, out)
	require.Contains(t, in, "RACER_PEER_TCP_NODELAY", "input is not modified")
	require.NoError(t, validateDataplaneConfig(out))

	out, changed = migrateRetiredSettings(map[string]string{"RACER_SHARES": "4"})
	require.False(t, changed)
	require.Equal(t, map[string]string{"RACER_SHARES": "4"}, out)

	out, changed = migrateRetiredSettings(nil)
	require.False(t, changed)
	require.Nil(t, out)
}

func TestRetiredSettingErrorNamesReplacement(t *testing.T) {
	err := validateDataplaneConfig(map[string]string{"RACER_PEER_TCP_NODELAY": "true"})
	require.ErrorContains(t, err, "RACER_PEER_TCP_NODELAY")
	require.ErrorContains(t, err, "TCP_NODELAY is always set")
}

func TestExistingConfigWithRetiredPeerTCPNodelayIsMigrated(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	config := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))

	config.Data["RACER_PEER_TCP_NODELAY"] = "false"
	require.NoError(t, env.Client.Update(t.Context(), config))

	plan := planPass(t, env)

	config.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMap"))
	require.Contains(t, dataplaneOp(t, plan).DependsOn, component.RefOf(component.ToUnstructured(config)), "the rollout waits for the ConfigMap patch")

	persist(t, env, plan)

	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))
	require.NotContains(t, config.Data, "RACER_PEER_TCP_NODELAY")
	require.Equal(t, component.ConfigMapPayloadHash(config), liveDataplane(t, env).Spec.Template.Annotations[dataplaneConfigHash])

	// A second pass is a no-op for the ConfigMap.
	for _, op := range planPass(t, env).Operations {
		require.False(t, op.Object.GetKind() == "ConfigMap" && op.Object.GetName() == dataplaneConfigName, "second pass touches %s with %v", dataplaneConfigName, op.Kind)
	}
}
