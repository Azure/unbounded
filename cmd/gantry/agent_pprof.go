// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

func startPprofEndpoint(addr string, logger *slog.Logger) (*http.Server, <-chan error, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	server := &http.Server{
		Addr:              listener.Addr().String(),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errorsChannel := make(chan error, 1)

	go func() {
		err := server.Serve(listener)
		if !errors.Is(err, http.ErrServerClosed) {
			errorsChannel <- err
		}

		close(errorsChannel)
	}()

	logger.Info("pprof endpoint listening",
		slog.String("addr", listener.Addr().String()),
		slog.String("path", "/debug/pprof/"),
	)

	return server, errorsChannel, nil
}
