// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Azure/unbounded/internal/version"
	racer "github.com/Azure/unbounded/pkg/racer"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(version.String())
		return
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("racer-object failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "frontend" && args[0] != "backend" {
		return fmt.Errorf("usage: racer-object {frontend|backend} --config objects.json [flags]")
	}

	mode := args[0]
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	config := flags.String("config", "", "explicit immutable object mapping JSON")
	socket := flags.String("socket", "", "Racer cache socket (frontend) or origin socket (backend)")
	listen := flags.String("listen", "127.0.0.1:8000", "frontend loopback HTTP address")
	limit := flags.Int("concurrency", 64, "maximum frontend connections or backend page buffers")
	timeout := flags.Duration("timeout", 5*time.Minute, "per-request deadline (including body transfer)")

	auth := flags.String("azure-auth", "workload-identity", "workload-identity, default, shared-key, or anonymous")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}

	if flags.NArg() != 0 || *config == "" || !filepath.IsAbs(*socket) || len(*socket) > 107 || *limit < 1 || *limit > 4096 || *timeout <= 0 {
		return fmt.Errorf("config, absolute socket, concurrency 1..4096 and positive timeout are required")
	}

	c, err := loadConfiguration(*config)
	if err != nil {
		return err
	}

	if mode == "frontend" {
		address, err := net.ResolveTCPAddr("tcp", *listen)
		if err != nil {
			return err
		}

		if !address.IP.IsLoopback() {
			return fmt.Errorf("frontend must bind a loopback IP")
		}

		listener, err := net.ListenTCP("tcp", address)
		if err != nil {
			return err
		}
		defer closeResource(listener)

		f := newFrontend(c, *socket, *limit, *timeout)
		slog.Info("frontend started", "listen", listener.Addr(), "socket", *socket, "body_transport", "splice")
		err = f.serve(ctx, listener)
		slog.Info("frontend stopped", "spliced_bytes", f.bytes.Load())

		return err
	}

	source, err := azureClient(c.Endpoint, *auth, *limit)
	if err != nil {
		return err
	}

	origin, err := racer.NewOrigin(newBackend(c, source, *limit))
	if err != nil {
		return err
	}
	// The cache volume owns the parent directory. Never unlink an existing socket.
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: *socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer closeResource(listener)

	if err := os.Chmod(*socket, 0o660); err != nil {
		return err
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestContext, cancel := context.WithTimeout(r.Context(), *timeout)
		defer cancel()

		origin.ServeHTTP(w, r.WithContext(requestContext))
	})
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second, WriteTimeout: *timeout,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	stop := context.AfterFunc(ctx, func() { closeResource(server) })
	defer stop()

	slog.Info("backend started", "socket", *socket, "azure_auth", *auth)

	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return err
}
