// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package net_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
	"github.com/Azure/unbounded/internal/net/config"
)

func TestAutomaticNeighborConfigMap(t *testing.T) {
	for _, tc := range []struct {
		name, value string
	}{
		{name: "default"},
		{name: "obsolete manual input ignored", value: "2000"},
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

			if strings.Contains(cm.Data["config.yaml"], "expectedNodes") {
				t.Fatal("automatic sizing must not require or expose expectedNodes")
			}

			if cfg.Node.WireGuardPort == nil || *cfg.Node.WireGuardPort != 51820 {
				t.Fatal("automatic neighbor sizing changed unrelated runtime settings")
			}
		})
	}
}
