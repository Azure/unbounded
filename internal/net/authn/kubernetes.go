// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package authn

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// kubernetesTokenClaims is the subset of a Kubernetes service account JWT
// payload needed to extract the node name bound to the token.
type kubernetesTokenClaims struct {
	Kubernetes struct {
		Node struct {
			Name string `json:"name"`
		} `json:"node"`
	} `json:"kubernetes.io"`
}

// ExtractNodeNameFromSAToken decodes the payload of a Kubernetes service account
// JWT token (without verifying the signature -- the caller must verify via
// TokenReview first) and extracts the kubernetes.io.node.name claim.
// Returns ("", nil) if the claim is not present.
func ExtractNodeNameFromSAToken(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("malformed JWT: expected 3 dot-separated parts, got %d", len(parts))
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Kubernetes tokens may use standard base64 with padding.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return "", fmt.Errorf("decoding JWT payload: %w", err)
		}
	}

	var claims kubernetesTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("unmarshaling JWT payload: %w", err)
	}

	return claims.Kubernetes.Node.Name, nil
}
