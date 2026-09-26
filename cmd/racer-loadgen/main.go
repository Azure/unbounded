// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// racer-loadgen serves a deterministic synthetic OCI image and repeatedly pulls
// that image through a registry mirror without a local content cache.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type options struct {
	image         imageOptions
	pull          pullOptions
	listen        string
	metricsListen string
	startDelay    time.Duration
	duration      time.Duration
}

func parseOptions(args []string, output io.Writer) (options, error) {
	var opts options

	f := flag.NewFlagSet("racer-loadgen", flag.ContinueOnError)
	f.SetOutput(output)
	f.StringVar(&opts.listen, "listen", ":8080", "Synthetic registry listen address")
	f.StringVar(&opts.metricsListen, "metrics-listen", ":9090", "Metrics and health listen address")
	f.StringVar(&opts.image.Repository, "repository", "benchmark/image", "Synthetic image repository (tag: latest)")
	f.IntVar(&opts.image.Layers, "layers", 8, "Number of synthetic layers")
	f.Int64Var(&opts.image.LayerBytes, "layer-bytes", 64<<20, "Payload bytes per layer, before jitter and tar overhead")
	f.Float64Var(&opts.image.Jitter, "jitter", 0.2, "Deterministic per-layer size jitter fraction in [0,1)")
	f.StringVar(&opts.image.Seed, "seed", "benchmark-v1", "Content seed; keep identical on all nodes")
	f.StringVar(&opts.pull.Target, "target", "http://127.0.0.1:5000", "Gantry mirror URL (or origin URL for baseline)")
	f.StringVar(&opts.pull.Namespace, "namespace", "loadgen.invalid", "Gantry upstream registry name sent as ns query parameter")
	f.IntVar(&opts.pull.Concurrency, "concurrency", 64, "Concurrent image pulls; zero serves only the origin")
	f.IntVar(&opts.pull.LayerConcurrency, "layer-concurrency", 4, "Concurrent layer requests per image pull")
	f.DurationVar(&opts.pull.Timeout, "pull-timeout", 2*time.Minute, "Deadline for one complete image pull")
	f.DurationVar(&opts.pull.RetryDelay, "retry-delay", time.Second, "Per-worker delay after failed pulls")
	f.DurationVar(&opts.pull.Interval, "interval", 0, "Per-worker delay after successful pulls")
	f.BoolVar(&opts.pull.Verify, "verify", true, "Verify SHA-256 for every downloaded object")
	f.DurationVar(&opts.startDelay, "start-delay", 10*time.Second, "Delay after origin readiness before starting workers")
	f.DurationVar(&opts.duration, "duration", 0, "Load phase duration; zero runs until signaled")

	if err := f.Parse(args); err != nil {
		return opts, err
	}

	if f.NArg() != 0 {
		return opts, errors.New("unexpected positional arguments")
	}

	if opts.startDelay < 0 || opts.duration < 0 {
		return opts, errors.New("start-delay and duration must be nonnegative")
	}

	return opts, nil
}

func main() {
	opts, err := parseOptions(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}

	if err == nil {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err = run(ctx, opts)

		stop()
	}

	if err != nil {
		slog.Error("racer-loadgen stopped", "error", err)
		os.Exit(1)
	}
}

func run(parent context.Context, opts options) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	reg := prometheus.NewRegistry()
	metrics := newMetrics(reg)

	p, err := newPuller(&syntheticImage{}, opts.pull, metrics)
	if err != nil {
		return err
	}
	defer p.transport.CloseIdleConnections()

	var ready atomic.Pointer[syntheticImage]

	ops := http.NewServeMux()
	ops.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	ops.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	ops.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready.Load() == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
	})
	origin := metrics.instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		img := ready.Load()
		if img == nil {
			http.Error(w, "image initializing", http.StatusServiceUnavailable)
			return
		}

		img.handler().ServeHTTP(w, r)
	}))
	servers := []*http.Server{
		{Addr: opts.listen, Handler: origin, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute},
		{Addr: opts.metricsListen, Handler: ops, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute},
	}

	var serving sync.WaitGroup

	serveErrors := make(chan error, len(servers))

	defer func() {
		ready.Store(nil)

		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()

		var closing sync.WaitGroup
		for _, server := range servers {
			closing.Go(func() {
				if err := server.Shutdown(shutdownCtx); err != nil {
					if closeErr := server.Close(); closeErr != nil {
						slog.Warn("close HTTP server", "error", closeErr)
					}
				}
			})
		}

		closing.Wait()
		serving.Wait()
	}()

	for _, server := range servers {
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", server.Addr, err)
		}

		serving.Go(func() {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErrors <- err

				cancel()
			}
		})
	}

	slog.Info("generating synthetic image", "layers", opts.image.Layers, "layer_bytes", opts.image.LayerBytes, "seed", opts.image.Seed)

	img, err := newImage(ctx, opts.image)
	if err == nil {
		p.img = img
		ready.Store(img)
		slog.Info("origin ready", "digest", img.Manifest.Digest, "repository", opts.image.Repository, "target", opts.pull.Target, "concurrency", opts.pull.Concurrency)

		if waitPullDelay(ctx, opts.startDelay) {
			loadCtx := ctx

			if opts.duration > 0 {
				var stop context.CancelFunc

				loadCtx, stop = context.WithTimeout(ctx, opts.duration)
				defer stop()
			}

			p.run(loadCtx)
		}
	}

	select {
	case serveErr := <-serveErrors:
		return serveErr
	default:
	}

	if errors.Is(err, context.Canceled) && parent.Err() != nil {
		return nil
	}

	return err
}
