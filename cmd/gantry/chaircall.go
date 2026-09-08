// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
		Handler:           coord.NewChairHTTPHandler(local, logger),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			logger.Error("chaircall: serve error", slog.Any("err", serveErr))
		}
	}()

	return srv.Shutdown, nil
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
