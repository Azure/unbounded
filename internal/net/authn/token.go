// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package authn

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	// RoleNode is the role for node agent tokens.
	RoleNode = "node"
	// RoleViewer is the role for viewer tokens.
	RoleViewer = "viewer"
	// HMACKeySize is the minimum key size in bytes for HMAC-SHA256 signing.
	HMACKeySize = 32
)

// Claims holds the payload of an HMAC-signed token.
type Claims struct {
	Subject   string   `json:"sub"`              // e.g. "system:serviceaccount:unbounded-net:unbounded-net-node"
	Role      string   `json:"role"`             // "node" or "viewer"
	NodeName  string   `json:"node,omitempty"`   // node name (node tokens only)
	Groups    []string `json:"groups,omitempty"` // user groups (for SAR checks)
	IssuedAt  int64    `json:"iat"`              // Unix timestamp
	ExpiresAt int64    `json:"exp"`              // Unix timestamp
}

// TokenIssuer generates and validates HMAC-signed tokens.
type TokenIssuer struct {
	hmacKey []byte
}

// NewTokenIssuer creates a TokenIssuer after validating that the key is at
// least HMACKeySize bytes.
func NewTokenIssuer(hmacKey []byte) (*TokenIssuer, error) {
	if len(hmacKey) < HMACKeySize {
		return nil, fmt.Errorf("HMAC key must be at least %d bytes, got %d", HMACKeySize, len(hmacKey))
	}

	dst := make([]byte, len(hmacKey))
	copy(dst, hmacKey)

	return &TokenIssuer{hmacKey: dst}, nil
}

// GenerateHMACKey returns HMACKeySize cryptographically random bytes suitable
// for use as an HMAC signing key.
func GenerateHMACKey() ([]byte, error) {
	key := make([]byte, HMACKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating HMAC key: %w", err)
	}

	return key, nil
}

// IssueNodeToken creates an HMAC-signed token with RoleNode and the given node
// name. It returns the token string, its expiry time, and any error.
func (ti *TokenIssuer) IssueNodeToken(subject, nodeName string, lifetime time.Duration) (string, time.Time, error) {
	now := time.Now()
	expiresAt := now.Add(lifetime)
	claims := &Claims{
		Subject:   subject,
		Role:      RoleNode,
		NodeName:  nodeName,
		IssuedAt:  now.Unix(),
		ExpiresAt: expiresAt.Unix(),
	}

	token, err := ti.sign(claims)
	if err != nil {
		return "", time.Time{}, err
	}

	return token, expiresAt, nil
}

// IssueViewerToken creates an HMAC-signed token with RoleViewer. It returns
// the token string, its expiry time, and any error. Groups are included in
// the token claims so downstream SAR checks can evaluate group-based RBAC.
func (ti *TokenIssuer) IssueViewerToken(subject string, groups []string, lifetime time.Duration) (string, time.Time, error) {
	now := time.Now()
	expiresAt := now.Add(lifetime)
	claims := &Claims{
		Subject:   subject,
		Role:      RoleViewer,
		Groups:    groups,
		IssuedAt:  now.Unix(),
		ExpiresAt: expiresAt.Unix(),
	}

	token, err := ti.sign(claims)
	if err != nil {
		return "", time.Time{}, err
	}

	return token, expiresAt, nil
}

// Validate parses and verifies an HMAC-signed token. It returns the decoded
// claims or an error if the token is malformed, the signature is invalid, or
// the token has expired.
func (ti *TokenIssuer) Validate(tokenStr string) (*Claims, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed token: expected 3 dot-separated parts, got %d", len(parts))
	}

	headerB64, payloadB64, sigB64 := parts[0], parts[1], parts[2]

	// Verify HMAC signature over "header.payload".
	signingInput := headerB64 + "." + payloadB64
	mac := hmac.New(sha256.New, ti.hmacKey)
	mac.Write([]byte(signingInput))
	expectedSig := mac.Sum(nil)

	actualSig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, fmt.Errorf("decoding signature: %w", err)
	}

	if !hmac.Equal(actualSig, expectedSig) {
		return nil, fmt.Errorf("invalid token signature")
	}

	// Decode payload.
	payloadJSON, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, fmt.Errorf("decoding payload: %w", err)
	}

	var claims Claims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, fmt.Errorf("unmarshaling claims: %w", err)
	}

	// Check expiry.
	if time.Now().Unix() > claims.ExpiresAt {
		return nil, fmt.Errorf("token expired at %d", claims.ExpiresAt)
	}

	return &claims, nil
}

// sign produces a dot-separated, base64url-encoded token string from the given
// claims using HMAC-SHA256.
func (ti *TokenIssuer) sign(claims *Claims) (string, error) {
	headerJSON := []byte(`{"alg":"HS256"}`)

	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshaling claims: %w", err)
	}

	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)

	signingInput := headerB64 + "." + payloadB64
	mac := hmac.New(sha256.New, ti.hmacKey)
	mac.Write([]byte(signingInput))
	sig := mac.Sum(nil)

	sigB64 := base64.RawURLEncoding.EncodeToString(sig)

	return signingInput + "." + sigB64, nil
}
