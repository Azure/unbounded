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
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

func TestRepositoryAuthenticationChallengeFallback(t *testing.T) {
	const bearer = `Bearer realm="https://auth.example/token",scope="repository:private/manifests/repo:pull",service="reg"`

	for _, kind := range []ifaces.OriginRefKind{ifaces.KindBlob, ifaces.KindConfig} {
		for _, tc := range []struct {
			name   string
			status int
			header string
			want   string
		}{
			{"bearer", 401, bearer, bearer},
			{"basic", 401, `Basic realm="private"`, `Basic realm="private"`},
			{"public", 200, bearer, ""},
			{"missing", 401, "", ""},
			{"unsupported", 401, `Digest realm="private"`, ""},
			{"insecure realm", 401, `Bearer realm="http://auth.example/token"`, ""},
			{"realm userinfo", 401, `Bearer realm="https://user:password@auth.example/token"`, ""},
			{"forbidden", 403, bearer, ""},
			{"not found", 404, bearer, ""},
			{"method unsupported", 405, bearer, ""},
			{"unavailable", 503, bearer, ""},
			{"redirect", 307, bearer, ""},
		} {
			t.Run(kind.MetricLabel()+"/"+tc.name, func(t *testing.T) {
				var hits, redirects atomic.Int32

				target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirects.Add(1) }))
				defer target.Close()

				d := digestOf([]byte("resource"))

				srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hit := hits.Add(1)

					resource, accept := "blobs", ""
					if hit == 2 {
						resource, accept = "manifests", manifestAccept
					}

					if hit > 2 || r.Method != http.MethodHead || r.URL.Path != "/prefix/v2/private/manifests/repo/"+resource+"/"+d.String() || r.URL.RawQuery != "" {
						t.Errorf("unexpected probe %d: %s %s", hit, r.Method, r.URL)
					}

					if r.Header.Get("Accept") != accept || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Range") != "" {
						t.Errorf("unexpected probe headers: %v", r.Header)
					}

					if hit == 1 {
						w.Header().Set("WWW-Authenticate", `Basic realm="ignore-404"`)
						w.WriteHeader(http.StatusNotFound)

						return
					}

					w.Header().Set("WWW-Authenticate", tc.header)
					w.Header().Set("Location", target.URL)
					w.WriteHeader(tc.status)
				}))
				defer srv.Close()

				c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL + "/prefix?ignored=1#fragment"})
				r := c.registries["reg"]
				r.hc = srv.Client()
				r.base.User = url.UserPassword("endpoint", "password")
				r.username, r.password = "shared", "password"

				jar, err := cookiejar.New(nil)
				if err != nil {
					t.Fatal(err)
				}

				jar.SetCookies(r.base, []*http.Cookie{{Name: "session", Value: "secret"}})
				r.hc.Jar = jar
				r.setToken("cached-token", time.Hour)
				r.rememberAuthenticationChallenge(`Basic realm="other-repository"`)
				cachedChallenge := r.challenge

				ctx := registryauth.WithAuthorization(context.Background(), "Bearer expired")
				got, required, err := c.RepositoryAuthenticationChallenge(ctx, ifaces.OriginRef{
					Registry: "reg", Repository: "private/manifests/repo", Digest: d, Kind: kind, Offset: 4,
				})

				wantErr := tc.want == "" && tc.status != http.StatusOK
				if (err != nil) != wantErr || got != tc.want || required != (tc.want != "") {
					t.Fatalf("challenge=%q required=%v err=%v", got, required, err)
				}

				if hits.Load() != 2 || redirects.Load() != 0 {
					t.Fatalf("hits=%d redirects=%d", hits.Load(), redirects.Load())
				}

				if r.challenge != cachedChallenge || r.cachedToken() != "cached-token" {
					t.Fatal("probe changed registry-wide auth cache")
				}
			})
		}
	}
}

func TestRepositoryAuthenticationChallengeAfterRangeFallback(t *testing.T) {
	const challenge = `Basic realm="private"`

	ref := ifaces.OriginRef{Registry: "reg", Repository: "repo", Digest: digestOf([]byte("manifest")), Kind: ifaces.KindBlob, Offset: 4}

	var gets, heads atomic.Int32

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			gets.Add(1)

			if r.Header.Get("Range") != "bytes=4-7" || r.Header.Get("Authorization") != "Bearer expired" {
				t.Error("pull lost range or delegated credentials")
			}
		case http.MethodHead:
			heads.Add(1)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}

		switch r.URL.Path {
		case "/v2/repo/blobs/" + ref.Digest.String():
			w.WriteHeader(http.StatusNotFound)
		case "/v2/repo/manifests/" + ref.Digest.String():
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	puller := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
	puller.registries["reg"].hc = srv.Client()
	ctx := registryauth.WithAuthorization(context.Background(), "Bearer expired")
	_, _, _, err := puller.PullRange(ctx, ref, 4)

	var oe *ifaces.OriginError
	if !errors.As(err, &oe) || oe.StatusCode != http.StatusUnauthorized || oe.Ref.Kind != ifaces.KindManifest {
		t.Fatalf("expected manifest rejection: %v", err)
	}

	requester := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})
	requester.registries["reg"].hc = srv.Client()

	got, required, err := requester.RepositoryAuthenticationChallenge(ctx, ref)
	if err != nil || !required || got != challenge {
		t.Fatalf("challenge=%q required=%v err=%v", got, required, err)
	}

	if gets.Load() != 2 || heads.Load() != 2 {
		t.Fatalf("gets=%d heads=%d", gets.Load(), heads.Load())
	}
}

func TestRepositoryAuthenticationChallengeFallbackLifecycle(t *testing.T) {
	for _, mode := range []string{"success", "transport", "canceled", "deadline", "internal bound"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if mode == "deadline" {
				var stop context.CancelFunc

				ctx, stop = context.WithTimeout(ctx, 50*time.Millisecond)
				defer stop()
			}

			c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: "https://registry.example"})
			blob, manifest := &unreadRangeBody{}, &unreadRangeBody{}
			transportErr := errors.New("manifest transport failed")

			var (
				calls         int
				firstDeadline time.Time
			)

			c.registries["reg"].hc.Transport = rangeRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++

				deadline, ok := req.Context().Deadline()
				if !ok || time.Until(deadline) > 2*time.Second {
					t.Fatal("probe has no bounded deadline")
				}

				if calls == 1 {
					firstDeadline = deadline
					return &http.Response{StatusCode: http.StatusNotFound, Body: blob, Request: req}, nil
				}

				if calls != 2 || !deadline.Equal(firstDeadline) || blob.closes.Load() != 1 {
					t.Fatal("fallback restarted deadline, retried, or left blob body open")
				}

				switch mode {
				case "transport":
					return nil, transportErr
				case "canceled":
					cancel()
					return nil, req.Context().Err()
				case "deadline", "internal bound":
					<-req.Context().Done()
					return nil, req.Context().Err()
				default:
					return &http.Response{
						StatusCode: http.StatusUnauthorized, Body: manifest, Request: req,
						Header: http.Header{"Www-Authenticate": {`Basic realm="private"`}},
					}, nil
				}
			})

			start := time.Now()
			got, required, err := c.RepositoryAuthenticationChallenge(ctx, ifaces.OriginRef{
				Registry: "reg", Repository: "repo", Digest: digestOf(nil), Kind: ifaces.KindBlob,
			})

			wantErr := map[string]error{
				"transport": transportErr, "canceled": context.Canceled,
				"deadline": context.DeadlineExceeded, "internal bound": context.DeadlineExceeded,
			}[mode]
			if !errors.Is(err, wantErr) || required != (mode == "success") || (got != "") != required {
				t.Fatalf("challenge=%q required=%v err=%v", got, required, err)
			}

			if time.Since(start) > 3*time.Second || calls != 2 || blob.reads.Load() != 0 || blob.closes.Load() != 1 || manifest.reads.Load() != 0 {
				t.Fatal("unbounded fallback, extra request, unclosed blob body, or content read")
			}

			if mode == "success" && manifest.closes.Load() != 1 {
				t.Fatal("manifest response body not closed")
			}
		})
	}
}
