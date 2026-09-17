// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestStatusDetailMode(t *testing.T) {
	if DefaultStatusDetailMode != "summary" {
		t.Fatal("routine publication must default to summaries")
	}

	for _, mode := range []string{"summary", "full", "", "SUMMARY", "other", " full "} {
		valid := mode == "summary" || mode == "full"
		if err := ValidateStatusDetailMode(mode); (err == nil) != valid {
			t.Errorf("ValidateStatusDetailMode(%q) = %v", mode, err)
		}
	}
}

func TestPositiveStatusDetailDurations(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		want  time.Duration
		valid bool
	}{
		{"", 0, true},
		{"300s", 300 * time.Second, true},
		{"1ns", time.Nanosecond, true},
		{"0s", 0, false},
		{"-1s", 0, false},
		{"invalid", 0, false},
	} {
		got, err := ParsePositiveDurationField(tc.raw, "controller.statusDetailCacheTTL")
		if (err == nil) != tc.valid || got != tc.want {
			t.Errorf("ParsePositiveDurationField(%q) = %v, %v", tc.raw, got, err)
		}

		if err != nil && !strings.Contains(err.Error(), "controller.statusDetailCacheTTL") {
			t.Errorf("missing field name in error: %v", err)
		}
	}

	for _, field := range []string{"cache", "request"} {
		for _, duration := range []time.Duration{0, -time.Second, time.Nanosecond} {
			cfg := &Config{
				StatusWSKeepaliveFailureCount: 2,
				StatusDetailCacheTTL:          DefaultStatusDetailCacheTTL,
				StatusDetailRequestTimeout:    DefaultStatusDetailRequestTimeout,
			}
			if field == "cache" {
				cfg.StatusDetailCacheTTL = duration
			} else {
				cfg.StatusDetailRequestTimeout = duration
			}

			if err := cfg.Validate(); (err == nil) != (duration > 0) {
				t.Errorf("Validate(%s=%s) = %v", field, duration, err)
			}
		}
	}
}

func TestStatusDetailRuntimeYAMLRoundTrip(t *testing.T) {
	for _, mode := range []string{"", "summary", "full"} {
		want := RuntimeConfig{
			Node: NodeRuntimeConfig{StatusDetailMode: mode},
			Controller: ControllerRuntimeConfig{
				StatusDetailCacheTTL: "300s", StatusDetailRequestTimeout: "120s",
			},
		}

		data, err := yaml.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}

		for _, field := range []string{"statusDetailMode:", "statusDetailCacheTTL: 300s", "statusDetailRequestTimeout: 120s"} {
			if !strings.Contains(string(data), field) {
				t.Errorf("missing YAML setting %q", field)
			}
		}

		var got RuntimeConfig
		if err := yaml.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}

		if got.Node.StatusDetailMode != mode ||
			got.Controller.StatusDetailCacheTTL != want.Controller.StatusDetailCacheTTL ||
			got.Controller.StatusDetailRequestTimeout != want.Controller.StatusDetailRequestTimeout {
			t.Fatalf("settings changed after YAML round trip: %+v", got)
		}
	}
}
