// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type teardownListener struct {
	net.Listener
	accept func() (net.Conn, error)
	close  func() error
}

func (l teardownListener) Accept() (net.Conn, error) { return l.accept() }
func (l teardownListener) Close() error              { return l.close() }

func TestServeTeardownErrorsAndLateAccept(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		s := &Server{Lifecycle: newLifecycle(nil)}
		s.Config.Limits.ShutdownTimeout = time.Second

		accepted, peer := net.Pipe()
		defer peer.Close()
		defer accepted.Close()

		closing := make(chan struct{})
		acceptErr := errors.New("accept failed during cancellation")
		closeErr := errors.New("listener close failed")
		calls := 0
		listener := teardownListener{
			accept: func() (net.Conn, error) {
				if accepted != nil {
					// Return a connection only after the force-close sweep, while
					// net/http is closing the listener and waiting for Serve.
					<-closing

					conn := accepted
					accepted = nil

					return conn, nil
				}

				return nil, acceptErr
			},
			close: func() error {
				calls++

				close(closing)

				return closeErr
			},
		}
		done := make(chan error, 1)

		go func() { done <- s.serve(ctx, listener, &tls.Config{}) }()

		synctest.Wait()
		cancel()
		synctest.Wait()

		// net/http reports ErrServerClosed once Close starts, even if Accept
		// itself failed. The listener's close failure must still be returned.
		if err := <-done; !errors.Is(err, closeErr) {
			t.Fatalf("teardown error: %v", err)
		}

		if calls != 1 || s.Lifecycle.serving {
			t.Fatalf("close calls = %d, serving = %v", calls, s.Lifecycle.serving)
		}

		if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("late connection was not closed: %v", err)
		}
	})
}

func TestServeTeardownCompletionBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		s := &Server{Lifecycle: newLifecycle(nil)}
		s.Config.Limits.ShutdownTimeout = time.Second
		unblock := make(chan struct{})

		release := sync.OnceFunc(func() { close(unblock) })
		defer release()

		listener := teardownListener{
			accept: func() (net.Conn, error) { <-unblock; return nil, net.ErrClosed },
			close:  func() error { <-unblock; return nil },
		}
		done := make(chan error, 1)

		go func() { done <- s.serve(ctx, listener, &tls.Config{}) }()

		synctest.Wait()
		cancel()

		start := time.Now()

		if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("blocked teardown: %v", err)
		}

		if elapsed := time.Since(start); elapsed != s.Config.Limits.ShutdownTimeout {
			t.Fatalf("completion wait = %s", elapsed)
		}

		if s.Lifecycle.serving {
			t.Fatal("timed-out server still serving ready")
		}

		release()
		synctest.Wait()
	})
}

func TestServeTeardownClosesSlowTLSWrite(t *testing.T) {
	for _, cause := range []string{"cancellation", "accept failure"} {
		t.Run(cause, func(t *testing.T) {
			f := newServingFixture(t)
			f.a.Server.Config.Limits.WriteTimeout = time.Minute
			f.a.Server.Config.Limits.ShutdownTimeout = time.Second
			f.a.Server.Publications.mu.Lock()
			large := *f.a.Server.Publications.current
			large.encoded += strings.Repeat(" ", 16*1024*1024)
			f.a.Server.Publications.current = &large
			f.a.Server.Publications.mu.Unlock()

			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { _ = listener.Close() })

			done := make(chan error, 1)

			config := f.a.Server.tlsConfigWithCertificate(f.ctx, func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return &f.serverCertificate, nil
			})

			go func() { done <- f.a.Server.serve(f.ctx, listener, config) }()

			conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{RootCAs: f.roots, MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{f.certificate}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()

			if _, err := io.WriteString(conn, "GET /v1/snapshot HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
				t.Fatal(err)
			}

			deadline := time.After(5 * time.Second)

			for len(f.a.Server.writes) == 0 {
				select {
				case <-deadline:
					t.Fatal("write not admitted")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			// Keep the peer open without reading. Teardown must release the
			// socket and admission before the much longer write deadline.
			if cause == "cancellation" {
				f.cancel()
			} else if err := listener.Close(); err != nil {
				t.Fatal(err)
			}

			select {
			case err := <-done:
				if cause == "cancellation" && err != nil || cause == "accept failure" && !errors.Is(err, net.ErrClosed) {
					t.Fatalf("serve result: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("TLS teardown blocked")
			}

			awaitServerPolls(t, f.a.Server, 0)

			if len(f.a.Server.writes) != 0 || f.a.Server.Ready(nil) == nil {
				t.Fatal("teardown retained admission or readiness")
			}
		})
	}
}
