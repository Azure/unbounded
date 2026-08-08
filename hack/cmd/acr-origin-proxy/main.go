// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownGrace     = 5 * time.Second
)

func main() {
	var err error
	if len(os.Args) <= 1 {
		err = run()
	} else {
		switch os.Args[1] {
		case "set-phase":
			err = runSetPhase(os.Args[2:])
		case "get-url":
			err = runURLCheck(os.Args[2:], os.Stdout, false)
		case "check-url":
			err = runURLCheck(os.Args[2:], io.Discard, false)
		case "probe-health":
			err = runURLCheck(os.Args[2:], io.Discard, true)
		default:
			err = fmt.Errorf("unknown subcommand %q", os.Args[1])
		}
	}

	if err != nil {
		slog.Error("ACR origin proxy stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := configFromEnv()
	if err != nil {
		return fmt.Errorf("configure proxy: %w", err)
	}

	controller, err := newPhaseController(cfg.runID)
	if err != nil {
		return fmt.Errorf("configure phase controller: %w", err)
	}

	registry := prometheus.NewRegistry()
	observer := newObserver(registry, time.Now(), controller)
	cache := newTokenCache(cfg.refreshSkewSecs)

	proxyMux := http.NewServeMux()
	proxyMux.HandleFunc("/healthz", healthzHandler)
	proxyMux.Handle("/", proxyHandler(cfg, cache, observer, http.DefaultClient))

	metricsMux := http.NewServeMux()
	metricsMux.HandleFunc("/healthz", healthzHandler)
	metricsMux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	metricsMux.Handle("/debug/summary", summaryHandler(observer))
	metricsMux.Handle("/control/phase", requireControlToken(cfg.controlToken, phaseControlHandler(controller)))

	proxyServer := &http.Server{
		Addr:              cfg.listen,
		Handler:           proxyMux,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	metricsServer := &http.Server{
		Addr:              cfg.metricsListen,
		Handler:           metricsMux,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errorChannel := make(chan error, 2)
	go serve(errorChannel, "proxy", proxyServer)
	go serve(errorChannel, "metrics", metricsServer)

	slog.Info(
		"ACR origin proxy started",
		"run_id", cfg.runID,
		"proxy_address", cfg.listen,
		"metrics_address", cfg.metricsListen,
		"upstream", cfg.upstream.Redacted(),
	)

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errorChannel:
		stop()
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	shutdownErr := errors.Join(
		proxyServer.Shutdown(shutdownContext),
		metricsServer.Shutdown(shutdownContext),
	)

	return errors.Join(serveErr, shutdownErr)
}

func serve(errorChannel chan<- error, name string, server *http.Server) {
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errorChannel <- fmt.Errorf("serve %s: %w", name, err)
	}
}

func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n") //nolint:errcheck // Health checks only need the status code.
}
