// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Azure/unbounded/internal/racer/pki"
)

// Compare fresh TLS polling with connection reuse, independently of topology
// and PKI lookups. Production dataplanes currently request Connection: close.
// Both endpoints run locally, so CPU profiles include client and server work.
func BenchmarkControlTLSHandshake(b *testing.B) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		b.Fatal(err)
	}

	manager, err := pki.New(fake.NewClientBuilder().WithScheme(scheme).Build(), "system", pki.Options{})
	if err != nil {
		b.Fatal(err)
	}

	if err := manager.AcquireLeadership(b.Context(), "benchmark"); err != nil {
		b.Fatal(err)
	}

	if err := manager.Publish(b.Context()); err != nil {
		b.Fatal(err)
	}

	serverCert, serverKey := issueTLSFixture(b, manager, pki.Identity{Kind: pki.ControlPlane, PodUID: "controller", BootID: "boot"}, false)
	clientCert, clientKey := issueTLSFixture(b, manager, pki.Identity{Kind: pki.Node, Universe: strings.Repeat("a", 64), Node: strings.Repeat("b", 64), PodUID: "pod", BootID: "boot"}, false)

	hot := pki.NewHotTLS()
	if err := hot.Update(serverCert.Bundle.JSON(), serverCert.CertificatePEM, serverKey); err != nil {
		b.Fatal(err)
	}

	pair, err := tls.X509KeyPair(clientCert.CertificatePEM, clientKey)
	if err != nil {
		b.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(serverCert.Bundle.Certificates))

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	server.TLS = hot.ServerConfig(tls.RequireAndVerifyClientCert)

	server.StartTLS()
	defer server.Close()

	for _, fresh := range []bool{true, false} {
		b.Run(fmt.Sprintf("fresh=%t", fresh), func(b *testing.B) {
			transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{pair}, ServerName: "racer-controlplane.system.svc"}, DisableKeepAlives: fresh}
			defer transport.CloseIdleConnections()

			client := &http.Client{Transport: transport}

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				response, err := client.Get(server.URL)
				if err != nil {
					b.Fatal(err)
				}

				_, err = io.Copy(io.Discard, response.Body)
				response.Body.Close()

				if err != nil || response.StatusCode != http.StatusNoContent {
					b.Fatalf("TLS request: status=%d error=%v", response.StatusCode, err)
				}
			}
		})
	}
}
