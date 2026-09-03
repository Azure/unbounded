// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStartPprofEndpointServesRuntimeProfiles(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	server, errorsChannel, err := startPprofEndpoint("127.0.0.1:0", logger)
	if err != nil {
		t.Fatalf("startPprofEndpoint: %v", err)
	}

	client := &http.Client{Timeout: 2 * time.Second}

	response, err := client.Get("http://" + server.Addr + "/debug/pprof/goroutine?debug=1")
	if err != nil {
		t.Fatalf("GET goroutine profile: %v", err)
	}

	body, readErr := io.ReadAll(response.Body)

	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		t.Fatalf("read response: %v", err)
	}

	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "goroutine profile") {
		t.Fatalf("profile response: status=%d body=%q", response.StatusCode, body)
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownContext); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case serveErr, ok := <-errorsChannel:
		if ok && serveErr != nil {
			t.Fatalf("Serve: %v", serveErr)
		}
	case <-shutdownContext.Done():
		t.Fatal("pprof server did not stop")
	}
}

func TestStartPprofEndpointReportsBindError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	defer func() { _ = listener.Close() }()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	server, errorsChannel, err := startPprofEndpoint(listener.Addr().String(), logger)
	if err == nil || server != nil || errorsChannel != nil {
		t.Fatalf("startPprofEndpoint = %#v, %#v, %v; want bind error", server, errorsChannel, err)
	}
}
