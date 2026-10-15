// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type sidecarShutdownListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *sidecarShutdownListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })

	return err
}

func startTestSidecarWithGrace(t *testing.T, handler http.Handler, requestTimeout, grace time.Duration) (string, context.CancelFunc, <-chan error, <-chan struct{}) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	l, err := listenSidecar(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = l.Close() })

	listener := &sidecarShutdownListener{Listener: l, closed: make(chan struct{})}

	done := make(chan error, 1)
	go func() { done <- serveSidecarWithGrace(ctx, listener, handler, requestTimeout, grace) }()

	return "http://" + l.Addr().String(), cancel, done, listener.closed
}

func waitSidecarBarrier(t *testing.T, barrier <-chan struct{}, failure string) {
	t.Helper()

	select {
	case <-barrier:
	case <-time.After(2 * time.Second):
		t.Fatal(failure)
	}
}

func TestSidecarShutdownDrainsActiveRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})

	var releaseOnce sync.Once

	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseRequest)

	requestContexts := make(chan context.Context, 1)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestContexts <- r.Context()

		close(entered)

		select {
		case <-release:
			_, _ = io.WriteString(w, "finished after signal")
		case <-r.Context().Done():
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})

	address, cancel, done, listenerClosed := startTestSidecarWithGrace(t, handler, time.Minute, 5*time.Second)

	client := &http.Client{Timeout: 7 * time.Second}
	defer client.CloseIdleConnections()

	responseDone := make(chan struct{})
	go func() {
		defer close(responseDone)

		response, err := client.Get(address)
		if err != nil {
			t.Error(err)
			return
		}
		defer response.Body.Close()

		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK || string(body) != "finished after signal" {
			t.Errorf("response after signal: status=%d body=%q err=%v", response.StatusCode, body, err)
		}
	}()

	waitSidecarBarrier(t, entered, "request did not start")

	requestCtx := <-requestContexts

	cancel()
	waitSidecarBarrier(t, listenerClosed, "shutdown did not close listener")

	if err := requestCtx.Err(); err != nil {
		t.Fatalf("active request canceled before draining: %v", err)
	}

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(address, "http://"), time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("shutdown accepted a new connection")
	}

	select {
	case err := <-done:
		t.Fatalf("server returned before active request drained: %v", err)
	default:
	}

	releaseRequest()
	waitSidecarBarrier(t, responseDone, "active request did not finish after signal")
	waitSidecar(t, done)
}

func TestSidecarShutdownForceClosesStuckRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	handlerDone := make(chan struct{})

	requestContexts := make(chan context.Context, 1)

	handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)

		requestContexts <- r.Context()

		close(entered)
		<-release
	})

	t.Cleanup(func() { close(release) })

	const grace = 250 * time.Millisecond

	address, cancel, done, listenerClosed := startTestSidecarWithGrace(t, handler, time.Minute, grace)

	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)

		client := &http.Client{Timeout: 3 * time.Second}
		defer client.CloseIdleConnections()

		response, err := client.Get(address)
		if err == nil {
			response.Body.Close()
			t.Error("stuck request unexpectedly completed")
		}
	}()

	waitSidecarBarrier(t, entered, "request did not start")

	requestCtx := <-requestContexts

	shutdownStarted := time.Now()

	cancel()
	waitSidecarBarrier(t, listenerClosed, "shutdown did not close listener")
	waitSidecar(t, done)

	if time.Since(shutdownStarted) < grace {
		t.Fatal("server stopped before shutdown grace expired")
	}

	if requestCtx.Err() != context.Canceled {
		t.Fatalf("request context after grace: %v", requestCtx.Err())
	}

	waitSidecarBarrier(t, requestDone, "force-close did not release HTTP client")

	select {
	case <-handlerDone:
		t.Fatal("handler finished without being released")
	default:
	}
}
