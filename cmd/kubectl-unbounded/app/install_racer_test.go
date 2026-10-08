// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestInstallIgnoresObsoleteRacerGate(t *testing.T) {
	for _, value := range []string{"FALSE", "true", "", "invalid"} {
		t.Run(value, func(t *testing.T) {
			cm := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "unbounded-operator-config", "namespace": "unbounded-system"},
				"data":     map[string]any{"ENABLE_RACER": value},
			}}
			cli, captured := newCapturingInstallClient(cm)
			h := installHandler{namespace: "unbounded-system", kubeResourcesCli: cli, logger: discardLogger()}

			require.NoError(t, h.execute(t.Context()))
			require.NotNil(t, captured.configMap)
			require.NotNil(t, captured.deployment)
			data, _, err := unstructured.NestedStringMap(captured.configMap.Object, "data")
			require.NoError(t, err)
			require.NotContains(t, data, "ENABLE_RACER")

			hash, _, err := unstructured.NestedString(captured.deployment.Object, "spec", "template", "metadata", "annotations", operatorConfigHashAnnotation)
			require.NoError(t, err)
			require.Equal(t, operatorConfigHash(data), hash)
		})
	}
}
