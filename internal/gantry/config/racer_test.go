// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"flag"
	"strconv"
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

func TestRacerTuning(t *testing.T) {
	for _, test := range []struct {
		name         string
		defaultValue string
		yamlValue    string
		envValue     string
		flagValue    string
		get          func(*Config) string
	}{
		{"racer_max_connections", "64", "12", "13", "14", func(c *Config) string { return strconv.Itoa(c.RacerMaxConnections) }},
		{"racer_origin_concurrent_requests", "64", "12", "13", "14", func(c *Config) string { return strconv.Itoa(c.RacerOriginConcurrentRequests) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := NewDefault()
			c.RacerEnabled = true

			c.UpstreamRegistries = []UpstreamRegistry{{Name: "registry.example", Endpoint: "https://registry.example"}}
			if got := test.get(c); got != test.defaultValue {
				t.Fatalf("default = %s, want %s", got, test.defaultValue)
			}

			if err := c.LoadYAML(strings.NewReader(test.name + ": " + test.yamlValue)); err != nil {
				t.Fatal(err)
			}

			if got := test.get(c); got != test.yamlValue {
				t.Fatalf("YAML = %s, want %s", got, test.yamlValue)
			}

			envName := "GANTRY_" + strings.ToUpper(test.name)

			env := func(value string) func(string) string {
				return func(key string) string {
					if key == envName {
						return value
					}

					return ""
				}
			}
			if err := c.LoadEnv(env(test.envValue)); err != nil {
				t.Fatal(err)
			}

			if got := test.get(c); got != test.envValue {
				t.Fatalf("env = %s, want %s", got, test.envValue)
			}

			flags := flag.NewFlagSet("test", flag.ContinueOnError)
			c.BindFlags(flags)

			if err := flags.Parse([]string{"--" + strings.ReplaceAll(test.name, "_", "-") + "=" + test.flagValue}); err != nil {
				t.Fatal(err)
			}

			if got := test.get(c); got != test.flagValue {
				t.Fatalf("flag = %s, want %s", got, test.flagValue)
			}

			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}

			if err := c.LoadEnv(env("invalid")); err == nil || !strings.Contains(err.Error(), envName) {
				t.Fatalf("parse error = %v", err)
			}

			negative := "-1"
			zero := "0"

			if err := c.LoadEnv(env(negative)); err != nil {
				t.Fatal(err)
			}

			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), test.name) {
				t.Fatalf("negative validation = %v", err)
			}

			if err := c.LoadEnv(env(zero)); err != nil {
				t.Fatal(err)
			}

			if got := test.get(c); got != zero {
				t.Fatalf("explicit zero = %s, want %s", got, zero)
			}

			if err := c.Validate(); err != nil {
				t.Fatalf("zero must select defaults: %v", err)
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

func TestRacerDisabledPreservesLegacyValidation(t *testing.T) {
	c := NewDefault()
	c.UpstreamRegistries = []UpstreamRegistry{{Name: "registry.example", Endpoint: "https://registry.example", CredentialsPath: "/legacy/credentials"}}
	c.RacerMaxConnections = -1

	c.RacerOriginConcurrentRequests = -1
	if err := c.Validate(); err != nil {
		t.Fatalf("disabled Racer tuning affected legacy mode: %v", err)
	}

	for _, test := range []struct {
		field  string
		change func(*Config)
	}{
		{"storage_mode", func(c *Config) { c.StorageMode = "invalid" }},
		{"containerd_socket", func(c *Config) { c.ContainerdSocket = "" }},
		{"transfer_listen", func(c *Config) { c.TransferListen = "" }},
		{"hrw_k", func(c *Config) { c.HRWK = 0 }},
		{"chair_count", func(c *Config) { c.ChairCount = 0 }},
		{"coord_peer_authz_enforce", func(c *Config) { c.CoordPeerAuthzEnforce = true }},
	} {
		t.Run(test.field, func(t *testing.T) {
			copy := *c
			test.change(&copy)

			if err := copy.Validate(); err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("legacy validation changed: %v", err)
			}

			copy.RacerEnabled = true
			copy.RacerMaxConnections = 0

			copy.RacerOriginConcurrentRequests = 0
			if err := copy.Validate(); err != nil {
				t.Fatalf("unused legacy field blocked Racer: %v", err)
			}
		})
	}
}
