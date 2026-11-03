// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/unbounded/internal/racerobject"
	"github.com/Azure/unbounded/internal/version"
	"github.com/Azure/unbounded/pkg/racersdk"
)

type options struct {
	mode           string
	cache          string
	api            string
	requestTimeout time.Duration
	listen         string
	debugListen    string
	maxConnections int
}

const (
	// defaultSidecarConnections bounds both accepted client connections and
	// concurrent Racer reads.
	defaultSidecarConnections = 128
	maxConcurrencyFlag        = 65536
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "racer-object:", err)
		os.Exit(1)
	}
}

func parseOptions(args []string, output io.Writer) (options, error) {
	var o options
	if len(args) == 0 {
		return o, errors.New("expected sidecar (use --help)")
	}

	switch args[0] {
	case "--help", "-h":
		return o, writeHelp(output, "Usage: racer-object sidecar [flags]\nUse racer-object sidecar --help for flags.\n")
	case "--version":
		return o, writeHelp(output, version.String()+"\n")
	case "sidecar":
		o.mode = args[0]
	default:
		return o, errors.New("unknown command (use --help)")
	}

	fs := flag.NewFlagSet(o.mode, flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var helpErr error

	fs.Usage = func() {
		var defaults bytes.Buffer

		fs.SetOutput(&defaults)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)

		helpErr = writeHelp(output, "Usage: racer-object "+o.mode+" [flags]\n"+defaults.String())
	}
	cache := fs.String("cache", "", "Racer cache name (ClusterCache); required")
	fs.StringVar(&o.debugListen, "debug-listen", "", "Optional pprof address (e.g. 127.0.0.1:6060); disabled when empty. Trusted diagnostic access only: nonloopback addresses such as :6060 expose unauthenticated profiles")

	showVersion := fs.Bool("version", false, "Print version")
	fs.StringVar(&o.api, "api", "s3", "Sidecar API: s3 or azure")
	fs.StringVar(&o.listen, "listen", "127.0.0.1:8080", "Loopback IP literal and port")
	fs.DurationVar(&o.requestTimeout, "request-timeout", 5*time.Minute, "Positive sidecar request timeout")
	fs.IntVar(&o.maxConnections, "max-connections", defaultSidecarConnections, "Concurrent client connections and Racer reads; 1..65536")

	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return o, helpErr
		}

		return o, errors.New("invalid flags (use --help)")
	}

	if fs.NArg() != 0 {
		return o, errors.New("unexpected positional arguments")
	}

	if *showVersion {
		return o, writeHelp(output, version.String()+"\n")
	}

	o.cache = *cache
	if o.cache == "" {
		return o, errors.New("--cache is required")
	}

	validationClient, err := racersdk.NewClient(racersdk.ClientConfig{Cache: o.cache})
	if err != nil {
		return o, errors.New("invalid --cache")
	}

	closeResource(validationClient)

	if o.requestTimeout <= 0 {
		return o, errors.New("--request-timeout must be positive")
	}

	if o.maxConnections < 1 || o.maxConnections > maxConcurrencyFlag {
		return o, errors.New("--max-connections must be between 1 and 65536")
	}

	if o.api != "s3" && o.api != "azure" {
		return o, errors.New("--api must be s3 or azure")
	}

	if err := validateListen(o.listen); err != nil {
		return o, err
	}

	return o, nil
}

func writeHelp(output io.Writer, text string) error {
	if _, err := io.WriteString(output, text); err != nil {
		return errors.New("could not write CLI output")
	}

	return flag.ErrHelp
}

func closeResource(c io.Closer) {
	if err := c.Close(); err != nil {
		return
	}
}

func validateListen(value string) error {
	host, port, err := net.SplitHostPort(value)

	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		return errors.New("--listen must use a loopback IP literal and port")
	}

	if port == "" || strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) != -1 {
		return errors.New("--listen port must be between 0 and 65535")
	}

	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return errors.New("--listen port must be between 0 and 65535")
	}

	return nil
}

func run(ctx context.Context, args []string, output io.Writer) error {
	o, err := parseOptions(args, output)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}

	if err != nil {
		return err
	}

	if ctx.Err() != nil {
		return nil
	}

	return runWithDebug(ctx, o.debugListen, func(ctx context.Context) error {
		return runSidecar(ctx, o)
	})
}

func runSidecar(ctx context.Context, o options) error {
	client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: o.cache, MaxConnections: o.maxConnections})
	if err != nil {
		return errors.New("could not configure Racer client")
	}
	defer closeResource(client)

	handler, err := createSidecar(client, o)
	if err != nil {
		return err
	}

	listener, err := listenSidecar(ctx, o.listen)
	if err != nil {
		return err
	}

	return serveSidecar(ctx, listener, handler, o.requestTimeout, o.maxConnections)
}

func createSidecar(client *racersdk.Client, o options) (http.Handler, error) {
	handler, err := racerobject.NewSidecar(client, racerobject.SidecarConfig{API: o.api})
	if err != nil {
		return nil, errors.New("could not configure sidecar")
	}

	return handler, nil
}

func listenSidecar(ctx context.Context, address string) (net.Listener, error) {
	if err := validateListen(address); err != nil {
		return nil, err
	}

	var lc net.ListenConfig

	l, err := lc.Listen(ctx, "tcp", address)
	if err != nil {
		return nil, errors.New("could not bind sidecar listener")
	}

	return l, nil
}

func serveSidecar(ctx context.Context, listener net.Listener, handler http.Handler, requestTimeout time.Duration, maxConnections int) error {
	lifetime, cancel := context.WithCancel(ctx)
	defer cancel()

	server := &http.Server{
		ReadHeaderTimeout: min(5*time.Second, requestTimeout),
		ReadTimeout:       requestTimeout, WriteTimeout: requestTimeout, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 32 << 10, ErrorLog: log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context { return lifetime },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCtx, done := context.WithTimeout(r.Context(), requestTimeout)
			defer done()

			handler.ServeHTTP(w, r.WithContext(requestCtx))
		}),
	}
	defer closeResource(server)
	defer closeResource(listener)

	finished := make(chan error, 1)

	go func() { finished <- server.Serve(limitSidecarListener(listener, maxConnections)) }()

	select {
	case err := <-finished:
		if ctx.Err() != nil {
			return nil
		}

		if err != nil {
			return errors.New("sidecar server failed")
		}

		return nil
	case <-ctx.Done():
		cancel()

		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()

		if err := server.Shutdown(shutdownCtx); err != nil {
			closeResource(server)
		}

		<-finished

		return nil
	}
}
