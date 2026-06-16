// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package registry holds the static, in-repo module registry the dashboard
// uses to discover component modules in the prototype. Each entry names a
// module and the base URL of its dashboard HTTP surface.
//
// Runtime discovery (a DashboardModule CRD or annotated Service) is a future
// evolution of this same shape and is intentionally out of scope here.
package registry

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

// Module is a single static registry entry.
type Module struct {
	// ID is the stable identifier used in dashboard URLs (e.g. "net").
	ID string `json:"id"`
	// BaseURL is the root of the module's dashboard surface, e.g.
	// "http://unbounded-net-controller.unbounded-net:9999/dashboard/v1".
	BaseURL string `json:"baseURL"`
}

// Config is the on-disk registry document.
type Config struct {
	Modules []Module `json:"modules"`
}

// Load reads and validates a registry config from the given path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading module registry %q: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing module registry %q: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid module registry %q: %w", path, err)
	}

	return &cfg, nil
}

// Validate checks the config for obvious mistakes: empty/duplicate IDs and
// missing base URLs.
func (c *Config) Validate() error {
	seen := make(map[string]struct{}, len(c.Modules))

	for i, m := range c.Modules {
		if strings.TrimSpace(m.ID) == "" {
			return fmt.Errorf("module[%d]: id is required", i)
		}

		if strings.TrimSpace(m.BaseURL) == "" {
			return fmt.Errorf("module %q: baseURL is required", m.ID)
		}

		if _, dup := seen[m.ID]; dup {
			return fmt.Errorf("module %q: duplicate id", m.ID)
		}

		seen[m.ID] = struct{}{}
	}

	return nil
}
