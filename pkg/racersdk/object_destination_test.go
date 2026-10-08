// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type destinationPipe struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (p *destinationPipe) Write(b []byte) (int, error) {
	p.once.Do(func() { close(p.started) })
	return p.Conn.Write(b)
}

func TestObjectDestinationInterrupts(t *testing.T) {
	for _, mode := range []string{"object close", "client close", "context", "write timeout"} {
		t.Run(mode, func(t *testing.T) {
			c := serveOnePage(t, func(conn net.Conn, _ *bufio.Reader) {
				_, _ = io.WriteString(conn, strings.Repeat("x", 100))
			})

			c.limits.bodyTimeout = time.Minute
			if mode == "write timeout" {
				c.limits.bodyTimeout = 100 * time.Millisecond
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			o, err := c.Get(ctx, Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(o)

			conn, peer := net.Pipe()
			defer closeQuietly(conn)
			defer closeQuietly(peer)

			dst := &destinationPipe{Conn: conn, started: make(chan struct{})}
			done := make(chan error, 1)

			go func() { _, err := o.WriteTo(dst); done <- err }()

			select {
			case <-dst.started:
			case <-time.After(5 * time.Second):
				t.Fatal("destination write did not start")
			}

			if got := len(c.bulk.slots); got != 1 {
				t.Fatalf("admitted slots = %d; want 1", got)
			}

			want := net.ErrClosed

			switch mode {
			case "object close":
				_ = o.Close()
			case "client close":
				_ = c.Close()
			case "context":
				cancel()

				want = context.Canceled
			case "write timeout":
				want = os.ErrDeadlineExceeded
			}

			select {
			case err := <-done:
				assertIs(t, err, want)
			case <-time.After(5 * time.Second):
				t.Fatal("destination write not interrupted")
			}

			if got := len(c.bulk.slots); got != 0 {
				t.Fatalf("retained %d admission slots", got)
			}

			// Reuse without resetting the write deadline or reopening the connection.
			if err := peer.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}

			go func() { _, err := conn.Write([]byte("ok")); done <- err }()

			var b [2]byte
			if _, err := io.ReadFull(peer, b[:]); err != nil {
				t.Fatal(err)
			}

			if err := <-done; err != nil {
				t.Fatal(err)
			}

			if string(b[:]) != "ok" {
				t.Fatalf("reused destination received %q", b)
			}

			if mode != "client close" {
				next, err := c.Get(t.Context(), Request{})
				if err != nil {
					t.Fatalf("admission was not reusable: %v", err)
				}

				closeQuietly(next)
			}
		})
	}
}

type destinationDeadlineWriter struct {
	set    func(time.Time) error
	writes int
}

func (w *destinationDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	return w.set(deadline)
}
func (w *destinationDeadlineWriter) Write(p []byte) (int, error) { w.writes++; return len(p), nil }

type destinationHTTPWriter struct{ *destinationDeadlineWriter }

func (*destinationHTTPWriter) Header() http.Header { return make(http.Header) }
func (*destinationHTTPWriter) WriteHeader(int)     {}

type destinationHTTPWrapper struct{ http.ResponseWriter }

func (w destinationHTTPWrapper) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestDestinationDeadlineErrors(t *testing.T) {
	failure := errors.New("deadline failed")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"supported", nil}, {"http unsupported", http.ErrNotSupported}, {"file unsupported", os.ErrNoDeadline}, {"failure", failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deadlines []time.Time

			w := &destinationDeadlineWriter{set: func(deadline time.Time) error { deadlines = append(deadlines, deadline); return tc.err }}
			d := newDestination(t.Context(), w, time.Minute)
			stop := d.interruptOnCancel()
			n, err := d.write([]byte("abc"))

			stop()

			if tc.err == failure {
				assertIs(t, err, failure)

				if n != 0 || w.writes != 0 {
					t.Fatal("wrote after deadline failure")
				}
			} else if err != nil || n != 3 || w.writes != 1 {
				t.Fatalf("write = %d, %v; calls %d", n, err, w.writes)
			}

			if len(deadlines) < 2 || deadlines[0].IsZero() || !deadlines[len(deadlines)-1].IsZero() {
				t.Fatalf("deadline lifecycle = %v", deadlines)
			}
		})
	}
}

func TestDestinationCancellationDeadlineOrdering(t *testing.T) {
	for _, httpWriter := range []bool{false, true} {
		name := "direct"
		if httpWriter {
			name = "http wrapped"
		}

		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			arming := make(chan struct{})
			release := make(chan struct{})

			releaseArm := sync.OnceFunc(func() { close(release) })
			defer releaseArm()

			interrupted := make(chan struct{})

			var (
				mu        sync.Mutex
				deadlines []time.Time
			)

			w := &destinationDeadlineWriter{set: func(deadline time.Time) error {
				if deadline.After(time.Now()) {
					close(arming)
					<-release
				}

				mu.Lock()

				deadlines = append(deadlines, deadline)
				mu.Unlock()

				if !deadline.IsZero() && !deadline.After(time.Now()) {
					close(interrupted)
				}

				return nil
			}}

			var writer io.Writer = w
			if httpWriter {
				writer = destinationHTTPWrapper{&destinationHTTPWriter{w}}
			}

			d := newDestination(ctx, writer, time.Minute)

			stop := d.interruptOnCancel()

			defer func() {
				releaseArm()
				stop()
			}()

			armed := make(chan error, 1)

			go func() { armed <- d.arm() }()

			select {
			case <-arming:
			case <-time.After(5 * time.Second):
				t.Fatal("arm did not start")
			}

			cancel()
			releaseArm()

			if err := <-armed; err != nil {
				t.Fatal(err)
			}

			select {
			case <-interrupted:
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation did not set a deadline")
			}

			assertIs(t, d.arm(), context.Canceled)
			assertIs(t, d.disarm(), context.Canceled)
			mu.Lock()
			beforeStop := len(deadlines)
			mu.Unlock()

			if beforeStop != 2 {
				t.Fatalf("cancellation deadline overwritten: %d updates", beforeStop)
			}

			stop()
			stop()
			mu.Lock()
			defer mu.Unlock()

			if len(deadlines) != 3 || !deadlines[2].IsZero() {
				t.Fatalf("cleanup deadlines = %v", deadlines)
			}
		})
	}
}

// TestObjectWriteToOwnsDestinationDeadline pins the documented contract:
// WriteTo replaces a deadline the caller already set on w and leaves w with
// no deadline when it returns.
func TestObjectWriteToOwnsDestinationDeadline(t *testing.T) {
	for _, httpWriter := range []bool{false, true} {
		name := "direct"
		if httpWriter {
			name = "http wrapped"
		}

		t.Run(name, func(t *testing.T) {
			c := fakeClient(t, offsetOrigin(4<<10))

			var (
				mu      sync.Mutex
				current time.Time
				history []time.Time
			)

			w := &destinationDeadlineWriter{set: func(deadline time.Time) error {
				mu.Lock()
				defer mu.Unlock()

				current = deadline
				history = append(history, deadline)

				return nil
			}}

			var writer io.Writer = w
			if httpWriter {
				writer = destinationHTTPWrapper{&destinationHTTPWriter{w}}
			}

			callerDeadline := time.Now().Add(time.Millisecond)
			if err := w.SetWriteDeadline(callerDeadline); err != nil {
				t.Fatal(err)
			}

			o, err := c.Get(t.Context(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(o)

			if n, err := o.WriteTo(writer); err != nil || n != 4<<10 {
				t.Fatalf("WriteTo = %d, %v", n, err)
			}

			mu.Lock()
			defer mu.Unlock()

			if len(history) < 3 || !history[1].After(callerDeadline) {
				t.Fatalf("caller deadline was not replaced: %v", history)
			}

			if !current.IsZero() {
				t.Fatalf("deadline after WriteTo = %v, want none", current)
			}
		})
	}
}
