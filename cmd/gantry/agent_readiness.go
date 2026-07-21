// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Azure/unbounded/internal/gantry/listener"
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
func startOpsEndpoint(addr string, reg *metrics.Registry, readyCheck func() (string, bool), logger *slog.Logger) (*http.Server, <-chan error, error) {
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
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := listener.Listen(addr)
	if err != nil {
		return nil, nil, fmt.Errorf("ops listen: %w", err)
	}

	errc := make(chan error, 1)

	go func() {
		err := srv.Serve(ln)
		if !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}

		close(errc)
	}()

	logger.Info("ops endpoint listening",
		slog.String("addr", addr),
		slog.String("paths", "/metrics, /livez, /healthz, /readyz"))

	return srv, errc, nil
}
