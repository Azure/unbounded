// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/containerd/containerd/v2/core/remotes/docker"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/mirror"
)

type staticAuthenticationOrigin struct {
	challenge string
}

func (o staticAuthenticationOrigin) AuthenticationChallenge(context.Context, string) (string, bool, error) {
	return o.challenge, true, nil
}

func (staticAuthenticationOrigin) Pull(context.Context, ifaces.OriginRef) (io.ReadCloser, int64, error) {
	return nil, 0, errors.New("origin must not be called on a local cache hit")
}

func (staticAuthenticationOrigin) Head(context.Context, ifaces.OriginRef) (int64, string, error) {
	return 0, "", errors.New("origin must not be called on a local cache hit")
}

func authenticatedMirror(t *testing.T, challenge string, body []byte) (*httptest.Server, digest.Digest) {
	t.Helper()

	d := digestOf(body)
	cache := fakes.NewCache()
	cache.Put(d, body)

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{
		{Name: "private.example", Endpoint: "https://private.example"},
	}}
	server := httptest.NewServer(mirror.New(cfg, cache, staticAuthenticationOrigin{challenge: challenge}).Handler())
	t.Cleanup(server.Close)

	return server, d
}

func performContainerdAuthenticationHandshake(
	t *testing.T,
	client *http.Client,
	authorizer docker.Authorizer,
	server *httptest.Server,
	d digest.Digest,
	wantChallenge string,
) string {
	t.Helper()

	ctx := docker.ContextWithAppendPullRepositoryScope(context.Background(), "repo")
	requestURL := server.URL + "/v2/repo/blobs/" + d.String() + "?ns=private.example"

	first, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := authorizer.Authorize(ctx, first); err != nil {
		t.Fatalf("Authorize first request: %v", err)
	}

	if got := first.Header.Get("Authorization"); got != "" {
		t.Fatalf("first request Authorization = %q, want empty", got)
	}

	firstResponse, err := client.Do(first)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}

	if firstResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("first status = %d, want 401", firstResponse.StatusCode)
	}

	if got := firstResponse.Header.Get("WWW-Authenticate"); got != wantChallenge {
		t.Fatalf("WWW-Authenticate = %q, want %q", got, wantChallenge)
	}

	if err := authorizer.AddResponses(ctx, []*http.Response{firstResponse}); err != nil {
		t.Fatalf("AddResponses: %v", err)
	}

	_ = firstResponse.Body.Close() //nolint:errcheck // best-effort close

	retry, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := authorizer.Authorize(ctx, retry); err != nil {
		t.Fatalf("Authorize retry: %v", err)
	}

	authorization := retry.Header.Get("Authorization")
	if authorization == "" {
		t.Fatal("retry Authorization is empty")
	}

	retryResponse, err := client.Do(retry)
	if err != nil {
		t.Fatalf("retry request: %v", err)
	}
	defer retryResponse.Body.Close()

	if retryResponse.StatusCode != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", retryResponse.StatusCode)
	}

	return authorization
}

func TestMirror_ContainerdBearerChallengeHandshake(t *testing.T) {
	var tokenRequests int

	tokenServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenRequests++

		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}

		if r.Method != http.MethodPost {
			t.Errorf("token method = %s, want POST", r.Method)
		}

		if r.Form.Get("username") != "requester" || r.Form.Get("password") != "secret" {
			t.Errorf("token credentials = %q/%q", r.Form.Get("username"), r.Form.Get("password"))
		}

		if got := r.Form.Get("scope"); got != "repository:repo:pull" {
			t.Errorf("token scope = %q, want repository:repo:pull", got)
		}

		_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // best-effort response
			"access_token": "scoped-token",
			"expires_in":   300,
		})
	}))
	defer tokenServer.Close()

	challenge := `Bearer realm="` + tokenServer.URL + `",service="private.example"`
	server, d := authenticatedMirror(t, challenge, []byte("bearer-protected"))
	client := tokenServer.Client()
	authorizer := docker.NewDockerAuthorizer(
		docker.WithAuthClient(client),
		docker.WithAuthCreds(func(string) (string, string, error) {
			return "requester", "secret", nil
		}),
	)

	if got := performContainerdAuthenticationHandshake(t, client, authorizer, server, d, challenge); got != "Bearer scoped-token" {
		t.Fatalf("retry Authorization = %q, want Bearer scoped-token", got)
	}

	if tokenRequests != 1 {
		t.Fatalf("token requests = %d, want 1", tokenRequests)
	}
}

func TestMirror_ContainerdBasicChallengeHandshake(t *testing.T) {
	challenge := `Basic realm="private"`
	server, d := authenticatedMirror(t, challenge, []byte("basic-protected"))
	client := http.DefaultClient
	authorizer := docker.NewDockerAuthorizer(
		docker.WithAuthClient(client),
		docker.WithAuthCreds(func(string) (string, string, error) {
			return "requester", "secret", nil
		}),
	)

	if got := performContainerdAuthenticationHandshake(t, client, authorizer, server, d, challenge); got != "Basic cmVxdWVzdGVyOnNlY3JldA==" {
		t.Fatalf("retry Authorization = %q, want delegated Basic credential", got)
	}
}
