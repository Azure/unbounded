// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateEndpointErrorsDoNotLeakCredentials(t *testing.T) {
	for _, endpoint := range []string{
		"ftp://private-user:private-password@registry.example",
		"private-user:private-password@registry.example",
		"https:private-user:private-password@registry.example",
		"https://",
		"https:///prefix",
		"https://:443/prefix",
		"https://private-user:private-password@/prefix?private-query#private-fragment",
		"http://private-user:private-password@:5000/private-path",
	} {
		t.Run(endpoint, func(t *testing.T) {
			for _, racer := range []bool{false, true} {
				c := NewDefault()
				c.RacerEnabled = racer
				c.UpstreamRegistries = []UpstreamRegistry{{Name: "registry.example", Endpoint: endpoint}}

				err := c.Validate()
				if err == nil || !strings.Contains(err.Error(), "upstream_registries[0].endpoint") {
					t.Fatalf("RacerEnabled=%t: expected endpoint validation error, got %v", racer, err)
				}

				if strings.Contains(err.Error(), "private") {
					t.Fatalf("validation leaked endpoint credentials: %v", err)
				}
			}
		})
	}
}

func TestValidateEndpointParseErrors(t *testing.T) {
	for _, tt := range []struct {
		name, endpoint string
	}{
		{"userinfo escape", "private-user:private-password%zz@registry.example"},
		{"port", "private-user:private-password@registry.example:bad"},
		{"port includes secret", "registry.example:private-password"},
		{"control", "private-user:private-password\n@registry.example"},
		{"path escape", "private-user:private-password@registry.example/private-path%zz?private-query#private-fragment"},
		{"fragment escape", "registry.example/private-path?private-query#private-fragment%zz"},
		{"ipv6", "private-user:private-password@[::1"},
		{"empty bracketed host", "[]:443/private-path"},
	} {
		for _, scheme := range []string{"http://", "https://"} {
			for _, racer := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/racer=%t", tt.name, scheme, racer), func(t *testing.T) {
					c := NewDefault()
					c.RacerEnabled = racer
					c.UpstreamRegistries = []UpstreamRegistry{{Name: "registry.example", Endpoint: scheme + tt.endpoint}}

					const want = "upstream_registries[0].endpoint: invalid URL"

					if err := c.Validate(); err == nil || err.Error() != want {
						t.Fatalf("expected generic endpoint validation error %q, got %v", want, err)
					}
				})
			}
		}
	}
}

func TestValidateEndpointValidHostname(t *testing.T) {
	for _, endpoint := range []string{
		"https://registry.example",
		"http://localhost:5000/prefix",
		"https://127.0.0.1:443/prefix",
		"https://[::1]/prefix",
		"https://[::1]:443/prefix",
		"https://private-user:private-password@registry.example:443/prefix?private-query#private-fragment",
		"http://private%2Duser:private%2Dpassword@registry.example:5000/prefix%2Fpath",
		"https://private%2Duser:private%2Dpassword@registry.example:443/prefix%2Fpath",
	} {
		t.Run(endpoint, func(t *testing.T) {
			for _, racer := range []bool{false, true} {
				c := NewDefault()
				c.RacerEnabled = racer
				c.UpstreamRegistries = []UpstreamRegistry{{Name: "registry.example", Endpoint: endpoint}}

				if err := c.Validate(); err != nil {
					t.Fatalf("RacerEnabled=%t: %v", racer, err)
				}
			}
		})
	}
}
