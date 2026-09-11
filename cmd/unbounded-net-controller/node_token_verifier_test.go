// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/Azure/unbounded/internal/net/authn"
)

func TestInitializeNodeTokenVerifier(t *testing.T) {
	for _, tc := range []struct {
		name         string
		issuer       string
		audience     string
		payload      string
		factoryErr   error
		wantIssuer   string
		wantAudience string
		wantFallback bool
		wantErr      bool
	}{
		{name: "explicit issuer skips token file", issuer: "https://explicit.example", audience: "explicit-audience", wantIssuer: "https://explicit.example", wantAudience: "explicit-audience"},
		{name: "discover issuer and audience", payload: `{"iss":"https://cluster.example","aud":["api-audience"]}`, wantIssuer: "https://cluster.example", wantAudience: "api-audience"},
		{name: "explicit audience overrides discovery", audience: "custom", payload: `{"iss":"https://cluster.example","aud":["api-audience"]}`, wantIssuer: "https://cluster.example", wantAudience: "custom"},
		{name: "missing token falls back", wantFallback: true},
		{name: "malformed token falls back", payload: `{`, wantFallback: true},
		{name: "missing issuer falls back", payload: `{"aud":["api-audience"]}`, wantFallback: true},
		{name: "legacy token falls back", payload: `{"iss":"kubernetes/serviceaccount"}`, wantFallback: true},
		{name: "ambiguous audience falls back", payload: `{"iss":"https://cluster.example","aud":["api","other"]}`, wantFallback: true},
		{name: "discovery or JWKS failure falls back", payload: `{"iss":"https://cluster.example","aud":["api-audience"]}`, wantIssuer: "https://cluster.example", wantAudience: "api-audience", factoryErr: errors.New("discovery unavailable"), wantFallback: true},
		{name: "explicit failure does not downgrade", issuer: "https://explicit.example", wantIssuer: "https://explicit.example", factoryErr: errors.New("discovery unavailable"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokenPath := filepath.Join(t.TempDir(), "token")

			if tc.payload != "" {
				token := "header." + base64.RawURLEncoding.EncodeToString([]byte(tc.payload)) + ".signature\n"
				if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			calls := 0
			expectedVerifier := &fakeServiceAccountTokenVerifier{}
			factory := func(_ context.Context, issuer, audience string) (serviceAccountTokenVerifier, error) {
				calls++

				if issuer != tc.wantIssuer || audience != tc.wantAudience {
					t.Fatalf("factory got issuer=%q audience=%q, want issuer=%q audience=%q", issuer, audience, tc.wantIssuer, tc.wantAudience)
				}

				return expectedVerifier, tc.factoryErr
			}
			client := k8sfake.NewClientset()
			caches := newNodeAuthInformers(t.Context(), client, "unbounded-system", 0)

			verifier, err := initializeNodeTokenVerifier(t.Context(), client, tc.issuer, tc.audience, tokenPath, caches.wrapOIDCFactory(factory))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error = %v", err, tc.wantErr)
			}

			if tc.wantFallback {
				if _, ok := verifier.(*authn.KubernetesTokenReviewVerifier); !ok {
					t.Fatalf("expected TokenReview fallback, got %T", verifier)
				}
			} else if !tc.wantErr {
				if _, ok := verifier.(*authn.PodBoundTokenVerifier); !ok {
					t.Fatalf("expected cache-validated OIDC verifier, got %T", verifier)
				}

				if identity, err := verifier.Verify(t.Context(), "token"); err == nil || identity != nil {
					t.Fatalf("unsynced caches bypassed for initialized OIDC verifier: %+v, %v", identity, err)
				}
			}

			wantCalls := 0
			if tc.wantIssuer != "" {
				wantCalls = 1
			}

			if calls != wantCalls {
				t.Fatalf("OIDC initialization calls = %d, want %d", calls, wantCalls)
			}

			if len(client.Actions()) != 0 {
				t.Fatal("initializing the verifier should not submit a TokenReview")
			}
		})
	}
}
