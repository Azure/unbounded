// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Azure/unbounded/internal/gantry/metrics"
)

// startOpsEndpoint binds the Prometheus + probe endpoints on
// c.MetricsListen and returns the *http.Server (for Shutdown) plus a
// receive-only channel that will report any listener-side error.
//
// Per /metrics for Prom, /livez for the pure liveness
// probe (always 200 while the process is up), /healthz and /readyz
// both gated by readyCheck. Kubernetes conventions vary on whether
// readiness goes through healthz or readyz; we expose all three so a
// hand-rolled probe block does not need to learn Gantry's preferences.
func startOpsEndpoint(addr string, reg *metrics.Registry, readyCheck func() (string, bool), logger *slog.Logger) (*http.Server, <-chan error) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler())
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok")) //nolint:errcheck // best-effort write
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if reason, ok := readyCheck(); !ok {
			http.Error(w, reason, http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok")) //nolint:errcheck // best-effort write
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if reason, ok := readyCheck(); !ok {
			http.Error(w, reason, http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready")) //nolint:errcheck // best-effort write
	})
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 1)

	go func() {
		err := srv.ListenAndServe()
		if !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}

		close(errc)
	}()

	logger.Info("ops endpoint listening",
		slog.String("addr", addr),
		slog.String("paths", "/metrics, /livez, /healthz, /readyz"))

	return srv, errc
}

// readinessGate is one named readiness condition. Gates are evaluated in
// slice order and the first unready gate supplies the reported reason, so
// ordering determines which cause an operator sees when several conditions
// are unsatisfied at once.
type readinessGate struct {
	reason string
	ready  func() bool
}

// firstUnreadyGate returns the reason of the first gate that is not ready.
func firstUnreadyGate(gates []readinessGate) (string, bool) {
	for _, g := range gates {
		if !g.ready() {
			return g.reason, false
		}
	}

	return "", true
}
