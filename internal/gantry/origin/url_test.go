// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestOriginEndpointURL(t *testing.T) {
	for _, endpoint := range []struct {
		name, suffix, prefix string
	}{
		{"root", "", ""},
		{"prefix", "/prefix", "/prefix"},
		{"query and fragment", "/prefix?ignored=1#fragment", "/prefix"},
		{"query", "/prefix?ignored=1", "/prefix"},
		{"fragment", "/prefix#fragment", "/prefix"},
		{"empty query", "/prefix?", "/prefix"},
		{"trailing slashes", "/prefix///?ignored=1#fragment", "/prefix"},
		{"escaped prefix", "/pre%2ffix/%3F%23%25%20?ignored=1#frag%2fment", "/pre%2ffix/%3F%23%25%20"},
		{"escaped trailing slash", "/prefix%2F/?#fragment", "/prefix%2F"},
	} {
		for _, operation := range []string{"range", "pull", "head", "range fallback", "pull fallback", "head fallback", "manifest", "root challenge", "repository challenge"} {
			t.Run(endpoint.name+"/"+operation, func(t *testing.T) {
				ref := ifaces.OriginRef{Registry: "reg", Repository: "private/repo", Digest: digestOf([]byte("data")), Kind: ifaces.KindBlob}
				if operation == "manifest" {
					ref.Kind = ifaces.KindManifest
				}

				var hits atomic.Int32

				challenge := strings.Contains(operation, "challenge")
				fallback := strings.Contains(operation, "fallback")
				bounded := strings.HasPrefix(operation, "range")

				srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					hit := hits.Add(1)

					resource := "blobs"
					if operation == "manifest" || fallback && hit == 2 {
						resource = "manifests"
					}

					wantURI := endpoint.prefix + "/v2/" + ref.Repository + "/" + resource + "/" + ref.Digest.String()
					if operation == "root challenge" {
						wantURI = endpoint.prefix + "/v2/"
					}

					wantMethod := http.MethodGet
					if strings.HasPrefix(operation, "head") || operation == "repository challenge" {
						wantMethod = http.MethodHead
					}

					if req.RequestURI != wantURI || req.Method != wantMethod {
						t.Errorf("request = %s %q, want %s %q", req.Method, req.RequestURI, wantMethod, wantURI)
						w.WriteHeader(http.StatusBadRequest)

						return
					}

					if bounded && req.Header.Get("Range") != "bytes=0-3" {
						t.Errorf("Range = %q", req.Header.Get("Range"))
					}

					if challenge {
						if req.Header.Get("Authorization") != "" {
							t.Error("probe sent endpoint credentials")
						}

						w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
						w.WriteHeader(http.StatusUnauthorized)

						return
					}

					if fallback && hit == 1 {
						w.WriteHeader(http.StatusNotFound)
						return
					}

					w.Header().Set("Content-Length", "4")

					if bounded {
						w.Header().Set("Content-Range", "bytes 0-3/4")
						w.WriteHeader(http.StatusPartialContent)
					}

					if req.Method != http.MethodHead {
						_, _ = io.WriteString(w, "data")
					}
				}))
				defer srv.Close()

				c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL + endpoint.suffix})
				r := c.registries["reg"]

				r.hc = srv.Client()
				if challenge {
					r.base.User = url.UserPassword("endpoint", "password")
				}

				base := *r.base
				ctx := context.Background()

				if challenge {
					var (
						got      string
						required bool
						err      error
					)
					if operation == "root challenge" {
						got, required, err = c.AuthenticationChallenge(ctx, "reg")
					} else {
						got, required, err = c.RepositoryAuthenticationChallenge(ctx, ref)
					}

					if err != nil || !required || got != `Basic realm="registry"` {
						t.Fatalf("challenge=%q required=%v error=%v", got, required, err)
					}
				} else {
					var (
						body io.ReadCloser
						size int64
						err  error
					)

					switch {
					case bounded:
						body, size, _, err = c.PullRange(ctx, ref, 4)
					case strings.HasPrefix(operation, "head"):
						size, _, err = c.Head(ctx, ref)
					default:
						body, size, err = c.Pull(ctx, ref)
					}

					if err != nil {
						t.Fatal(err)
					}

					if body != nil {
						got, readErr := io.ReadAll(body)
						_ = body.Close()

						if readErr != nil || string(got) != "data" {
							t.Fatalf("body=%q error=%v", got, readErr)
						}
					}

					if size != 4 {
						t.Fatalf("size=%d, want 4", size)
					}
				}

				wantHits := int32(1)
				if fallback {
					wantHits = 2
				}

				if hits.Load() != wantHits || *r.base != base {
					t.Fatalf("hits=%d, want %d; base changed=%v", hits.Load(), wantHits, *r.base != base)
				}
			})
		}
	}
}
