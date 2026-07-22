// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const minimumCapabilityKeySize = 32

type capabilityClaims struct {
	KeyID      string `json:"kid"`
	Session    string `json:"session"`
	SessionUID string `json:"sessionUID"`
	Expires    int64  `json:"expires"`
}

// CapabilitySigner issues operation-scoped bearer capabilities without
// persisting the bearer value in Kubernetes.
type CapabilitySigner struct {
	key   []byte
	keyID string
	now   func() time.Time
}

func NewCapabilitySigner(key []byte, keyID string, now func() time.Time) (*CapabilitySigner, error) {
	if len(key) < minimumCapabilityKeySize {
		return nil, fmt.Errorf("capability HMAC key must be at least %d bytes", minimumCapabilityKeySize)
	}
	if strings.TrimSpace(keyID) == "" {
		return nil, errors.New("capability key ID is required")
	}
	if now == nil {
		now = time.Now
	}

	return &CapabilitySigner{key: append([]byte(nil), key...), keyID: keyID, now: now}, nil
}

func (s *CapabilitySigner) Sign(session *v1alpha3.NetbootSession) (string, error) {
	if session == nil || session.Name == "" || session.UID == "" {
		return "", errors.New("session name and UID are required")
	}

	claims := capabilityClaims{
		KeyID:      s.keyID,
		Session:    session.Name,
		SessionUID: string(session.UID),
		Expires:    session.Spec.ExpiresAt.Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal capability claims: %w", err)
	}

	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + s.signature(encoded), nil
}

func (s *CapabilitySigner) Verify(session *v1alpha3.NetbootSession, capability string) error {
	encoded, signature, ok := strings.Cut(capability, ".")
	if !ok || encoded == "" || signature == "" || !hmac.Equal([]byte(signature), []byte(s.signature(encoded))) {
		return errors.New("invalid capability")
	}

	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return errors.New("invalid capability")
	}
	var claims capabilityClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return errors.New("invalid capability")
	}
	if session == nil || claims.KeyID != s.keyID || claims.Session != session.Name || claims.SessionUID != string(session.UID) || claims.Expires != session.Spec.ExpiresAt.Unix() {
		return errors.New("invalid capability")
	}
	if !s.now().Before(time.Unix(claims.Expires, 0)) {
		return errors.New("expired capability")
	}

	return nil
}

func (s *CapabilitySigner) signature(payload string) string {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(payload))

	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *CapabilitySigner) IsExpired(session *v1alpha3.NetbootSession) bool {
	return session == nil || !s.now().Before(session.Spec.ExpiresAt.Time)
}
