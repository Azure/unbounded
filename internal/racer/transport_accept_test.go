// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

type temporaryAcceptError struct{}

func (temporaryAcceptError) Error() string   { return "temporary accept failure" }
func (temporaryAcceptError) Timeout() bool   { return false }
func (temporaryAcceptError) Temporary() bool { return true }

func TestTransportTemporaryAcceptRecoversTLS(t *testing.T) {
	f := newServingFixture(t)

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	first := true
	listener := teardownListener{Listener: raw, close: raw.Close, accept: func() (net.Conn, error) {
		if first {
			first = false
			return nil, fmt.Errorf("accept: %w", temporaryAcceptError{})
		}

		return raw.Accept()
	}}
	l := newTransportListener(f.ctx, listener, f.a.Server.tlsConfig(f.ctx, f.serverCertificate), Limits{MaxConnections: 1, MaxConcurrentHandshakes: 1, WriteTimeout: time.Second})

	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}

		select {
		case <-l.acceptDone:
		case <-time.After(time.Second):
			t.Error("accept pump leaked")
		}

		awaitTransport(t, l, 0, 0)
	})

	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()

	client := tls.Client(dialTransport(t, l), &tls.Config{RootCAs: f.roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{f.certificate}})
	if err := client.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}

	conn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}

	secured, ok := conn.(*tls.Conn)
	if !ok || !secured.ConnectionState().HandshakeComplete || len(secured.ConnectionState().VerifiedChains) == 0 {
		t.Fatal("recovered accept lost TLS or client certificate")
	}

	closeTransport(conn)
	awaitTransport(t, l, 0, 0)
}

func TestTransportTemporaryAcceptBackoffCapResetAndTerminal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		accepted, peer := net.Pipe()
		defer accepted.Close()
		defer peer.Close()

		terminal := errors.New("terminal accept failure")

		var calls []time.Time

		listener := teardownListener{close: func() error { return nil }, accept: func() (net.Conn, error) {
			calls = append(calls, time.Now())
			switch len(calls) {
			case 11:
				return accepted, nil
			case 13:
				return nil, terminal
			default:
				return nil, temporaryAcceptError{}
			}
		}}
		// Zero connection capacity rejects the successful accept without a TLS
		// worker; even rejected sockets must reset the accept-error backoff.
		l := newTransportListener(t.Context(), listener, &tls.Config{}, Limits{})
		defer l.Close()

		<-l.acceptDone

		want := []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond, 160 * time.Millisecond, 320 * time.Millisecond, 640 * time.Millisecond, time.Second, time.Second, 0, 5 * time.Millisecond}
		if len(calls) != len(want)+1 {
			t.Fatalf("accept calls=%d", len(calls))
		}

		for i, delay := range want {
			if got := calls[i+1].Sub(calls[i]); got != delay {
				t.Fatalf("retry %d delay=%s want=%s", i, got, delay)
			}
		}

		for range 2 {
			if conn, err := l.Accept(); conn != nil || !errors.Is(err, terminal) {
				t.Fatalf("terminal accept=%v, %v", conn, err)
			}
		}

		if len(l.connections) != 0 || len(l.handshakes) != 0 {
			t.Fatal("accept errors leaked admission")
		}
	})
}

func TestTransportTemporaryAcceptCancellation(t *testing.T) {
	for _, closeListener := range []bool{false, true} {
		t.Run(fmt.Sprintf("close=%v", closeListener), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				calls := 0
				listener := teardownListener{close: func() error { return nil }, accept: func() (net.Conn, error) {
					calls++
					return nil, temporaryAcceptError{}
				}}

				l := newTransportListener(ctx, listener, &tls.Config{}, Limits{})
				defer l.Close()
				// Reach the capped retry window. Virtual time and exact call
				// counts prove the pump sleeps rather than spinning/spawning work.
				time.Sleep(1500 * time.Millisecond)
				synctest.Wait()

				if calls != 9 {
					t.Fatalf("accept calls=%d want=9", calls)
				}

				start := time.Now()

				if closeListener {
					if err := l.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					cancel()
				}

				synctest.Wait()

				select {
				case <-l.acceptDone:
				default:
					t.Fatal("cancellation left accept pump sleeping")
				}

				if conn, err := l.Accept(); conn != nil || !errors.Is(err, net.ErrClosed) {
					t.Fatalf("canceled accept=%v, %v", conn, err)
				}

				if time.Since(start) != 0 || calls != 9 || len(l.connections) != 0 || len(l.handshakes) != 0 {
					t.Fatal("cancellation delayed or leaked accept work")
				}
			})
		})
	}
}
