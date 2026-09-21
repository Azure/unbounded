// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	racer "github.com/Azure/unbounded/pkg/racer"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	c, err := parseConfig(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}

	if err == nil {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		err = serve(ctx, c)
	}

	if err != nil {
		slog.Error("loadgen failed", "error", err)
		os.Exit(1)
	}
}

func handler(d *dataset, reg *prometheus.Registry) http.Handler {
	origin, _ := racer.NewOrigin(d) //nolint:errcheck // A concrete *dataset always supplies a non-nil Store interface.
	metrics := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	// Dispatch exact raw targets without ServeMux's path cleaning redirects.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.RequestURI {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/metrics":
			metrics.ServeHTTP(w, r)
		default:
			origin.ServeHTTP(w, r)
		}
	})
}

func serve(ctx context.Context, c config) error {
	// Validate the endpoint before opening a listener.
	client, err := racer.NewClient(c.endpoint, racer.ClientOptions{Concurrency: c.pageConcurrency})
	if err != nil {
		return err
	}

	client.CloseIdleConnections()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if c.duration > 0 {
		var stop context.CancelFunc

		ctx, stop = context.WithTimeout(ctx, c.duration)
		defer stop()
	}

	d := newDataset(c)
	reg := prometheus.NewRegistry()
	m := newMetrics(reg)

	listener, err := net.Listen("tcp", c.listen)
	if err != nil {
		return err
	}

	server := &http.Server{
		Handler: handler(d, reg), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second,
		WriteTimeout: c.timeout, BaseContext: func(net.Listener) context.Context { return ctx },
	}
	serverDone := make(chan error, 1)

	go func() {
		err := server.Serve(listener)
		serverDone <- err

		cancel() // An unexpected listener failure must also stop the load workers.
	}()

	slog.Info("loadgen started", "endpoint", c.endpoint, "listen", listener.Addr().String(),
		"footprint_bytes", c.footprint, "object_bytes", c.objectSize, "objects", d.count,
		"exponent", c.exponent, "seed", c.seed, "concurrency", c.concurrency,
		"page_concurrency", c.pageConcurrency, "timeout", c.timeout.String(), "ttl", c.ttl.String(), "duration", c.duration.String())
	loadErr := runLoad(ctx, c, d, m)

	cancel()

	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()

	if err := server.Shutdown(shutdown); err != nil {
		server.Close() //nolint:errcheck // Best effort cleanup after shutdown failure.
	}

	serverErr := <-serverDone
	// A compact final summary also makes finite local runs useful without scraping.
	families, gatherErr := reg.Gather()
	if gatherErr != nil {
		slog.Warn("gather final metrics", "error", gatherErr)
	}

	for _, family := range families {
		if family.GetName() == "racer_loadgen_received_bytes_total" {
			slog.Info("loadgen stopped", "received_bytes", family.Metric[0].GetCounter().GetValue())
		}

		if family.GetName() == "racer_loadgen_downloads_total" {
			for _, metric := range family.Metric {
				slog.Info("download totals", "result", metric.Label[0].GetValue(), "count", metric.GetCounter().GetValue())
			}
		}
	}

	if loadErr != nil {
		return loadErr
	}

	if !errors.Is(serverErr, http.ErrServerClosed) {
		return fmt.Errorf("HTTP server: %w", serverErr)
	}

	return nil
}
