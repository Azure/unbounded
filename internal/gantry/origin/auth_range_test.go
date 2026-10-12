// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

func TestPullRangeAuthRetriesKeepBounds(t *testing.T) {
	for _, mode := range []string{"basic", "bearer", "stale bearer", "no challenge"} {
		t.Run(mode, func(t *testing.T) {
			var requests, tokens atomic.Int64

			var srv *httptest.Server

			srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					tokens.Add(1)

					user, password, ok := r.BasicAuth()
					if !ok || user != "shared" || password != "secret" || r.Header.Get("Range") != "" {
						t.Error("token exchange lost shared credentials or received resource range")
					}

					_, _ = io.WriteString(w, `{"token":"fresh"}`)

					return
				}

				n := requests.Add(1)

				if r.Header.Get("Range") != "bytes=4-6" || r.Header.Get("Accept") == "" {
					t.Error("auth retry lost bounded range or manifest Accept")
				}

				if n == 1 {
					if mode == "stale bearer" && r.Header.Get("Authorization") != "Bearer stale" {
						t.Error("cached token not used")
					}

					switch mode {
					case "basic":
						w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
					case "bearer", "stale bearer":
						w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token"`)
					}

					w.WriteHeader(401)

					return
				}

				if mode == "bearer" || mode == "stale bearer" {
					if r.Header.Get("Authorization") != "Bearer fresh" {
						t.Error("missing refreshed token")
					}
				} else if user, password, ok := r.BasicAuth(); !ok || user != "shared" || password != "secret" {
					t.Error("legacy Basic retry lost credentials")
				}

				w.Header().Set("Content-Range", "bytes 4-6/10")
				w.WriteHeader(206)
				_, _ = io.WriteString(w, "456")
			}))
			defer srv.Close()

			c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
			r := c.registries["reg"]
			r.username, r.password = "shared", "secret"

			r.hc = srv.Client()
			if mode == "stale bearer" {
				r.setToken("stale", time.Hour)
			}

			body, size, _, err := c.PullRange(context.Background(), ifaces.OriginRef{Registry: "reg", Repository: "repo", Kind: ifaces.KindManifest, Offset: 4}, 3)
			if err != nil {
				t.Fatal(err)
			}

			data, err := io.ReadAll(body)
			_ = body.Close()

			wantTokens := int64(0)
			if mode == "bearer" || mode == "stale bearer" {
				wantTokens = 1
			}

			if err != nil || string(data) != "456" || size != 10 || requests.Load() != 2 || tokens.Load() != wantTokens {
				t.Fatalf("data=%q size=%d requests=%d tokens=%d err=%v", data, size, requests.Load(), tokens.Load(), err)
			}
		})
	}
}

func TestDelegatedCredentialsOnly(t *testing.T) {
	for _, mode := range []string{"basic", "bearer", "public", "delegated", "rejected"} {
		t.Run(mode, func(t *testing.T) {
			var requests, tokens atomic.Int64

			var srv *httptest.Server

			srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth := r.Header.Get("Authorization")
				if r.URL.Path == "/token" {
					tokens.Add(1)

					if auth != "" {
						t.Error("anonymous token exchange used shared credentials")
					}

					_, _ = io.WriteString(w, `{"token":"anonymous"}`)

					return
				}

				requests.Add(1)

				if auth != "" && auth != "Bearer anonymous" && auth != "Bearer requester" {
					t.Error("request used shared credentials")
				}

				if mode == "delegated" || mode == "rejected" {
					if auth != "Bearer requester" {
						t.Error("request lost delegated identity")
					}
				}

				if mode == "basic" || mode == "rejected" || mode == "bearer" && auth == "" {
					challenge := `Basic realm="private"`
					if mode != "basic" {
						challenge = `Bearer realm="` + srv.URL + `/token"`
					}

					w.Header().Set("WWW-Authenticate", challenge)
					w.WriteHeader(401)

					return
				}

				w.Header().Set("Content-Range", "bytes 0-3/10")
				w.WriteHeader(206)
				_, _ = io.WriteString(w, "0123")
			}))
			defer srv.Close()

			path := filepath.Join(t.TempDir(), "credentials")
			if err := os.WriteFile(path, []byte("shared:secret"), 0o600); err != nil {
				t.Fatal(err)
			}

			endpoint, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatal(err)
			}

			endpoint.User = url.UserPassword("endpoint", "secret")
			cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg", Endpoint: endpoint.String(), CredentialsPath: path}}}

			c, err := New(cfg, WithDelegatedCredentialsOnly())
			if err != nil {
				t.Fatal(err)
			}

			r := c.registries["reg"]
			if r.username != "" || r.password != "" || r.base.User != nil || cfg.UpstreamRegistries[0].CredentialsPath != path {
				t.Fatal("option retained shared identity or mutated caller config")
			}

			r.hc = srv.Client()

			if mode == "basic" {
				challenge, required, err := c.AuthenticationChallenge(context.Background(), "reg")
				if err != nil || !required || challenge == "" || requests.Load() != 1 {
					t.Fatalf("shared config suppressed initial challenge: %q %v %v", challenge, required, err)
				}
			}

			ctx := context.Background()
			if mode == "delegated" || mode == "rejected" {
				ctx = registryauth.WithAuthorization(ctx, "Bearer requester")
			}

			body, _, _, err := c.PullRange(ctx, ifaces.OriginRef{Registry: "reg", Repository: "repo"}, 4)

			if mode == "basic" || mode == "rejected" {
				var oe *ifaces.OriginError
				if !errors.As(err, &oe) || oe.StatusCode != 401 || oe.Challenge == "" {
					t.Fatalf("expected auth rejection: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}

				_, _ = io.Copy(io.Discard, body)
				_ = body.Close()
			}

			if mode == "rejected" && (requests.Load() != 1 || tokens.Load() != 0) {
				t.Fatal("rejected delegated identity triggered fallback")
			}

			if mode == "basic" {
				challenge, required, err := c.AuthenticationChallenge(context.Background(), "reg")
				if err != nil || !required || challenge == "" {
					t.Fatalf("shared config suppressed delegated challenge: %q %v %v", challenge, required, err)
				}
			}
		})
	}
}

func TestDelegatedCredentialsOnlySkipsCredentialFile(t *testing.T) {
	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg", Endpoint: "https://registry.example", CredentialsPath: filepath.Join(t.TempDir(), "missing")}}}
	if _, err := New(cfg, WithDelegatedCredentialsOnly()); err != nil {
		t.Fatalf("delegated client read shared credential file: %v", err)
	}

	if _, err := New(cfg); err == nil {
		t.Fatal("legacy client stopped validating credential file")
	}
}

func TestBearerRealmUserinfoRejectedBeforeIO(t *testing.T) {
	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: "https://registry.example"})
	r := c.registries["reg"]

	var hits atomic.Int64

	r.hc.Transport = rangeRoundTripper(func(*http.Request) (*http.Response, error) {
		hits.Add(1)
		return nil, errors.New("unexpected network request")
	})

	for _, realm := range []string{"https://user:secret@auth.example/token", "https://user@auth.example/token", "https://%zz:secret@auth.example/token"} {
		challenge := `Bearer realm="` + realm + `"`

		_, _, err := r.fetchBearerToken(context.Background(), challenge)
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), realm) {
			t.Fatalf("unsafe realm accepted or leaked: %v", err)
		}

		request, err := http.NewRequest(http.MethodGet, "https://registry.example/v2/", nil)
		if err != nil {
			t.Fatal(err)
		}

		resp := &http.Response{Request: request, Header: http.Header{"Www-Authenticate": []string{challenge}}}
		if got, err := validatedAuthenticationChallenge(resp); err == nil || got != "" {
			t.Fatal("unsafe realm relayed")
		}
	}

	if hits.Load() != 0 {
		t.Fatal("unsafe realm reached network")
	}
}

func TestTokenStatusPropagation(t *testing.T) {
	for _, status := range []int{401, 403, 429, 503} {
		for _, operation := range []string{"pull", "head", "range", "pull fallback", "head fallback", "range fallback"} {
			t.Run(fmt.Sprintf("%d/%s", status, operation), func(t *testing.T) {
				var srv *httptest.Server

				srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/token" {
						w.WriteHeader(status)
						return
					}

					if strings.HasSuffix(operation, "fallback") && strings.Contains(r.URL.Path, "/blobs/") {
						w.WriteHeader(404)
						return
					}

					w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token"`)
					w.WriteHeader(401)
				}))
				defer srv.Close()

				c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
				c.registries["reg"].hc = srv.Client()
				ref := ifaces.OriginRef{Registry: "reg", Repository: "repo"}

				var err error

				switch strings.Fields(operation)[0] {
				case "pull":
					_, _, err = c.Pull(context.Background(), ref)
				case "head":
					_, _, err = c.Head(context.Background(), ref)
				case "range":
					_, _, _, err = c.PullRange(context.Background(), ref, 4)
				}

				class := ifaces.FailureTransient

				switch status {
				case 401, 403:
					class = ifaces.FailureAuth
				case 429:
					class = ifaces.FailureRateLimited
				}

				var oe *ifaces.OriginError
				if !errors.As(err, &oe) || oe.StatusCode != status || oe.Class != class {
					t.Fatalf("token status/class lost: %v", err)
				}
			})
		}
	}
}
