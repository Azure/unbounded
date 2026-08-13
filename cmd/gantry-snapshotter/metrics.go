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

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/internal/gantry/metrics"
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

// runMetrics serves Prometheus metrics and a liveness endpoint, and pprof when
// it has been asked for.
//
// A failure here is logged and dropped rather than propagated. Losing the
// ability to scrape a snapshotter is not a reason to stop running containers
// on the node.
//
// health is what /healthz reports. It is a parameter rather than something
// built here so the endpoint cannot drift back into answering without asking
// anybody.
func runMetrics(ctx context.Context, cfg *Config, reg *metrics.Registry, health func(context.Context) error, log *slog.Logger) {
	if cfg.EnablePprof {
		log.Warn("pprof enabled on the metrics listener", slog.String("addr", cfg.MetricsAddr))
	}

	server := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           metricsMux(cfg, reg, health, log),
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

// metricsMux builds the observability routes.
//
// pprof is opt-in. This listener is on the pod network because the kubelet's
// probe has to reach it, and pprof over that reach hands out heap contents,
// command lines and execution traces to anything that can dial the pod. The
// probe is why the port cannot simply be moved to loopback instead.
func metricsMux(cfg *Config, reg *metrics.Registry, health func(context.Context) error, log *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler())
	mux.HandleFunc("/healthz", healthHandler(health, log))

	if cfg.EnablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	return mux
}

// healthHandler answers the kubelet's liveness probe.
//
// A failed check is a 503 rather than a log line, because the whole reason this
// endpoint exists is to get a wedged daemon restarted. The log line is there
// too: three of these in a row kills the pod, and the operator should be able
// to find out why afterwards.
func healthHandler(health func(context.Context) error, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if health == nil {
			http.Error(w, "no health check configured", http.StatusServiceUnavailable)
			return
		}

		if err := health(r.Context()); err != nil {
			log.Warn("liveness check failed", slog.Any("err", err))
			http.Error(w, err.Error(), http.StatusServiceUnavailable)

			return
		}

		w.WriteHeader(http.StatusOK)

		_, _ = w.Write([]byte("ok")) //nolint:errcheck // best effort
	}
}
