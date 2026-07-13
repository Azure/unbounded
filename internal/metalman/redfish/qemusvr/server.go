// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package qemusvr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/signal"
	"strconv"
	"syscall"
)

// Serve wires the QEMU machine layer to the Redfish server and serves the API
// over TLS until the process is terminated. On SIGINT/SIGTERM it tears down any
// child dnsmasq before exiting so no DHCP server is left bound to the bridge.
func Serve(cfg Config) error {
	machine, err := NewMachine(cfg)
	if err != nil {
		return err
	}

	server, err := NewServer(cfg, machine)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Handler: server.Handler(),
	}

	address := net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port))

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", address, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		//nolint:errcheck // Best-effort teardown of the boundary dnsmasq on exit.
		_ = machine.ClearHTTPBoot()
		//nolint:errcheck // Best-effort shutdown; the process is exiting anyway.
		_ = httpServer.Close()
	}()

	if err := httpServer.ServeTLS(listener, cfg.Cert, cfg.Key); err != nil &&
		!errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}
