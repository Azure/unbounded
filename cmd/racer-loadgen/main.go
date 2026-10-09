// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// racer-loadgen serves deterministic synthetic blobs and repeatedly reads them
// through Gantry or the Racer SDK, without a local content cache.
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

	"github.com/Azure/unbounded/pkg/racersdk"
)

type options struct {
	s3             s3Options
	image          imageOptions
	pull           pullOptions
	listen         string
	metricsListen  string
	startDelay     time.Duration
	duration       time.Duration
	catalogImages  int
	catalogBlobs   int
	blobBytes      int64
	startupTimeout time.Duration
}

func parseOptions(args []string, output io.Writer) (options, error) {
	var opts options

	f := flag.NewFlagSet("racer-loadgen", flag.ContinueOnError)
	f.SetOutput(output)
	f.StringVar(&opts.pull.Backend, "backend", "gantry", "Acquisition and origin transport: gantry (OCI HTTP), uds (direct Racer SDK), or s3 (HTTP object GET)")
	f.StringVar(&opts.pull.Volume, "volume", "", "Racer volume name; required for uds, using /run/racer/<volume>/{client,origin}/socket")
	f.StringVar(&opts.s3.Endpoint, "endpoint", "", "S3 HTTP endpoint: racer-object sidecar or direct synthetic origin (default http://127.0.0.1:8080)")
	f.StringVar(&opts.s3.Bucket, "bucket", "", "Synthetic S3 bucket (default benchmark; requires backend=s3)")
	f.IntVar(&opts.s3.Count, "object-count", 0, "Synthetic S3 object count, 1-512 (default 128)")
	f.Int64Var(&opts.s3.Bytes, "object-bytes", 0, "Exact bytes per S3 object (default 67108864)")
	f.BoolVar(&opts.s3.Origin, "s3-origin", false, "Serve synthetic S3 on listen; requires backend=s3, use concurrency=0 for origin only")
	f.StringVar(&opts.listen, "listen", ":8080", "Synthetic registry listen address")
	f.StringVar(&opts.metricsListen, "metrics-listen", ":9090", "Metrics and health listen address")
	f.StringVar(&opts.image.Repository, "repository", "benchmark/image", "Synthetic image repository (tag: latest)")
	f.IntVar(&opts.image.Layers, "layers", 8, "Number of synthetic layers")
	f.Int64Var(&opts.image.LayerBytes, "layer-bytes", 64<<20, "Payload bytes per layer, before jitter and tar overhead")
	f.Float64Var(&opts.image.Jitter, "jitter", 0.2, "Deterministic per-layer size jitter fraction in [0,1)")
	f.StringVar(&opts.image.Seed, "seed", "benchmark-v1", "Content seed; keep identical on all nodes")
	f.IntVar(&opts.catalogImages, "catalog-images", 1, "Number of deterministic images, 1-512; keep identical on all origins")
	f.IntVar(&opts.catalogBlobs, "catalog-blobs", 0, "Select generic workload: 1-512 independent raw blobs, one blob per operation; excludes image sizing flags")
	f.Int64Var(&opts.blobBytes, "blob-bytes", 64<<20, "Exact bytes per generic blob (no jitter or tar framing); requires catalog-blobs")
	f.StringVar(&opts.pull.Profile, "profile", profileShuffle, "Catalog selection profile: shuffle or zipf")
	f.Float64Var(&opts.pull.ZipfExponent, "zipf-exponent", defaultZipfExponent, "Finite positive Zipf exponent; larger values increase skew (zipf profile only)")
	f.DurationVar(&opts.startupTimeout, "startup-timeout", 0, "Deadline for catalog generation, hashing, and origin startup; zero disables the overall deadline (UDS readiness is always bounded)")
	f.StringVar(&opts.pull.Target, "target", "http://127.0.0.1:5000", "Gantry mirror URL (or origin URL for baseline)")
	f.StringVar(&opts.pull.Namespace, "namespace", "loadgen.invalid", "Gantry upstream registry name sent as ns query parameter")
	f.IntVar(&opts.pull.Concurrency, "concurrency", 64, "Concurrent blob-batch operations (one image or one generic blob); zero serves only the origin")
	f.StringVar(&opts.pull.ConcurrencyFile, "concurrency-file", "", "Optional regular file containing concurrency 0-256; polled every second without restarting the origin")
	f.StringVar(&opts.pull.NodeCapsFile, "node-concurrency-caps-file", "", "Optional versioned node-cap JSON key in the same projected ConfigMap as concurrency-file")
	f.StringVar(&opts.pull.NodeName, "node-name", "", "Exact Kubernetes node name for optional node concurrency caps")
	f.IntVar(&opts.pull.LayerConcurrency, "blob-concurrency", 4, "Concurrent blob requests per batch (generic single-blob operations use one)")
	f.IntVar(&opts.pull.LayerConcurrency, "layer-concurrency", 4, "Compatibility alias for blob-concurrency")
	f.DurationVar(&opts.pull.Timeout, "pull-timeout", 2*time.Minute, "Deadline for one complete blob-batch operation")
	f.DurationVar(&opts.pull.RetryDelay, "retry-delay", time.Second, "Per-worker delay after failed pulls")
	f.DurationVar(&opts.pull.Interval, "interval", 0, "Per-worker delay after successful pulls")
	f.BoolVar(&opts.pull.Verify, "verify", false, "Verify SHA-256 for every downloaded object (copies bytes instead of the default no-copy drain)")
	f.BoolVar(&opts.pull.DiagnoseIntegrity, "diagnose-integrity", false, "Compare deterministic bytes and retain bounded page hashes on integrity failure (requires verify)")
	f.DurationVar(&opts.startDelay, "start-delay", 10*time.Second, "Delay after origin readiness before starting workers")
	f.DurationVar(&opts.duration, "duration", 0, "Load phase duration; zero runs until signaled")

	if err := f.Parse(args); err != nil {
		return opts, err
	}

	if f.NArg() != 0 {
		return opts, errors.New("unexpected positional arguments")
	}

	seen := make(map[string]bool)

	f.Visit(func(flag *flag.Flag) { seen[flag.Name] = true })

	if opts.pull.Backend != "gantry" && opts.pull.Backend != "uds" && opts.pull.Backend != "s3" {
		return opts, errors.New("backend must be gantry, uds, or s3")
	}

	if err := configureS3Options(&opts, seen); err != nil {
		return opts, err
	}

	if opts.pull.Backend == "uds" {
		if opts.pull.Volume == "" {
			return opts, fmt.Errorf("uds requires a valid volume: empty volume name: %w", racersdk.ErrInvalidRequest)
		}

		client, err := racersdk.NewClient(racersdk.ClientConfig{Volume: opts.pull.Volume})
		if err != nil {
			return opts, fmt.Errorf("uds requires a valid volume: %w", err)
		}

		if err := client.Close(); err != nil {
			return opts, err
		}
	}

	if seen["blob-concurrency"] && seen["layer-concurrency"] {
		return opts, errors.New("specify only one of blob-concurrency and layer-concurrency")
	}

	if seen["catalog-blobs"] {
		if opts.catalogBlobs < 1 || opts.catalogBlobs > maxCatalogImages || opts.blobBytes < 1 {
			return opts, errors.New("catalog-blobs must be in [1, 512] and blob-bytes must be positive")
		}

		for _, name := range []string{"catalog-images", "layers", "layer-bytes", "jitter"} {
			if seen[name] {
				return opts, fmt.Errorf("catalog-blobs cannot be combined with %s", name)
			}
		}
	} else if seen["blob-bytes"] {
		return opts, errors.New("blob-bytes requires catalog-blobs")
	}

	if opts.startDelay < 0 || opts.duration < 0 {
		return opts, errors.New("start-delay and duration must be nonnegative")
	}

	if opts.catalogImages < 1 || opts.catalogImages > maxCatalogImages {
		return opts, fmt.Errorf("catalog-images must be in [1, %d]", maxCatalogImages)
	}

	if opts.startupTimeout < 0 {
		return opts, errors.New("startup-timeout must be nonnegative")
	}

	if err := validateProfile(opts.pull.Profile, opts.pull.ZipfExponent); err != nil {
		return opts, err
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
	return runWithOriginStarter(parent, opts, startUDSOrigin)
}

type originStarter func(context.Context, context.Context, racersdk.OriginConfig, racersdk.Origin, racersdk.Key, func(error)) (func(), error)

func runWithOriginStarter(parent context.Context, opts options, startOrigin originStarter) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	reg := prometheus.NewRegistry()
	metrics := newMetrics(reg)

	p, err := newPuller(&syntheticImage{}, opts.pull, metrics)
	if err != nil {
		return err
	}
	defer p.close()

	slog.Info("catalog selection configured", "profile", p.opts.Profile, "zipf_exponent", p.opts.ZipfExponent)

	var ready atomic.Pointer[blobCatalog]

	ops := http.NewServeMux()
	ops.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	ops.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	ops.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready.Load() == nil || ctx.Err() != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
	})
	origin := metrics.instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		catalog := ready.Load()
		if catalog == nil {
			http.Error(w, "catalog initializing", http.StatusServiceUnavailable)
			return
		}

		if opts.pull.Backend == "s3" {
			catalog.s3Handler(opts.s3.Bucket).ServeHTTP(w, r)
		} else {
			catalog.handler().ServeHTTP(w, r)
		}
	}))

	servers := []*http.Server{
		{Addr: opts.metricsListen, Handler: ops, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute},
	}
	if p.opts.Backend == "gantry" || opts.s3.Origin {
		servers = append(servers, &http.Server{Addr: opts.listen, Handler: origin, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute})
	}

	var serving sync.WaitGroup

	serveErrors := make(chan error, len(servers)+1)

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

	slog.Info("generating synthetic catalog", "catalog_images", opts.catalogImages, "catalog_blobs", opts.catalogBlobs, "blob_bytes", opts.blobBytes, "seed", opts.image.Seed)

	startupCtx := ctx

	stopStartup := func() {}
	if opts.startupTimeout > 0 {
		startupCtx, stopStartup = context.WithTimeout(ctx, opts.startupTimeout)
	}

	defer stopStartup()

	var catalog *blobCatalog
	if opts.pull.Backend == "s3" {
		catalog, err = newBlobCatalog(startupCtx, "benchmark/s3", opts.image.Seed, opts.s3.Count, opts.s3.Bytes)
	} else if opts.catalogBlobs > 0 {
		catalog, err = newBlobCatalog(startupCtx, opts.image.Repository, opts.image.Seed, opts.catalogBlobs, opts.blobBytes)
	} else {
		catalog, err = newCatalog(startupCtx, opts.image, opts.catalogImages)
	}

	if err == nil {
		p.img = &syntheticImage{repository: catalog.repository}
		if len(catalog.images) != 0 {
			p.img = catalog.images[0]
		}

		if err := p.configureDiagnostics(catalog); err != nil {
			return err
		}

		p.images = catalog.images

		p.batches = catalog.batches
		if p.opts.Backend == "s3" {
			p.configureS3(catalog, opts.s3.Bucket)
		}

		if p.opts.Backend == "uds" {
			var (
				adapter racersdk.Origin
				probe   racersdk.Key
			)

			adapter, probe, err = syntheticOrigin(catalog, metrics)
			if err == nil {
				capacity := p.opts.Concurrency
				if p.opts.ConcurrencyFile != "" {
					capacity = maxLiveConcurrency
				}

				limit := max(1, capacity*p.opts.BlobConcurrency)

				var stopOrigin func()

				stopOrigin, err = startOrigin(ctx, startupCtx, racersdk.OriginConfig{
					Volume: p.opts.Volume, RecoverStaleSocket: true,
					MaxConcurrentRequests: max(64, limit),
				}, adapter, probe, func(originErr error) { serveErrors <- originErr; cancel() })
				if err == nil {
					defer stopOrigin()
					defer ready.Store(nil)
				}
			}

			if err != nil {
				select {
				case originErr := <-serveErrors:
					return originErr
				default:
				}

				if parent.Err() != nil {
					return nil
				}

				return err
			}
		}

		stopStartup()
		ready.Store(catalog)
		slog.Info("origin ready", "backend", p.opts.Backend, "batches", len(p.batches), "repository", opts.image.Repository, "target", opts.pull.Target, "volume", p.opts.Volume, "concurrency", opts.pull.Concurrency)

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
