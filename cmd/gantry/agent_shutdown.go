// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/cdsub"
	"github.com/Azure/unbounded/internal/gantry/mirror"
)

// shutdownDeps bundles the subsystems the agent has to drain on
// SIGTERM. All fields are required; nil is undefined behaviour.
type shutdownDeps struct {
	logger         *slog.Logger
	mirrorSrv      *mirror.Server
	transferStop   func(context.Context) error
	mirrorStop     func(context.Context) error
	cdsubSrc       cdsub.ImageSource
	cdsubDone      <-chan error
	pullerPumpWG   *sync.WaitGroup
	metricsHTTP    *http.Server
	shutdownBudget time.Duration
}

// gracefulShutdown drains the agent in the plan order:
//
// 1. Mirror.Drain - every new /v2/ request immediately gets 503 so
// containerd's hosts.toml falls through to origin. Does NOT close
// the listener yet - existing kubelet connections need a chance
// to complete.
// 2. Transfer.Shutdown - drains in-flight peer transfers so a
// requesting peer doesn't see its pull cut mid-stream.
// 3. Mirror.Shutdown - closes the listener, drains in-flight
// handlers up to the shutdown deadline.
// 4. Wait for cdsub.Run + outstanding pull-pump advertise calls to
// flush before libp2p is closed by the runAgent defer chain.
// 5. Ops endpoint (Shutdown) - last so /readyz can keep reporting
// NotReady while we drain.
//
// discovery.Close + members.Stop run from the runAgent defer chain
// after this returns.
func gracefulShutdown(d shutdownDeps) {
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), d.shutdownBudget)
	defer cancelShutdown()

	d.mirrorSrv.Drain()

	if err := d.transferStop(shutdownCtx); err != nil {
		d.logger.Warn("transfer shutdown error", slog.Any("err", err))
	}

	if err := d.mirrorStop(shutdownCtx); err != nil {
		d.logger.Warn("mirror shutdown error", slog.Any("err", err))
	}
	// cdsub already cancelled by the outer ctx; wait briefly for its
	// pending advertise calls to flush.
	select {
	case <-d.cdsubDone:
	case <-shutdownCtx.Done():
		d.logger.Warn("cdsub did not drain within shutdown budget")
	}
	// Release the underlying containerd gRPC client if the source
	// owns one. NoOpSource doesn't implement io.Closer so this is a
	// best-effort type assertion.
	if closer, ok := d.cdsubSrc.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			d.logger.Warn("cdsub source close error", slog.Any("err", err))
		}
	}
	// "flushes DHT Provide for any newly committed entries."
	// runOriginPull goroutines own one inflight handle and one advertiser
	// mark-present call each; pullerPumpWG counts them so we can let
	// pending advertisement flush before disco.Close fires below.
	pumpDone := make(chan struct{})

	go func() { d.pullerPumpWG.Wait(); close(pumpDone) }()

	select {
	case <-pumpDone:
	case <-shutdownCtx.Done():
		d.logger.Warn("puller-pump did not drain within shutdown budget")
	}

	if err := d.metricsHTTP.Shutdown(shutdownCtx); err != nil {
		d.logger.Warn("metrics shutdown error", slog.Any("err", err))
	}
}
