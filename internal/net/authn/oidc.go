// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	oidcHTTPTimeout      = 10 * time.Second
	oidcKeyRefreshPeriod = 15 * time.Minute
	// Unknown keys and failed refreshes retry at most once per cooldown, measured
	// from completion. Newly rotated keys may require a retry after this interval.
	oidcKeyRefreshCooldown = 30 * time.Second
)

// KubernetesServiceAccountIdentity is the authenticated identity and node
// binding carried by a Kubernetes service account token.
type KubernetesServiceAccountIdentity struct {
	Subject            string
	Namespace          string
	ServiceAccountName string
	ServiceAccountUID  string
	PodName            string
	PodUID             string
	NodeName           string
}

type kubernetesServiceAccountClaims struct {
	jwt.RegisteredClaims
	Kubernetes struct {
		Namespace      string `json:"namespace"`
		ServiceAccount struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"serviceaccount"`
		Pod struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"pod"`
		Node struct {
			Name string `json:"name"`
		} `json:"node"`
	} `json:"kubernetes.io"`
}

type oidcDiscoveryDocument struct {
	Issuer  string `json:"issuer"`
	JWKSURL string `json:"jwks_uri"`
}

type jsonWebKeySet struct {
	Keys []jsonWebKey `json:"keys"`
}

type jsonWebKey struct {
	KeyID     string `json:"kid"`
	KeyType   string `json:"kty"`
	Algorithm string `json:"alg"`
	Use       string `json:"use"`
	Modulus   string `json:"n"`
	Exponent  string `json:"e"`
	Curve     string `json:"crv"`
	X         string `json:"x"`
	Y         string `json:"y"`
}

type oidcSigningKey struct {
	key       any
	algorithm string
}

type oidcKeyRefresh struct {
	done chan struct{}
	err  error
}

func (r *oidcKeyRefresh) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return r.err
	}
}

// KubernetesOIDCVerifier validates Kubernetes service account JWTs locally
// using the cluster issuer's OIDC discovery document and JWKS.
type KubernetesOIDCVerifier struct {
	issuer     string
	audience   string
	jwksURL    string
	client     *http.Client
	now        func() time.Time
	mu         sync.RWMutex
	keys       map[string]oidcSigningKey
	loadedAt   time.Time
	refresh    *oidcKeyRefresh
	refreshAt  time.Time
	refreshErr error
}

// NewKubernetesOIDCVerifier initializes a verifier and loads its signing keys.
func NewKubernetesOIDCVerifier(ctx context.Context, issuerURL, audience string) (*KubernetesOIDCVerifier, error) {
	return newKubernetesOIDCVerifier(ctx, issuerURL, audience, newOIDCHTTPClient(), time.Now)
}

func newKubernetesOIDCVerifier(ctx context.Context, issuerURL, audience string, client *http.Client, now func() time.Time) (*KubernetesOIDCVerifier, error) {
	issuerURL = strings.TrimSpace(issuerURL)
	if issuerURL == "" {
		return nil, fmt.Errorf("OIDC issuer URL is required")
	}

	if err := validateOIDCHTTPSURL(issuerURL, false); err != nil {
		return nil, fmt.Errorf("OIDC issuer URL: %w", err)
	}

	discoveryURL := strings.TrimRight(issuerURL, "/") + "/.well-known/openid-configuration"

	var discovery oidcDiscoveryDocument
	if err := getJSON(ctx, client, discoveryURL, &discovery); err != nil {
		return nil, fmt.Errorf("load OIDC discovery document: %w", err)
	}

	if discovery.Issuer != issuerURL {
		return nil, fmt.Errorf("OIDC discovery issuer %q does not match expected issuer %q", discovery.Issuer, issuerURL)
	}

	if discovery.JWKSURL == "" {
		return nil, fmt.Errorf("OIDC discovery document has no jwks_uri")
	}

	if err := validateOIDCHTTPSURL(discovery.JWKSURL, true); err != nil {
		return nil, fmt.Errorf("OIDC jwks_uri: %w", err)
	}

	audience = strings.TrimSpace(audience)
	if audience == "" {
		audience = discovery.Issuer
	}

	verifier := &KubernetesOIDCVerifier{
		issuer:   discovery.Issuer,
		audience: audience,
		jwksURL:  discovery.JWKSURL,
		client:   client,
		now:      now,
	}
	if err := verifier.loadKeys(ctx); err != nil {
		return nil, err
	}

	verifier.refreshAt = now()

	return verifier, nil
}

func newOIDCHTTPClient() *http.Client {
	rootCAs, err := x509.SystemCertPool()
	if err != nil || rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}

	if clusterCA, readErr := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"); readErr == nil {
		rootCAs.AppendCertsFromPEM(clusterCA)
	}

	return &http.Client{
		Timeout: oidcHTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    rootCAs,
			},
		},
	}
}

// Verify validates a service account JWT and returns its authenticated node identity.
func (v *KubernetesOIDCVerifier) Verify(ctx context.Context, tokenString string) (*KubernetesServiceAccountIdentity, error) {
	claims := &kubernetesServiceAccountClaims{}

	token, err := jwt.ParseWithClaims(
		tokenString,
		claims,
		func(token *jwt.Token) (any, error) {
			keyID, ok := token.Header["kid"].(string)
			if !ok || keyID == "" {
				return nil, fmt.Errorf("JWT header has no kid")
			}

			if v.keysStale() {
				if refreshErr := v.refreshKeys(ctx, false); refreshErr != nil {
					return nil, refreshErr
				}
			}

			key, ok := v.lookupKey(keyID)
			if ok {
				if key.algorithm != "" && token.Method.Alg() != key.algorithm {
					return nil, fmt.Errorf("JWT algorithm %q does not match signing key algorithm %q", token.Method.Alg(), key.algorithm)
				}

				return key.key, nil
			}

			if refreshErr := v.refreshKeys(ctx, true); refreshErr != nil {
				return nil, refreshErr
			}

			key, ok = v.lookupKey(keyID)
			if !ok {
				return nil, fmt.Errorf("OIDC signing key %q not found", keyID)
			}

			if key.algorithm != "" && token.Method.Alg() != key.algorithm {
				return nil, fmt.Errorf("JWT algorithm %q does not match signing key algorithm %q", token.Method.Alg(), key.algorithm)
			}

			return key.key, nil
		},
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512"}),
	)
	if err != nil {
		return nil, fmt.Errorf("validate service account JWT: %w", err)
	}

	if !token.Valid {
		return nil, fmt.Errorf("service account JWT is invalid")
	}

	if claims.Subject == "" {
		return nil, fmt.Errorf("service account JWT has no subject")
	}

	if claims.Kubernetes.Namespace == "" || claims.Kubernetes.ServiceAccount.Name == "" {
		return nil, fmt.Errorf("service account JWT is missing Kubernetes service account claims")
	}

	if claims.Kubernetes.Node.Name == "" {
		return nil, fmt.Errorf("service account JWT is not bound to a node")
	}

	expectedSubject := fmt.Sprintf(
		"system:serviceaccount:%s:%s",
		claims.Kubernetes.Namespace,
		claims.Kubernetes.ServiceAccount.Name,
	)
	if claims.Subject != expectedSubject {
		return nil, fmt.Errorf("service account JWT subject %q does not match Kubernetes claims %q", claims.Subject, expectedSubject)
	}

	return &KubernetesServiceAccountIdentity{
		Subject:            claims.Subject,
		Namespace:          claims.Kubernetes.Namespace,
		ServiceAccountName: claims.Kubernetes.ServiceAccount.Name,
		ServiceAccountUID:  claims.Kubernetes.ServiceAccount.UID,
		PodName:            claims.Kubernetes.Pod.Name,
		PodUID:             claims.Kubernetes.Pod.UID,
		NodeName:           claims.Kubernetes.Node.Name,
	}, nil
}

func (v *KubernetesOIDCVerifier) lookupKey(keyID string) (oidcSigningKey, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	key, ok := v.keys[keyID]

	return key, ok
}

func (v *KubernetesOIDCVerifier) keysStale() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()

	return v.loadedAt.IsZero() || v.now().Sub(v.loadedAt) >= oidcKeyRefreshPeriod
}

func (v *KubernetesOIDCVerifier) refreshKeys(ctx context.Context, force bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	v.mu.Lock()

	if refresh := v.refresh; refresh != nil {
		v.mu.Unlock()

		if !force {
			// A concurrent refresh must not block verification with cached keys.
			return nil
		}

		return refresh.wait(ctx)
	}

	if !force && !v.loadedAt.IsZero() && v.now().Sub(v.loadedAt) < oidcKeyRefreshPeriod {
		v.mu.Unlock()
		return nil
	}

	if !v.refreshAt.IsZero() && v.now().Sub(v.refreshAt) < oidcKeyRefreshCooldown {
		err := v.refreshErr
		v.mu.Unlock()

		return err
	}

	refresh := &oidcKeyRefresh{done: make(chan struct{})}
	v.refresh = refresh
	v.mu.Unlock()

	// A disconnected caller must not cancel shared work or poison the cooldown.
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oidcHTTPTimeout)
	go func() {
		defer cancel()

		err := v.loadKeys(refreshCtx)

		v.mu.Lock()
		refresh.err = err
		v.refreshAt = v.now()
		v.refreshErr = err
		v.refresh = nil
		close(refresh.done)
		v.mu.Unlock()
	}()

	return refresh.wait(ctx)
}

func (v *KubernetesOIDCVerifier) loadKeys(ctx context.Context) error {
	var keySet jsonWebKeySet
	if err := getJSON(ctx, v.client, v.jwksURL, &keySet); err != nil {
		return fmt.Errorf("load OIDC signing keys: %w", err)
	}

	keys := make(map[string]oidcSigningKey, len(keySet.Keys))
	for _, jwk := range keySet.Keys {
		if jwk.KeyID == "" {
			continue
		}

		if jwk.Use != "" && jwk.Use != "sig" {
			continue
		}

		key, err := oidcPublicKey(jwk)
		if err != nil {
			return fmt.Errorf("parse OIDC signing key %q: %w", jwk.KeyID, err)
		}

		if key == nil {
			continue
		}

		keys[jwk.KeyID] = oidcSigningKey{
			key:       key,
			algorithm: jwk.Algorithm,
		}
	}

	if len(keys) == 0 {
		return fmt.Errorf("OIDC JWKS contains no supported signing keys")
	}

	v.mu.Lock()
	v.keys = keys
	v.loadedAt = v.now()
	v.mu.Unlock()

	return nil
}

func oidcPublicKey(jwk jsonWebKey) (any, error) {
	switch jwk.KeyType {
	case "RSA":
		if jwk.Algorithm != "" && jwk.Algorithm != "RS256" && jwk.Algorithm != "RS384" && jwk.Algorithm != "RS512" {
			return nil, nil
		}

		return rsaPublicKey(jwk)
	case "EC":
		if jwk.Algorithm != "" && jwk.Algorithm != "ES256" && jwk.Algorithm != "ES384" && jwk.Algorithm != "ES512" {
			return nil, nil
		}

		return ecdsaPublicKey(jwk)
	default:
		return nil, nil
	}
}

func rsaPublicKey(jwk jsonWebKey) (*rsa.PublicKey, error) {
	modulus, err := base64.RawURLEncoding.DecodeString(jwk.Modulus)
	if err != nil {
		return nil, fmt.Errorf("decode modulus: %w", err)
	}

	exponentBytes, err := base64.RawURLEncoding.DecodeString(jwk.Exponent)
	if err != nil {
		return nil, fmt.Errorf("decode exponent: %w", err)
	}

	if len(modulus) == 0 || len(exponentBytes) == 0 {
		return nil, fmt.Errorf("RSA modulus and exponent are required")
	}

	exponent := 0
	for _, b := range exponentBytes {
		exponent = exponent<<8 | int(b)
	}

	if exponent < 2 {
		return nil, fmt.Errorf("invalid RSA exponent %d", exponent)
	}

	return &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: exponent}, nil
}

func ecdsaPublicKey(jwk jsonWebKey) (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve

	switch jwk.Curve {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		return nil, fmt.Errorf("unsupported EC curve %q", jwk.Curve)
	}

	xBytes, err := base64.RawURLEncoding.DecodeString(jwk.X)
	if err != nil {
		return nil, fmt.Errorf("decode x coordinate: %w", err)
	}

	yBytes, err := base64.RawURLEncoding.DecodeString(jwk.Y)
	if err != nil {
		return nil, fmt.Errorf("decode y coordinate: %w", err)
	}

	coordinateSize := (curve.Params().BitSize + 7) / 8
	if len(xBytes) == 0 || len(xBytes) > coordinateSize || len(yBytes) == 0 || len(yBytes) > coordinateSize {
		return nil, fmt.Errorf("invalid EC public key coordinate size")
	}

	encoded := make([]byte, 1+2*coordinateSize)
	encoded[0] = 4
	copy(encoded[1+coordinateSize-len(xBytes):1+coordinateSize], xBytes)
	copy(encoded[1+2*coordinateSize-len(yBytes):], yBytes)

	key, err := ecdsa.ParseUncompressedPublicKey(curve, encoded)
	if err != nil {
		return nil, fmt.Errorf("parse EC public key: %w", err)
	}

	return key, nil
}

func getJSON(ctx context.Context, client *http.Client, endpoint string, target any) (returnErr error) {
	if err := validateOIDCHTTPSURL(endpoint, true); err != nil {
		return err
	}

	// Enforce HTTPS on every hop, including when tests supply a trusted client.
	safeClient := *client
	safeClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := validateOIDCHTTPSURL(req.URL.String(), true); err != nil {
			return err
		}

		if client.CheckRedirect != nil {
			return client.CheckRedirect(req, via)
		}

		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}

		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := safeClient.Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", endpoint, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("close response from %s: %w", endpoint, err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("request %s returned %s", endpoint, resp.Status)
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target); err != nil {
		return fmt.Errorf("decode response from %s: %w", endpoint, err)
	}

	return nil
}

func validateOIDCHTTPSURL(endpoint string, allowQuery bool) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.Opaque != "" {
		return fmt.Errorf("must be an absolute HTTPS URL")
	}

	if parsed.User != nil || strings.Contains(endpoint, "#") {
		return fmt.Errorf("HTTPS URL must not contain credentials or a fragment")
	}

	if !allowQuery && (parsed.RawQuery != "" || parsed.ForceQuery) {
		return fmt.Errorf("HTTPS issuer URL must not contain a query")
	}

	return nil
}
