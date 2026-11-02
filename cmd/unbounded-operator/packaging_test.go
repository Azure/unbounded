// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/operator/component"
)

func TestOperatorMakeTargetBuildsWholePackage(t *testing.T) {
	data, err := os.ReadFile("../../Makefile")
	require.NoError(t, err)

	_, target, ok := strings.Cut(string(data), "\nunbounded-operator-build:")
	require.True(t, ok)

	recipe, _, _ := strings.Cut(target, "\n\n")
	require.Contains(t, recipe, "$(UNBOUNDED_OPERATOR_CMD)")
	require.NotContains(t, recipe, "$(UNBOUNDED_OPERATOR_CMD)/main.go", "image builds must include the provider and node-helper command files")
}

func TestRegistryAuthImageConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, env, flag, want string }{
		{name: "default"},
		{name: "environment", env: "example.azurecr.io/operator:dev", want: "example.azurecr.io/operator:dev"},
		{name: "flag overrides environment", env: "example.azurecr.io/operator:old", flag: "example.azurecr.io/operator:new", want: "example.azurecr.io/operator:new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("UNBOUNDED_REGISTRY_AUTH_IMAGE", tc.env)

			called := false
			cmd := newCommand(func(_ context.Context, cfg config) error {
				called = true

				require.Equal(t, tc.want, cfg.registryAuthImage)

				return nil
			})

			args := []string{}
			if tc.flag != "" {
				args = append(args, "--registry-auth-image="+tc.flag)
			}

			cmd.SetArgs(args)
			require.NoError(t, cmd.Execute())
			require.True(t, called)
		})
	}

	cfg := component.Config{ImageRegistry: "ghcr.io/azure", ImageTag: "v0.8.0"}
	require.Equal(t, "ghcr.io/azure/unbounded-operator:v0.8.0", cfg.RegistryAuthNodeImage())
	cfg.RegistryAuthImage = "example.azurecr.io/operator:dev"
	require.Equal(t, cfg.RegistryAuthImage, cfg.RegistryAuthNodeImage())
	require.Equal(t, "ghcr.io/azure/gantry:v0.8.0", cfg.Image("gantry"), "testing a helper image must not reimage unrelated components")
}
