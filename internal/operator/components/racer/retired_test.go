// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Azure/unbounded/internal/operator/component"
)

// oldTemplateData is the racer-settings payload the earlier template seeded.
func oldTemplateData() map[string]string {
	return map[string]string{
		"RACER_CONTROL_SEATS":            "auto",
		"RACER_ENABLE_RDMA":              "auto",
		"RACER_SHARES":                   "4",
		"RACER_METADATA_ENTRIES":         "4096",
		"RACER_DISK_PAGE_ENTRIES":        "65536",
		"RACER_CHECKPOINT_BYTES":         "67108864",
		"RACER_RETAINED_SEGMENT_PERCENT": "50",
		"RACER_SLAB_BYTES":               "1073741824",
		"RACER_SEGMENT_BYTES":            "67108864",
		"RACER_FREE_SEGMENT_RESERVE":     "2",
	}
}

func TestMigrateRetiredSettings(t *testing.T) {
	in := oldTemplateData()
	out, changed := migrateRetiredSettings(in)
	require.True(t, changed)
	require.NotContains(t, out, "RACER_SLAB_BYTES")
	require.NotContains(t, out, "RACER_FREE_SEGMENT_RESERVE")
	require.Equal(t, "4", out["RACER_SHARES"])
	require.Contains(t, in, "RACER_SLAB_BYTES", "input is not modified")
	require.NoError(t, validateDataplaneConfig(out))

	custom := map[string]string{"RACER_SLAB_BYTES": "2147483648", "RACER_SHARES": "4"}
	out, changed = migrateRetiredSettings(custom)
	require.False(t, changed)
	require.Equal(t, custom, out)

	out, changed = migrateRetiredSettings(nil)
	require.False(t, changed)
	require.Nil(t, out)
}

func TestRetiredSettingErrorNamesReplacement(t *testing.T) {
	for key, retired := range retiredSettings {
		err := validateDataplaneConfig(map[string]string{key: "12345"})
		require.ErrorContains(t, err, key)
		require.ErrorContains(t, err, retired.replacement)
	}
}

func TestExistingConfigWithOldDefaultsIsMigrated(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	config := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))

	config.Data = oldTemplateData()
	require.NoError(t, env.Client.Update(t.Context(), config))

	persist(t, env, planPass(t, env))

	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))
	require.NotContains(t, config.Data, "RACER_SLAB_BYTES")
	require.NotContains(t, config.Data, "RACER_FREE_SEGMENT_RESERVE")
	require.Equal(t, "4", config.Data["RACER_SHARES"], "other settings survive")
	require.Equal(t, component.ConfigMapPayloadHash(config), liveDataplane(t, env).Spec.Template.Annotations[dataplaneConfigHash])

	// A second pass is a no-op for the ConfigMap.
	persist(t, env, planPass(t, env))
}

func TestExistingConfigWithCustomRetiredValueFails(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	config := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))

	config.Data = oldTemplateData()
	config.Data["RACER_SLAB_BYTES"] = "2147483648"
	require.NoError(t, env.Client.Update(t.Context(), config))

	_, _, err := (Component{}).Plan(t.Context(), env, nil)
	require.ErrorContains(t, err, "RACER_SLAB_BYTES is retired; set RACER_SLAB_BYTES_PER_WORKER")
	require.NotContains(t, err.Error(), "RACER_FREE_SEGMENT_RESERVE ", "default retired keys are migrated, not reported")
}

func TestRenamedConfigWithOldDefaultsIsMigrated(t *testing.T) {
	data := map[string]string{}
	for key, value := range oldTemplateData() {
		data[renamedKeyPrefix+key[len("RACER_"):]] = value
	}

	data["RACER_DATAPLANE_SHARES"] = "7"

	ns := "custom-system"
	renamed := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: renamedConfig, Namespace: ns, UID: types.UID(uuid.NewString()), Annotations: managed()}, Data: data}
	env := testEnv(t, renamed, cacheObject("cache"))
	initialize(t, env)

	config := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))
	require.Equal(t, "7", config.Data["RACER_SHARES"], "settings carry over")
	require.NotContains(t, config.Data, "RACER_SLAB_BYTES")
	require.NotContains(t, config.Data, "RACER_FREE_SEGMENT_RESERVE")
}
