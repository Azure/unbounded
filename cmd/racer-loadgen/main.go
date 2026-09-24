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

	"github.com/Azure/unbounded/internal/version"
	"github.com/Azure/unbounded/pkg/racersdk"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(version.String())
		return
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	c, err := parseConfig(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}

	if err == nil && c.showVersion {
		fmt.Println(version.String())
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

func managementHandler(reg *prometheus.Registry) http.Handler {
	metrics := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	// Dispatch exact raw targets without ServeMux's path cleaning redirects.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.RequestURI {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
		case "/metrics":
			metrics.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func serve(ctx context.Context, c config) error {
	if c.mode == "container-image" {
		return serveImages(ctx, c)
	}
	// Validate the endpoint before opening a listener.
	client, err := racersdk.NewClient(c.endpoint, racersdk.ClientOptions{Concurrency: c.pageConcurrency})
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

	d := newDataset(ctx, c)
	defer d.Close()

	reg := prometheus.NewRegistry()
	m := newMetrics(reg)

	listener, err := net.Listen("tcp", c.listen)
	if err != nil {
		return err
	}
	defer listener.Close() //nolint:errcheck // Best effort cleanup after server shutdown or setup failure.

	originListener, err := listenOrigin(c.originSocket)
	if err != nil {
		return err
	}
	defer originListener.Close() //nolint:errcheck // Best effort cleanup after server shutdown or setup failure.

	origin, err := racersdk.NewOrigin(d)
	if err != nil {
		return err
	}

	server := &http.Server{
		Handler: managementHandler(reg), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second,
		WriteTimeout: c.timeout, BaseContext: func(net.Listener) context.Context { return ctx },
	}
	originServer := &http.Server{
		Handler: origin, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second,
		WriteTimeout: c.timeout, BaseContext: func(net.Listener) context.Context { return ctx },
	}
	serverDone := make(chan error, 2)

	go func() {
		err := server.Serve(listener)
		serverDone <- err

		cancel() // An unexpected listener failure must also stop the load workers.
	}()
	go func() {
		serverDone <- originServer.Serve(originListener)

		cancel()
	}()

	slog.Info("loadgen started", "endpoint", c.endpoint, "origin_socket", c.originSocket, "listen", listener.Addr().String(),
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

	if err := originServer.Shutdown(shutdown); err != nil {
		originServer.Close() //nolint:errcheck // Best effort cleanup after shutdown failure.
	}

	serverErr := <-serverDone
	originErr := <-serverDone
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

	if !errors.Is(originErr, http.ErrServerClosed) {
		return fmt.Errorf("HTTP server: %w", originErr)
	}

	return nil
}
