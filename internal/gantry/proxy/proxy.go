// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package proxy bridges a network listener to a Gantry Unix endpoint.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/Azure/unbounded/internal/gantry/listener"
)

// Serve accepts connections on listenEndpoint and proxies them to
// upstreamEndpoint until ctx is canceled.
func Serve(ctx context.Context, listenEndpoint, upstreamEndpoint string) error {
	listenNetwork, _, err := listener.Parse(listenEndpoint)
	if err != nil {
		return fmt.Errorf("proxy listener: %w", err)
	}

	if listenNetwork != "tcp" {
		return fmt.Errorf("proxy listener %q must be a TCP endpoint", listenEndpoint)
	}

	upstreamNetwork, upstreamAddress, err := listener.Parse(upstreamEndpoint)
	if err != nil {
		return fmt.Errorf("proxy upstream: %w", err)
	}

	if upstreamNetwork != "unix" {
		return fmt.Errorf("proxy upstream %q must be a Unix endpoint", upstreamEndpoint)
	}

	ln, err := listener.Listen(listenEndpoint)
	if err != nil {
		return fmt.Errorf("proxy listener: %w", err)
	}
	defer ln.Close() //nolint:errcheck // best-effort shutdown

	go func() {
		<-ctx.Done()

		_ = ln.Close() //nolint:errcheck // context cancellation closes the accept loop
	}()

	var dialer net.Dialer

	for {
		downstream, acceptErr := ln.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil || errors.Is(acceptErr, net.ErrClosed) {
				return nil
			}

			return fmt.Errorf("proxy accept: %w", acceptErr)
		}

		go bridge(ctx, downstream, &dialer, upstreamNetwork, upstreamAddress)
	}
}

func bridge(ctx context.Context, downstream net.Conn, dialer *net.Dialer, network, address string) {
	defer downstream.Close() //nolint:errcheck // connection cleanup

	upstream, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return
	}
	defer upstream.Close() //nolint:errcheck // connection cleanup

	var copies sync.WaitGroup
	copies.Add(2)

	go copyConnection(&copies, upstream, downstream)
	go copyConnection(&copies, downstream, upstream)

	copies.Wait()
}

func copyConnection(copies *sync.WaitGroup, dst, src net.Conn) {
	defer copies.Done()

	_, _ = io.Copy(dst, src) //nolint:errcheck // either direction closing ends the bridge

	if closer, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = closer.CloseWrite() //nolint:errcheck // best-effort half-close
	}
}
