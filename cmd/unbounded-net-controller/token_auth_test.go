// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestNewTokenAuthenticator tests NewTokenAuthenticator.
func TestNewTokenAuthenticator(t *testing.T) {
	client := k8sfake.NewClientset()

	auth := newTokenAuthenticator(client, []string{"kube-system:unbounded-net-node"})
	if auth == nil {
		t.Fatalf("expected token authenticator instance")
	}

	if auth.tokenReviewer == nil {
		t.Fatalf("expected tokenReviewer to be set")
	}

	if !auth.allowedSANames["kube-system:unbounded-net-node"] {
		t.Fatalf("expected allowed service account to be recorded")
	}

	if auth.cacheTTL != 5*time.Minute {
		t.Fatalf("expected default cache TTL of 5m, got %s", auth.cacheTTL)
	}

	auth2 := newTokenAuthenticator(client, nil)
	if auth2 == nil {
		t.Fatalf("expected token authenticator instance")
	}

	if len(auth2.allowedSANames) != 0 {
		t.Fatalf("expected empty allowed service account map when list is nil")
	}
}

// TestTokenAuthenticatorAuthenticate_TokenReviewFallback tests TokenAuthenticatorAuthenticate_TokenReviewFallback.
func TestTokenAuthenticatorAuthenticate_TokenReviewFallback(t *testing.T) {
	requestWithToken := func(token string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
		req.Header.Set("Authorization", "Bearer "+token)

		return req
	}

	t.Run("allows assigned service account and caches result", func(t *testing.T) {
		client := k8sfake.NewClientset()
		tokenReviewCalls := 0

		client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
			tokenReviewCalls++

			createAction, ok := action.(k8stesting.CreateAction)
			if !ok {
				t.Fatalf("expected create action, got %T", action)
			}

			review, ok := createAction.GetObject().(*authenticationv1.TokenReview)
			if !ok {
				t.Fatalf("expected TokenReview object, got %T", createAction.GetObject())
			}

			if review.Spec.Token != "token-allow" {
				t.Fatalf("expected token token-allow, got %q", review.Spec.Token)
			}

			return true, &authenticationv1.TokenReview{
				Status: authenticationv1.TokenReviewStatus{
					Authenticated: true,
					User:          authenticationv1.UserInfo{Username: "system:serviceaccount:kube-system:unbounded-net-node"},
				},
			}, nil
		})

		auth := &tokenAuthenticator{
			cache:          make(map[string]*tokenAuthResult),
			cacheTTL:       time.Minute,
			allowedSANames: map[string]bool{"kube-system:unbounded-net-node": true},
			tokenReviewer:  client,
		}

		if !auth.authenticate(requestWithToken("token-allow")) {
			t.Fatalf("expected token to authenticate via TokenReview fallback")
		}

		if !auth.authenticate(requestWithToken("token-allow")) {
			t.Fatalf("expected cached token to remain authenticated")
		}

		if tokenReviewCalls != 1 {
			t.Fatalf("expected one TokenReview call due to caching, got %d", tokenReviewCalls)
		}
	})

	t.Run("denies authenticated but disallowed service account", func(t *testing.T) {
		client := k8sfake.NewClientset()
		client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, &authenticationv1.TokenReview{
				Status: authenticationv1.TokenReviewStatus{
					Authenticated: true,
					User:          authenticationv1.UserInfo{Username: "system:serviceaccount:default:other-sa"},
				},
			}, nil
		})

		auth := &tokenAuthenticator{
			cache:          make(map[string]*tokenAuthResult),
			cacheTTL:       time.Minute,
			allowedSANames: map[string]bool{"kube-system:unbounded-net-node": true},
			tokenReviewer:  client,
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
