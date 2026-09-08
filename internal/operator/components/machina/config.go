// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machina

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// SetAPIServerEndpoint updates only the top-level apiServerEndpoint field. Using
// a YAML node preserves all other migrated or user-managed fields.
func SetAPIServerEndpoint(config, endpoint string) (string, error) {
	if endpoint == "" {
		return config, nil
	}

	var document yaml.Node
	if config == "" {
		document = yaml.Node{
			Kind: yaml.DocumentNode,
			Content: []*yaml.Node{{
				Kind: yaml.MappingNode,
				Tag:  "!!map",
			}},
		}
	} else if err := yaml.Unmarshal([]byte(config), &document); err != nil {
		return "", fmt.Errorf("parse machina config: %w", err)
	}

	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("parse machina config: expected a top-level mapping")
	}

	mapping := document.Content[0]
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != "apiServerEndpoint" {
			continue
		}

		mapping.Content[i+1].Kind = yaml.ScalarNode
		mapping.Content[i+1].Tag = "!!str"
		mapping.Content[i+1].Value = endpoint
		mapping.Content[i+1].Style = yaml.DoubleQuotedStyle

		encoded, err := yaml.Marshal(&document)
		if err != nil {
			return "", fmt.Errorf("encode machina config: %w", err)
		}

		return string(encoded), nil
	}

	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "apiServerEndpoint"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: endpoint, Style: yaml.DoubleQuotedStyle},
	)

	encoded, err := yaml.Marshal(&document)
	if err != nil {
		return "", fmt.Errorf("encode machina config: %w", err)
	}

	return string(encoded), nil
}
