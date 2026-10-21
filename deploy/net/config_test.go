// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package net_test

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
	"github.com/Azure/unbounded/internal/net/config"
)

func TestExpectedNodesConfigMap(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        int
	}{
		{name: "default"},
		{name: "configured", value: "2000", want: 2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := render.Render(".", dir, map[string]string{"NodeExpectedNodes": tc.value}); err != nil {
				t.Fatal(err)
			}

			data, err := os.ReadFile(filepath.Join(dir, "01-configmap.yaml"))
			if err != nil {
				t.Fatal(err)
			}

			var cm struct {
				Data map[string]string `yaml:"data"`
			}
			if err := yaml.Unmarshal(data, &cm); err != nil {
				t.Fatal(err)
			}

			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(cm.Data["config.yaml"]), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg, err := config.LoadRuntimeConfig(path)
			if err != nil {
				t.Fatal(err)
			}

			if cfg.Node.ExpectedNodes == nil || *cfg.Node.ExpectedNodes != tc.want {
				t.Fatalf("expectedNodes = %v, want %d", cfg.Node.ExpectedNodes, tc.want)
			}
		})
	}
}
