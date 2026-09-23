// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Azure/unbounded/internal/gantry/cdsub"
	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/discovery"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/metrics"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	racermeta "github.com/Azure/unbounded/internal/racer"
	sdk "github.com/Azure/unbounded/pkg/racer"
)

// runRacerAgent deliberately has no transfer client/server, chair client/server,
// coordinator, advertiser, or content-selection dependencies. libp2p retains a
// separate namespace, and incompatible direct coord streams fail negotiation.
func runRacerAgent(ctx context.Context, c *config.Config, origin ifaces.OriginPuller, reg *metrics.Registry, inst *phase1Metrics, p2 *phase2Metrics, p9 *phase9Metrics, progress *layerProgressTracker, logger *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	opts := racerDiscoveryOptions(c)

	disco, err := discovery.New(ctx, opts)
	if err != nil {
		return err
	}
	defer disco.Close() //nolint:errcheck // Shutdown cleanup.

	src := newContainerdImageSource(c, logger)
	if closer, ok := src.(io.Closer); ok {
		defer closer.Close() //nolint:errcheck // Shutdown cleanup.
	}

	store, local, _, err := buildContainerdStorage(c, src, logger, p9)
	if err != nil {
		return err
	}

	ranges, ok := origin.(ifaces.OriginRangePuller)
	if !ok {
		return errors.New("racer requires a range-capable registry client")
	}

	cacheSocket, originSocket, err := racermeta.CacheSockets(racermeta.SocketRoot, c.RacerCacheName)
	if err != nil {
		return err
	}

	client, err := sdk.NewClient(cacheSocket, sdk.ClientOptions{Timeout: c.PeerFetchTimeout})
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()

	registries := make(map[string]bool)
	for _, registry := range c.UpstreamRegistries {
		registries[registry.Name] = true
	}

	handler, err := sdk.NewRangeOrigin(&gantryracer.Origin{Local: store, Registry: ranges, Registries: registries})
	if err != nil {
		return err
	}
	// Origin is serving before the mirror listener and its startup gate exist.
	originHTTP, originErrors, err := startRacerOrigin(originSocket, handler)
	if err != nil {
		return err
	}
	defer originHTTP.Close() //nolint:errcheck // Shutdown cleanup.

	spliceCalls := reg.NewCounter("racer", prometheus.CounterOpts{Name: "gantry_racer_splice_calls_total", Help: "Actual SDK splice syscalls."})
	spliceBytes := reg.NewCounter("racer", prometheus.CounterOpts{Name: "gantry_racer_splice_bytes_total", Help: "Payload bytes forwarded with splice."})
	teeCalls := reg.NewCounter("racer", prometheus.CounterOpts{Name: "gantry_racer_tee_calls_total", Help: "Actual SDK verification tee syscalls."})
	teeBytes := reg.NewCounter("racer", prometheus.CounterOpts{Name: "gantry_racer_tee_bytes_total", Help: "Bytes duplicated for SHA-256 verification."})
	bufferedBytes := reg.NewCounter("racer", prometheus.CounterOpts{Name: "gantry_racer_buffered_bytes_total", Help: "Payload bytes forwarded through userspace."})
	streams := reg.NewCounterVec("racer", prometheus.CounterOpts{Name: "gantry_racer_stream_total", Help: "Racer response outcomes; partial is not full digest verification."}, []string{"outcome"})
	fallback := reg.NewCounter("racer", prometheus.CounterOpts{Name: "gantry_racer_fallback_total", Help: "Pre-header ordinary registry fallbacks."})
	available := reg.NewGauge("racer", prometheus.GaugeOpts{Name: "gantry_racer_available", Help: "Cache and origin UDS and containerd readiness."})
	tracker := newStreamCommitTracker(store, logger,
		func(n int) { p9.containerdCommitObserved.Add(float64(n)) },
		func(d time.Duration) { p9.containerdCommitObserveDur.Observe(d.Seconds()) },
		func(n int) { p9.commitMissingAfterStream.Add(float64(n)) })

	go func() {
		if err := tracker.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("stream tracker stopped", slog.Any("err", err))
		}
	}()

	server := mirror.New(c, local, origin,
		mirror.WithLogger(logger), mirror.WithLiveStreamThrough(), mirror.WithStartupReadinessGate(),
		mirror.WithRacer(&gantryracer.Backend{Client: client}, func(stats sdk.TransferStats, partial bool, err error) {
			spliceCalls.Add(float64(stats.SpliceCalls))
			spliceBytes.Add(float64(stats.SpliceBytes))
			teeCalls.Add(float64(stats.TeeCalls))
			teeBytes.Add(float64(stats.TeeBytes))
			bufferedBytes.Add(float64(stats.BufferedBytes))

			outcome := "verified"
			if partial {
				outcome = "partial"
			}

			if err != nil {
				outcome = "aborted"
			}

			if errors.Is(err, sdk.ErrDigestMismatch) {
				outcome = "digest_mismatch"
			}

			streams.WithLabelValues(outcome).Inc()
		}, fallback.Inc),
		mirror.WithMetrics(inst.cacheHit.Inc, inst.cacheMiss.Inc),
		mirror.WithByteMetrics(func(kind, source string, bytes int64) {
			p2.mirrorServeBytes.WithLabelValues(kind, source).Add(float64(bytes))
		}),
		mirror.WithLiveStreamCompletedHook(tracker.RecordCompleted),
		mirror.WithMirrorResponseCompletedHook(func(d digest.Digest, kind, source string) {
			progress.completed(d)
			p2.mirrorCompletedAt.WithLabelValues(kind, source).SetToCurrentTime()
		}),
		mirror.WithOriginStreamMetrics(func(k string) { p9.originStreamStarted.WithLabelValues(k).Inc() }, func(k string) { p9.originStreamCompleted.WithLabelValues(k).Inc() }, func(k string) { p9.originStreamFailed.WithLabelValues(k).Inc() }),
		// Demand-only: observe committed manifests, without speculative registry
		// downloads or chair work competing with Racer's page fetch ownership.
		mirror.WithLayerPrefetcher(newLayerPrefetcher(nil, local, logger, progress.observeManifest)),
	)

	stopMirror, err := server.ListenAndServe(c.MirrorListen)
	if err != nil {
		return err
	}

	defer func() {
		cancel()
		server.Drain()

		shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()

		_ = stopMirror(shutdown) //nolint:errcheck // Bounded shutdown cleanup.
	}()

	subscriber := cdsub.New(src, nil, cdsub.WithLogger(logger))

	go func() {
		if err := subscriber.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("containerd observer stopped", slog.Any("err", err))
		}
	}()

	var ready atomic.Bool

	check := func() bool {
		probeCtx, done := context.WithTimeout(ctx, time.Second)
		defer done()

		if store.Ping(probeCtx) != nil {
			return false
		}

		return racerSocketReady(probeCtx, originSocket) && racerSocketReady(probeCtx, cacheSocket)
	}

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			ok := check()
			ready.Store(ok)

			if ok {
				available.Set(1)
				server.MarkReady()
			} else {
				available.Set(0)
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	opsHTTP, opsErrors := startOpsEndpoint(c.MetricsListen, reg, func() (string, bool) { return "Racer cache/origin UDS or containerd unavailable", ready.Load() }, logger)
	defer opsHTTP.Close() //nolint:errcheck // Shutdown cleanup.

	if c.PprofListen != "" {
		pprof, _, listenErr := startPprofEndpoint(c.PprofListen, logger)
		if listenErr != nil {
			logger.Warn("pprof endpoint unavailable", slog.Any("err", listenErr))
		} else {
			defer pprof.Close() //nolint:errcheck // Shutdown cleanup.
		}
	}

	logger.Info("Racer backend listening", slog.String("cache_socket", cacheSocket), slog.String("origin_socket", originSocket), slog.String("prefetch", "demand-only"))

	select {
	case <-ctx.Done():
		return nil
	case err := <-originErrors:
		return fmt.Errorf("racer origin: %w", err)
	case err := <-opsErrors:
		return fmt.Errorf("operations endpoint: %w", err)
	}
}

func startRacerOrigin(socket string, handler http.Handler) (*http.Server, <-chan error, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0o750); err != nil {
		return nil, nil, err
	}

	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, nil, errors.New("racer origin path is not a socket")
		}

		conn, dialErr := net.DialTimeout("unix", socket, time.Second)
		if dialErr == nil {
			_ = conn.Close() //nolint:errcheck // Probe connection cleanup.
			return nil, nil, errors.New("racer origin socket already active")
		}

		if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("probe existing racer origin socket: %w", dialErr)
		}

		if err := os.Remove(socket); err != nil {
			return nil, nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}

	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, nil, err
	}

	if err := os.Chmod(socket, 0o660); err != nil {
		_ = listener.Close() //nolint:errcheck // Failed startup cleanup.
		return nil, nil, err
	}

	admission := make(chan struct{}, 64)
	bounded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case admission <- struct{}{}:
			defer func() { <-admission }()

			handler.ServeHTTP(w, r)
		default:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	server := &http.Server{Handler: bounded, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, WriteTimeout: 15 * time.Minute, MaxHeaderBytes: 72 << 10}
	done := make(chan error, 1)

	go func() { done <- server.Serve(listener) }()

	return server, done, nil
}

// A reserved invalid target must reach HTTP and return 404. A mere filesystem
// existence check could release readiness before either socket is serving.
func racerSocketReady(ctx context.Context, socket string) bool {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "http://localhost/gantry-readiness", nil)
	if err != nil {
		return false
	}

	response, err := client.Do(req)
	if err != nil {
		return false
	}
	defer response.Body.Close() //nolint:errcheck // HEAD response cleanup.

	return response.StatusCode == http.StatusNotFound
}

func racerDiscoveryOptions(c *config.Config) discovery.Options {
	opts := discovery.FromConfig(c)
	opts.ProtocolPrefix, opts.SelfTestPeriod = "/gantry/racer", 0

	return opts
}
