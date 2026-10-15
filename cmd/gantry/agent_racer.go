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
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/metrics"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/origin"
	"github.com/Azure/unbounded/internal/gantry/racer"
	"github.com/Azure/unbounded/pkg/racersdk"
)

const racerShutdownBudget = 10 * time.Second

type racerAgentClient interface {
	racer.Client
	Close() error
}

// racerAgentDeps keeps lifecycle tests independent of the canonical /run sockets.
type racerAgentDeps struct {
	client      racerAgentClient
	serveOrigin func(context.Context, racersdk.OriginConfig, racersdk.Origin) error
	probe       func(context.Context, string) error
}

func runRacerAgent(c *config.Config, logger *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	volume := racer.VolumeName

	client, err := racersdk.NewClient(racer.ClientConfig(c, volume))
	if err != nil {
		return fmt.Errorf("racer client: %w", err)
	}

	return serveRacerAgent(ctx, c, logger, volume, racerAgentDeps{
		client: client, serveOrigin: racer.ServeOrigin, probe: probeRacerSocket,
	})
}

// serveRacerAgent owns client and keeps origin service alive while HTTP drains.
// There is deliberately no fallback to the legacy agent on any failure.
func serveRacerAgent(ctx context.Context, c *config.Config, logger *slog.Logger, volume string, deps racerAgentDeps) (retErr error) {
	if deps.client != nil {
		defer func() { retErr = errors.Join(retErr, deps.client.Close()) }()
	}

	reg := metrics.New()
	reg.RegisterDefaultCollectors()
	telemetry := racer.NewMetrics(reg)

	originClient, err := origin.New(c, origin.WithLogger(logger),
		origin.WithRequestMetrics(telemetry.OriginRequest), origin.WithByteMetrics(telemetry.OriginBytes))
	if err != nil {
		return fmt.Errorf("racer origin client: %w", err)
	}

	// Signal cancellation starts draining; cancelOrigin runs after HTTP drain.
	originCtx, cancelOrigin := context.WithCancel(context.WithoutCancel(ctx))
	originDone := make(chan struct{})

	var originErr error

	go func() {
		defer close(originDone)

		originErr = deps.serveOrigin(originCtx, racer.OriginConfig(c, volume), racer.Origin(c, originClient))
	}()

	// Always install the handler, even without a client, so unavailable requests
	// retain Racer correlation headers and rate-limited failure diagnostics.
	mirrorSrv := mirror.New(c, nil, originClient,
		mirror.WithContentBackend(racer.NewHandler(deps.client, originClient, logger)), mirror.WithLogger(logger), mirror.WithStartupReadinessGate())

	var (
		ready   atomic.Bool
		servers []*http.Server
	)

	defer func() {
		ready.Store(false)
		mirrorSrv.Drain()

		retErr = errors.Join(retErr, shutdownRacerAgent(servers, cancelOrigin, originDone, &originErr))
	}()

	listener, err := net.Listen("tcp", c.MirrorListen)
	if err != nil {
		return fmt.Errorf("racer mirror listen: %w", err)
	}
	// Own the HTTP server so serve failures propagate and timed-out drains can
	// force-close connections, including requests blocked in the SDK.
	listener = racer.LimitListener(listener, 0)
	mirrorHTTP := &http.Server{Handler: racer.WrapHTTP(mirrorSrv.Handler(), 0, telemetry.MirrorResponse), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 * 1024}
	servers = append(servers, mirrorHTTP)
	mirrorErrors := make(chan error, 1)

	go func() { mirrorErrors <- mirrorHTTP.Serve(listener) }()

	readyCheck := racerReadiness(ctx, &ready, originDone)
	opsHTTP, opsErrors := startOpsEndpoint(c.MetricsListen, reg, readyCheck, logger)
	servers = append(servers, opsHTTP)

	var pprofErrors <-chan error

	if c.PprofListen != "" {
		pprofHTTP, errc, err := startPprofEndpoint(c.PprofListen, logger)
		if err != nil {
			logger.Warn("pprof endpoint unavailable", slog.Any("err", err))
		} else {
			servers = append(servers, pprofHTTP)
			pprofErrors = errc
		}
	}

	logger.Info("Racer agent started", slog.String("volume", racer.VolumeName), slog.String("mirror", listener.Addr().String()))

	return superviseRacerAgent(ctx, logger, mirrorSrv, &ready, deps.probe, racerAgentEvents{
		originDone: originDone, originErr: &originErr,
		mirror: mirrorErrors, ops: opsErrors, pprof: pprofErrors,
	})
}

type racerAgentEvents struct {
	originDone <-chan struct{}
	originErr  *error // Read only after originDone closes.
	mirror     <-chan error
	ops        <-chan error
	pprof      <-chan error
}

// superviseRacerAgent watches endpoint exits and socket availability. Startup and
// cleanup stay in serveRacerAgent, which registers ownership before serving HTTP.
func superviseRacerAgent(ctx context.Context, logger *slog.Logger, mirrorSrv *mirror.Server, ready *atomic.Bool, probe func(context.Context, string) error, events racerAgentEvents) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	markedReady := false

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-events.originDone:
			if *events.originErr == nil || errors.Is(*events.originErr, context.Canceled) {
				return errors.New("racer origin stopped unexpectedly")
			}
			// The deferred cleanup returns the original origin error.
			return nil
		case err := <-events.mirror:
			return fmt.Errorf("racer mirror serve: %w", err)
		case err := <-events.ops:
			if err == nil {
				return errors.New("racer ops stopped unexpectedly")
			}

			return fmt.Errorf("racer ops serve: %w", err)
		case err := <-events.pprof:
			if err != nil {
				logger.Warn("pprof endpoint died", slog.Any("err", err))
			}

			events.pprof = nil
		case <-ticker.C:
			available := racerSocketsAvailable(ctx, probe)
			ready.Store(available)

			if available && !markedReady {
				mirrorSrv.MarkReady()

				markedReady = true

				logger.Info("Racer sockets accept connections; mirror startup gate released")
			}
		}
	}
}

// A successful Unix dial proves socket availability only, not volume readiness,
// cluster health, origin registration, or the ability to serve an object.
func racerSocketsAvailable(ctx context.Context, probe func(context.Context, string) error) bool {
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	for _, role := range []string{"origin", "client"} {
		if err := probe(ctx, racer.SocketPath(role)); err != nil {
			return false
		}
	}

	return ctx.Err() == nil
}

func probeRacerSocket(ctx context.Context, path string) error {
	var dialer net.Dialer

	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}

	return conn.Close()
}

func racerReadiness(ctx context.Context, ready *atomic.Bool, originDone <-chan struct{}) func() (string, bool) {
	return func() (string, bool) {
		select {
		case <-originDone:
			return "Racer origin stopped", false
		default:
		}

		if ctx.Err() != nil || !ready.Load() {
			return "Racer sockets unavailable or agent draining", false
		}

		return "", true
	}
}

func shutdownRacerAgent(servers []*http.Server, cancelOrigin context.CancelFunc, originDone <-chan struct{}, originErr *error) (retErr error) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), racerShutdownBudget)
	defer cancel()
	// Reserve part of the overall budget for SDK socket cleanup after HTTP.
	drainCtx, cancelDrain := context.WithTimeout(shutdownCtx, racerShutdownBudget-2*time.Second)
	defer cancelDrain()

	for _, server := range servers {
		if err := server.Shutdown(drainCtx); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("racer HTTP shutdown: %w", err))
			_ = server.Close() //nolint:errcheck // force-close after the bounded drain
		}
	}

	cancelOrigin()

	select {
	case <-originDone:
		if *originErr != nil && !errors.Is(*originErr, context.Canceled) {
			retErr = errors.Join(retErr, fmt.Errorf("racer origin: %w", *originErr))
		}
	case <-shutdownCtx.Done():
		retErr = errors.Join(retErr, fmt.Errorf("racer origin shutdown: %w", shutdownCtx.Err()))
	}

	return retErr
}
