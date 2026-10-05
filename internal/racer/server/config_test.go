// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestServingChainFreezesBeforeFirstRequest(t *testing.T) {
	for _, boundary := range []string{"handler", "tls"} {
		t.Run(boundary, func(t *testing.T) {
			f := newServingFixture(t)
			s := f.a.Server
			// Pre-use overrides now belong to construction, not mutable engines.
			cfg := f.a.Topology.Config
			cfg.CertificateLifetime = 2 * time.Minute
			cfg.SnapshotMaxAge = 30 * time.Second
			d := fixtureDependencies[f.a.authority]

			s.authority = authority.New(cfg.authorityConfig(), authority.Dependencies{Writer: d, Reader: d})
			if err := s.authority.Observe(f.ctx); err != nil {
				t.Fatal(err)
			}

			f.a.Replication.Config = cfg

			want := cfg
			wantServer := s.Config

			var handler http.Handler
			if boundary == "handler" {
				handler = s.Handler()
			} else {
				s.tlsConfigWithCertificate(f.ctx, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &f.serverCertificate, nil })
			}
			// Mutate sequentially, before any request, without calling dependency
			// getters first: those calls would accidentally hide lazy freezing.
			cfg.DataplaneServiceAccount = "wrong-account"
			cfg.Cluster = ""
			cfg.CertificateLifetime = time.Second
			s.Config.Limits.HeaderBytes = 1

			cfg.ControllerServiceAccount = "wrong-controller"

			if handler == nil {
				handler = s.Handler()
			}

			if s.config != wantServer {
				t.Fatal("server did not freeze transport before exposure")
			}

			encoded, err := wire.EncodeBootstrapRequest(f.request)
			if err != nil {
				t.Fatal(err)
			}

			request := httptest.NewRequest(http.MethodPost, wire.BootstrapPath, bytes.NewReader(encoded))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+f.token)
			request.TLS = &tls.ConnectionState{HandshakeComplete: true}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, request)

			if w.Code != http.StatusOK {
				t.Fatal("first request used post-exposure config", w.Code)
			}

			response := decodeIssuedResponse(t, w.Body.Bytes())

			leaf, err := x509.ParseCertificate(response.CertificateChain[0])
			if err != nil {
				t.Fatal(err)
			}

			if response.Cluster != want.Cluster || leaf.NotAfter.Sub(leaf.NotBefore) != want.CertificateLifetime+time.Minute {
				t.Fatal("first issuance ignored frozen identity/lifetime")
			}
		})
	}
}
