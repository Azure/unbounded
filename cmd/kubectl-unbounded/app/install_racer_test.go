// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestInstallIgnoresObsoleteRacerGate(t *testing.T) {
	// Keep the original regression name; ENABLE_RACER is just an unknown key.
	for _, value := range []string{"FALSE", "true", "", "invalid"} {
		t.Run(value, func(t *testing.T) {
			for _, key := range []string{"ENABLE_RACER", "UNKNOWN_SETTING", "UNBOUNDED_UNKNOWN_SETTING"} {
				t.Run(key, func(t *testing.T) {
					cm := &unstructured.Unstructured{Object: map[string]any{
						"apiVersion": "v1", "kind": "ConfigMap",
						"metadata": map[string]any{"name": "unbounded-operator-config", "namespace": "unbounded-system"},
						"data": map[string]any{
							key:                               value,
							"UNBOUNDED_API_SERVER_ENDPOINT":   "https://api.example.test:6443",
							"UNBOUNDED_REAP_LEGACY_RESOURCES": "FALSE",
							"UNBOUNDED_IMAGE_REGISTRY":        "old.example.test/components",
						},
					}}
					cli, captured := newCapturingInstallClient(cm)
					h := installHandler{namespace: "unbounded-system", kubeResourcesCli: cli, logger: discardLogger(), imageRegistry: "registry.example.test/components"}

					require.NoError(t, h.execute(t.Context()))
					require.NotNil(t, captured.configMap)
					require.NotNil(t, captured.deployment)
					data, _, err := unstructured.NestedStringMap(captured.configMap.Object, "data")
					require.NoError(t, err)

					want := map[string]string{
						"UNBOUNDED_API_SERVER_ENDPOINT":   "https://api.example.test:6443",
						"UNBOUNDED_REAP_LEGACY_RESOURCES": "false",
						"UNBOUNDED_IMAGE_REGISTRY":        "registry.example.test/components",
					}
					require.Equal(t, want, data)
					require.NotContains(t, data, key)

					hash, _, err := unstructured.NestedString(captured.deployment.Object, "spec", "template", "metadata", "annotations", operatorConfigHashAnnotation)
					require.NoError(t, err)
					require.Equal(t, operatorConfigHash(want), hash)
				})
			}
		})
	}
}
