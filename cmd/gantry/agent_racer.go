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
	"net/url"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Azure/unbounded/internal/gantry/cdsub"
	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/metrics"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	racermeta "github.com/Azure/unbounded/internal/racer"
	sdk "github.com/Azure/unbounded/pkg/racersdk"
)

// runRacerAgent deliberately starts no libp2p host, DHT, transfer client/server,
// chair client/server, coordinator, advertiser, or content-selection machinery.
// Direct coordination has no listener; Racer owns peer discovery and transport.
func runRacerAgent(ctx context.Context, c *config.Config, origin gantryracer.Registry, reg *metrics.Registry, inst *phase1Metrics, p2 *phase2Metrics, p9 *phase9Metrics, progress *layerProgressTracker, logger *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	readinessTarget, err := racerReadinessTarget(c.NodeName, os.Hostname)
	if err != nil {
		return err
	}

	src := newContainerdImageSource(c, logger)
	if closer, ok := src.(io.Closer); ok {
		defer closer.Close() //nolint:errcheck // Shutdown cleanup.
	}

	store, local, _, err := buildContainerdStorage(c, src, logger, p9)
	if err != nil {
		return err
	}

	clientSocket, originSocket, err := racermeta.CacheSockets(racermeta.SocketRoot, c.RacerCacheName)
	if err != nil {
		return err
	}

	client, err := sdk.NewClient(clientSocket, sdk.ClientOptions{Timeout: c.PeerFetchTimeout, PageLookahead: true})
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()

	registries := make(map[string]bool)
	for _, registry := range c.UpstreamRegistries {
		registries[registry.Name] = true
	}

	handler, err := sdk.NewRangeOrigin(&gantryracer.Origin{Local: store, Registry: origin, Registries: registries})
	if err != nil {
		return err
	}
	// Origin is serving before the mirror listener and its startup gate exist.
	originHTTP, originErrors, err := startRacerOrigin(originSocket, handler)
	if err != nil {
		return err
	}
	defer originHTTP.Close() //nolint:errcheck // Shutdown cleanup.

	onRacerStream := newRacerStreamMetrics(reg)
	reg.NewCounter("racer", prometheus.CounterOpts{Name: "gantry_racer_fallback_total", Help: "Deprecated compatibility counter; always zero because Racer requests never bypass Racer."})
	available := reg.NewGauge("racer", prometheus.GaugeOpts{Name: "gantry_racer_available", Help: "Client and origin UDS and containerd readiness."})
	tracker := newStreamCommitTracker(store, logger,
		func(n int) { p9.containerdCommitObserved.Add(float64(n)) },
		func(d time.Duration) { p9.containerdCommitObserveDur.Observe(d.Seconds()) },
		func(n int) { p9.commitMissingAfterStream.Add(float64(n)) })

	go func() {
		if err := tracker.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("stream tracker stopped", slog.Any("err", err))
		}
	}()

	server := mirror.NewRacer(c, local, origin, &gantryracer.Backend{Client: client},
		mirror.WithLogger(logger), mirror.WithLiveStreamThrough(), mirror.WithStartupReadinessGate(),
		mirror.WithRacerMetrics(onRacerStream, nil),
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

	// Keep the source's List/Subscribe walks: they populate the store's
	// media-type index even without a DHT provider or presence notifier.
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

		return racerSocketReady(probeCtx, originSocket, readinessTarget) && racerClientSocketReady(probeCtx, clientSocket)
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

	opsHTTP, opsErrors := startOpsEndpoint(c.MetricsListen, reg, func() (string, bool) { return "Racer client/origin UDS or containerd unavailable", ready.Load() }, logger)
	defer opsHTTP.Close() //nolint:errcheck // Shutdown cleanup.

	if c.PprofListen != "" {
		pprof, _, listenErr := startPprofEndpoint(c.PprofListen, logger)
		if listenErr != nil {
			logger.Warn("pprof endpoint unavailable", slog.Any("err", listenErr))
		} else {
			defer pprof.Close() //nolint:errcheck // Shutdown cleanup.
		}
	}

	logger.Info("Racer backend listening", slog.String("client_socket", clientSocket), slog.String("origin_socket", originSocket), slog.String("prefetch", "demand-only"))

	select {
	case <-ctx.Done():
		return nil
	case err := <-originErrors:
		return fmt.Errorf("racer origin: %w", err)
	case err := <-opsErrors:
		return fmt.Errorf("operations endpoint: %w", err)
	}
}

func newRacerStreamMetrics(reg *metrics.Registry) func(sdk.TransferStats, bool, error) {
	spliceCalls := reg.NewCounter("racer", prometheus.CounterOpts{Name: "gantry_racer_splice_calls_total", Help: "Actual SDK splice syscalls."})
	spliceBytes := reg.NewCounter("racer", prometheus.CounterOpts{Name: "gantry_racer_splice_bytes_total", Help: "Payload bytes forwarded with splice."})
	reg.NewCounter("racer", prometheus.CounterOpts{Name: "gantry_racer_tee_calls_total", Help: "Compatibility counter, always zero: SDK verification tee syscalls have been removed."})
	reg.NewCounter("racer", prometheus.CounterOpts{Name: "gantry_racer_tee_bytes_total", Help: "Compatibility counter, always zero: SDK verification tee bytes have been removed."})
	bufferedBytes := reg.NewCounter("racer", prometheus.CounterOpts{Name: "gantry_racer_buffered_bytes_total", Help: "Payload bytes forwarded through userspace."})
	streams := reg.NewCounterVec("racer", prometheus.CounterOpts{Name: "gantry_racer_stream_total", Help: "Racer response forwarding outcomes: completed (full response), partial (range response), or aborted. Completion does not imply OCI digest verification or a containerd commit; containerd verifies the digest separately."}, []string{"outcome"})

	return func(stats sdk.TransferStats, partial bool, err error) {
		spliceCalls.Add(float64(stats.SpliceCalls))
		spliceBytes.Add(float64(stats.SpliceBytes))
		bufferedBytes.Add(float64(stats.BufferedBytes))

		outcome := "completed"
		if partial {
			outcome = "partial"
		}

		if err != nil {
			outcome = "aborted"
		}

		streams.WithLabelValues(outcome).Inc()
	}
}

func startRacerOrigin(socket string, handler http.Handler) (*http.Server, <-chan error, error) {
	if err := racermeta.PrepareSocketDirectory(socket); err != nil {
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

// Resolve once at startup for the local origin's reserved invalid target. Never
// send this HEAD through the cache: its metadata owner may be an unswitched peer
// whose Gantry origin is not running yet, which would block a rolling update.
func racerReadinessTarget(nodeName string, hostname func() (string, error)) (string, error) {
	if nodeName == "" {
		var err error

		nodeName, err = hostname()
		if err != nil {
			return "", fmt.Errorf("racer readiness hostname: %w", err)
		}
	}

	if nodeName == "" {
		return "", errors.New("racer readiness requires a node name or hostname")
	}

	return "/gantry-readiness?node=" + url.QueryEscape(nodeName), nil
}

// The local origin must handle HTTP and reject the reserved invalid target.
func racerSocketReady(ctx context.Context, socket, target string) bool {
	return racerProbeSocket(ctx, socket, http.MethodHead, target, func(response *http.Response) bool {
		return response.StatusCode == http.StatusNotFound
	})
}

// Racer rejects OPTIONS in its worker-local HTTP parser before cache admission
// or origin/peer routing. This proves the selected client UDS can accept, parse
// and respond, without making rollout readiness depend on other Gantry origins.
// Require the parser's exact response rather than accepting arbitrary errors.
// Cache data availability is still enforced per request, without bypassing Racer;
// this probe does not claim storage health or fleet-wide origin availability.
func racerClientSocketReady(ctx context.Context, socket string) bool {
	return racerProbeSocket(ctx, socket, http.MethodOptions, "/", func(response *http.Response) bool {
		return response.StatusCode == http.StatusMethodNotAllowed &&
			response.Header.Get("Allow") == "GET, HEAD" && response.ContentLength == 0
	})
}

func racerProbeSocket(ctx context.Context, socket, method, target string, ready func(*http.Response) bool) bool {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+target, nil)
	if err != nil {
		return false
	}

	response, err := client.Do(req)
	if err != nil {
		return false
	}
	defer response.Body.Close() //nolint:errcheck // Probe response cleanup.

	return ready(response)
}
