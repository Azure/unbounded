// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type oidcTestTransport func(*http.Request) (*http.Response, error)

func (f oidcTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestOIDCRejectsUnsafeURLsBeforeRequests(t *testing.T) {
	for _, endpoint := range []string{
		"", "http://issuer.example", "/relative", "//issuer.example",
		"https:issuer.example", "https:///missing-host", "https://:443",
		"https://%", "https://user:password@issuer.example",
		"https://issuer.example/#fragment", "https://issuer.example/#",
	} {
		for _, field := range []string{"issuer", "jwks_uri"} {
			t.Run(fmt.Sprintf("%s/%s", field, endpoint), func(t *testing.T) {
				var requests int

				client := &http.Client{
					Transport: oidcTestTransport(func(_ *http.Request) (*http.Response, error) {
						requests++

						discovery, err := json.Marshal(oidcDiscoveryDocument{Issuer: "https://issuer.example", JWKSURL: endpoint})
						if err != nil {
							return nil, err
						}

						return &http.Response{
							StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(discovery))),
							Header: make(http.Header),
						}, nil
					}),
				}
				issuer := endpoint
				wantRequests := 0

				if field == "jwks_uri" {
					issuer = "https://issuer.example"
					wantRequests = 1
				}

				if _, err := newKubernetesOIDCVerifier(t.Context(), issuer, "api", client, time.Now); err == nil {
					t.Fatal("expected unsafe URL rejection")
				}

				if requests != wantRequests {
					t.Fatalf("requests = %d, want %d", requests, wantRequests)
				}
			})
		}
	}
}

func TestOIDCURLQueryConstraints(t *testing.T) {
	for _, suffix := range []string{"?version=1", "?"} {
		endpoint := "https://issuer.example/path" + suffix
		if err := validateOIDCHTTPSURL(endpoint, false); err == nil {
			t.Fatalf("issuer with query accepted: %s", endpoint)
		}

		if err := validateOIDCHTTPSURL(endpoint, true); err != nil {
			t.Fatalf("JWKS with query rejected: %v", err)
		}
	}

	for _, endpoint := range []string{"https://issuer.example", "https://issuer.example:8443/cluster/"} {
		if err := validateOIDCHTTPSURL(endpoint, false); err != nil {
			t.Fatalf("valid issuer rejected: %v", err)
		}
	}
}

func TestOIDCRejectsHTTPSDowngradeRedirect(t *testing.T) {
	for _, redirectPath := range []string{"/.well-known/openid-configuration", "/keys"} {
		t.Run(redirectPath, func(t *testing.T) {
			var insecureRequests atomic.Int64

			insecure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				insecureRequests.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer insecure.Close()

			var secureRequests atomic.Int64

			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secureRequests.Add(1)

				switch r.URL.Path {
				case redirectPath:
					http.Redirect(w, r, "/redirect", http.StatusFound)
				case "/redirect":
					http.Redirect(w, r, insecure.URL, http.StatusFound)
				default:
					_ = json.NewEncoder(w).Encode(oidcDiscoveryDocument{
						Issuer: "https://" + r.Host, JWKSURL: "https://" + r.Host + "/keys",
					})
				}
			}))
			defer server.Close()

			_, err := newKubernetesOIDCVerifier(t.Context(), server.URL, "api", server.Client(), time.Now)
			if err == nil || !strings.Contains(err.Error(), "absolute HTTPS URL") {
				t.Fatalf("downgrade redirect error = %v", err)
			}

			if got := insecureRequests.Load(); got != 0 {
				t.Fatalf("made %d insecure requests", got)
			}

			wantSecure := int64(2)
			if redirectPath == "/keys" {
				wantSecure++
			}

			if got := secureRequests.Load(); got != wantSecure {
				t.Fatalf("secure requests = %d, want %d", got, wantSecure)
			}
		})
	}
}

func TestOIDCRejectsUntrustedTLS(t *testing.T) {
	var requests atomic.Int64

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if _, err := NewKubernetesOIDCVerifier(t.Context(), server.URL, "api"); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("untrusted TLS error = %v", err)
	}

	if got := requests.Load(); got != 0 {
		t.Fatalf("made %d requests to untrusted TLS server", got)
	}
}
