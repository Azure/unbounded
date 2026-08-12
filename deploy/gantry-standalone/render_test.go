// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gantrystandalone

import (
	"bytes"
	"io"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	gantryconfig "github.com/Azure/unbounded/internal/gantry/config"
)

func TestRenderProducesIsolatedInertInstallation(t *testing.T) {
	manifests, err := Render(Values{Namespace: "gantry-test", Image: "ghcr.io/azure/gantry:test"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	if len(manifests) != 4 {
		t.Fatalf("manifest count = %d, want 4", len(manifests))
	}

	var (
		configYAML         string
		standaloneOptIn    bool
		criticalAgent      bool
		criticalNodeConfig bool
	)

	for _, manifest := range manifests {
		if manifest.Name == "02-agent.yaml.tmpl" && bytes.Contains(manifest.Data, []byte("--allow-no-upstream-registries=true")) {
			standaloneOptIn = true
		}

		if manifest.Name == "02-agent.yaml.tmpl" && bytes.Contains(manifest.Data, []byte("priorityClassName: system-node-critical")) {
			criticalAgent = true
		}

		if manifest.Name == "03-node-config.yaml.tmpl" && bytes.Contains(manifest.Data, []byte("priorityClassName: system-node-critical")) {
			criticalNodeConfig = true
		}

		decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest.Data), 4096)

		for {
			var object map[string]any
			if err := decoder.Decode(&object); err != nil {
				if err == io.EOF {
					break
				}

				t.Fatalf("decode %s: %v", manifest.Name, err)
			}

			metadata, _ := object["metadata"].(map[string]any)

			name, _ := metadata["name"].(string)
			if object["kind"] == "DaemonSet" && name == "gantry" {
				t.Fatal("standalone manifests reused the operator DaemonSet name gantry")
			}

			if object["kind"] == "ConfigMap" && name == "gantry-standalone-config" {
				data, _ := object["data"].(map[string]any)
				configYAML, _ = data["config.yaml"].(string)
			}
		}
	}

	if configYAML == "" {
		t.Fatal("rendered agent ConfigMap has no config.yaml")
	}

	if !standaloneOptIn {
		t.Fatal("standalone agent does not opt into empty-registry mode")
	}

	if !criticalAgent || !criticalNodeConfig {
		t.Fatalf("node replacement priorities = agent:%v node-config:%v, want both critical", criticalAgent, criticalNodeConfig)
	}

	config := gantryconfig.NewDefault()
	if err := config.LoadYAML(bytes.NewBufferString(configYAML)); err != nil {
		t.Fatalf("load rendered Gantry config: %v", err)
	}

	if config.AllowNoUpstreamRegistries || len(config.UpstreamRegistries) != 0 {
		t.Fatalf("rendered YAML registry state = allowEmpty:%v registries:%v", config.AllowNoUpstreamRegistries, config.UpstreamRegistries)
	}

	config.AllowNoUpstreamRegistries = true
	if err := config.Validate(); err != nil {
		t.Fatalf("validate rendered inert config: %v", err)
	}
}
