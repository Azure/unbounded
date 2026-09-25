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
	Get(context.Context, racersdk.Request) (*racersdk.Value, error)
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

	cache, err := racersdk.ParseCacheName(racer.CacheName)
	if err != nil {
		return fmt.Errorf("racer cache: %w", err)
	}

	client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: cache})
	if err != nil {
		return fmt.Errorf("racer client: %w", err)
	}

	return serveRacerAgent(ctx, c, logger, cache, racerAgentDeps{
		client: client, serveOrigin: racersdk.ServeOrigin, probe: probeRacerSocket,
	})
}

// serveRacerAgent owns client and keeps origin service alive while HTTP drains.
// There is deliberately no fallback to the legacy agent on any failure.
func serveRacerAgent(ctx context.Context, c *config.Config, logger *slog.Logger, cache racersdk.CacheName, deps racerAgentDeps) (retErr error) {
	defer func() { retErr = errors.Join(retErr, deps.client.Close()) }()

	originClient, err := origin.New(c, origin.WithLogger(logger))
	if err != nil {
		return fmt.Errorf("racer origin client: %w", err)
	}

	// Signal cancellation starts draining; cancelOrigin runs after HTTP drain.
	originCtx, cancelOrigin := context.WithCancel(context.WithoutCancel(ctx))
	originDone := make(chan struct{})

	var originErr error

	go func() {
		defer close(originDone)

		originErr = deps.serveOrigin(originCtx, racersdk.OriginConfig{Cache: cache}, racer.Origin(c, originClient))
	}()

	mirrorSrv := mirror.New(c, nil, originClient,
		mirror.WithRacer(deps.client), mirror.WithLogger(logger), mirror.WithStartupReadinessGate())

	var (
		ready   atomic.Bool
		servers []*http.Server
	)

	defer func() {
		ready.Store(false)
		mirrorSrv.Drain()

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
			if originErr != nil && !errors.Is(originErr, context.Canceled) {
				retErr = errors.Join(retErr, fmt.Errorf("racer origin: %w", originErr))
			}
		case <-shutdownCtx.Done():
			retErr = errors.Join(retErr, fmt.Errorf("racer origin shutdown: %w", shutdownCtx.Err()))
		}
	}()

	listener, err := net.Listen("tcp", c.MirrorListen)
	if err != nil {
		return fmt.Errorf("racer mirror listen: %w", err)
	}
	// Own the HTTP server so serve failures propagate and timed-out drains can
	// force-close connections, including requests blocked in the SDK.
	mirrorHTTP := &http.Server{Handler: mirrorSrv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	servers = append(servers, mirrorHTTP)
	mirrorErrors := make(chan error, 1)

	go func() { mirrorErrors <- mirrorHTTP.Serve(listener) }()

	reg := metrics.New()
	reg.RegisterDefaultCollectors()

	readyCheck := func() (string, bool) {
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

	logger.Info("Racer agent started", slog.String("cache", racer.CacheName), slog.String("mirror", listener.Addr().String()))

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	markedReady := false

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-originDone:
			if originErr == nil || errors.Is(originErr, context.Canceled) {
				return errors.New("racer origin stopped unexpectedly")
			}
			// The deferred cleanup returns the original origin error.
			return nil
		case err := <-mirrorErrors:
			return fmt.Errorf("racer mirror serve: %w", err)
		case err := <-opsErrors:
			if err == nil {
				return errors.New("racer ops stopped unexpectedly")
			}

			return fmt.Errorf("racer ops serve: %w", err)
		case err := <-pprofErrors:
			if err != nil {
				logger.Warn("pprof endpoint died", slog.Any("err", err))
			}

			pprofErrors = nil
		case <-ticker.C:
			available := racerSocketsAvailable(ctx, deps.probe)
			ready.Store(available)

			if available && !markedReady {
				mirrorSrv.MarkReady()

				markedReady = true

				logger.Info("Racer sockets accept connections; mirror startup gate released")
			}
		}
	}
}

// A successful Unix dial proves socket availability only, not cache readiness,
// cluster health, origin registration, or the ability to serve an object.
func racerSocketsAvailable(ctx context.Context, probe func(context.Context, string) error) bool {
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	for _, role := range []string{"origin", "client"} {
		if err := probe(ctx, "/run/racer/"+racer.CacheName+"/"+role+"/socket"); err != nil {
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
