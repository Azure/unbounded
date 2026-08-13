// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
)

// catalogConflictErrnos resolves the configured conflict errnos, wrapping the
// error so a typo in a flag is obvious rather than showing up later as an
// ingest that never retries.
func catalogConflictErrnos(cfg *Config) ([]unix.Errno, error) {
	errnos, err := catalog.ParseConflictErrnos(cfg.ConflictErrnos)
	if err != nil {
		return nil, fmt.Errorf("conflict-errnos: %w", err)
	}

	return errnos, nil
}

// metricsShutdown bounds how long the observability server is given to drain.
// It carries no request the daemon's correctness depends on.
const metricsShutdown = 2 * time.Second

// runMetrics serves Prometheus metrics, pprof and a liveness endpoint.
//
// A failure here is logged and dropped rather than propagated. Losing the
// ability to scrape a snapshotter is not a reason to stop running containers
// on the node.
func runMetrics(ctx context.Context, cfg *Config, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)

		_, _ = w.Write([]byte("ok")) //nolint:errcheck // best effort
	})
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	server := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("metrics server stopped", slog.Any("err", err))
		}
	}()

	log.Info("metrics listening", slog.String("addr", cfg.MetricsAddr))

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), metricsShutdown)
	defer cancel()

	_ = server.Shutdown(shutdownCtx) //nolint:errcheck // shutdown is best effort

	<-done
}
