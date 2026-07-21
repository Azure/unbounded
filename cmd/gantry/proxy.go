// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"flag"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/Azure/unbounded/internal/gantry/proxy"
)

func runProxy(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	listenEndpoint := fs.String("listen", "", "TCP endpoint to expose as host:port")
	upstreamEndpoint := fs.String("upstream", "", "Gantry Unix endpoint to proxy to as unix:///path")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *listenEndpoint == "" {
		return fmt.Errorf("gantry proxy: --listen is required")
	}

	if *upstreamEndpoint == "" {
		return fmt.Errorf("gantry proxy: --upstream is required")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	return proxy.Serve(ctx, *listenEndpoint, *upstreamEndpoint)
}
