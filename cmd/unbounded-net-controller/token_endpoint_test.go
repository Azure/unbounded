// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Azure/unbounded/internal/net/authn"
)

type fakeServiceAccountTokenVerifier struct {
	identity *authn.KubernetesServiceAccountIdentity
	err      error
}

func readyTokenAuthenticator() *tokenAuthenticator {
	return &tokenAuthenticator{
		verifier: fakeServiceAccountTokenVerifier{
			identity: &authn.KubernetesServiceAccountIdentity{
				Subject: "system:serviceaccount:unbounded-system:unbounded-net-node",
			},
		},
		configured: true,
	}
}

func (f fakeServiceAccountTokenVerifier) Verify(context.Context, string) (*authn.KubernetesServiceAccountIdentity, error) {
	return f.identity, f.err
}

func TestDirectNodeTokenExchange(t *testing.T) {
	issuer := testTokenIssuer(t)
	mux := http.NewServeMux()
	registerTokenEndpoints(mux, &healthState{}, nil, issuer, tokenEndpointConfig{
		nodeTokenLifetime:  time.Hour,
		nodeServiceAccount: "unbounded-system:unbounded-net-node",
		verifier: fakeServiceAccountTokenVerifier{
			identity: &authn.KubernetesServiceAccountIdentity{
				Subject:            "system:serviceaccount:unbounded-system:unbounded-net-node",
				Namespace:          "unbounded-system",
				ServiceAccountName: "unbounded-net-node",
				NodeName:           "node-a",
			},
		},
	})

	req := httptest.NewRequest(http.MethodPost, directTokenNodePath, strings.NewReader(`{"serviceAccountToken":"sa-token"}`))
	req.Header.Set("Authorization", "Bearer sa-token")

	resp := httptest.NewRecorder()

	mux.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	var tokenResp tokenNodeResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &tokenResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	claims, err := issuer.Validate(tokenResp.Token)
	if err != nil {
		t.Fatalf("validate issued token: %v", err)
	}

	if claims.NodeName != "node-a" || claims.Role != authn.RoleNode {
		t.Fatalf("unexpected issued claims: %#v", claims)
	}
}

func TestDirectNodeTokenExchangeRejectsInvalidToken(t *testing.T) {
	issuer := testTokenIssuer(t)
	mux := http.NewServeMux()
	registerTokenEndpoints(mux, &healthState{}, nil, issuer, tokenEndpointConfig{
		nodeTokenLifetime:  time.Hour,
		nodeServiceAccount: "unbounded-system:unbounded-net-node",
		verifier:           fakeServiceAccountTokenVerifier{err: errors.New("invalid signature")},
	})

	req := httptest.NewRequest(http.MethodPost, directTokenNodePath, strings.NewReader(`{"serviceAccountToken":"bad-token"}`))
	req.Header.Set("Authorization", "Bearer bad-token")

	resp := httptest.NewRecorder()

	mux.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestAggregatedNodeTokenExchangeRequiresVerifier(t *testing.T) {
	issuer := testTokenIssuer(t)
	mux := http.NewServeMux()
	registerTokenEndpoints(mux, &healthState{}, nil, issuer, tokenEndpointConfig{
		nodeTokenLifetime:  time.Hour,
		nodeServiceAccount: "unbounded-system:unbounded-net-node",
	})

	req := httptest.NewRequest(http.MethodPost, aggregatedTokenNodePath, strings.NewReader(`{"serviceAccountToken":"unsigned-token"}`))
	resp := httptest.NewRecorder()

	mux.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token validation, got %d: %s", resp.Code, resp.Body.String())
	}
}

func testTokenReviewVerifier(t *testing.T, namespace, serviceAccount, nodeName string, authenticated bool) (*authn.KubernetesTokenReviewVerifier, *k8sfake.Clientset, string) {
	t.Helper()

	subject := "system:serviceaccount:" + namespace + ":" + serviceAccount

	payload, err := json.Marshal(map[string]any{
		"sub": subject,
		"kubernetes.io": map[string]any{
			"namespace":      namespace,
			"serviceaccount": map[string]string{"name": serviceAccount},
			"node":           map[string]string{"name": nodeName},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	token := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	client := k8sfake.NewClientset()
	client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		if review.Spec.Token != token {
			t.Fatal("reviewed a different token")
		}

		return true, &authenticationv1.TokenReview{
			Status: authenticationv1.TokenReviewStatus{
				Authenticated: authenticated,
				User:          authenticationv1.UserInfo{Username: subject},
			},
		}, nil
	})

	return authn.NewKubernetesTokenReviewVerifier(client.AuthenticationV1()), client, token
}

func TestNodeTokenExchangeTokenReviewFallback(t *testing.T) {
	for _, path := range []string{directTokenNodePath, aggregatedTokenNodePath} {
		for _, tc := range []struct {
			name           string
			serviceAccount string
			nodeName       string
			authenticated  bool
			wantCode       int
		}{
			{"valid", "unbounded-net-node", "node-a", true, http.StatusOK},
			{"wrong service account", "other", "node-a", true, http.StatusUnauthorized},
			{"missing node", "unbounded-net-node", "", true, http.StatusUnauthorized},
			{"invalid token", "unbounded-net-node", "node-a", false, http.StatusUnauthorized},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				verifier, client, token := testTokenReviewVerifier(t, "unbounded-system", tc.serviceAccount, tc.nodeName, tc.authenticated)
				issuer := testTokenIssuer(t)
				mux := http.NewServeMux()
				registerTokenEndpoints(mux, &healthState{clientset: client}, nil, issuer, tokenEndpointConfig{
					nodeServiceAccount: "unbounded-system:unbounded-net-node",
					verifier:           verifier,
				})

				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"serviceAccountToken":"`+token+`"}`))
				req.Header.Set("Authorization", "Bearer "+token)

				resp := httptest.NewRecorder()
				mux.ServeHTTP(resp, req)

				if resp.Code != tc.wantCode {
					t.Fatalf("expected %d, got %d: %s", tc.wantCode, resp.Code, resp.Body.String())
				}

				if tc.wantCode == http.StatusOK {
					var result tokenNodeResponse
					if err := json.Unmarshal(resp.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}

					claims, err := issuer.Validate(result.Token)
					if err != nil {
						t.Fatal(err)
					}

					if claims.NodeName != tc.nodeName || claims.Role != authn.RoleNode {
						t.Fatalf("unexpected issued claims: %+v", claims)
					}
				}

				actions := client.Actions()
				if len(actions) != 1 || actions[0].GetResource().Resource != "tokenreviews" {
					t.Fatalf("expected one TokenReview and no SAR: %v", actions)
				}
			})
		}
	}
}
