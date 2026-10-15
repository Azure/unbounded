// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStatusPushRequiresTLS13(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
		server.TLS = &tls.Config{MinVersion: version, MaxVersion: version}
		server.StartTLS()

		client := newStatusPushHTTPClient(5 * time.Second)
		transport := client.Transport.(*http.Transport)
		transport.TLSClientConfig.RootCAs = x509.NewCertPool()
		transport.TLSClientConfig.RootCAs.AddCert(server.Certificate())

		response, err := client.Get(server.URL)
		if response != nil {
			response.Body.Close()
		}

		client.CloseIdleConnections()
		server.Close()

		if (err == nil) != (version == tls.VersionTLS13) {
			t.Fatalf("TLS %x: %v", version, err)
		}
	}
}
