// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

func TestRepositoryAuthenticationChallenge(t *testing.T) {
	const bearer = `Bearer realm="https://auth.example/token",scope="repository:private/repo:pull",service="reg"`
	for _, tc := range []struct {
		name    string
		status  int
		header  string
		want    string
		wantErr bool
	}{
		{"bearer", 401, bearer, bearer, false},
		{"basic", 401, `Basic realm="private"`, `Basic realm="private"`, false},
		{"public", 200, bearer, "", false},
		{"missing", 401, "", "", true},
		{"unsupported", 401, `Digest realm="private"`, "", true},
		{"insecure realm", 401, `Bearer realm="http://auth.example/token"`, "", true},
		{"realm userinfo", 401, `Bearer realm="https://user:password@auth.example/token"`, "", true},
		{"forbidden", 403, bearer, "", true},
		{"not found", 404, bearer, "", true},
		{"method unsupported", 405, bearer, "", true},
		{"unavailable", 503, bearer, "", true},
		{"redirect", 307, bearer, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits, redirects atomic.Int32

			target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirects.Add(1) }))
			defer target.Close()

			d := digestOf([]byte("resource"))

			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)

				if r.Method != http.MethodHead || r.URL.Path != "/prefix/v2/private/repo/manifests/"+d.String() || r.URL.RawQuery != "" {
					t.Errorf("unexpected probe %s %s", r.Method, r.URL)
				}

				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Range") != "" {
					t.Error("probe forwarded credentials, cookies or range")
				}

				w.Header().Set("WWW-Authenticate", tc.header)
				w.Header().Set("Location", target.URL)
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL + "/prefix?ignored=1#fragment"})
			r := c.registries["reg"]
			r.base.User = url.UserPassword("endpoint", "password")
			r.hc = srv.Client()

			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}

			jar.SetCookies(r.base, []*http.Cookie{{Name: "session", Value: "secret"}})
			r.hc.Jar = jar
			r.setToken("cached-token", time.Hour)
			r.rememberAuthenticationChallenge(`Basic realm="wrong-repository"`)

			ctx := registryauth.WithAuthorization(context.Background(), "Bearer expired")

			got, required, err := c.RepositoryAuthenticationChallenge(ctx, ifaces.OriginRef{Registry: "reg", Repository: "private/repo", Digest: d, Kind: ifaces.KindManifest})
			if (err != nil) != tc.wantErr || got != tc.want || required != (tc.want != "") {
				t.Fatalf("challenge=%q required=%v err=%v", got, required, err)
			}

			if hits.Load() != 1 || redirects.Load() != 0 {
				t.Fatalf("hits=%d redirects=%d", hits.Load(), redirects.Load())
			}

			if r.challenge.value != `Basic realm="wrong-repository"` {
				t.Fatal("probe changed registry-wide cache")
			}
		})
	}
}

func TestRepositoryAuthenticationChallengeAccept(t *testing.T) {
	const (
		bearer         = `Bearer realm="https://auth.example/token",scope="repository:private/manifests/repo:pull",service="reg"`
		manifestAccept = "application/vnd.oci.image.manifest.v1+json, " +
			"application/vnd.oci.image.index.v1+json, " +
			"application/vnd.docker.distribution.manifest.v2+json, " +
			"application/vnd.docker.distribution.manifest.list.v2+json"
	)

	for _, resource := range []struct {
		name   string
		kind   ifaces.OriginRefKind
		path   string
		accept string
	}{
		{"manifest", ifaces.KindManifest, "manifests", manifestAccept},
		{"blob", ifaces.KindBlob, "blobs", ""},
		{"config", ifaces.KindConfig, "blobs", ""},
	} {
		t.Run(resource.name, func(t *testing.T) {
			for _, response := range []struct {
				name    string
				status  int
				wantErr bool
				want    string
			}{
				{"challenge", http.StatusUnauthorized, false, bearer},
				{"public", http.StatusOK, false, ""},
				{"not found", http.StatusNotFound, true, ""},
				{"not acceptable", http.StatusNotAcceptable, true, ""},
			} {
				t.Run(response.name, func(t *testing.T) {
					d := digestOf([]byte("resource"))

					var hits atomic.Int32

					srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						hits.Add(1)

						if r.Method != http.MethodHead || r.URL.Path != "/v2/private/manifests/repo/"+resource.path+"/"+d.String() {
							t.Errorf("unexpected probe %s %s", r.Method, r.URL)
						}

						if got := r.Header.Get("Accept"); got != resource.accept {
							t.Errorf("Accept = %q, want %q", got, resource.accept)
							w.WriteHeader(http.StatusNotAcceptable)

							return
						}

						w.Header().Set("WWW-Authenticate", bearer)
						w.WriteHeader(response.status)
					}))
					defer srv.Close()

					c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
					c.registries["reg"].hc = srv.Client()

					got, required, err := c.RepositoryAuthenticationChallenge(context.Background(), ifaces.OriginRef{
						Registry: "reg", Repository: "private/manifests/repo", Digest: d, Kind: resource.kind,
					})
					if (err != nil) != response.wantErr || got != response.want || required != (response.want != "") {
						t.Errorf("challenge=%q required=%v err=%v", got, required, err)
					}

					if hits.Load() != 1 {
						t.Errorf("probe requests = %d, want 1", hits.Load())
					}
				})
			}
		})
	}
}

func TestRepositoryAuthenticationChallengeRejectsInvalidTargets(t *testing.T) {
	var hits atomic.Int32

	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
	c.registries["reg"].hc = srv.Client()

	for _, tc := range []struct {
		name   string
		mutate func(*ifaces.OriginRef)
	}{
		{"unknown registry", func(r *ifaces.OriginRef) { r.Registry = "attacker" }},
		{"traversal", func(r *ifaces.OriginRef) { r.Repository = "../other" }},
		{"encoded path", func(r *ifaces.OriginRef) { r.Repository = "private%2frepo" }},
		{"query", func(r *ifaces.OriginRef) { r.Repository = "private?repo" }},
		{"empty digest", func(r *ifaces.OriginRef) { r.Digest = digest.Digest{} }},
		{"invalid kind", func(r *ifaces.OriginRef) { r.Kind = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := ifaces.OriginRef{Registry: "reg", Repository: "private/repo", Digest: digestOf(nil)}
			tc.mutate(&ref)

			if _, _, err := c.RepositoryAuthenticationChallenge(context.Background(), ref); err == nil {
				t.Fatal("accepted invalid target")
			}
		})
	}

	if hits.Load() != 0 {
		t.Fatalf("invalid requests reached origin: %d", hits.Load())
	}
}

func TestRepositoryAuthenticationChallengeTrustAndCancellation(t *testing.T) {
	for _, mode := range []string{"plaintext", "untrusted TLS", "canceled", "deadline", "internal bound"} {
		t.Run(mode, func(t *testing.T) {
			var hits atomic.Int32

			handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { hits.Add(1); <-r.Context().Done() })

			srv := httptest.NewUnstartedServer(handler)
			if mode == "plaintext" {
				srv.Start()
			} else {
				srv.StartTLS()
			}
			defer srv.Close()

			c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
			if mode != "untrusted TLS" {
				c.registries["reg"].hc = srv.Client()
			}

			ctx := context.Background()

			switch mode {
			case "canceled":
				var cancel context.CancelFunc

				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "deadline":
				var cancel context.CancelFunc

				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}

			start := time.Now()

			got, required, err := c.RepositoryAuthenticationChallenge(ctx, ifaces.OriginRef{Registry: "reg", Repository: "private/repo", Digest: digestOf(nil)})
			if err == nil || required || got != "" {
				t.Fatalf("challenge=%q required=%v err=%v", got, required, err)
			}

			if mode == "deadline" || mode == "internal bound" {
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
					t.Fatalf("unbounded or wrong failure: %v", err)
				}
			} else if hits.Load() != 0 {
				t.Fatalf("unsafe probe reached origin: %d", hits.Load())
			}
		})
	}
}
