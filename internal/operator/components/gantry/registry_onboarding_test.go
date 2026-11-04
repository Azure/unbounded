// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/unbounded/internal/operator/component"
)

func TestRegistryAuthPolicyDoesNotModifyGantryConfiguration(t *testing.T) {
	for _, mode := range []string{"managed", "external", "registry-only"} {
		t.Run(mode, func(t *testing.T) {
			source := authSource("")

			switch mode {
			case "external":
				source.Data["node-auth.yaml"] = "mode: external\nregistry: example.azurecr.io\n"
			case "registry-only":
				source.Data["node-auth.yaml"] = "registry: example.azurecr.io\n"
			}

			source.Data["config.yaml"] = "# administrator edit\nupstream_registries:\n- name: ghcr.io\n  endpoint: https://ghcr.io\n"
			source.Data["other.yaml"] = "preserve"
			source.BinaryData = map[string][]byte{"keep": []byte("preserve")}
			env := testEnv(t, source)
			hash, op, err := planConfig(t.Context(), env)
			require.NoError(t, err)
			require.Nil(t, op)
			require.Equal(t, component.ConfigMapPayloadHash(source), hash)

			operations, result, err := planRegistryAuth(t.Context(), env)
			if mode == "managed" {
				require.ErrorContains(t, err, "requires its registry in Gantry upstream_registries")
			} else {
				require.NoError(t, err)
			}

			require.Empty(t, operations)
			require.Nil(t, result)

			var got corev1.ConfigMap
			require.NoError(t, env.Client.Get(t.Context(), componentKey(configName), &got))
			require.Equal(t, source.Data, got.Data)
			require.Equal(t, source.BinaryData, got.BinaryData)
		})
	}
}
