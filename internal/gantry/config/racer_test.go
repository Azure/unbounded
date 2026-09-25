// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"flag"
	"strings"
	"testing"
)

func TestRacerEnabledEnvironment(t *testing.T) {
	for _, test := range []struct {
		value   string
		want    bool
		invalid bool
	}{
		{value: ""}, {value: "true", want: true}, {value: "false"}, {value: "invalid", invalid: true},
	} {
		t.Run("value="+test.value, func(t *testing.T) {
			c := NewDefault()
			if c.RacerEnabled {
				t.Fatal("Racer must default to disabled")
			}

			err := c.LoadEnv(func(key string) string {
				if key == "GANTRY_RACER_ENABLED" {
					return test.value
				}

				return ""
			})
			if test.invalid {
				if err == nil || !strings.Contains(err.Error(), "GANTRY_RACER_ENABLED") {
					t.Fatalf("expected named parse error, got %v", err)
				}

				return
			}

			if err != nil || c.RacerEnabled != test.want {
				t.Fatalf("enabled=%v err=%v, want %v", c.RacerEnabled, err, test.want)
			}
		})
	}
}

func TestRacerEnabledEnvOnly(t *testing.T) {
	c := NewDefault()
	if err := c.LoadYAML(strings.NewReader("racer_enabled: true\n")); err == nil {
		t.Fatal("YAML must reject the environment-only switch")
	}

	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	c.BindFlags(flags)

	if flags.Lookup("racer-enabled") != nil {
		t.Fatal("Racer must not expose a flag")
	}
}

func TestRacerValidation(t *testing.T) {
	// A common-only config proves none of the legacy required fields are needed.
	minimal := Config{
		RacerEnabled: true, MirrorListen: "127.0.0.1:5000", MetricsListen: "127.0.0.1:9095",
		LogLevel: "info", LogFormat: "json",
		UpstreamRegistries: []UpstreamRegistry{{Name: "registry.example.com", Endpoint: "https://registry.example.com"}},
	}
	if err := minimal.Validate(); err != nil {
		t.Fatal(err)
	}

	legacy := minimal

	legacy.RacerEnabled = false
	if err := legacy.Validate(); err == nil {
		t.Fatal("legacy requirements must still be enforced")
	}

	for _, test := range []struct {
		field  string
		mutate func(*Config)
	}{
		{"mirror_listen", func(c *Config) { c.MirrorListen = "0.0.0.0:5000" }},
		{"metrics_listen", func(c *Config) { c.MetricsListen = "" }},
		{"pprof_listen", func(c *Config) { c.PprofListen = "0.0.0.0:6060" }},
		{"upstream_registries", func(c *Config) { c.UpstreamRegistries = nil }},
		{"log_level", func(c *Config) { c.LogLevel = "bad" }},
		{"log_format", func(c *Config) { c.LogFormat = "bad" }},
	} {
		t.Run(test.field, func(t *testing.T) {
			c := minimal
			test.mutate(&c)

			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("expected %s validation error, got %v", test.field, err)
			}
		})
	}
}
