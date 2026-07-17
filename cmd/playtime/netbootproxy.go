// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"
)

// startNetbootProxy runs an HTTP reverse proxy inside the demo pod. It listens
// on the pod overlay IP (OverlayRemoteIP) and forwards to the client-side
// netboot HTTP server at http://OverlayLocalIP:port.
//
// The guest bootloader (grub) has a tiny built-in TCP receive window, so
// fetching the netboot payload directly from the client across the
// high-latency overlay is bandwidth-starved (window/RTT limited). By pointing
// the guest at this pod-local proxy, grub's small window only governs the fast
// pod<->guest LAN hop, while the pod re-originates the request to the client
// over the overlay using the pod's real Linux kernel TCP (window scaling), so
// the slow overlay hop runs at full bandwidth.
func startNetbootProxy(ctx context.Context, cfg Config) error {
	if cfg.NetbootProxyPort <= 0 {
		return nil
	}

	target := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(cfg.OverlayLocalIP, strconv.Itoa(cfg.NetbootProxyPort)),
	}
	listenAddr := net.JoinHostPort(cfg.OverlayRemoteIP, strconv.Itoa(cfg.NetbootProxyPort))

	proxy := httputil.NewSingleHostReverseProxy(target)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           proxy,
		ReadHeaderTimeout: 30 * time.Second,
	}

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen netboot proxy on %s: %w", listenAddr, err)
	}

	fmt.Printf("netboot HTTP proxy listening on %s, forwarding to %s\n", listenAddr, target)

	go func() {
		<-ctx.Done()

		if closeErr := srv.Close(); closeErr != nil {
			fmt.Printf("netboot proxy close: %v\n", closeErr)
		}
	}()

	if serveErr := srv.Serve(ln); serveErr != nil && ctx.Err() == nil {
		return fmt.Errorf("netboot proxy serve: %w", serveErr)
	}

	return nil
}
