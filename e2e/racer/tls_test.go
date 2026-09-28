//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newTLSOrigin(t *testing.T, gateway string, handler http.Handler) (*httptest.Server, string) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Racer e2e origin CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	ip := net.ParseIP(gateway)
	require.NotNil(t, ip)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Racer e2e origin"}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{ip, net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	require.NoError(t, err)
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	require.NoError(t, err)

	server := httptest.NewUnstartedServer(handler)
	require.NoError(t, server.Listener.Close())
	server.Listener = listener
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der, caDER}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)

	return server, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
}

func TestTLSOriginRequiresTrustAndMatchingName(t *testing.T) {
	server, ca := newTLSOrigin(t, "192.0.2.10", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NotNil(t, r.TLS)
		w.WriteHeader(http.StatusNoContent)
	}))
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM([]byte(ca)))

	for _, test := range []struct {
		name  string
		roots *x509.CertPool
		valid bool
	}{
		{"192.0.2.10", pool, true},
		{"127.0.0.1", pool, true},
		{"localhost", pool, true},
		{"wrong.invalid", pool, false},
		{"localhost", x509.NewCertPool(), false},
	} {
		transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: test.roots, ServerName: test.name}}
		client := &http.Client{Transport: transport, Timeout: time.Second}

		response, err := client.Get("https://" + net.JoinHostPort("127.0.0.1", fmt.Sprint(server.Listener.Addr().(*net.TCPAddr).Port)))
		if test.valid {
			require.NoError(t, err)
			require.Equal(t, http.StatusNoContent, response.StatusCode)
			response.Body.Close()
		} else {
			require.Error(t, err)
		}

		transport.CloseIdleConnections()
	}
}
