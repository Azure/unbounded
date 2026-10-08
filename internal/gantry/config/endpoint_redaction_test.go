// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

func TestRedactedEndpoints(t *testing.T) {
	for _, tt := range []struct {
		name, endpoint, want string
	}{
		{"password", "https://private-user:private-password@registry.example/prefix?ns=example#fragment", "https://registry.example/prefix?ns=example#fragment"},
		{"username only", "http://private-user@registry.example:5000", "http://registry.example:5000"},
		{"encoded credentials", "https://private%2Duser:private%2Dpassword@registry.example", "https://registry.example"},
		{"empty password", "https://private-user:@registry.example", "https://registry.example"},
		{"ipv6", "https://private-user:private-password@[::1]:5000/v2/", "https://[::1]:5000/v2/"},
		{"public", "https://registry.example/prefix%2fpath?ns=example#fragment", "https://registry.example/prefix%2fpath?ns=example#fragment"},
		{"empty", "", ""},
		{"invalid escape", "https://private-user:private-password%zz@registry.example", "[REDACTED]"},
		{"invalid port", "https://private-user:private-password@registry.example:bad", "[REDACTED]"},
		{"invalid control", "https://private-user:private-password\n@registry.example", "[REDACTED]"},
		{"invalid path", "https://private-user:private-password@registry.example/%zz", "[REDACTED]"},
		{"unsupported scheme", "ftp://private-user:private-password@registry.example", "[REDACTED]"},
		{"opaque", "https:private-user:private-password@registry.example", "[REDACTED]"},
		{"relative", "//private-user:private-password@registry.example", "[REDACTED]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := UpstreamRegistry{Name: "registry.example", Endpoint: tt.endpoint, CredentialsPath: "/credentials", NSAlias: "alias"}
			c := &Config{UpstreamRegistries: []UpstreamRegistry{upstream, {Name: "public.example", Endpoint: "https://public.example"}}}
			redacted := c.Redacted()
			want := upstream

			want.Endpoint = tt.want
			if redacted == c || !reflect.DeepEqual(redacted.UpstreamRegistries, []UpstreamRegistry{want, c.UpstreamRegistries[1]}) {
				t.Fatalf("unexpected redacted copy: %+v", redacted.UpstreamRegistries)
			}

			for _, format := range []string{"json", "text"} {
				var (
					output  bytes.Buffer
					handler slog.Handler = slog.NewJSONHandler(&output, nil)
				)
				if format == "text" {
					handler = slog.NewTextHandler(&output, nil)
				}

				slog.New(handler).InfoContext(context.Background(), "gantry starting", slog.Any("config", redacted))

				if strings.Contains(output.String(), "private") {
					t.Fatalf("%s log leaked endpoint credentials: %s", format, output.String())
				}
			}

			redacted.UpstreamRegistries[0].Endpoint = "changed"
			if c.UpstreamRegistries[0] != upstream {
				t.Fatal("redaction mutated the real endpoint or shared its slice")
			}
		})
	}

	if (&Config{}).Redacted().UpstreamRegistries != nil {
		t.Fatal("redaction changed nil upstreams")
	}
}

func TestValidateEndpointErrorsDoNotLeakCredentials(t *testing.T) {
	for _, endpoint := range []string{
		"ftp://private-user:private-password@registry.example",
		"private-user:private-password@registry.example",
		"https:private-user:private-password@registry.example",
	} {
		c := NewDefault()
		c.UpstreamRegistries = []UpstreamRegistry{{Name: "registry.example", Endpoint: endpoint}}

		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "upstream_registries[0].endpoint") {
			t.Fatalf("expected endpoint validation error, got %v", err)
		}

		if strings.Contains(err.Error(), "private") {
			t.Fatalf("validation leaked endpoint credentials: %v", err)
		}
	}
}
