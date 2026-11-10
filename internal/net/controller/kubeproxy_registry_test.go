// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
)

func TestKubeProxyTagSelection(t *testing.T) {
	for _, tt := range []struct {
		name string
		tags []string
		want string
	}{
		{name: "highest maintenance revision", tags: []string{"v1.36.2-1", "v1.36.2", "v1.36.2-2"}, want: "v1.36.2-2"},
		{name: "numeric rather than alphabetical revisions", tags: []string{"v1.36.2-2", "v1.36.2-10"}, want: "v1.36.2-10"},
		{name: "exact release without maintenance", tags: []string{"v1.36.2"}, want: "v1.36.2"},
		{name: "maintenance without an unsuffixed tag", tags: []string{"v1.36.2-1", "v1.36.2-2"}, want: "v1.36.2-2"},
		{name: "other patch and minor versions are ignored", tags: []string{"v1.36.20", "v1.37.2-9", "v1.36.3-9", "v1.36.2"}, want: "v1.36.2"},
		{name: "prereleases and architecture tags are ignored", tags: []string{"v1.36.2-rc.10", "v1.36.2-beta.2", "v1.36.2-2-amd64", "v1.36.2_linux-amd64", "v1.36.2"}, want: "v1.36.2"},
		{name: "no leading v is preserved", tags: []string{"1.36.2"}, want: "1.36.2"},
		{name: "maintenance without a leading v", tags: []string{"1.36.2", "1.36.2-2", "1.36.2-10"}, want: "1.36.2-10"},
		{name: "equivalent tags prefer the conventional leading v", tags: []string{"1.36.2-2", "v1.36.2-2", "v1.36.2-2"}, want: "v1.36.2-2"},
		{name: "a zero maintenance revision is still suffixed", tags: []string{"v1.36.2", "v1.36.2-0"}, want: "v1.36.2-0"},
		{name: "invalid numeric suffixes are ignored", tags: []string{"v1.36.2--1", "v1.36.2-+1", "v1.36.2-1.2", "v1.36.2-18446744073709551616", "v1.36.2"}, want: "v1.36.2"},
		{name: "latest and unrelated releases cannot be selected", tags: []string{"latest", "v1.36.20", "v1.36.3", "v1.36.2-rc.1"}},
		{name: "empty tag list"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reversed := slices.Clone(tt.tags)
			slices.Reverse(reversed)

			for _, tags := range [][]string{tt.tags, reversed} {
				selection := kubeProxyTagSelection{version: "1.36.2"}
				for _, tag := range tags {
					selection.consider(tag)
				}

				if selection.tag != tt.want {
					t.Fatalf("selected %q from %v, want %q", selection.tag, tags, tt.want)
				}
			}
		})
	}
}

func TestKubeProxyRegistryReference(t *testing.T) {
	for _, tt := range []struct {
		name string
		want string
	}{
		{name: "kube-proxy", want: "docker.io/library/kube-proxy"},
		{name: "team/kube-proxy", want: "docker.io/team/kube-proxy"},
		{name: "docker.io/kube-proxy", want: "docker.io/library/kube-proxy"},
		{name: "DOCKER.IO/kube-proxy", want: "docker.io/library/kube-proxy"},
		{name: "index.docker.io/kube-proxy", want: "docker.io/library/kube-proxy"},
		{name: "registry-1.docker.io/team/kube-proxy", want: "docker.io/team/kube-proxy"},
		{name: "registry.k8s.io/kube-proxy", want: "registry.k8s.io/kube-proxy"},
		{name: "myreg.example/nested/team/kube-proxy", want: "myreg.example/nested/team/kube-proxy"},
		{name: "localhost:5000/kube-proxy", want: "localhost:5000/kube-proxy"},
		{name: "[::1]:5000/kube-proxy", want: "[::1]:5000/kube-proxy"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := kubeProxyRegistryReference(tt.name); got != tt.want {
				t.Fatalf("normalized repository = %q, want %q", got, tt.want)
			}

			repo, err := newKubeProxyTagRepository(tt.name)
			if err != nil {
				t.Fatal(err)
			}

			if repo.Reference.String() != tt.want || repo.TagListMaxPages != 100 {
				t.Fatalf("repository = %q, page limit = %d", repo.Reference.String(), repo.TagListMaxPages)
			}
		})
	}
}

func TestKubeProxyRegistryTagsPagination(t *testing.T) {
	var requests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)

		if r.URL.Path != "/v2/team/kube-proxy/tags/list" || r.URL.Query().Get("n") != "1000" {
			t.Errorf("unexpected tag request: %s", r.URL)
		}

		if r.URL.Query().Get("last") == "" {
			w.Header().Set("Link", `<?last=v1.36.2-2&n=1000>; rel="next"`)
			writeKubeProxyRegistryTags(t, w, []string{"v1.36.2", "v1.36.2-2"})

			return
		}

		writeKubeProxyRegistryTags(t, w, []string{"v1.36.2-10", "v1.36.20"})
	}))
	defer server.Close()

	repo := kubeProxyRegistryForTest(t, server, "team/kube-proxy")

	got, err := kubeProxyTagFromRepository(t.Context(), repo, "v1.36.2")
	if err != nil || got != "v1.36.2-10" || requests.Load() != 2 {
		t.Fatalf("tag = %q, error = %v, requests = %d", got, err, requests.Load())
	}
}

func TestKubeProxyRegistryTagsRedirectedPagination(t *testing.T) {
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/internal/images/kube-proxy/tags/list" {
			t.Errorf("pagination used the wrong path: %s", r.URL)
		}

		if r.URL.Query().Get("last") == "" {
			w.Header().Set("Link", `<?last=v1.36.2-1>; rel="next"`)
			writeKubeProxyRegistryTags(t, w, []string{"v1.36.2-1"})

			return
		}

		writeKubeProxyRegistryTags(t, w, []string{"v1.36.2-2"})
	}))
	defer mirror.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, mirror.URL+"/v2/internal/images/kube-proxy/tags/list", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	got, err := kubeProxyTagFromRepository(t.Context(), kubeProxyRegistryForTest(t, source, "kube-proxy"), "v1.36.2")
	if err != nil || got != "v1.36.2-2" {
		t.Fatalf("tag = %q, error = %v", got, err)
	}
}

func TestKubeProxyRegistryAnonymousAuthentication(t *testing.T) {
	for _, tokenField := range []string{"token", "access_token"} {
		t.Run(tokenField, func(t *testing.T) {
			var (
				serverURL     string
				tokenRequests atomic.Int32
			)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					tokenRequests.Add(1)

					if r.Header.Get("Authorization") != "" || r.URL.Query().Get("scope") != "repository:team/kube-proxy:pull" {
						t.Errorf("unexpected token request: %s", r.URL)
					}

					json.NewEncoder(w).Encode(map[string]string{tokenField: "public-token"})

					return
				}

				if r.Header.Get("Authorization") != "Bearer public-token" {
					w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test-registry",scope="repository:team/kube-proxy:pull"`, serverURL))
					w.WriteHeader(http.StatusUnauthorized)

					return
				}

				writeKubeProxyRegistryTags(t, w, []string{"v1.36.2", "v1.36.2-2"})
			}))
			defer server.Close()

			serverURL = server.URL

			got, err := kubeProxyTagFromRepository(t.Context(), kubeProxyRegistryForTest(t, server, "team/kube-proxy"), "v1.36.2")
			if err != nil || got != "v1.36.2-2" || tokenRequests.Load() != 1 {
				t.Fatalf("tag = %q, error = %v, token requests = %d", got, err, tokenRequests.Load())
			}
		})
	}
}

func TestKubeProxyRegistryAuthenticationAfterRedirect(t *testing.T) {
	var mirrorURL string

	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			json.NewEncoder(w).Encode(map[string]string{"token": "mirror-token"})
			return
		}

		if r.Header.Get("Authorization") != "Bearer mirror-token" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",scope="repository:internal/kube-proxy:pull"`, mirrorURL))
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		writeKubeProxyRegistryTags(t, w, []string{"v1.36.2-2"})
	}))
	defer mirror.Close()

	mirrorURL = mirror.URL

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("mirror credentials were forwarded to the origin registry")
		}

		http.Redirect(w, r, mirror.URL+"/v2/internal/kube-proxy/tags/list", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	got, err := kubeProxyTagFromRepository(t.Context(), kubeProxyRegistryForTest(t, source, "kube-proxy"), "v1.36.2")
	if err != nil || got != "v1.36.2-2" {
		t.Fatalf("tag = %q, error = %v", got, err)
	}
}

func TestKubeProxyRegistryDoesNotForwardAuthorizationAcrossRedirects(t *testing.T) {
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("authorization crossed registry origins")
		}

		if r.URL.Path == "/first-hop" {
			http.Redirect(w, r, "/second-hop", http.StatusTemporaryRedirect)
			return
		}

		writeKubeProxyRegistryTags(t, w, []string{"v1.36.2"})
	}))
	defer mirror.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, mirror.URL+"/first-hop", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, source.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Authorization", "Bearer origin-token")

	resp, err := newKubeProxyRegistryClient(source.Client()).Do(req)
	if err != nil {
		t.Fatal(err)
	}

	resp.Body.Close()
}

func TestKubeProxyRegistryRejectsHTTPSDowngrade(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an HTTPS-to-HTTP redirect was followed")
	}))
	defer plain.Close()

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusTemporaryRedirect)
	}))
	defer secure.Close()

	got, err := kubeProxyTagFromRepository(t.Context(), kubeProxyRegistryForTest(t, secure, "kube-proxy"), "v1.36.2")
	if err == nil || !strings.Contains(err.Error(), "HTTPS-to-HTTP") || got != "" {
		t.Fatalf("tag = %q, error = %v, want a downgrade error and no tag", got, err)
	}
}

func TestKubeProxyRegistryLookupFailures(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		body    string
		link    string
		message string
	}{
		{name: "repository not found", status: http.StatusNotFound, message: "404"},
		{name: "private repository", status: http.StatusForbidden, message: "403"},
		{name: "registry unavailable", status: http.StatusServiceUnavailable, message: "503"},
		{name: "invalid JSON", status: http.StatusOK, body: "invalid", message: "decode"},
		{name: "empty tag list", status: http.StatusOK, body: `{"tags":[]}`, message: "no kube-proxy tag"},
		{name: "no exact or maintenance match", status: http.StatusOK, body: `{"tags":["latest","v1.36.20","v1.36.2-rc.1"]}`, message: "no kube-proxy tag"},
		{name: "cross-origin pagination", status: http.StatusOK, body: `{"tags":["v1.36.2"]}`, link: `<https://other.example/v2/kube-proxy/tags/list>; rel="next"`, message: "same origin"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.link != "" {
					w.Header().Set("Link", tt.link)
				}

				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer server.Close()

			got, err := kubeProxyTagFromRepository(t.Context(), kubeProxyRegistryForTest(t, server, "kube-proxy"), "v1.36.2")
			if err == nil || !strings.Contains(err.Error(), tt.message) || got != "" {
				t.Fatalf("tag = %q, error = %v, want no tag and %q", got, err, tt.message)
			}
		})
	}
}

func TestKubeProxyRegistryDoesNotUsePartialTagLists(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("last") != "" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Link", `<?last=v1.36.2>; rel="next"`)
		writeKubeProxyRegistryTags(t, w, []string{"v1.36.2"})
	}))
	defer server.Close()

	got, err := kubeProxyTagFromRepository(t.Context(), kubeProxyRegistryForTest(t, server, "kube-proxy"), "v1.36.2")
	if err == nil || got != "" {
		t.Fatalf("tag = %q, error = %v, want no tag from an incomplete listing", got, err)
	}
}

func TestKubeProxyRegistryPaginationIsBounded(t *testing.T) {
	var requests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Link", `<?n=1000>; rel="next"`)
		writeKubeProxyRegistryTags(t, w, []string{"v1.36.2"})
	}))
	defer server.Close()

	repo := kubeProxyRegistryForTest(t, server, "kube-proxy")
	repo.TagListMaxPages = 3

	got, err := kubeProxyTagFromRepository(t.Context(), repo, "v1.36.2")
	if !errors.Is(err, errdef.ErrTooManyPages) || got != "" || requests.Load() != 3 {
		t.Fatalf("tag = %q, error = %v, requests = %d", got, err, requests.Load())
	}
}

func TestKubeProxyRegistryLookupHonorsDeadlines(t *testing.T) {
	for _, tt := range []struct {
		name          string
		parentTimeout time.Duration
	}{
		{name: "lookup deadline"},
		{name: "parent deadline", parentTimeout: 5 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				repo, err := newKubeProxyTagRepository("registry.example/kube-proxy")
				if err != nil {
					t.Fatal(err)
				}

				repo.Client = newKubeProxyRegistryClient(&http.Client{Transport: kubeProxyRegistryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					<-req.Context().Done()
					return nil, req.Context().Err()
				})})
				ctx := t.Context()
				wantElapsed := 30 * time.Second

				if tt.parentTimeout != 0 {
					var cancel context.CancelFunc

					ctx, cancel = context.WithTimeout(ctx, tt.parentTimeout)
					defer cancel()

					wantElapsed = tt.parentTimeout
				}

				started := time.Now()

				got, err := kubeProxyTagFromRepository(ctx, repo, "v1.36.2")
				if !errors.Is(err, context.DeadlineExceeded) || got != "" || time.Since(started) != wantElapsed {
					t.Fatalf("tag = %q, error = %v, elapsed = %v, want %v", got, err, time.Since(started), wantElapsed)
				}
			})
		})
	}
}

func TestManagedKubeProxyImageQueriesConfiguredRegistry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/team/kube-proxy/tags/list" {
			t.Errorf("unexpected registry request: %s", r.URL)
		}

		writeKubeProxyRegistryTags(t, w, []string{"latest", "v1.36.2", "v1.36.2-1", "v1.36.2-2", "v1.36.20"})
	}))
	defer server.Close()

	repository := strings.TrimPrefix(server.URL, "http://") + "/team/kube-proxy"

	got, err := ManagedKubeProxyImage(t.Context(), clientsetWithServerVersion("v1.36.2"), repository)
	if err != nil || got != repository+":v1.36.2-2" {
		t.Fatalf("ManagedKubeProxyImage() = %q, %v", got, err)
	}
}

func TestKubeProxyRegistryLookupCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo, err := newKubeProxyTagRepository("registry.example/kube-proxy")
		if err != nil {
			t.Fatal(err)
		}

		repo.Client = newKubeProxyRegistryClient(&http.Client{Transport: kubeProxyRegistryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		timer := time.AfterFunc(5*time.Second, cancel)
		defer timer.Stop()

		started := time.Now()

		got, err := kubeProxyTagFromRepository(ctx, repo, "v1.36.2")
		if !errors.Is(err, context.Canceled) || got != "" || time.Since(started) != 5*time.Second {
			t.Fatalf("tag = %q, error = %v, elapsed = %v", got, err, time.Since(started))
		}
	})
}

func kubeProxyRegistryForTest(t *testing.T, server *httptest.Server, repository string) *remote.Repository {
	t.Helper()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	repo, err := newKubeProxyTagRepository(u.Host + "/" + repository)
	if err != nil {
		t.Fatal(err)
	}

	repo.PlainHTTP = u.Scheme == "http"
	repo.Client = newKubeProxyRegistryClient(server.Client())

	return repo
}

func writeKubeProxyRegistryTags(t *testing.T, w http.ResponseWriter, tags []string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(map[string][]string{"tags": tags}); err != nil {
		t.Error(err)
	}
}

type kubeProxyRegistryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f kubeProxyRegistryRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
