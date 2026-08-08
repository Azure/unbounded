// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	gantryconfig "github.com/Azure/unbounded/internal/gantry/config"
)

func validateDirectGantryRegistry(raw []byte, registryName string) error {
	config := gantryconfig.NewDefault()
	if err := config.LoadYAML(bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("load Gantry config: %w", err)
	}

	matches := 0
	wantEndpoint := "https://" + registryName

	for _, registry := range config.UpstreamRegistries {
		if registry.Name != registryName {
			continue
		}

		matches++

		if strings.TrimSuffix(registry.Endpoint, "/") != wantEndpoint {
			return fmt.Errorf("gantry registry %q endpoint is %q, want %q", registryName, registry.Endpoint, wantEndpoint)
		}
	}

	if matches != 1 {
		return fmt.Errorf("gantry config has %d upstream_registries entries named %q, want exactly 1", matches, registryName)
	}

	return nil
}

func patchGantryRegistry(raw []byte, registryName, endpoint, namespaceAlias string) ([]byte, error) {
	if registryName == "" {
		return nil, fmt.Errorf("registry name is required")
	}

	if endpoint == "" {
		return nil, fmt.Errorf("registry endpoint is required")
	}

	if namespaceAlias == "" {
		return nil, fmt.Errorf("registry namespace alias is required")
	}

	var document map[string]any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("decode Gantry config: %w", err)
	}

	upstreamsValue, ok := document["upstream_registries"]
	if !ok {
		return nil, fmt.Errorf("gantry config has no upstream_registries")
	}

	upstreams, ok := upstreamsValue.([]any)
	if !ok {
		return nil, fmt.Errorf("gantry config upstream_registries has type %T, want sequence", upstreamsValue)
	}

	matches := 0

	for index, value := range upstreams {
		upstream, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("upstream_registries[%d] has type %T, want mapping", index, value)
		}

		nameValue, exists := upstream["name"]
		if !exists {
			return nil, fmt.Errorf("upstream_registries[%d] has no name", index)
		}

		name, ok := nameValue.(string)
		if !ok {
			return nil, fmt.Errorf("upstream_registries[%d].name has type %T, want string", index, nameValue)
		}

		if name != registryName {
			continue
		}

		matches++
		upstream["endpoint"] = endpoint
		upstream["ns_alias"] = namespaceAlias
	}

	if matches != 1 {
		return nil, fmt.Errorf("gantry config has %d upstream_registries entries named %q, want exactly 1", matches, registryName)
	}

	patched, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode gantry config: %w", err)
	}

	validated := gantryconfig.NewDefault()
	if err := validated.LoadYAML(bytes.NewReader(patched)); err != nil {
		return nil, fmt.Errorf("validate patched gantry config syntax: %w", err)
	}

	if err := validated.Validate(); err != nil {
		return nil, fmt.Errorf("validate patched gantry config: %w", err)
	}

	return patched, nil
}
