// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func testTransport(t *testing.T, f *servingFixture, connections, handshakes int, deadline time.Duration) *transportListener {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	l := newTransportListener(f.ctx, listener, f.a.Server.tlsConfig(f.ctx, f.serverCertificate), Limits{MaxConnections: connections, MaxConcurrentHandshakes: handshakes, WriteTimeout: deadline})

	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}

		awaitTransport(t, l, 0, 0)
	})

	return l
}

func awaitTransport(t *testing.T, l *transportListener, connections, handshakes int) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for len(l.connections) != connections || len(l.handshakes) != handshakes {
		if time.Now().After(deadline) {
			t.Fatalf("transport slots: connections=%d handshakes=%d, want %d/%d", len(l.connections), len(l.handshakes), connections, handshakes)
		}

		time.Sleep(time.Millisecond)
	}
}

func dialTransport(t *testing.T, l *transportListener) net.Conn {
	t.Helper()

	conn, err := net.DialTimeout("tcp", l.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeTransport(conn) })

	return conn
}

func expectTransportClosed(t *testing.T, conn net.Conn) {
	t.Helper()

	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	_, err := conn.Read(make([]byte, 1))

	var timeout net.Error
	if err == nil || errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatalf("excess or closed transport retained: %v", err)
	}
}

func TestTransportSilentPeersBoundAndRelease(t *testing.T) {
	for _, limits := range []struct {
		name                    string
		connections, handshakes int
	}{
		{"connections", 2, 3}, {"handshakes", 3, 2},
	} {
		t.Run(limits.name, func(t *testing.T) {
			f := newServingFixture(t)
			l := testTransport(t, f, limits.connections, limits.handshakes, time.Minute)
			first, second := dialTransport(t, l), dialTransport(t, l)
			awaitTransport(t, l, 2, 2)
			// Rejection never enters a per-peer waiter or handshake goroutine.
			for range 16 {
				expectTransportClosed(t, dialTransport(t, l))
			}

			awaitTransport(t, l, 2, 2)
			closeTransport(first)
			awaitTransport(t, l, 1, 1)
			dialTransport(t, l)
			awaitTransport(t, l, 2, 2)

			if err := l.Close(); err != nil {
				t.Fatal(err)
			}

			expectTransportClosed(t, second)
			awaitTransport(t, l, 0, 0)
		})
	}
}

func TestTransportHandshakeDeadlineAndFailure(t *testing.T) {
	f := newServingFixture(t)
	l := testTransport(t, f, 1, 1, 100*time.Millisecond)
	conn := dialTransport(t, l)
	awaitTransport(t, l, 1, 1)
	expectTransportClosed(t, conn)
	awaitTransport(t, l, 0, 0)

	conn = dialTransport(t, l)
	if _, err := io.WriteString(conn, "not a TLS record"); err != nil {
		t.Fatal(err)
	}

	expectTransportClosed(t, conn)
	awaitTransport(t, l, 0, 0)
}

func TestTransportHandshakeBudgetThroughClientCertificate(t *testing.T) {
	f := newServingFixture(t)
	l := testTransport(t, f, 2, 1, time.Minute)

	entered, resume := make(chan struct{}), make(chan struct{})
	defer close(resume)

	raw := dialTransport(t, l)
	conn := tls.Client(raw, &tls.Config{
		RootCAs: f.roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			close(entered)
			<-resume

			return &f.certificate, nil
		},
	})
	done := make(chan error, 1)

	go func() { done <- conn.HandshakeContext(f.ctx) }()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("client certificate callback not reached")
	}
	// ServerHello was already delivered: GetConfigForClient has returned, but
	// the server must retain admission while waiting for the client's flight.
	awaitTransport(t, l, 1, 1)
	expectTransportClosed(t, dialTransport(t, l))

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	awaitTransport(t, l, 0, 0)
	// The client callback is deliberately parked; resume it during cleanup.
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("client handshake leaked")
		}
	})
}

func TestTransportCompletedTLSRetainsOnlyConnectionSlot(t *testing.T) {
	f := newServingFixture(t)
	l := testTransport(t, f, 1, 1, time.Second)

	client := tls.Client(dialTransport(t, l), &tls.Config{RootCAs: f.roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{f.certificate}})
	if err := client.HandshakeContext(f.ctx); err != nil {
		t.Fatal(err)
	}

	awaitTransport(t, l, 1, 0)

	conn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}

	secured, ok := conn.(*tls.Conn)
	if !ok || !secured.ConnectionState().HandshakeComplete || len(secured.ConnectionState().VerifiedChains) == 0 {
		t.Fatal("listener lost concrete TLS type or client certificate authentication")
	}

	expectTransportClosed(t, dialTransport(t, l))
	// Both TLS Close and force-close may happen concurrently; release once.
	closeTransport(conn)
	closeTransport(conn)
	awaitTransport(t, l, 0, 0)
}

func TestTransportProductionLongPollAndIdleAdmission(t *testing.T) {
	f := newServingFixture(t)
	f.a.Server.Config.Limits.MaxConnections = 2
	f.a.Server.Config.Limits.MaxConcurrentHandshakes = 1
	f.a.Server.Config.Limits.WriteTimeout = 100 * time.Millisecond
	f.a.Server.initializeAdmission()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = listener.Close() })

	done := make(chan error, 1)

	go func() { done <- f.a.Server.serve(f.ctx, listener, f.a.Server.tlsConfig(f.ctx, f.serverCertificate)) }()

	t.Cleanup(func() {
		f.cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("serve shutdown blocked")
		}
	})
	client := f.client(t, &f.certificate)

	publication, err := f.a.authority.Current()
	if err != nil {
		t.Fatal(err)
	}

	requestDone := make(chan error, 1)

	go func() {
		response, err := client.Get(fmt.Sprintf("https://%s/v1/snapshot?after=%d", listener.Addr(), publication.Sequence()))
		if response != nil {
			response.Body.Close()
		}

		requestDone <- err
	}()

	awaitServerPolls(t, f.a.Server, 1)
	// A long poll outlives the handshake deadline and does not monopolize it.
	time.Sleep(150 * time.Millisecond)

	other := f.client(t, &f.certificate)
	response, err := other.Get("https://" + listener.Addr().String() + "/invalid")
	responseBody(t, response, err, http.StatusBadRequest)
	// Both the poll and the HTTP keep-alive consume their connection slots.
	raw, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	expectTransportClosed(t, raw)
	f.cancel()

	select {
	case <-requestDone:
	case <-time.After(3 * time.Second):
		t.Fatal("poll retained on shutdown")
	}

	awaitServerPolls(t, f.a.Server, 0)
}

func TestTransportLimitsValidation(t *testing.T) {
	for _, field := range []string{"connections", "handshakes"} {
		for _, value := range []int{0, -1} {
			cfg := testConfig(t).ServerConfig

			if field == "connections" {
				cfg.Limits.MaxConnections = value
			} else {
				cfg.Limits.MaxConcurrentHandshakes = value
			}

			if cfg.Validate() == nil {
				t.Fatalf("accepted %s=%d", field, value)
			}
		}
	}
}

var _ net.Listener = (*transportListener)(nil)
