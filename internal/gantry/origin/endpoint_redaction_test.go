// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestEndpointStartupErrorsDoNotLeakCredentials(t *testing.T) {
	for _, tt := range []struct {
		name, endpoint, want string
	}{
		{"scheme", "ftp://private-user:private-password@registry.example", "scheme must be http or https"},
		{"escape", "https://private-user:private-password%zz@registry.example", "parse endpoint: invalid URL"},
		{"port", "https://private-user:private-password@registry.example:bad", "parse endpoint: invalid URL"},
		{"port includes secret", "https://registry.example:private-password", "parse endpoint: invalid URL"},
		{"control", "https://private-user:private-password\n@registry.example", "parse endpoint: invalid URL"},
		{"path", "https://private-user:private-password@registry.example/%zz", "parse endpoint: invalid URL"},
		{"ipv6", "https://private-user:private-password@[::1", "parse endpoint: invalid URL"},
		{"empty host", "https://", "endpoint: hostname is required"},
		{"path without host", "https:///prefix", "endpoint: hostname is required"},
		{"port without host", "https://:443/prefix", "endpoint: hostname is required"},
		{"userinfo without host", "https://private-user:private-password@/prefix?private-query#private-fragment", "endpoint: hostname is required"},
		{"userinfo and port without host", "http://private-user:private-password@:5000/private-path", "endpoint: hostname is required"},
		{"empty bracketed host", "https://[]:443/prefix", "parse endpoint: invalid URL"},
		{"opaque", "https:private-secret", "endpoint: hostname is required"},
	} {
		for _, delegated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/delegated=%t", tt.name, delegated), func(t *testing.T) {
				cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "registry.example", Endpoint: tt.endpoint}}}

				var opts []Option
				if delegated {
					opts = append(opts, WithDelegatedCredentialsOnly())
				}

				client, err := New(cfg, opts...)
				if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), `registry "registry.example"`) {
					t.Fatalf("expected registry endpoint error, got %v", err)
				}

				if client != nil {
					t.Fatal("invalid endpoint returned a client")
				}

				if strings.Contains(fmt.Sprintf("%+v", err), "private") {
					t.Fatalf("startup error leaked credentials: %v", err)
				}
			})
		}
	}
}

func TestEndpointStartupValidHostname(t *testing.T) {
	for _, endpoint := range []string{
		"https://registry.example",
		"http://localhost:5000/prefix",
		"https://127.0.0.1:443/prefix",
		"https://[::1]/prefix",
		"https://[::1]:443/prefix",
		"https://private-user:private-password@registry.example:443/prefix?private-query#private-fragment",
	} {
		for _, delegated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/delegated=%t", endpoint, delegated), func(t *testing.T) {
				cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg", Endpoint: endpoint}}}

				var opts []Option
				if delegated {
					opts = append(opts, WithDelegatedCredentialsOnly())
				}

				client, err := New(cfg, opts...)
				if err != nil {
					t.Fatal(err)
				}

				if client.registries["reg"].base.Hostname() == "" {
					t.Fatal("constructed registry has no hostname")
				}
			})
		}
	}
}

func TestRedactionPreservesLegacyEndpointCredentials(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "legacy-user" || password != "legacy-password" {
			t.Error("legacy endpoint credentials were not sent")
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		_, _ = io.WriteString(w, "blob")
	}))
	defer srv.Close()

	endpoint, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	endpoint.User = url.UserPassword("legacy-user", "legacy-password")

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg", Endpoint: endpoint.String()}}}
	if got := cfg.Redacted().UpstreamRegistries[0].Endpoint; got != srv.URL {
		t.Fatalf("redacted endpoint = %q, want %q", got, srv.URL)
	}

	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	c.registries["reg"].hc = srv.Client()

	body, _, err := c.Pull(context.Background(), ifaces.OriginRef{Registry: "reg", Repository: "repo", Digest: digestOf([]byte("blob")), Kind: ifaces.KindBlob})
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()

	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatal(err)
	}

	if cfg.UpstreamRegistries[0].Endpoint != endpoint.String() {
		t.Fatal("real endpoint was changed")
	}
}
