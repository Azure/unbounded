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

// TestNodeDaemonSetRollsOneNodeAtATime guards the update strategy of the pod
// that serves the IPAM socket every pod sandbox on a node dials. Replacing the
// whole fleet at once removes that socket on every node for as long as each
// image pull takes, so no pod sandbox can be created anywhere until they
// finish. That is the v0.25.0 release deploy failure.
func TestNodeDaemonSetRollsOneNodeAtATime(t *testing.T) {
	dir := t.TempDir()
	if err := render.Render(".", dir, map[string]string{"NodeImage": "ghcr.io/azure/unbounded-net-node:test"}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "node", "03-daemonset.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	// Only the fields this test reads, so an unrelated manifest change does not
	// require updating the fixture.
	var ds struct {
		Spec struct {
			UpdateStrategy struct {
				Type          string `yaml:"type"`
				RollingUpdate struct {
					MaxUnavailable any `yaml:"maxUnavailable"`
					MaxSurge       any `yaml:"maxSurge"`
				} `yaml:"rollingUpdate"`
			} `yaml:"updateStrategy"`
		} `yaml:"spec"`
	}

	if err := yaml.Unmarshal(data, &ds); err != nil {
		t.Fatal(err)
	}

	strategy := ds.Spec.UpdateStrategy
	if strategy.Type != "RollingUpdate" {
		t.Fatalf("updateStrategy.type = %q, want RollingUpdate", strategy.Type)
	}

	if got := strategy.RollingUpdate.MaxUnavailable; got != 1 {
		t.Fatalf("updateStrategy.rollingUpdate.maxUnavailable = %v, want 1", got)
	}

	// A surge pod cannot bind the IPAM socket while the old pod on the same
	// node still serves it, so a surge would stall the rollout rather than
	// avoid the gap.
	if got := strategy.RollingUpdate.MaxSurge; got != nil && got != 0 {
		t.Fatalf("updateStrategy.rollingUpdate.maxSurge = %v, want 0 or unset", got)
	}
}
