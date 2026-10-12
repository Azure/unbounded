// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/racer"
)

func TestRacerRegistryNameValidation(t *testing.T) {
	for _, tt := range []struct {
		label string
		name  string
		valid bool
	}{
		{"hostname", "registry.example", true},
		{"localhost", "localhost", true},
		{"port", "registry.example:5000", true},
		{"IPv4", "127.0.0.1", true},
		{"IPv4 port", "127.0.0.1:5000", true},
		{"IPv6", "[2001:db8::1]", true},
		{"IPv6 port", "[2001:db8::1]:5000", true},
		{"unbracketed IPv6", "2001:db8::1", true},
		{"uppercase", "REGISTRY.EXAMPLE", true},
		{"trailing dot", "registry.example.", true},
		{"underscore", "registry_internal", true},
		{"empty port", "registry.example:", true},
		{"zero port", "registry.example:0", true},
		{"large port", "registry.example:65536", true},
		{"empty", "", false},
		{"https URL", "https://registry.example", false},
		{"http URL", "http://registry.example", false},
		{"scheme relative URL", "//registry.example", false},
		{"path", "registry.example/repository", false},
		{"trailing slash", "registry.example/", false},
		{"query", "registry.example?token=secret", false},
		{"empty query", "registry.example?", false},
		{"fragment", "registry.example#fragment", false},
		{"empty fragment", "registry.example#", false},
		{"userinfo", "user:secret@registry.example", false},
		{"empty userinfo", "@registry.example", false},
		{"leading space", " registry.example", false},
		{"trailing space", "registry.example ", false},
		{"newline", "registry.example\n", false},
		{"tab", "registry\texample", false},
		{"backslash", "registry.example\\repository", false},
		{"escaped host", "registry%2eexample", false},
		{"invalid escape", "registry%zz.example", false},
		{"named port", "registry.example:https", false},
		{"negative port", "registry.example:-1", false},
		{"missing host", ":5000", false},
		{"missing IPv6 bracket", "[2001:db8::1", false},
		{"empty IPv6 host", "[]:5000", false},
		{"escaped IPv6 zone", "[fe80::1%25eth0]:5000", false},
		{"unescaped IPv6 zone", "[fe80::1%eth0]:5000", false},
	} {
		t.Run(tt.label, func(t *testing.T) {
			ref := ifaces.OriginRef{
				Registry: tt.name, Repository: "library/image", Kind: ifaces.KindBlob,
				Digest: digest.MustParse("sha256:" + strings.Repeat("a", 64)),
			}
			if _, err := racer.Request(ref, ""); (err == nil) != tt.valid {
				t.Fatalf("request registry validity = %v, want %v: %v", err == nil, tt.valid, err)
			}

			c := config.NewDefault()
			c.RacerEnabled = true
			c.UpstreamRegistries = []config.UpstreamRegistry{{Name: tt.name, Endpoint: "https://registry.example"}}

			err := c.Validate()
			if (err == nil) != tt.valid {
				t.Fatalf("startup registry validity = %v, want %v: %v", err == nil, tt.valid, err)
			}

			if err != nil && !strings.Contains(err.Error(), "upstream_registries[0].name:") {
				t.Fatalf("missing indexed name error: %v", err)
			}

			c.RacerEnabled = false
			if err := c.Validate(); (err == nil) != (tt.name != "") {
				t.Fatalf("legacy name validation changed: %v", err)
			}
		})
	}
}

func TestRacerRegistryNameValidationJoinsErrors(t *testing.T) {
	c := config.NewDefault()
	c.RacerEnabled = true
	c.RacerMaxConnections = -1
	c.UpstreamRegistries = []config.UpstreamRegistry{
		{Name: "registry.example", Endpoint: "https://registry.example"},
		{Name: "https://second.example", Endpoint: "https://second.example"},
		{Name: "third.example/path", Endpoint: "https://third.example"},
	}

	err := c.Validate()
	if err == nil {
		t.Fatal("invalid names and Racer tuning accepted")
	}

	for _, field := range []string{"upstream_registries[1].name:", "upstream_registries[2].name:", "racer_max_connections:"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("missing %s error: %v", field, err)
		}
	}

	if strings.Contains(err.Error(), "upstream_registries[0].name:") {
		t.Fatalf("valid registry rejected: %v", err)
	}
}
