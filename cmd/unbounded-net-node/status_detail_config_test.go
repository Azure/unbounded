// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNodeStatusDetailConfig(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, flag, want string
		invalid                bool
	}{
		{name: "default", yaml: "node: {}", want: "summary"},
		{name: "summary", yaml: "node:\n  statusDetailMode: summary", want: "summary"},
		{name: "full", yaml: "node:\n  statusDetailMode: full", want: "full"},
		{name: "invalid YAML value", yaml: "node:\n  statusDetailMode: invalid", invalid: true},
		{name: "flag wins", yaml: "node:\n  statusDetailMode: summary", flag: "full", want: "full"},
		{name: "flag overrides invalid YAML", yaml: "node:\n  statusDetailMode: invalid", flag: "summary", want: "summary"},
		{name: "invalid flag", yaml: "node: {}", flag: "invalid", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg := &config{
				ConfigFile: path, GeneveInterfaceName: "geneve0", VXLANInterfaceName: "vxlan0",
				IPIPInterfaceName: "ipip0", WireGuardInterfacePrefix: "wg",
			}

			cmd := newNodeConfigTestCommand(cfg)
			if tc.flag != "" {
				if err := cmd.Flags().Set("status-detail-mode", tc.flag); err != nil {
					t.Fatal(err)
				}
			}

			err := applyNodeRuntimeConfig(cmd, cfg)
			if (err != nil) != tc.invalid {
				t.Fatalf("applyNodeRuntimeConfig() = %v", err)
			}

			if !tc.invalid && cfg.StatusDetailMode != tc.want {
				t.Errorf("mode = %q, want %q", cfg.StatusDetailMode, tc.want)
			}
		})
	}
}
