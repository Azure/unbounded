// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

type nopChairPuller struct{}

func (nopChairPuller) StartLocalChairPull(_ context.Context, _, _ string, _ ifaces.OriginRefKind, digests []digest.Digest, _ ifaces.ChairAssignment) ([]ifaces.PleasePullOutcome, error) {
	out := make([]ifaces.PleasePullOutcome, 0, len(digests))
	for _, d := range digests {
		out = append(out, ifaces.PleasePullOutcome{Digest: d, Outcome: ifaces.PleasePullStarted})
	}

	return out, nil
}

// TestChairServerBoundsStalledRequestBody covers a client that completes its
// request headers, declares a body, and then stops sending. ReadHeaderTimeout
// does not bound body reads, so without ReadTimeout the handler would sit in
// io.ReadAll holding a goroutine and its buffer for as long as the client
// chose to stall.
func TestChairServerBoundsStalledRequestBody(t *testing.T) {
	priv, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatalf("GenerateEd25519Key: %v", err)
	}

	addr := reserveLoopbackAddr(t)

	stop, err := serveChairCalls(addr, priv, nopChairPuller{}, discardLogger())
	if err != nil {
		t.Fatalf("serveChairCalls: %v", err)
	}

	defer func() { _ = stop(context.Background()) }() //nolint:errcheck // test cleanup

	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test dials its own listener
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	defer func() { _ = conn.Close() }() //nolint:errcheck // test cleanup

	// Announce a body far larger than what is actually sent, then stall.
	head := "POST /gantry/v1/please-pull HTTP/1.1\r\nHost: x\r\nContent-Length: 4096\r\n\r\npartial"
	if _, err := conn.Write([]byte(head)); err != nil {
		t.Fatalf("write headers: %v", err)
	}

	// Read past the server's ReadTimeout. A bounded server either replies or
	// closes; an unbounded one leaves this blocked until the deadline below.
	if err := conn.SetReadDeadline(time.Now().Add(chairReadTimeout + 15*time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	start := time.Now()
	buf := make([]byte, 256)

	_, readErr := conn.Read(buf)
	elapsed := time.Since(start)

	var netErr net.Error
	if errors.As(readErr, &netErr) && netErr.Timeout() {
		t.Fatalf("server neither replied to nor closed a stalled body within %v; ReadTimeout is not bounding it", elapsed)
	}

	if elapsed > chairReadTimeout+10*time.Second {
		t.Fatalf("stalled body held the handler for %v; want it bounded by ReadTimeout %v", elapsed, chairReadTimeout)
	}
}

// TestChairServerShutdownIsBounded covers the shutdown path: a graceful
// Shutdown that cannot drain must fall back to closing connections rather than
// holding up process exit.
func TestChairServerShutdownIsBounded(t *testing.T) {
	priv, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatalf("GenerateEd25519Key: %v", err)
	}

	addr := reserveLoopbackAddr(t)

	stop, err := serveChairCalls(addr, priv, nopChairPuller{}, discardLogger())
	if err != nil {
		t.Fatalf("serveChairCalls: %v", err)
	}

	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test dials its own listener
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	defer func() { _ = conn.Close() }() //nolint:errcheck // test cleanup

	// Leave a partial request outstanding so graceful drain has something to
	// wait on.
	if _, err := conn.Write([]byte("POST /gantry/v1/please-pull HTTP/1.1\r\nHost: x\r\nContent-Length: 4096\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	start := time.Now()

	if err := stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if elapsed := time.Since(start); elapsed > chairShutdownGrace+10*time.Second {
		t.Fatalf("shutdown took %v; want it bounded by the grace period %v", elapsed, chairShutdownGrace)
	}
}

// reserveLoopbackAddr returns a loopback address that is free at call time.
func reserveLoopbackAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	addr := ln.Addr().String()

	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	return addr
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
