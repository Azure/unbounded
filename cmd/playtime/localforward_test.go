// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

// TestProxyConnSplice verifies proxyConn splices two connections in both
// directions and tears them down together.
func TestProxyConnSplice(t *testing.T) {
	aClient, aServer := net.Pipe()
	bClient, bServer := net.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go proxyConn(ctx, aServer, bServer)

	// a -> b
	go func() {
		_, _ = aClient.Write([]byte("ping")) //nolint:errcheck // test write
	}()

	buf := make([]byte, 4)

	_ = bClient.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck // test

	if _, err := bClient.Read(buf); err != nil {
		t.Fatalf("read a->b: %v", err)
	}

	if string(buf) != "ping" {
		t.Errorf("a->b got %q, want ping", buf)
	}

	// b -> a
	go func() {
		_, _ = bClient.Write([]byte("pong")) //nolint:errcheck // test write
	}()

	_ = aClient.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck // test

	if _, err := aClient.Read(buf); err != nil {
		t.Fatalf("read b->a: %v", err)
	}

	if string(buf) != "pong" {
		t.Errorf("b->a got %q, want pong", buf)
	}
}

// TestProxyConnCancelTearsDown verifies cancelling the context closes both ends.
func TestProxyConnCancelTearsDown(t *testing.T) {
	aClient, aServer := net.Pipe()
	bClient, bServer := net.Pipe()

	defer aClient.Close() //nolint:errcheck // test cleanup
	defer bClient.Close() //nolint:errcheck // test cleanup

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		proxyConn(ctx, aServer, bServer)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("proxyConn did not return after context cancel")
	}
}

// TestStartLocalForwardLifecycle verifies the local listener binds and is
// released when the context is cancelled.
func TestStartLocalForwardLifecycle(t *testing.T) {
	port := freeLocalPort(t)

	ctx, cancel := context.WithCancel(context.Background())

	if err := startLocalForward(ctx, nil, port, 8443); err != nil {
		t.Fatalf("startLocalForward: %v", err)
	}

	// The port should now be bound: a second listen fails.
	if l, err := net.Listen("tcp", localAddr(port)); err == nil {
		_ = l.Close() //nolint:errcheck // test cleanup

		t.Fatal("expected port to be bound while forward is active")
	}

	cancel()

	// After cancel the listener is closed and the port is released.
	deadline := time.Now().Add(2 * time.Second)

	for {
		l, err := net.Listen("tcp", localAddr(port))
		if err == nil {
			_ = l.Close() //nolint:errcheck // test cleanup
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("port not released after cancel: %v", err)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

func localAddr(port uint16) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
}

// freeLocalPort returns a currently-free loopback TCP port.
func freeLocalPort(t *testing.T) uint16 {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	defer l.Close() //nolint:errcheck // test cleanup

	return uint16(l.Addr().(*net.TCPAddr).Port)
}
