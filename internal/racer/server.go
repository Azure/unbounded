// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/tls"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/Azure/unbounded/internal/racer/wire"
)

type Server struct {
	Config       Config
	APIReader    client.Reader
	Bootstrap    *Bootstrap
	Publications *Publications
	Lifecycle    *Lifecycle
}

var (
	_ manager.Runnable               = (*Server)(nil)
	_ manager.LeaderElectionRunnable = (*Server)(nil)
)

func (*Server) NeedLeaderElection() bool { return true }

// TLSConfig must use VerifyClientCertIfGiven: bootstrap can omit the client
// certificate, while snapshot explicitly requires VerifiedChains. Resumption
// and pooled requests must not extend certificate validity or stale trust.
func (*Server) TLSConfig(_ context.Context) (*tls.Config, error) {
	return nil, pending("server.tls_config")
}

// Start waits for synchronized inputs and initialized issuer/publication state.
// Leadership cancellation closes listeners/connections and cancels every poll.
func (*Server) Start(_ context.Context) error { return pending("server.start") }

func (s *Server) Ready(r *http.Request) error {
	if s.Lifecycle == nil {
		return wire.Unavailable
	}

	return s.Lifecycle.Ready(r)
}

// Handler reserves routes, but deliberately cannot return successful operations.
// This also prevents accidental plaintext httptest use from bypassing scaffold
// authentication when individual handlers are exercised before TLS exists.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+wire.BootstrapPath, s.serveBootstrap)
	mux.HandleFunc("GET "+wire.SnapshotPath, s.serveSnapshot)

	return mux
}

func (*Server) serveBootstrap(w http.ResponseWriter, _ *http.Request) {
	writeUnavailable(w)
}

func (*Server) serveSnapshot(w http.ResponseWriter, _ *http.Request) {
	writeUnavailable(w)
}

func writeUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusServiceUnavailable)

	if _, err := w.Write([]byte(`{"code":"unavailable"}`)); err != nil {
		return // Client disconnected; no response recovery is possible.
	}
}
