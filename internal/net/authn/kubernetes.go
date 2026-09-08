// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// kubernetesTokenClaims is the subset of a Kubernetes service account JWT
// payload needed to extract the node name bound to the token.
type kubernetesTokenClaims struct {
	Issuer     string   `json:"iss"`
	Subject    string   `json:"sub"`
	Audiences  []string `json:"aud"`
	Kubernetes struct {
		Namespace      string `json:"namespace"`
		ServiceAccount struct {
			Name string `json:"name"`
		} `json:"serviceaccount"`
		Node struct {
			Name string `json:"name"`
		} `json:"node"`
	} `json:"kubernetes.io"`
}

// DecodeKubernetesServiceAccountIdentity decodes Kubernetes service account
// claims without verifying the JWT signature. Callers must authenticate the
// exact token through another trusted mechanism before using the result.
func DecodeKubernetesServiceAccountIdentity(token string) (*KubernetesServiceAccountIdentity, error) {
	claims, err := decodeKubernetesTokenClaims(token)
	if err != nil {
		return nil, err
	}

	return &KubernetesServiceAccountIdentity{
		Subject:            claims.Subject,
		Namespace:          claims.Kubernetes.Namespace,
		ServiceAccountName: claims.Kubernetes.ServiceAccount.Name,
		NodeName:           claims.Kubernetes.Node.Name,
	}, nil
}

// DiscoverKubernetesOIDCConfig reads discovery hints from the controller's own
// trusted, mounted service account token. It must not be used on client tokens.
func DiscoverKubernetesOIDCConfig(token, audienceOverride string) (string, string, error) {
	claims, err := decodeKubernetesTokenClaims(token)
	if err != nil {
		return "", "", err
	}

	issuer, err := url.Parse(claims.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
		return "", "", fmt.Errorf("mounted token issuer must be an HTTPS URL without credentials, query, or fragment")
	}

	audience := strings.TrimSpace(audienceOverride)
	if audience == "" {
		if len(claims.Audiences) != 1 || strings.TrimSpace(claims.Audiences[0]) == "" {
			return "", "", fmt.Errorf("mounted token has no unambiguous audience; set controller.oidcAudience")
		}

		audience = claims.Audiences[0]
	}

	return claims.Issuer, audience, nil
}

func decodeKubernetesTokenClaims(token string) (*kubernetesTokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed JWT: expected 3 dot-separated parts, got %d", len(parts))
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Kubernetes tokens may use standard base64 with padding.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("decoding JWT payload: %w", err)
		}
	}

	var claims kubernetesTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("unmarshaling JWT payload: %w", err)
	}

	return &claims, nil
}

// ExtractNodeNameFromSAToken decodes the node binding without verifying the JWT.
func ExtractNodeNameFromSAToken(token string) (string, error) {
	identity, err := DecodeKubernetesServiceAccountIdentity(token)
	if err != nil {
		return "", err
	}

	return identity.NodeName, nil
}
