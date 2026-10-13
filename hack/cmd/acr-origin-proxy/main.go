// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
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

	"github.com/Azure/unbounded/internal/logger"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownGrace     = 5 * time.Second
)

// usage is printed by -h and after a flag error, followed by the flags.
const usage = `Usage: acr-origin-proxy [flags] [subcommand [arguments]]

Without a subcommand, runs the proxy, configured from the environment.

Subcommands:
  set-phase <setup|baseline|gantry_cold|idle> <timeout>
        Set the running proxy's benchmark phase.
  get-url <url> <timeout> [accept]
        Fetch a URL and print the response body.
  check-url <url> <timeout> [accept]
        Fetch a URL and discard the body.
  probe-health <url> <timeout> [accept]
        Fetch a URL, then wait for a signal.

Flags, which come before the subcommand:
`

// errFlags means the flag package has already printed a flag error and the
// usage text.
var errFlags = errors.New("invalid flags")

func main() {
	err := runMain(os.Args[1:])

	switch {
	case err == nil:
	case errors.Is(err, errFlags):
		os.Exit(2)
	default:
		slog.Error("ACR origin proxy stopped", "error", err)
		os.Exit(1)
	}
}

// parseLogFlags reads --log-level and --log-format, which come before the
// subcommand name, and returns the remaining arguments.
func parseLogFlags(args []string, output io.Writer) (logger.Options, []string, error) {
	var logOpts logger.Options

	fs := flag.NewFlagSet("acr-origin-proxy", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		if _, err := fmt.Fprint(fs.Output(), usage); err == nil {
			fs.PrintDefaults()
		}
	}

	logOpts.BindGoFlags(fs)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return logger.Options{}, nil, err
		}

		return logger.Options{}, nil, fmt.Errorf("%w: %w", errFlags, err)
	}

	return logOpts, fs.Args(), nil
}

func runMain(args []string) error {
	logOpts, args, err := parseLogFlags(args, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}

	if err != nil {
		return err
	}

	logOpts.Setup(os.Stderr)

	if len(args) == 0 {
		return run()
	}

	switch args[0] {
	case "set-phase":
		return runSetPhase(args[1:])
	case "get-url":
		return runURLCheck(args[1:], os.Stdout, false)
	case "check-url":
		return runURLCheck(args[1:], io.Discard, false)
	case "probe-health":
		return runURLCheck(args[1:], io.Discard, true)
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
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
