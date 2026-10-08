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
	} {
		for _, delegated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/delegated=%t", tt.name, delegated), func(t *testing.T) {
				cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "registry.example", Endpoint: tt.endpoint}}}

				var opts []Option
				if delegated {
					opts = append(opts, WithDelegatedCredentialsOnly())
				}

				_, err := New(cfg, opts...)
				if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), `registry "registry.example"`) {
					t.Fatalf("expected registry endpoint error, got %v", err)
				}

				if strings.Contains(fmt.Sprintf("%+v", err), "private") {
					t.Fatalf("startup error leaked credentials: %v", err)
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
