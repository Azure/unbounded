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
	// The old keys were already per-worker, so the values carry over rather
	// than falling back to the new, larger defaults.
	require.Equal(t, "1073741824", out["RACER_SLAB_BYTES_PER_WORKER"])
	require.Equal(t, "2", out["RACER_FREE_SEGMENT_RESERVE_PER_WORKER"])
	require.Equal(t, "4", out["RACER_SHARES"])
	require.Contains(t, in, "RACER_SLAB_BYTES", "input is not modified")
	require.NoError(t, validateDataplaneConfig(out))

	custom := map[string]string{"RACER_SLAB_BYTES": "2147483648", "RACER_SHARES": "4"}
	out, changed = migrateRetiredSettings(custom)
	require.True(t, changed)
	require.Equal(t, map[string]string{"RACER_SLAB_BYTES_PER_WORKER": "2147483648", "RACER_SHARES": "4"}, out)

	out, changed = migrateRetiredSettings(nil)
	require.False(t, changed)
	require.Nil(t, out)
}

func TestMigrateRetiredSettingsRenames(t *testing.T) {
	for key, retired := range retiredSettings {
		if !retired.rename {
			continue
		}

		_, known := dataplaneSettings[retired.replacement]
		require.True(t, known, "%s replacement %s is a dataplane setting", key, retired.replacement)

		out, changed := migrateRetiredSettings(map[string]string{key: "/x"})
		require.True(t, changed)
		require.Equal(t, map[string]string{retired.replacement: "/x"}, out, key)
	}
}

func TestMigrateRetiredSettingsReplacementAlreadySet(t *testing.T) {
	same := map[string]string{"RACER_SLAB_BYTES": "1073741824", "RACER_SLAB_BYTES_PER_WORKER": "1073741824"}
	out, changed := migrateRetiredSettings(same)
	require.True(t, changed)
	require.Equal(t, map[string]string{"RACER_SLAB_BYTES_PER_WORKER": "1073741824"}, out)

	conflict := map[string]string{"RACER_SLAB_BYTES": "1073741824", "RACER_SLAB_BYTES_PER_WORKER": "2147483648"}
	out, changed = migrateRetiredSettings(conflict)
	require.False(t, changed)
	require.Equal(t, conflict, out)
	require.ErrorContains(t, validateDataplaneConfig(out), "RACER_SLAB_BYTES is retired and conflicts with RACER_SLAB_BYTES_PER_WORKER")
}

func TestMigrateRetiredSettingsDropsWarmKeys(t *testing.T) {
	data := oldTemplateData()
	for key := range warmKeys() {
		data[key] = "30"
	}

	out, changed := migrateRetiredSettings(data)
	require.True(t, changed)

	for key := range warmKeys() {
		require.NotContains(t, out, key)
	}

	require.NoError(t, validateDataplaneConfig(out))
}

func warmKeys() map[string]string {
	return map[string]string{
		"RACER_WARM_MAX_TTL_SECS":      "3600",
		"RACER_WARM_FILL_TIMEOUT_SECS": "300",
		"RACER_WARM_MAX_JOBS":          "4",
		"RACER_WARM_WINDOW_PAGES":      "16",
	}
}

func TestPeerPortIsReportedNotMigrated(t *testing.T) {
	data := map[string]string{"RACER_PEER_PORT": "7000"}
	out, changed := migrateRetiredSettings(data)
	require.False(t, changed)
	require.ErrorContains(t, validateDataplaneConfig(out), "RACER_PEER_PORT is retired; set the port of RACER_PEER_LISTEN")
}

func TestRetiredSettingErrorNamesReplacement(t *testing.T) {
	for key, retired := range retiredSettings {
		err := validateDataplaneConfig(map[string]string{key: "12345"})
		require.ErrorContains(t, err, key)
		require.ErrorContains(t, err, retired.replacement)
	}
}

func TestMigrationPatchFoldsIntoRestamp(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	config := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))

	config.Data = oldTemplateData()
	require.NoError(t, env.Client.Update(t.Context(), config))
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))

	// A restamp patch planned earlier in the pass from the same version.
	config.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
	restamped := config.DeepCopy()
	restamped.Annotations = map[string]string{"example.test/restamped": "true"}

	plan := component.NewPlan()
	plan.Add(component.Operation{Kind: component.OpMergePatch, Component: name, Base: component.ToUnstructured(config), Object: component.ToUnstructured(restamped)})

	_, err := planDataplaneConfig(t.Context(), env, plan)
	require.NoError(t, err)
	require.Len(t, plan.Operations, 1, "migration folds into the existing patch")

	persist(t, env, plan)
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))
	require.Equal(t, "1073741824", config.Data["RACER_SLAB_BYTES_PER_WORKER"])
	require.NotContains(t, config.Data, "RACER_SLAB_BYTES")
	require.Equal(t, "true", config.Annotations["example.test/restamped"])
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
	require.Equal(t, "1073741824", config.Data["RACER_SLAB_BYTES_PER_WORKER"])
	require.Equal(t, "2", config.Data["RACER_FREE_SEGMENT_RESERVE_PER_WORKER"])
	require.Equal(t, "4", config.Data["RACER_SHARES"], "other settings survive")
	require.Equal(t, component.ConfigMapPayloadHash(config), liveDataplane(t, env).Spec.Template.Annotations[dataplaneConfigHash])

	// A second pass is a no-op for the ConfigMap.
	second := planPass(t, env)
	for _, op := range second.Operations {
		require.False(t, op.Object.GetKind() == "ConfigMap" && op.Object.GetName() == dataplaneConfigName, "second pass touches %s with %v", dataplaneConfigName, op.Kind)
	}

	persist(t, env, second)
}

func TestDataplaneWaitsForConfigMigration(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	config := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))

	config.Data = oldTemplateData()
	require.NoError(t, env.Client.Update(t.Context(), config))

	op := dataplaneOp(t, planPass(t, env))
	config.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMap"))
	require.Contains(t, op.DependsOn, component.RefOf(component.ToUnstructured(config)))
}

func TestExistingConfigWithWarmKeysIsMigrated(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	config := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))

	config.Data = oldTemplateData()
	for key, value := range warmKeys() {
		config.Data[key] = value
	}

	require.NoError(t, env.Client.Update(t.Context(), config))

	persist(t, env, planPass(t, env))

	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))

	for key := range warmKeys() {
		require.NotContains(t, config.Data, key)
	}
}

func TestExistingConfigWithConflictingRetiredValueFails(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	initialize(t, env)

	config := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneConfigName), config))

	config.Data = oldTemplateData()
	config.Data["RACER_SLAB_BYTES_PER_WORKER"] = "2147483648"
	config.Data["RACER_PEER_PORT"] = "7000"
	require.NoError(t, env.Client.Update(t.Context(), config))

	_, _, err := (Component{}).Plan(t.Context(), env, nil)
	require.ErrorContains(t, err, "RACER_SLAB_BYTES is retired and conflicts with RACER_SLAB_BYTES_PER_WORKER")
	require.ErrorContains(t, err, "RACER_PEER_PORT is retired")
	require.NotContains(t, err.Error(), "RACER_FREE_SEGMENT_RESERVE ", "migrated retired keys are not reported")
}

func TestRenamedConfigWithOldDefaultsIsMigrated(t *testing.T) {
	data := map[string]string{}
	for key, value := range oldTemplateData() {
		data[renamedKeyPrefix+key[len("RACER_"):]] = value
	}

	for key, value := range warmKeys() {
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
	require.Equal(t, "1073741824", config.Data["RACER_SLAB_BYTES_PER_WORKER"])

	for key := range warmKeys() {
		require.NotContains(t, config.Data, key)
	}
}
