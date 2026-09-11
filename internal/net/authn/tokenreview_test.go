// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"encoding/base64"
	"errors"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestKubernetesTokenReviewVerifier(t *testing.T) {
	const (
		subject = "system:serviceaccount:unbounded-system:unbounded-net-node"
		payload = `{"sub":"` + subject + `","aud":["https://kubernetes.default.svc"],"kubernetes.io":{"namespace":"unbounded-system","serviceaccount":{"name":"unbounded-net-node"},"node":{"name":"node-a"}}}`
	)

	validStatus := authenticationv1.TokenReviewStatus{
		Authenticated: true,
		User:          authenticationv1.UserInfo{Username: subject},
	}
	for _, tc := range []struct {
		name    string
		payload string
		status  authenticationv1.TokenReviewStatus
		apiErr  error
		wantOK  bool
	}{
		{name: "valid node identity", payload: payload, status: validStatus, wantOK: true},
		{name: "unauthenticated", payload: payload},
		{name: "API timeout", payload: payload, apiErr: errors.New("request timed out")},
		{name: "review error", payload: payload, status: authenticationv1.TokenReviewStatus{
			Authenticated: true, Error: "authentication failed", User: validStatus.User,
		}},
		{name: "different reviewed subject", payload: payload, status: authenticationv1.TokenReviewStatus{
			Authenticated: true, User: authenticationv1.UserInfo{Username: "system:serviceaccount:default:other"},
		}},
		{name: "missing node", payload: `{"sub":"` + subject + `","kubernetes.io":{"namespace":"unbounded-system","serviceaccount":{"name":"unbounded-net-node"}}}`, status: validStatus},
		{name: "mismatched service account", payload: `{"sub":"` + subject + `","kubernetes.io":{"namespace":"unbounded-system","serviceaccount":{"name":"other"},"node":{"name":"node-a"}}}`, status: validStatus},
		{name: "malformed claims", payload: `{`, status: validStatus},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := "header." + base64.RawURLEncoding.EncodeToString([]byte(tc.payload)) + ".signature"
			client := k8sfake.NewClientset()
			client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				review := action.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
				if review.Spec.Token != token {
					t.Fatal("TokenReview must authenticate the exact supplied token")
				}

				if len(review.Spec.Audiences) != 0 {
					t.Fatal("expected API-server audience validation for the mounted token")
				}

				return true, &authenticationv1.TokenReview{Status: tc.status}, tc.apiErr
			})

			verifier := NewKubernetesTokenReviewVerifier(client.AuthenticationV1())

			identity, err := verifier.Verify(t.Context(), token)
			if (err == nil) != tc.wantOK {
				t.Fatalf("Verify error = %v, want success = %v", err, tc.wantOK)
			}

			if tc.wantOK && (identity.Subject != subject || identity.NodeName != "node-a" || identity.Namespace != "unbounded-system" || identity.ServiceAccountName != "unbounded-net-node") {
				t.Fatalf("unexpected identity: %+v", identity)
			}

			actions := client.Actions()
			if len(actions) != 1 || actions[0].GetResource().Resource != "tokenreviews" {
				t.Fatalf("expected only one TokenReview and no SAR, got %v", actions)
			}
		})
	}
}

func TestKubernetesTokenReviewVerifierRejectsEmptyToken(t *testing.T) {
	client := k8sfake.NewClientset()
	verifier := NewKubernetesTokenReviewVerifier(client.AuthenticationV1())

	if _, err := verifier.Verify(t.Context(), ""); err == nil {
		t.Fatal("expected empty token rejection")
	}

	if len(client.Actions()) != 0 {
		t.Fatal("empty tokens must not reach the API server")
	}
}
