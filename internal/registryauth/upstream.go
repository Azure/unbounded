// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"bytes"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
	sigyaml "sigs.k8s.io/yaml"
)

// EnsureUpstream merges only the explicitly selected registry during attachment.
// It preserves unrelated runtime fields, registry entries, and YAML comments.
func EnsureUpstream(runtimeConfig, policy string) (string, bool, error) {
	if policy == "" {
		return runtimeConfig, false, nil
	}

	var cfg Config
	if err := sigyaml.UnmarshalStrict([]byte(policy), &cfg); err != nil {
		return "", false, fmt.Errorf("parse %s: %w", ConfigKey, err)
	}

	if cfg.Registry == "" {
		return runtimeConfig, false, nil
	}

	if !IsACR(cfg.Registry) {
		return "", false, errors.New("node-auth registry must be a public-cloud ACR login server")
	}

	var document yaml.Node
	if err := yaml.Unmarshal([]byte(runtimeConfig), &document); err != nil {
		return "", false, fmt.Errorf("parse Gantry runtime configuration: %w", err)
	}

	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return "", false, errors.New("gantry config.yaml must be an existing YAML mapping before registry onboarding")
	}

	root := document.Content[0]
	// Decoding validates duplicate keys rather than silently selecting one.
	var validated map[string]any
	if err := root.Decode(&validated); err != nil {
		return "", false, fmt.Errorf("validate Gantry runtime configuration: %w", err)
	}

	var upstreams *yaml.Node

	for index := 0; index < len(root.Content); index += 2 {
		if root.Content[index].Value == "upstream_registries" {
			upstreams = root.Content[index+1]
			break
		}
	}

	if upstreams == nil {
		if _, inherited := validated["upstream_registries"]; inherited {
			return "", false, errors.New("make inherited upstream_registries explicit before registry attachment")
		}

		upstreams = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "upstream_registries"}, upstreams)
	}

	if upstreams.Kind != yaml.SequenceNode {
		return "", false, errors.New("gantry upstream_registries must be a sequence")
	}

	entries := upstreams.Content
	fields := make([]map[string]string, len(entries))

	for index, entry := range entries {
		if entry.Kind != yaml.MappingNode && entry.Kind != yaml.AliasNode {
			return "", false, errors.New("gantry upstream registry must be a mapping")
		}

		if err := entry.Decode(&fields[index]); err != nil {
			return "", false, fmt.Errorf("decode Gantry upstream registry: %w", err)
		}
	}

	// Validate every entry before returning unchanged, so an ns_alias that shadows
	// the registry is rejected no matter where it appears in the sequence.
	for _, entry := range fields {
		if entry["name"] != cfg.Registry && entry["ns_alias"] == cfg.Registry {
			return "", false, fmt.Errorf("registry %q collides with upstream %q ns_alias; resolve the alias before attachment", cfg.Registry, entry["name"])
		}
	}

	for _, entry := range fields {
		if entry["name"] != cfg.Registry {
			if len(entries) == 1 && len(entry) == 2 && entry["name"] == "registry.example.com" && entry["endpoint"] == "https://registry.example.com" {
				upstreams.Content = nil
			}

			continue
		}

		if cfg.Mode == IdentityBindingMode && entry["credentials_path"] != "" {
			return "", false, errors.New("managed authentication conflicts with shared Gantry credentials_path; use external authentication")
		}

		if cfg.Mode == IdentityBindingMode && !matchingRegistryEndpoint(entry["endpoint"], cfg.Registry) {
			return "", false, errors.New("managed ACR authentication requires the registry's matching HTTPS endpoint")
		}

		return runtimeConfig, false, nil
	}

	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, value := range []string{"name", cfg.Registry, "endpoint", "https://" + cfg.Registry} {
		entry.Content = append(entry.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
	}

	upstreams.Content = append(upstreams.Content, entry)

	var output bytes.Buffer

	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)

	if err := encoder.Encode(&document); err != nil {
		return "", false, err
	}

	if err := encoder.Close(); err != nil {
		return "", false, err
	}

	return output.String(), true, nil
}
