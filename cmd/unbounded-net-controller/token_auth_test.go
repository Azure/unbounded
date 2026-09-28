// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/net/authn"
)

type countingServiceAccountTokenVerifier struct {
	identity *authn.KubernetesServiceAccountIdentity
	calls    int
}

func (v *countingServiceAccountTokenVerifier) Verify(context.Context, string) (*authn.KubernetesServiceAccountIdentity, error) {
	v.calls++
	return v.identity, nil
}

// TestNewTokenAuthenticator tests NewTokenAuthenticator.
func TestNewTokenAuthenticator(t *testing.T) {
	verifier := &countingServiceAccountTokenVerifier{}

	auth := newTokenAuthenticator(verifier, []string{"kube-system:unbounded-net-node"})
	if auth == nil {
		t.Fatalf("expected token authenticator instance")
	}

	if auth.verifier == nil {
		t.Fatalf("expected verifier to be set")
	}

	if !auth.allowedSANames["kube-system:unbounded-net-node"] {
		t.Fatalf("expected allowed service account to be recorded")
	}

	if auth.cacheTTL != 5*time.Minute {
		t.Fatalf("expected default cache TTL of 5m, got %s", auth.cacheTTL)
	}

	auth2 := newTokenAuthenticator(verifier, nil)
	if auth2 == nil {
		t.Fatalf("expected token authenticator instance")
	}

	if len(auth2.allowedSANames) != 0 {
		t.Fatalf("expected empty allowed service account map when list is nil")
	}
}

// TestTokenAuthenticatorAuthenticateLocal verifies the cached local bearer
// authentication path.
func TestTokenAuthenticatorAuthenticateLocal(t *testing.T) {
	requestWithToken := func(token string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		return req
	}

	t.Run("allows assigned service account and caches result", func(t *testing.T) {
		verifier := &countingServiceAccountTokenVerifier{
			identity: &authn.KubernetesServiceAccountIdentity{
				Subject: "system:serviceaccount:kube-system:unbounded-net-node",
			},
		}

		auth := &tokenAuthenticator{
			cache:          make(map[string]*tokenAuthResult),
			cacheTTL:       time.Minute,
			allowedSANames: map[string]bool{"kube-system:unbounded-net-node": true},
			verifier:       verifier,
			configured:     true,
		}

		if !auth.authenticate(requestWithToken("token-allow")) {
			t.Fatalf("expected token to authenticate locally")
		}

		if !auth.authenticate(requestWithToken("token-allow")) {
			t.Fatalf("expected cached token to remain authenticated")
		}

		if verifier.calls != 1 {
			t.Fatalf("expected one verifier call due to caching, got %d", verifier.calls)
		}
	})

	t.Run("denies authenticated but disallowed service account", func(t *testing.T) {
		verifier := &countingServiceAccountTokenVerifier{
			identity: &authn.KubernetesServiceAccountIdentity{
				Subject: "system:serviceaccount:default:other-sa",
			},
		}

		auth := &tokenAuthenticator{
			cache:          make(map[string]*tokenAuthResult),
			cacheTTL:       time.Minute,
			allowedSANames: map[string]bool{"kube-system:unbounded-net-node": true},
			verifier:       verifier,
			configured:     true,
		}

		if auth.authenticate(requestWithToken("token-deny")) {
			t.Fatalf("expected token to be denied when service account is not allowed")
		}
	})
}

// TestServiceAccountIDFromUsername tests ServiceAccountIDFromUsername.
func TestServiceAccountIDFromUsername(t *testing.T) {
	tests := []struct {
		name     string
		username string
		wantID   string
		wantOK   bool
	}{
		{name: "valid", username: "system:serviceaccount:kube-system:node-sa", wantID: "kube-system:node-sa", wantOK: true},
		{name: "missing name", username: "system:serviceaccount:kube-system:", wantOK: false},
		{name: "not service account", username: "system:node:aks-node-1", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotOK := serviceAccountIDFromUsername(tc.username)
			if gotOK != tc.wantOK {
				t.Fatalf("serviceAccountIDFromUsername(%q) ok=%v, want %v", tc.username, gotOK, tc.wantOK)
			}

			if gotID != tc.wantID {
				t.Fatalf("serviceAccountIDFromUsername(%q) id=%q, want %q", tc.username, gotID, tc.wantID)
			}
		})
	}
}
