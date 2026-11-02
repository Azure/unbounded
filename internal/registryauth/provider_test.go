// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/stretchr/testify/require"
)

type tokenCredential func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error)

func (f tokenCredential) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return f(ctx, opts)
}

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func testProvider(t *testing.T, handler func(*http.Request) (int, string)) *Provider {
	t.Helper()

	return &Provider{Credential: tokenCredential(func(_ context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
		require.Equal(t, []string{"https://containerregistry.azure.net/.default"}, opts.Scopes)
		return azcore.AccessToken{Token: "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"tid":"tenant"}`)) + ".signature", ExpiresOn: time.Now().Add(time.Hour)}, nil
	}), HTTP: &http.Client{Transport: transport(func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "https", req.URL.Scheme)
		require.Equal(t, testRegistry, req.URL.Host)
		status, body := handler(req)

		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}
}

func TestCredentialProviderProtocolAndScopedCredential(t *testing.T) {
	p := testProvider(t, func(req *http.Request) (int, string) {
		require.Equal(t, "/oauth2/exchange", req.URL.Path)
		require.NoError(t, req.ParseForm())
		require.Equal(t, "access_token", req.Form.Get("grant_type"))
		require.Equal(t, testRegistry, req.Form.Get("service"))
		require.Equal(t, "tenant", req.Form.Get("tenant"))
		require.NotEmpty(t, req.Form.Get("access_token"))

		return 200, `{"refresh_token":"private-refresh-token"}`
	})
	request := `{"kind":"CredentialProviderRequest","apiVersion":"credentialprovider.kubelet.k8s.io/v1","image":"example.azurecr.io/private/image:v1"}`

	var output bytes.Buffer
	require.NoError(t, p.Run(t.Context(), strings.NewReader(request), &output, []string{testRegistry}))

	var response ProviderResponse
	require.NoError(t, json.Unmarshal(output.Bytes(), &response))
	require.Equal(t, "CredentialProviderResponse", response.Kind)
	require.Equal(t, providerVersion, response.APIVersion)
	require.Equal(t, "Registry", response.CacheKeyType)
	require.Equal(t, "5m", response.CacheDuration)
	require.Equal(t, map[string]Credential{testRegistry: {Username: tokenUsername, Password: "private-refresh-token"}}, response.Auth)
}

func TestProviderRejectsMalformedRequestsAndNeverContactsOtherRegistries(t *testing.T) {
	p := testProvider(t, func(*http.Request) (int, string) { t.Fatal("unexpected token request"); return 0, "" })

	for _, input := range []string{"{", `{}`, `{"kind":"CredentialProviderRequest","apiVersion":"v2"}`, strings.Repeat("a", maxResponseBytes+1)} {
		var output bytes.Buffer
		require.Error(t, p.Run(t.Context(), strings.NewReader(input), &output, []string{testRegistry}))
		require.Empty(t, output.String())
	}

	for _, image := range []string{"docker.io/library/busybox:latest", "other.azurecr.io/image:v1", "example.azurecr.io.evil.test/image:v1"} {
		input, err := json.Marshal(map[string]string{"kind": "CredentialProviderRequest", "apiVersion": providerVersion, "image": image})
		require.NoError(t, err)

		var output bytes.Buffer
		require.NoError(t, p.Run(t.Context(), bytes.NewReader(input), &output, []string{testRegistry}))

		var response ProviderResponse
		require.NoError(t, json.Unmarshal(output.Bytes(), &response))
		require.Empty(t, response.Auth)
	}
}

func TestExchangeFailuresDoNotLeakTokensOrBodies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", 401, `private-token-sensitive-body`},
		{"invalid JSON", 200, `private-token-sensitive-body`},
		{"missing token", 200, `{}`},
		{"oversized", 200, strings.Repeat("private-token", maxResponseBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testProvider(t, func(*http.Request) (int, string) { return tc.status, tc.body })
			_, err := p.RefreshToken(t.Context(), testRegistry)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "private-token")
		})
	}

	p := testProvider(t, func(*http.Request) (int, string) { t.Fatal("unexpected request"); return 0, "" })
	p.Credential = tokenCredential(func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
		return azcore.AccessToken{}, errors.New("private-token")
	})
	_, err := p.RefreshToken(t.Context(), testRegistry)
	require.ErrorContains(t, err, "federated token")
	require.NotContains(t, err.Error(), "private-token")
	_, err = p.RefreshToken(t.Context(), "example.azurecr.io.evil.test")
	require.ErrorContains(t, err, "non-ACR")
}

func TestExchangeRefusesRedirects(t *testing.T) {
	calls := 0
	p := testProvider(t, func(*http.Request) (int, string) { return 0, "" })
	p.HTTP.Transport = transport(func(req *http.Request) (*http.Response, error) {
		calls++

		require.Equal(t, testRegistry, req.URL.Host)

		return &http.Response{StatusCode: 307, Header: http.Header{"Location": []string{"https://evil.test/oauth2/exchange"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})
	_, err := p.RefreshToken(t.Context(), testRegistry)
	require.ErrorContains(t, err, "HTTP 307")
	require.Equal(t, 1, calls)
}

func TestPrivateImageProbeAndAuthorizationFailures(t *testing.T) {
	for _, status := range []int{200, 401, 403, 404} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			p := testProvider(t, func(req *http.Request) (int, string) {
				calls++

				switch req.URL.Path {
				case "/oauth2/exchange":
					return 200, `{"refresh_token":"private-refresh"}`
				case "/oauth2/token":
					require.NoError(t, req.ParseForm())
					require.Equal(t, "repository:private/image:pull", req.Form.Get("scope"))
					require.Equal(t, "private-refresh", req.Form.Get("refresh_token"))

					return 200, `{"access_token":"private-access"}`
				case "/v2/private/image/manifests/v1":
					require.Equal(t, http.MethodHead, req.Method)
					require.Equal(t, "Bearer private-access", req.Header.Get("Authorization"))

					return status, ""
				default:
					t.Fatalf("unexpected path %s", req.URL.Path)
					return 500, ""
				}
			})

			err := p.Probe(t.Context(), testRegistry+"/private/image:v1")
			if status == 200 {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "AcrPull")
				require.NotContains(t, err.Error(), "private-access")
			}

			require.Equal(t, 3, calls)
		})
	}
}
