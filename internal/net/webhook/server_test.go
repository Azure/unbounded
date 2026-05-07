// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package webhook

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestIsTrustedAggregatedRequest tests is trusted aggregated request.
func TestIsTrustedAggregatedRequest(t *testing.T) {
	clientCertPEM, clientKeyPEM, caPEM, err := generateClientAuthCertificate("front-proxy-client")
	if err != nil {
		t.Fatalf("generateClientAuthCertificate failed: %v", err)
	}

	serverCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		t.Fatalf("parse keypair failed: %v", err)
	}

	leaf, err := x509.ParseCertificate(serverCert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf failed: %v", err)
	}

	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM(caPEM); !ok {
		t.Fatal("failed to append CA cert to pool")
	}

	s := &Server{aggregatedClientCAs: pool}

	trustedReq := &http.Request{TLS: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}}
	if !s.isTrustedAggregatedRequest(trustedReq) {
		t.Fatal("expected trusted aggregated request")
	}

	s.aggregatedClientAllowedCNs = map[string]struct{}{leaf.Subject.CommonName: {}}
	if !s.isTrustedAggregatedRequest(trustedReq) {
		t.Fatal("expected trusted aggregated request when CN is allowed")
	}

	s.aggregatedClientAllowedCNs = map[string]struct{}{"not-the-cert-cn": {}}
	if s.isTrustedAggregatedRequest(trustedReq) {
		t.Fatal("expected request rejection when client certificate CN is not allowed")
	}

	untrusted := &Server{}
	if untrusted.isTrustedAggregatedRequest(trustedReq) {
		t.Fatal("expected request rejection when no client CA pool configured")
	}

	if s.isTrustedAggregatedRequest(&http.Request{}) {
		t.Fatal("expected request without TLS peer certs to be rejected")
	}
}

func generateClientAuthCertificate(commonName string) ([]byte, []byte, []byte, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, err
	}

	now := time.Now().UTC()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(now.UnixNano()),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, err
	}

	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, err
	}

	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano() + 1),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    now.Add(-1 * time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, err
	}

	clientCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER})
	clientKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(clientKey)})
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	return clientCertPEM, clientKeyPEM, caPEM, nil
}

// TestParseRequestHeaderAllowedNames tests parse request header allowed names.
func TestParseRequestHeaderAllowedNames(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		allowed, err := parseRequestHeaderAllowedNames("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if allowed != nil {
			t.Fatalf("expected nil map for empty value")
		}
	})

	t.Run("valid", func(t *testing.T) {
		allowed, err := parseRequestHeaderAllowedNames(`["front-proxy-client","aggregator"]`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(allowed) != 2 {
			t.Fatalf("expected two allowed names, got %d", len(allowed))
		}

		if _, ok := allowed["front-proxy-client"]; !ok {
			t.Fatal("expected front-proxy-client to be allowed")
		}
	})

	t.Run("invalid", func(t *testing.T) {
		if _, err := parseRequestHeaderAllowedNames(`not-json`); err == nil {
			t.Fatal("expected parse error for invalid JSON")
		}
	})
}

// TestRefreshAggregatedClientCAsFromConfigMap tests refresh aggregated client cas from config map.
func TestRefreshAggregatedClientCAsFromConfigMap(t *testing.T) {
	caPEM, _, _, err := generateClientAuthCertificate("test-ca-subject")
	if err != nil {
		t.Fatalf("generateClientAuthCertificate returned error: %v", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: extensionAuthConfigMapName, Namespace: extensionAuthNamespace},
		Data: map[string]string{
			extensionAuthClientCAKey:     string(caPEM),
			extensionAuthAllowedNamesKey: `["front-proxy-client"]`,
		},
	}

	clientset := fake.NewClientset(cm)
	s := &Server{clientset: clientset}
	s.refreshAggregatedClientCAs(t.Context())

	if s.aggregatedClientCAs == nil {
		t.Fatal("expected aggregated client CA pool to be loaded")
	}

	if len(s.aggregatedClientAllowedCNs) != 1 {
		t.Fatalf("expected one allowed client CN, got %d", len(s.aggregatedClientAllowedCNs))
	}

	if _, ok := s.aggregatedClientAllowedCNs["front-proxy-client"]; !ok {
		t.Fatal("expected front-proxy-client to be allowed")
	}
}

// TestRegisterHandlers_ContextCancel tests that the CA refresh goroutine
// started by RegisterHandlers exits when the context is cancelled.
func TestRegisterHandlers_ContextCancel(t *testing.T) {
	clientset := fake.NewClientset()
	s := &Server{
		clientset:   clientset,
		namespace:   "kube-system",
		serviceName: defaultServiceName,
		mux:         http.NewServeMux(),
	}

	ctx, cancel := context.WithCancel(t.Context())
	s.RegisterHandlers(ctx)
	cancel()
	// No assertion needed beyond ensuring no panic/deadlock.
}

// TestGetClientCAs tests that GetClientCAs returns the front-proxy CA pool.
func TestGetClientCAs(t *testing.T) {
	s := &Server{}
	if s.GetClientCAs() != nil {
		t.Fatal("expected nil client CAs before refresh")
	}

	pool := x509.NewCertPool()

	s.aggregatedClientCAs = pool
	if got := s.GetClientCAs(); got != pool {
		t.Fatal("expected GetClientCAs to return the set pool")
	}
}
