// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

// anonymousRegistry serves a public and a private repository behind a
// Bearer challenge. The token endpoint issues a "shared" token only to the
// configured Basic credential and an "anon" token otherwise; the private
// repository accepts only the shared token.
func anonymousRegistry(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()

	var seen []string

	var srv *httptest.Server

	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization"))

		if r.URL.Path == "/token" {
			if u, p, ok := r.BasicAuth(); ok && u == "user" && p == "pw" {
				_, _ = w.Write([]byte(`{"token":"shared"}`))
				return
			}

			_, _ = w.Write([]byte(`{"token":"anon"}`))

			return
		}

		auth := r.Header.Get("Authorization")
		ok := auth == "Bearer shared" ||
			(auth == "Bearer anon" && strings.HasPrefix(r.URL.Path, "/v2/public/"))

		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="reg"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		_, _ = w.Write([]byte(`{"schemaVersion":2}`))
	}))
	t.Cleanup(srv.Close)

	return srv, &seen
}

func anonymousClient(t *testing.T, srv *httptest.Server, endpoint string) *Client {
	t.Helper()

	path := filepath.Join(t.TempDir(), "creds")
	if err := os.WriteFile(path, []byte("user:pw"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: endpoint, CredentialsPath: path})
	c.registries["reg"].hc = srv.Client()

	return c
}

func TestManifestByTagAnonymous(t *testing.T) {
	t.Run("credentialed context uses shared credentials", func(t *testing.T) {
		srv, _ := anonymousRegistry(t)
		c := anonymousClient(t, srv, srv.URL)

		if _, _, _, err := c.ManifestByTag(context.Background(), "reg", "private/app", "v1"); err != nil {
			t.Fatalf("credentialed private fetch: %v", err)
		}
	})

	t.Run("anonymous context reads public repository", func(t *testing.T) {
		srv, _ := anonymousRegistry(t)
		c := anonymousClient(t, srv, srv.URL)

		ctx := registryauth.WithAnonymous(context.Background())
		if _, _, _, err := c.ManifestByTag(ctx, "reg", "public/app", "v1"); err != nil {
			t.Fatalf("anonymous public fetch: %v", err)
		}
	})

	t.Run("anonymous context cannot read private repository", func(t *testing.T) {
		srv, seen := anonymousRegistry(t)
		c := anonymousClient(t, srv, srv.URL)

		// Warm the shared token cache so a leak through it would succeed.
		if _, _, _, err := c.ManifestByTag(context.Background(), "reg", "private/app", "v1"); err != nil {
			t.Fatalf("warm: %v", err)
		}

		*seen = nil

		ctx := registryauth.WithAnonymous(registryauth.WithAuthorization(context.Background(), "Bearer shared"))

		_, _, _, err := c.ManifestByTag(ctx, "reg", "private/app", "v1")

		var oe *ifaces.OriginError
		if !errors.As(err, &oe) || oe.Class != ifaces.FailureAuth {
			t.Fatalf("want auth failure, got %v", err)
		}

		for _, s := range *seen {
			if strings.Contains(s, "shared") || strings.Contains(s, "Basic") {
				t.Fatalf("anonymous request carried credentials: %q", s)
			}
		}
	})

	t.Run("anonymous context drops URL user information", func(t *testing.T) {
		srv, seen := anonymousRegistry(t)
		endpoint := strings.Replace(srv.URL, "https://", "https://user:pw@", 1)
		c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: endpoint})
		c.registries["reg"].hc = srv.Client()

		ctx := registryauth.WithAnonymous(context.Background())
		if _, _, _, err := c.ManifestByTag(ctx, "reg", "private/app", "v1"); err == nil {
			t.Fatal("want auth failure")
		}

		for _, s := range *seen {
			if strings.Contains(s, "Basic") {
				t.Fatalf("anonymous request carried URL credentials: %q", s)
			}
		}
	})

	t.Run("anonymous context refuses endpoint query", func(t *testing.T) {
		srv, seen := anonymousRegistry(t)
		c := anonymousClient(t, srv, srv.URL+"/?sig=secret")

		ctx := registryauth.WithAnonymous(context.Background())

		_, _, _, err := c.ManifestByTag(ctx, "reg", "public/app", "v1")

		var oe *ifaces.OriginError
		if !errors.As(err, &oe) || oe.Class != ifaces.FailureAuth {
			t.Fatalf("want auth failure, got %v", err)
		}

		if len(*seen) != 0 {
			t.Fatalf("request reached origin: %v", *seen)
		}
	})
}
