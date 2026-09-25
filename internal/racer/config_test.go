// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"errors"
	"testing"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestConfigDeploymentIdentityAndBounds(t *testing.T) {
	cfg := testConfig(t)
	for name, mutate := range map[string]func(*Config){
		"cluster":                    func(c *Config) { c.Cluster = "" },
		"namespace":                  func(c *Config) { c.Namespace = "../namespace" },
		"missing marker name":        func(c *Config) { c.InstallationConfigMapName = "" },
		"aliased durable objects":    func(c *Config) { c.InstallationConfigMapName = c.VersionConfigMapName },
		"aliased credential secrets": func(c *Config) { c.IssuerSecretName = c.KeyringSecretName },
		"no preparation":             func(c *Config) { c.Rotation.PrepareFor = 0 },
		"short overlap":              func(c *Config) { c.Rotation.RetainFor = wire.CertificateLifetime - 1 },
		"short interval":             func(c *Config) { c.Rotation.Interval = c.Rotation.PrepareFor - 1 },
		"zero port":                  func(c *Config) { c.PeerPort = 0 },
		"unbounded polls":            func(c *Config) { c.Limits.MaxPolls = 0 },
		"unbounded writes":           func(c *Config) { c.Limits.MaxConcurrentWrites = 0 },
		"unbounded bootstrap":        func(c *Config) { c.Limits.MaxConcurrentBootstrap = 0 },
		"unbounded headers":          func(c *Config) { c.Limits.HeaderBytes = 0 },
		"unbounded write duration":   func(c *Config) { c.Limits.WriteTimeout = 0 },
		"unbounded shutdown":         func(c *Config) { c.Limits.ShutdownTimeout = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := cfg
			mutate(&invalid)

			if err := invalid.Validate(); !errors.Is(err, wire.InvalidRequest) {
				t.Fatalf("invalid config accepted: %v", err)
			}
		})
	}

	for _, port := range []string{"0", "65536", "-1", "invalid"} {
		t.Setenv("RACER_PEER_PORT", port)

		if _, err := LoadConfig(); !errors.Is(err, wire.InvalidRequest) {
			t.Fatalf("port %q: %v", port, err)
		}
	}

	t.Setenv("RACER_PEER_PORT", "65535")
	t.Setenv("RACER_INSTALLATION_CONFIGMAP_NAME", "permanent-installation")

	loaded, err := LoadConfig()
	if err != nil || loaded.PeerPort != 65535 || loaded.InstallationConfigMapName != "permanent-installation" {
		t.Fatalf("deployment overrides: %+v, %v", loaded, err)
	}
}
