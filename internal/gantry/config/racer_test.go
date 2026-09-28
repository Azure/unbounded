// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"flag"
	"strconv"
	"strings"
	"testing"
	"time"
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
		{"racer_metadata_connections", "4", "2", "3", "6", func(c *Config) string { return strconv.Itoa(c.RacerMetadataConnections) }},
		{"racer_metadata_queued_requests", "16", "5", "6", "7", func(c *Config) string { return strconv.Itoa(c.RacerMetadataQueuedRequests) }},
		{"racer_small_object_connections", "4", "2", "3", "6", func(c *Config) string { return strconv.Itoa(c.RacerSmallObjectConnections) }},
		{"racer_small_object_queued_requests", "128", "8", "9", "10", func(c *Config) string { return strconv.Itoa(c.RacerSmallObjectQueuedRequests) }},
		{"racer_max_queued_requests", "128", "20", "21", "22", func(c *Config) string { return strconv.Itoa(c.RacerMaxQueuedRequests) }},
		{"racer_queue_timeout", "5s", "2s", "3s", "4s", func(c *Config) string { return c.RacerQueueTimeout.String() }},
		{"racer_response_header_timeout", "1m0s", "2s", "3s", "4s", func(c *Config) string { return c.RacerResponseHeaderTimeout.String() }},
		{"racer_max_conn_age", "5m0s", "2s", "3s", "4s", func(c *Config) string { return c.RacerMaxConnAge.String() }},
		{"racer_idle_conn_timeout", "20s", "2s", "3s", "4s", func(c *Config) string { return c.RacerIdleConnTimeout.String() }},
		{"racer_dial_timeout", "5s", "2s", "3s", "4s", func(c *Config) string { return c.RacerDialTimeout.String() }},
		{"racer_body_read_timeout", "1m0s", "2s", "3s", "4s", func(c *Config) string { return c.RacerBodyReadTimeout.String() }},
		{"racer_http_max_connections", "512", "12", "13", "14", func(c *Config) string { return strconv.Itoa(c.RacerHTTPMaxConnections) }},
		{"racer_origin_max_connections", "128", "12", "13", "14", func(c *Config) string { return strconv.Itoa(c.RacerOriginMaxConnections) }},
		{"racer_origin_concurrent_requests", "64", "12", "13", "14", func(c *Config) string { return strconv.Itoa(c.RacerOriginConcurrentRequests) }},
		{"racer_origin_concurrent_head_requests", "4", "2", "3", "6", func(c *Config) string { return strconv.Itoa(c.RacerOriginConcurrentHeadRequests) }},
		{"racer_origin_request_timeout", "1m0s", "2s", "3s", "4s", func(c *Config) string { return c.RacerOriginRequestTimeout.String() }},
		{"racer_write_timeout", "30s", "2s", "3s", "4s", func(c *Config) string { return c.RacerWriteTimeout.String() }},
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

			if strings.HasSuffix(test.name, "timeout") || strings.HasSuffix(test.name, "age") {
				negative = (-time.Second).String()
				zero = "0s"
			}

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
