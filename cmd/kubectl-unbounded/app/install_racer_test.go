// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestInstallPreservesRacerGate(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    string
		wantErr bool
	}{
		{value: "FALSE", want: "false"},
		{value: "true", want: "true"},
		{value: "", wantErr: true},
		{value: "invalid", wantErr: true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cm := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "unbounded-operator-config", "namespace": "unbounded-system"},
				"data":     map[string]any{"ENABLE_RACER": tc.value},
			}}
			cli, captured := newCapturingInstallClient(cm)
			h := installHandler{namespace: "unbounded-system", kubeResourcesCli: cli, logger: discardLogger()}

			err := h.execute(t.Context())
			if tc.wantErr {
				require.ErrorContains(t, err, "ENABLE_RACER")
				require.Zero(t, captured.applyCount)

				return
			}

			require.NoError(t, err)
			data, _, err := unstructured.NestedStringMap(captured.configMap.Object, "data")
			require.NoError(t, err)
			require.Equal(t, tc.want, data["ENABLE_RACER"])

			hash, _, err := unstructured.NestedString(captured.deployment.Object, "spec", "template", "metadata", "annotations", operatorConfigHashAnnotation)
			require.NoError(t, err)
			require.Equal(t, operatorConfigHash(data), hash)
		})
	}
}
