// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/net/config"
)

func TestControllerStatusDetailConfig(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, flagName, flagValue string
		ttl, timeout                    time.Duration
		invalid                         bool
	}{
		{name: "defaults", yaml: "controller: {}", ttl: 300 * time.Second, timeout: 120 * time.Second},
		{name: "configured", yaml: "controller:\n  statusDetailCacheTTL: 30s\n  statusDetailRequestTimeout: 10s", ttl: 30 * time.Second, timeout: 10 * time.Second},
		{name: "TTL zero", yaml: "controller:\n  statusDetailCacheTTL: 0s", invalid: true},
		{name: "TTL negative", yaml: "controller:\n  statusDetailCacheTTL: -1s", invalid: true},
		{name: "TTL malformed", yaml: "controller:\n  statusDetailCacheTTL: invalid", invalid: true},
		{name: "timeout zero", yaml: "controller:\n  statusDetailRequestTimeout: 0s", invalid: true},
		{name: "timeout negative", yaml: "controller:\n  statusDetailRequestTimeout: -1s", invalid: true},
		{name: "timeout malformed", yaml: "controller:\n  statusDetailRequestTimeout: invalid", invalid: true},
		{name: "TTL flag wins", yaml: "controller:\n  statusDetailCacheTTL: invalid", flagName: "status-detail-cache-ttl", flagValue: "15s", ttl: 15 * time.Second, timeout: 120 * time.Second},
		{name: "timeout flag wins", yaml: "controller:\n  statusDetailRequestTimeout: invalid", flagName: "status-detail-request-timeout", flagValue: "15s", ttl: 300 * time.Second, timeout: 15 * time.Second},
		{name: "zero flag", yaml: "controller: {}", flagName: "status-detail-cache-ttl", flagValue: "0s", invalid: true},
		{name: "negative flag", yaml: "controller: {}", flagName: "status-detail-request-timeout", flagValue: "-1s", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg := &config.Config{}

			cmd := newControllerConfigTestCommand(cfg)
			if tc.flagName != "" {
				if err := cmd.Flags().Set(tc.flagName, tc.flagValue); err != nil {
					t.Fatal(err)
				}
			}

			err := applyControllerRuntimeConfig(cmd, cfg, path)
			if err == nil {
				err = cfg.Validate()
			}

			if (err != nil) != tc.invalid {
				t.Fatalf("startup config validation = %v", err)
			}

			if !tc.invalid && (cfg.StatusDetailCacheTTL != tc.ttl || cfg.StatusDetailRequestTimeout != tc.timeout) {
				t.Errorf("lifetimes = %s/%s, want %s/%s", cfg.StatusDetailCacheTTL, cfg.StatusDetailRequestTimeout, tc.ttl, tc.timeout)
			}
		})
	}
}
