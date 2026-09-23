// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func serveImages(ctx context.Context, c config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	reg := prometheus.NewRegistry()
	m := newImageMetrics(reg)
	registry := newImageRegistry(m)

	var ready atomic.Bool

	management := managementHandler(reg)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "/readyz" {
			if !ready.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
			}

			return
		}

		management.ServeHTTP(w, r)
	})

	var servers []*http.Server

	done := make(chan error, 2)
	startServer := func(address string, handler http.Handler) error {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return err
		}

		server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second, WriteTimeout: c.timeout, BaseContext: func(net.Listener) context.Context { return ctx }}
		servers = append(servers, server)

		go func() {
			err := server.Serve(listener)
			done <- err

			cancel()
		}()

		slog.Info("image service listening", "address", listener.Addr().String(), "role", c.role)

		return nil
	}

	defer func() {
		cancel()

		for _, server := range servers {
			_ = server.Close() //nolint:errcheck // Final cleanup after bounded shutdown.
		}
	}()

	if err := startServer(c.listen, handler); err != nil {
		return err
	}

	if c.role == "registry" {
		if err := startServer(c.registryListen, registry); err != nil {
			return err
		}
	}

	client := imageHTTPClient()
	defer client.CloseIdleConnections()

	var (
		catalog  imageCatalog
		setupErr error
	)

	if c.role == "registry" {
		setupErr = registry.prepare(ctx, c)
		catalog = registry.catalog
	} else {
		// Registry preparation can be long for large footprints. Keep health and
		// metrics live while waiting, and honor cancellation between retries.
		for ctx.Err() == nil {
			catalog, setupErr = fetchCatalog(ctx, client, c)
			if setupErr == nil {
				break
			}

			slog.Warn("waiting for image catalog", "error", setupErr)

			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
	}

	if setupErr == nil && ctx.Err() == nil {
		ready.Store(true)

		runCtx := ctx

		if c.duration > 0 {
			var stop context.CancelFunc

			runCtx, stop = context.WithTimeout(ctx, c.duration)
			defer stop()
		}

		slog.Info("image service ready", "role", c.role, "images", len(catalog.Images), "gantry_endpoint", c.gantryEndpoint, "seed", c.seed, "concurrency", c.concurrency, "layer_concurrency", c.layerConcurrency)

		if c.role == "load" {
			runImageLoad(runCtx, client, c, catalog, m)
		} else {
			<-runCtx.Done()
		}
	}

	ready.Store(false)

	interrupted := ctx.Err() != nil

	cancel()

	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()

	for _, server := range servers {
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close() //nolint:errcheck // Shutdown fallback.
		}
	}

	for range servers {
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("image HTTP server: %w", err)
		}
	}

	families, err := reg.Gather()
	if err != nil {
		return err
	}

	for _, family := range families {
		if family.GetName() == "racer_loadgen_image_pulls_total" {
			for _, metric := range family.Metric {
				slog.Info("image pull totals", "result", metric.Label[0].GetValue(), "count", metric.GetCounter().GetValue())
			}
		}

		if family.GetName() == "racer_loadgen_image_received_bytes_total" {
			slog.Info("image loadgen stopped", "received_bytes", family.Metric[0].GetCounter().GetValue())
		}
	}

	if setupErr != nil && !interrupted {
		return setupErr
	}

	return nil
}
