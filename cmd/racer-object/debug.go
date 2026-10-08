// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

func debugMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return mux
}

// runWithDebug ties diagnostics to the main server, including startup failures.
// The caller must return when its context is canceled, as both server modes do.
func runWithDebug(ctx context.Context, address string, serve func(context.Context) error) error {
	if address == "" {
		return serve(ctx)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var lc net.ListenConfig

	listener, err := lc.Listen(ctx, "tcp", address)
	if err != nil {
		return errors.New("could not bind debug listener")
	}
	defer closeResource(listener)

	server := &http.Server{
		Handler: debugMux(), ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 30 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	defer closeResource(server)

	debugDone := make(chan error, 1)
	mainDone := make(chan error, 1)

	go func() { debugDone <- server.Serve(listener) }()
	go func() { mainDone <- serve(ctx) }()

	select {
	case err := <-mainDone:
		cancel()
		closeResource(server)
		<-debugDone

		return err
	case <-debugDone:
		cancel()
		closeResource(server)
		<-mainDone

		return errors.New("debug server failed")
	}
}
