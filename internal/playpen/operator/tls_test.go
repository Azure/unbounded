// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestServingTLSRequiresTLS13(t *testing.T) {
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cert := fixture.TLS.Certificates[0]
	roots := x509.NewCertPool()
	roots.AddCert(fixture.Certificate())
	fixture.Close()

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	server.TLS = (&Operator{}).servingTLSConfig(cert)

	server.StartTLS()
	defer server.Close()

	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: version, MaxVersion: version}}
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}

		response, err := client.Get(server.URL)
		if response != nil {
			response.Body.Close()
		}

		transport.CloseIdleConnections()

		if (err == nil) != (version == tls.VersionTLS13) {
			t.Fatalf("TLS %x: %v", version, err)
		}
	}
}
