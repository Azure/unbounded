// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bytes"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func spliceUnixPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()

	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}

	var pair [2]*net.UnixConn

	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "splice-source")
		defer closeQuietly(f)

		conn, err := net.FileConn(f)
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { closeQuietly(conn) })

		pair[i] = conn.(*net.UnixConn) //nolint:forcetypeassert // AF_UNIX stream socket.
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	return pair[0], pair[1]
}

func TestPeekSourceExpiredDeadline(t *testing.T) {
	for _, state := range []string{"empty", "pending", "peer closed", "pending after peer close", "local closed"} {
		t.Run(state, func(t *testing.T) {
			raw, peer := spliceUnixPair(t)

			pending := state == "pending" || state == "pending after peer close"
			if pending {
				if _, err := peer.Write([]byte("abc")); err != nil {
					t.Fatal(err)
				}
			}

			if state == "peer closed" || state == "pending after peer close" {
				closeQuietly(peer)
			}

			if err := raw.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}

			if state == "local closed" {
				closeQuietly(raw)
			}

			n, err := peekSource(raw)

			switch {
			case pending:
				if n != 1 || err != nil {
					t.Fatalf("peek = %d, %v; want 1, nil", n, err)
				}
			case state == "empty":
				assertIs(t, err, unix.EAGAIN)
			case state == "local closed":
				assertIs(t, err, net.ErrClosed)
			default:
				if n != 0 || err != nil {
					t.Fatalf("peek = %d, %v; want EOF", n, err)
				}
			}

			if got := sourceOpen(raw); got != (pending || state == "empty") {
				t.Fatalf("sourceOpen = %v for %s", got, state)
			}

			if pending {
				if err := raw.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}

				var payload [3]byte
				if _, err := io.ReadFull(raw, payload[:]); err != nil || string(payload[:]) != "abc" {
					t.Fatalf("peek consumed data: read %q, %v", payload, err)
				}
			}
		})
	}
}

func TestSpliceFailureExpiredDeadline(t *testing.T) {
	for _, state := range []string{"empty", "pending", "peer closed", "pending after peer close", "local closed"} {
		t.Run(state, func(t *testing.T) {
			raw, peer := spliceUnixPair(t)

			pending := state == "pending" || state == "pending after peer close"
			if pending {
				if _, err := peer.Write([]byte("abc")); err != nil {
					t.Fatal(err)
				}
			}

			if state == "peer closed" || state == "pending after peer close" {
				closeQuietly(peer)
			}

			if err := raw.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}

			if state == "local closed" {
				closeQuietly(raw)
			}

			for _, cause := range []error{os.ErrDeadlineExceeded, unix.ENOSPC, unix.ECONNRESET, unix.EPIPE} {
				err := ioFailure("write", spliceFailure(raw, cause))
				assertIs(t, err, cause)

				wantDestination := pending || cause == unix.EPIPE || (state == "empty" && cause != os.ErrDeadlineExceeded)
				if wantDestination {
					assertIs(t, err, ErrDestination)
					assertNotIs(t, err, ErrUnavailable)
					assertNotIs(t, err, io.ErrUnexpectedEOF)
				} else if cause == os.ErrDeadlineExceeded {
					assertNotIs(t, err, ErrDestination)
					assertNotIs(t, err, ErrUnavailable)
					assertNotIs(t, err, io.ErrUnexpectedEOF)
				} else {
					assertIs(t, err, ErrUnavailable)
					assertIs(t, err, io.ErrUnexpectedEOF)
					assertNotIs(t, err, ErrDestination)
				}
			}
		})
	}
}

func spliceTCPPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()

	l, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(l)

	if err := l.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	dialer := net.Dialer{Timeout: 5 * time.Second}

	conn, err := dialer.DialContext(t.Context(), "tcp4", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeQuietly(conn) })

	peer, err := l.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeQuietly(peer) })

	return conn.(*net.TCPConn), peer //nolint:forcetypeassert // Dialed TCP.
}

func TestObjectWriteToSpliceTCPTimeout(t *testing.T) {
	for _, mode := range []string{"destination stall", "source stall", "success"} {
		t.Run(mode, func(t *testing.T) {
			raw, source := spliceUnixPair(t)

			conn, peer := spliceTCPPair(t)
			if err := source.SetWriteBuffer(2 * copyBufferSize); err != nil {
				t.Fatal(err)
			}

			size := 1
			if mode == "destination stall" {
				// Keep bytes queued even if the splice pipe holds a whole batch.
				size = copyBufferSize + 1

				if err := conn.SetWriteBuffer(4096); err != nil {
					t.Fatal(err)
				}

				if err := peer.SetReadBuffer(4096); err != nil {
					t.Fatal(err)
				}
			}

			payload := bytes.Repeat([]byte("x"), size)
			if n, err := source.Write(payload); n != size || err != nil {
				t.Fatalf("source write = %d, %v", n, err)
			}

			if mode == "source stall" {
				done := make(chan struct{})

				go func() {
					defer close(done)

					_, _ = io.Copy(io.Discard, peer)
				}()

				defer func() {
					closeQuietly(peer)
					<-done
				}()
			}

			o := &Object{conn: &clientConn{Conn: raw}, remaining: copyBufferSize, timeout: 200 * time.Millisecond}
			if mode == "success" {
				o.remaining = int64(size)
			}

			// Call splice directly so buffered writes cannot mask the failure.
			// TCPConn.ReadFrom receives a LimitedReader over the Unix socket.
			dst := newDestination(t.Context(), conn, o.timeout)

			n, err := o.splice(dst, raw)
			if mode == "success" {
				if err != nil || n != int64(size) || o.remaining != 0 {
					t.Fatalf("splice = %d, %v; remaining %d", n, err, o.remaining)
				}

				if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}

				got := make([]byte, size)
				if _, err := io.ReadFull(peer, got); err != nil || !bytes.Equal(got, payload) {
					t.Fatalf("destination read = %q, %v", got, err)
				}

				return
			}

			assertIs(t, err, os.ErrDeadlineExceeded)
			assertNotIs(t, err, ErrUnavailable)
			assertNotIs(t, err, io.ErrUnexpectedEOF)

			if mode == "destination stall" {
				assertIs(t, err, ErrDestination)

				if queued, err := peekSource(raw); queued != 1 || err != nil {
					t.Fatalf("destination stalled without pending source data: %d, %v", queued, err)
				}

				if n <= 0 || n >= int64(size) {
					t.Fatalf("splice = %d bytes; want partial write", n)
				}
			} else {
				assertNotIs(t, err, ErrDestination)

				if n != int64(size) {
					t.Fatalf("splice = %d bytes; source sent %d", n, size)
				}
			}
		})
	}
}
