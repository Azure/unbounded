// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"

	"github.com/Azure/unbounded/internal/gantry/chaircall"
	"github.com/Azure/unbounded/internal/gantry/coord"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// Request lifetime bounds for the chair listener. ReadHeaderTimeout alone
// leaves a client free to send headers and then stall mid-body, holding a
// handler goroutine and its buffer open indefinitely; ReadTimeout bounds the
// whole request. The libp2p coord handler this transport replaced had an
// equivalent whole-stream deadline and inbound stream cap.
const (
	chairReadHeaderTimeout = 5 * time.Second
	chairReadTimeout       = 15 * time.Second
	chairWriteTimeout      = 30 * time.Second
	chairIdleTimeout       = 60 * time.Second

	// chairMaxConcurrentRequests bounds in-flight handlers. A chair is asked
	// to pull by many requesters at once, and admission here is the only
	// bound before the request is parsed; the origin-pull semaphore sits
	// further in.
	chairMaxConcurrentRequests = 256

	// chairShutdownGrace bounds graceful shutdown before connections are
	// closed outright, so a stalled handler cannot hold up process exit.
	chairShutdownGrace = 5 * time.Second
)

// serveChairCalls starts the HTTPS listener that serves cold-start
// please_pull. The certificate is signed with the agent's libp2p identity so
// requesters can pin it to the peer ID published in this node's chair Lease.
func serveChairCalls(addr string, priv crypto.PrivKey, local ifaces.LocalChairPullStarter, logger *slog.Logger) (func(context.Context) error, error) {
	if priv == nil {
		return nil, fmt.Errorf("chaircall: no libp2p identity available for %s", addr)
	}

	tlsCfg, err := chaircall.ServerTLSConfig(priv)
	if err != nil {
		return nil, err
	}

	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("chaircall: listen %s: %w", addr, err)
	}

	srv := &http.Server{
		Handler:           limitConcurrency(coord.NewChairHTTPHandler(local, logger), chairMaxConcurrentRequests),
		ReadHeaderTimeout: chairReadHeaderTimeout,
		ReadTimeout:       chairReadTimeout,
		WriteTimeout:      chairWriteTimeout,
		IdleTimeout:       chairIdleTimeout,
	}

	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			logger.Error("chaircall: serve error", slog.Any("err", serveErr))
		}
	}()

	stop := func(ctx context.Context) error {
		graceCtx, cancel := context.WithTimeout(ctx, chairShutdownGrace)
		defer cancel()

		if shutdownErr := srv.Shutdown(graceCtx); shutdownErr != nil {
			// Graceful shutdown timed out, so close listeners and connections
			// outright rather than blocking process exit on a stalled handler.
			return srv.Close()
		}

		return nil
	}

	return stop, nil
}

// limitConcurrency rejects requests beyond n in-flight handlers with 503 rather
// than letting each one hold a goroutine and read buffer.
func limitConcurrency(next http.Handler, n int) http.Handler {
	sem := make(chan struct{}, n)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()

			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "chair busy", http.StatusServiceUnavailable)
		}
	})
}

// listenPort extracts the port from a listen address. Agents share the chair
// port by convention, so the local value is also the port used to reach peers.
func listenPort(addr string) (int, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, fmt.Errorf("port %q: %w", portStr, err)
	}

	if port <= 0 {
		return 0, fmt.Errorf("port %d must be positive", port)
	}

	return port, nil
}
