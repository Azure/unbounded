// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package authn

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestGenerateHMACKey(t *testing.T) {
	k1, err := GenerateHMACKey()
	if err != nil {
		t.Fatalf("GenerateHMACKey() error: %v", err)
	}

	if len(k1) != HMACKeySize {
		t.Fatalf("expected key length %d, got %d", HMACKeySize, len(k1))
	}

	k2, err := GenerateHMACKey()
	if err != nil {
		t.Fatalf("GenerateHMACKey() second call error: %v", err)
	}

	if string(k1) == string(k2) {
		t.Fatal("two generated keys should not be identical")
	}
}

func TestNewTokenIssuer(t *testing.T) {
	// Key that is too short.
	_, err := NewTokenIssuer(make([]byte, HMACKeySize-1))
	if err == nil {
		t.Fatal("expected error for short key")
	}

	// Exact minimum size.
	key := make([]byte, HMACKeySize)

	ti, err := NewTokenIssuer(key)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ti == nil {
		t.Fatal("expected non-nil issuer")
	}

	// Larger key.
	_, err = NewTokenIssuer(make([]byte, HMACKeySize+16))
	if err != nil {
		t.Fatalf("unexpected error for larger key: %v", err)
	}
}

func TestIssueAndValidateNodeToken(t *testing.T) {
	key, err := GenerateHMACKey()
	if err != nil {
		t.Fatalf("GenerateHMACKey: %v", err)
	}

	ti, err := NewTokenIssuer(key)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}

	subject := "system:serviceaccount:unbounded-net:unbounded-net-node"
	nodeName := "worker-1"

	token, expiresAt, err := ti.IssueNodeToken(subject, nodeName, 10*time.Minute)
	if err != nil {
		t.Fatalf("IssueNodeToken: %v", err)
	}

	if token == "" {
		t.Fatal("expected non-empty token")
	}

	if expiresAt.Before(time.Now()) {
		t.Fatal("expiresAt should be in the future")
	}

	claims, err := ti.Validate(token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if claims.Subject != subject {
		t.Errorf("subject: got %q, want %q", claims.Subject, subject)
	}

	if claims.Role != RoleNode {
		t.Errorf("role: got %q, want %q", claims.Role, RoleNode)
	}

	if claims.NodeName != nodeName {
		t.Errorf("nodeName: got %q, want %q", claims.NodeName, nodeName)
	}
}

func TestIssueAndValidateViewerToken(t *testing.T) {
	key, err := GenerateHMACKey()
	if err != nil {
		t.Fatalf("GenerateHMACKey: %v", err)
	}

	ti, err := NewTokenIssuer(key)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}

	subject := "viewer-user@example.com"

	token, expiresAt, err := ti.IssueViewerToken(subject, nil, 30*time.Minute)
	if err != nil {
		t.Fatalf("IssueViewerToken: %v", err)
	}

	if token == "" {
		t.Fatal("expected non-empty token")
	}

	if expiresAt.Before(time.Now()) {
		t.Fatal("expiresAt should be in the future")
	}

	claims, err := ti.Validate(token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if claims.Subject != subject {
		t.Errorf("subject: got %q, want %q", claims.Subject, subject)
	}

	if claims.Role != RoleViewer {
		t.Errorf("role: got %q, want %q", claims.Role, RoleViewer)
	}
}

func TestValidateExpiredToken(t *testing.T) {
	key, err := GenerateHMACKey()
	if err != nil {
		t.Fatalf("GenerateHMACKey: %v", err)
	}

	ti, err := NewTokenIssuer(key)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}

	// Issue a token that is already expired (negative lifetime).
	token, _, err := ti.IssueNodeToken("sub", "node-1", -1*time.Second)
	if err != nil {
		t.Fatalf("IssueNodeToken: %v", err)
	}

	_, err = ti.Validate(token)
	if err == nil {
		t.Fatal("expected error for expired token")
	}

	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error should mention 'expired', got: %v", err)
	}
}

func TestValidateInvalidSignature(t *testing.T) {
	key, err := GenerateHMACKey()
	if err != nil {
		t.Fatalf("GenerateHMACKey: %v", err)
	}

	ti, err := NewTokenIssuer(key)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}

	token, _, err := ti.IssueNodeToken("sub", "node-1", 10*time.Minute)
	if err != nil {
		t.Fatalf("IssueNodeToken: %v", err)
	}

	// Tamper with the signature part.
	parts := strings.Split(token, ".")
	parts[2] = base64.RawURLEncoding.EncodeToString([]byte("tampered-signature"))
	tampered := strings.Join(parts, ".")

	_, err = ti.Validate(tampered)
	if err == nil {
		t.Fatal("expected error for tampered signature")
	}

	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("error should mention 'signature', got: %v", err)
	}
}

func TestValidateMalformedToken(t *testing.T) {
	key, err := GenerateHMACKey()
	if err != nil {
		t.Fatalf("GenerateHMACKey: %v", err)
	}

	ti, err := NewTokenIssuer(key)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}

	cases := []struct {
		name  string
		token string
	}{
		{"empty string", ""},
		{"no dots", "abc123"},
		{"one dot", "abc.def"},
		{"four parts", "a.b.c.d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ti.Validate(tc.token)
			if err == nil {
				t.Fatalf("expected error for token %q", tc.token)
			}
		})
	}
}

func TestValidateWrongKey(t *testing.T) {
	key1, _ := GenerateHMACKey()
	key2, _ := GenerateHMACKey()

	ti1, _ := NewTokenIssuer(key1)
	ti2, _ := NewTokenIssuer(key2)

	token, _, err := ti1.IssueNodeToken("sub", "node-1", 10*time.Minute)
	if err != nil {
		t.Fatalf("IssueNodeToken: %v", err)
	}

	_, err = ti2.Validate(token)
	if err == nil {
		t.Fatal("expected error when validating with different key")
	}

	if !strings.Contains(err.Error(), "signature") {
		t.Errorf("error should mention 'signature', got: %v", err)
	}
}

func TestNodeTokenHasNodeClaim(t *testing.T) {
	key, _ := GenerateHMACKey()
	ti, _ := NewTokenIssuer(key)

	token, _, err := ti.IssueNodeToken("sub", "my-node", 10*time.Minute)
	if err != nil {
		t.Fatalf("IssueNodeToken: %v", err)
	}

	claims, err := ti.Validate(token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if claims.NodeName != "my-node" {
		t.Errorf("NodeName: got %q, want %q", claims.NodeName, "my-node")
	}
}

func TestViewerTokenHasNoNodeClaim(t *testing.T) {
	key, _ := GenerateHMACKey()
	ti, _ := NewTokenIssuer(key)

	token, _, err := ti.IssueViewerToken("viewer", nil, 10*time.Minute)
	if err != nil {
		t.Fatalf("IssueViewerToken: %v", err)
	}

	claims, err := ti.Validate(token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if claims.NodeName != "" {
		t.Errorf("NodeName should be empty for viewer token, got %q", claims.NodeName)
	}

	// Also verify the "node" key is omitted from the JSON payload.
	parts := strings.Split(token, ".")
	payloadJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])

	var raw map[string]interface{}
	if err := json.Unmarshal(payloadJSON, &raw); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if _, ok := raw["node"]; ok {
		t.Error("viewer token payload should not contain 'node' key")
	}
}

func TestExtractNodeNameFromSAToken(t *testing.T) {
	// Build a fake JWT payload with kubernetes.io claims.
	payload := map[string]interface{}{
		"iss": "https://kubernetes.default.svc.cluster.local",
		"sub": "system:serviceaccount:unbounded-net:unbounded-net-node",
		"kubernetes.io": map[string]interface{}{
			"namespace":      "unbounded-net",
			"serviceaccount": map[string]string{"name": "unbounded-net-node"},
			"node":           map[string]string{"name": "worker-42"},
		},
	}
	payloadJSON, _ := json.Marshal(payload)
	fakeJWT := "eyJhbGciOiJSUzI1NiJ9." +
		base64.RawURLEncoding.EncodeToString(payloadJSON) +
		".fake-signature"

	name, err := ExtractNodeNameFromSAToken(fakeJWT)
	if err != nil {
		t.Fatalf("ExtractNodeNameFromSAToken: %v", err)
	}

	if name != "worker-42" {
		t.Errorf("got %q, want %q", name, "worker-42")
	}
}

func TestExtractNodeNameMissingClaim(t *testing.T) {
	// JWT payload without kubernetes.io.node.
	payload := map[string]interface{}{
		"iss": "https://kubernetes.default.svc.cluster.local",
		"sub": "system:serviceaccount:default:my-sa",
	}
	payloadJSON, _ := json.Marshal(payload)
	fakeJWT := "eyJhbGciOiJSUzI1NiJ9." +
		base64.RawURLEncoding.EncodeToString(payloadJSON) +
		".fake-signature"

	name, err := ExtractNodeNameFromSAToken(fakeJWT)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if name != "" {
		t.Errorf("expected empty string, got %q", name)
	}
}

func TestExtractNodeNameMalformedToken(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"one part", "abc"},
		{"two parts", "abc.def"},
		{"bad base64 payload", "header.!!!invalid!!!.sig"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ExtractNodeNameFromSAToken(tc.token)
			if err == nil {
				t.Fatalf("expected error for token %q", tc.token)
			}
		})
	}
}
