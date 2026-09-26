// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"errors"
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/intstr"

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
		"zero unavailable":           func(c *Config) { c.DataplaneMaxUnavailable = intstr.FromInt32(0) },
		"negative unavailable":       func(c *Config) { c.DataplaneMaxUnavailable = intstr.FromInt32(-1) },
		"invalid unavailable type":   func(c *Config) { c.DataplaneMaxUnavailable = intstr.IntOrString{Type: 2, IntVal: 1} },
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

func TestConfigDataplaneMaxUnavailable(t *testing.T) {
	// Preserve any ambient value while explicitly testing the unset default.
	t.Setenv("RACER_DATAPLANE_MAX_UNAVAILABLE", "1")

	if err := os.Unsetenv("RACER_DATAPLANE_MAX_UNAVAILABLE"); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig(t)
	if cfg.DataplaneMaxUnavailable != intstr.FromInt32(1) {
		t.Fatalf("default maxUnavailable = %v, want integer 1", cfg.DataplaneMaxUnavailable)
	}

	for _, tc := range []struct {
		value string
		want  intstr.IntOrString
	}{
		{"1", intstr.FromInt32(1)},
		{"5", intstr.FromInt32(5)},
		{"101", intstr.FromInt32(101)},
		{"2147483647", intstr.FromInt32(2147483647)},
		{"1%", intstr.FromString("1%")},
		{"25%", intstr.FromString("25%")},
		{"100%", intstr.FromString("100%")},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("RACER_DATAPLANE_MAX_UNAVAILABLE", tc.value)

			loaded, err := LoadConfig()
			if err != nil || loaded.DataplaneMaxUnavailable != tc.want {
				t.Fatalf("maxUnavailable = %v, want %v: %v", loaded.DataplaneMaxUnavailable, tc.want, err)
			}
		})
	}

	for _, value := range []string{"", "0", "-1", "0%", "-1%", "101%", "2147483648", "999999999999999999999%", "all", "1.5", "1.5%", " 1", "1 ", " 25%", "+25%", "25%%"} {
		t.Run("invalid/"+value, func(t *testing.T) {
			t.Setenv("RACER_DATAPLANE_MAX_UNAVAILABLE", value)

			if _, err := LoadConfig(); !errors.Is(err, wire.InvalidRequest) || !strings.Contains(err.Error(), "RACER_DATAPLANE_MAX_UNAVAILABLE") {
				t.Fatalf("invalid maxUnavailable %q: %v", value, err)
			}
		})
	}
}
