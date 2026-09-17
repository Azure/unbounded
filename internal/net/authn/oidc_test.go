// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestKubernetesOIDCVerifierRejectsDiscoveryIssuerMismatch(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": "https://different-issuer.example", "jwks_uri": "https://different-issuer.example/jwks",
		})
	}))
	defer server.Close()

	if _, err := newKubernetesOIDCVerifier(t.Context(), server.URL, "api", server.Client(), time.Now); err == nil {
		t.Fatal("expected mismatched discovery issuer rejection")
	}
}

func TestKubernetesOIDCVerifier(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	const keyID = "test-key"

	var issuer string

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer":   issuer,
				"jwks_uri": issuer + "/openid/v1/jwks",
			})
		case "/openid/v1/jwks":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"keys": []map[string]string{{
					"kid": keyID,
					"kty": "RSA",
					"alg": "RS256",
					"use": "sig",
					"n":   base64.RawURLEncoding.EncodeToString(privateKey.N.Bytes()),
					"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privateKey.PublicKey.E)).Bytes()),
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	issuer = server.URL

	verifier, err := newKubernetesOIDCVerifier(t.Context(), issuer, "https://kubernetes.default.svc", server.Client(), time.Now)
	if err != nil {
		t.Fatalf("create verifier: %v", err)
	}

	claims := &kubernetesServiceAccountClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   "system:serviceaccount:unbounded-system:unbounded-net-node",
			Audience:  jwt.ClaimStrings{"https://kubernetes.default.svc"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	claims.Kubernetes.Namespace = "unbounded-system"
	claims.Kubernetes.ServiceAccount.Name = "unbounded-net-node"
	claims.Kubernetes.Node.Name = "node-a"

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = keyID

	tokenString, err := token.SignedString(privateKey)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	identity, err := verifier.Verify(context.Background(), tokenString)
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}

	if identity.Subject != claims.Subject || identity.NodeName != "node-a" {
		t.Fatalf("unexpected identity: %#v", identity)
	}

	for _, tt := range []struct {
		name   string
		mutate func(*kubernetesServiceAccountClaims)
		want   string
	}{
		{"missing expiry", func(c *kubernetesServiceAccountClaims) { c.ExpiresAt = nil }, "exp claim is required"},
		{"expired", func(c *kubernetesServiceAccountClaims) {
			c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))
		}, "token is expired"},
		{"wrong issuer", func(c *kubernetesServiceAccountClaims) { c.Issuer = "https://other.example" }, "invalid issuer"},
		{"missing subject", func(c *kubernetesServiceAccountClaims) { c.Subject = "" }, "has no subject"},
		{"wrong subject", func(c *kubernetesServiceAccountClaims) { c.Subject += "-other" }, "does not match Kubernetes claims"},
		{"missing namespace", func(c *kubernetesServiceAccountClaims) { c.Kubernetes.Namespace = "" }, "missing Kubernetes service account claims"},
		{"missing service account", func(c *kubernetesServiceAccountClaims) {
			c.Kubernetes.ServiceAccount.Name = ""
		}, "missing Kubernetes service account claims"},
		{"wrong service account", func(c *kubernetesServiceAccountClaims) {
			c.Kubernetes.ServiceAccount.Name = "other"
		}, "does not match Kubernetes claims"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			invalidClaims := *claims
			tt.mutate(&invalidClaims)
			invalidToken := jwt.NewWithClaims(jwt.SigningMethodRS256, &invalidClaims)
			invalidToken.Header["kid"] = keyID

			signed, err := invalidToken.SignedString(privateKey)
			if err != nil {
				t.Fatal(err)
			}

			rejectedIdentity, err := verifier.Verify(t.Context(), signed)
			if err == nil || !strings.Contains(err.Error(), tt.want) || rejectedIdentity != nil {
				t.Fatalf("Verify() = (%#v, %v), want nil identity and %q", rejectedIdentity, err, tt.want)
			}
		})
	}

	t.Run("tampered signature", func(t *testing.T) {
		parts := strings.Split(tokenString, ".")

		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Fatal(err)
		}

		signature[0] ^= 1
		parts[2] = base64.RawURLEncoding.EncodeToString(signature)

		rejectedIdentity, err := verifier.Verify(t.Context(), strings.Join(parts, "."))
		if err == nil || !strings.Contains(err.Error(), "signature is invalid") || rejectedIdentity != nil {
			t.Fatalf("Verify() = (%#v, %v), want nil identity and invalid signature", rejectedIdentity, err)
		}
	})

	claims.Audience = jwt.ClaimStrings{"other-service"}
	wrongAudienceToken := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	wrongAudienceToken.Header["kid"] = keyID

	wrongAudienceTokenString, err := wrongAudienceToken.SignedString(privateKey)
	if err != nil {
		t.Fatalf("sign wrong-audience token: %v", err)
	}

	if _, err := verifier.Verify(context.Background(), wrongAudienceTokenString); err == nil {
		t.Fatal("expected token with the wrong audience to be rejected")
	}

	claims.Audience = jwt.ClaimStrings{"https://kubernetes.default.svc"}
	claims.Kubernetes.Node.Name = ""
	unboundToken := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	unboundToken.Header["kid"] = keyID

	unboundTokenString, err := unboundToken.SignedString(privateKey)
	if err != nil {
		t.Fatalf("sign unbound token: %v", err)
	}

	if _, err := verifier.Verify(context.Background(), unboundTokenString); err == nil {
		t.Fatal("expected unbound token to be rejected")
	}
}

func TestKubernetesOIDCVerifierECDSA(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}

	const keyID = "ec-test-key"

	var issuer string

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer":   issuer,
				"jwks_uri": issuer + "/openid/v1/jwks",
			})
		case "/openid/v1/jwks":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"keys": []map[string]string{{
					"kid": keyID,
					"kty": "EC",
					"alg": "ES256",
					"use": "sig",
					"crv": "P-256",
					"x":   base64.RawURLEncoding.EncodeToString(privateKey.X.Bytes()),
					"y":   base64.RawURLEncoding.EncodeToString(privateKey.Y.Bytes()),
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	issuer = server.URL

	verifier, err := newKubernetesOIDCVerifier(t.Context(), issuer, "https://kubernetes.default.svc", server.Client(), time.Now)
	if err != nil {
		t.Fatalf("create verifier: %v", err)
	}

	claims := &kubernetesServiceAccountClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   "system:serviceaccount:unbounded-system:unbounded-net-node",
			Audience:  jwt.ClaimStrings{"https://kubernetes.default.svc"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	claims.Kubernetes.Namespace = "unbounded-system"
	claims.Kubernetes.ServiceAccount.Name = "unbounded-net-node"
	claims.Kubernetes.Node.Name = "node-a"

	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = keyID

	tokenString, err := token.SignedString(privateKey)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	identity, err := verifier.Verify(t.Context(), tokenString)
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}

	if identity.Subject != claims.Subject || identity.NodeName != "node-a" {
		t.Fatalf("unexpected identity: %#v", identity)
	}
}
