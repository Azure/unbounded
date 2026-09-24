// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func downstreamPair(t *testing.T, network string) (net.Conn, net.Conn) {
	t.Helper()

	address := "127.0.0.1:0"
	if network == "unix" {
		address = filepath.Join(socketDirectory(t), "down")
	}

	l, err := net.Listen(network, address)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	client, err := net.Dial(network, l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = client.Close() })

	server, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = server.Close() })

	return server, client
}

func TestSpliceContentAndReuse(t *testing.T) {
	data := payload(2 << 20)

	for _, network := range []string{"tcp", "unix"} {
		t.Run(network, func(t *testing.T) {
			var (
				connections atomic.Int64
				remote      string
			)

			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", checksumTag(data))
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))

				if r.Method == "HEAD" {
					return
				}

				if originData, status := decodeOriginData(r.Header); status != 0 || string(originData) != "Bearer stream" {
					t.Error("lost authorization")
				}

				if remote != r.RemoteAddr {
					connections.Add(1)

					remote = r.RemoteAddr
				}

				w.Header().Set("Content-Range", contentRange(0, int64(len(data))-1, int64(len(data))))
				w.WriteHeader(206)
				_, _ = w.Write(data)
			}), ClientOptions{})

			c, err := c.WithOriginData([]byte("Bearer stream"))
			if err != nil {
				t.Fatal(err)
			}

			o, err := c.Open(t.Context(), "/blob")
			if err != nil {
				t.Fatal(err)
			}

			// Exercise exact forwarding and reuse with denied enlargement on a
			// real pipe, independent of the host's configured default capacity.
			pipe, err := newSplicePipeWithFcntl(func(fd uintptr, cmd, value int) (int, error) {
				if cmd == unix.F_SETPIPE_SZ {
					return 0, unix.EPERM
				}

				return unix.FcntlInt(fd, cmd, value)
			})
			if err != nil {
				t.Fatal(err)
			}

			c.streamPool.pipes.put(pipe, true, c.workers)

			var previous *streamConn

			for range 2 {
				dst, receiver := downstreamPair(t, network)

				s, err := o.Stream(t.Context())
				if err != nil {
					t.Fatal(err)
				}

				result := make(chan []byte, 1)

				go func() { body, _ := io.ReadAll(receiver); result <- body }()

				n, err := s.WriteTo(dst)
				_ = dst.Close()
				body := <-result
				stats := s.Stats()
				_ = s.Close()

				if err != nil || n != int64(len(data)) || !bytes.Equal(body, data) {
					t.Fatal(n, err, len(body))
				}

				if stats.SpliceBytes < 1<<20 || stats.SpliceCalls == 0 || stats.BufferedBytes+stats.SpliceBytes != int64(len(data)) {
					t.Fatalf("not actual splice: %+v", stats)
				}

				if idle := c.streamPool.pipes.idle; len(idle) != 1 || idle[0] != pipe || pipe.buffered != 0 {
					t.Fatal("drained pipe not reused")
				}

				var probe [1]byte
				if _, err := unix.Read(pipe.fd[0], probe[:]); !errors.Is(err, unix.EAGAIN) {
					t.Fatal("cached pipe contains payload", err)
				}

				c.streamPool.mu.Lock()
				if len(c.streamPool.idle) != 1 {
					t.Error("complete connection not pooled")
				} else {
					current := c.streamPool.idle[0]
					if previous != nil && previous != current {
						t.Error("connection not reused")
					}

					previous = current
				}
				c.streamPool.mu.Unlock()
			}

			c.CloseIdleConnections()
			assertPipeClosed(t, pipe.fd)
		})
	}
}

func TestSpliceCancellationAndDownstreamFailures(t *testing.T) {
	for _, action := range []string{"cancel", "close", "deadline", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			c := generatedStreamClient(t, 2*PageSize, nil)

			o, err := c.Open(t.Context(), "/blob")
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			s, err := o.Stream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			dst, receiver := downstreamPair(t, "tcp")
			_ = dst.(*net.TCPConn).SetWriteBuffer(4096)
			_ = receiver.(*net.TCPConn).SetReadBuffer(4096)
			done := make(chan error, 1)

			go func() { _, err := s.WriteTo(dst); done <- err }()

			time.Sleep(20 * time.Millisecond)

			switch action {
			case "cancel":
				cancel()
			case "close":
				_ = s.Close()
			case "deadline":
				_ = dst.SetWriteDeadline(time.Now())
			case "disconnect":
				_ = receiver.Close()
			}

			select {
			case err := <-done:
				if err == nil {
					t.Fatal("blocked transfer succeeded")
				}

				if (action == "cancel" || action == "close") && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("blocked transfer did not stop")
			}
		})
	}
}

func TestStreamRawFailures(t *testing.T) {
	for _, mode := range []string{"short", "chunked", "type", "tag", "length", "auth", "headers", "blocked"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "HEAD" {
					w.Header().Set("Content-Length", "100000")
					w.Header().Set("ETag", checksumTag(nil))

					return
				}

				if mode == "blocked" {
					close(started)
					<-r.Context().Done()

					return
				}

				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()

				header := fmt.Sprintf("HTTP/1.1 206 Partial Content\r\nETag: %s\r\nContent-Length: 100000\r\nContent-Range: bytes 0-99999/100000\r\n", checksumTag(nil))

				switch mode {
				case "chunked":
					header += "Transfer-Encoding: chunked\r\n"
				case "type":
					header += "Content-Type: text/plain\r\n"
				case "tag":
					header += "ETag: \"wrong\"\r\n"
				case "length":
					header += "Content-Length: 7\r\n"
				case "auth":
					header = "HTTP/1.1 401 Unauthorized\r\nWWW-Authenticate: Bearer registry\r\nRetry-After: 9\r\nTransfer-Encoding: chunked\r\n"
				case "headers":
					header += "X-Large: " + string(bytes.Repeat([]byte("x"), 8192)) + "\r\n"
				}

				_, _ = io.WriteString(conn, header+"\r\nshort")
			}), ClientOptions{})

			o, err := c.Open(t.Context(), "/blob")
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			s, err := o.Stream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			dst, receiver := downstreamPair(t, "unix")

			go func() { _, _ = io.Copy(io.Discard, receiver) }()

			if mode == "blocked" {
				go func() { <-started; cancel() }()
			}

			_, err = s.WriteTo(dst)
			_ = dst.Close()

			if err == nil {
				t.Fatal("accepted failed response")
			}

			if mode == "short" && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal(err)
			}

			if mode == "blocked" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}

			if mode == "auth" {
				var status *HTTPError
				if !errors.As(err, &status) || status.WWWAuthenticate != "Bearer registry" || status.RetryAfter != "9" {
					t.Fatal(err)
				}
			}

			if len(c.streamPool.idle) != 0 {
				t.Fatal("failed response pooled")
			}
		})
	}
}

func TestSpliceAcrossPagesAndBufferedPrefix(t *testing.T) {
	size := PageSize + 137
	c := generatedStreamClient(t, size, nil)

	o, err := c.Open(t.Context(), "/blob")
	if err != nil {
		t.Fatal(err)
	}

	h := sha256.New()

	zero := make([]byte, 32<<10)
	for left := size; left > 0; {
		n, _ := h.Write(zero[:min(left, int64(len(zero)))])
		left -= int64(n)
	}

	var expected [32]byte
	copy(expected[:], h.Sum(nil))

	s, err := o.Stream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.Prepare(); err != nil {
		t.Fatal(err)
	}

	prefix := make([]byte, 13)
	if _, err := io.ReadFull(s, prefix); err != nil {
		t.Fatal(err)
	}

	dst, receiver := downstreamPair(t, "unix")
	result := make(chan [32]byte, 1)

	go func() {
		h := sha256.New()
		_, _ = h.Write(prefix)
		_, _ = io.Copy(h, receiver)

		var sum [32]byte
		copy(sum[:], h.Sum(nil))

		result <- sum
	}()

	n, err := s.WriteTo(dst)
	_ = dst.Close()

	if err != nil || n != size-13 {
		t.Fatal(n, err)
	}

	if got := <-result; got != expected {
		t.Fatal("corrupt downstream content")
	}

	stats := s.Stats()
	if stats.SpliceBytes < PageSize-16384 || stats.BufferedBytes < int64(len(prefix)) || stats.BufferedBytes+stats.SpliceBytes != size {
		t.Fatal(stats)
	}
}

func TestStreamPrepareEmptyAndTimeout(t *testing.T) {
	var requests []string

	c := generatedStreamClient(t, 0, &requests)

	o, err := c.Open(t.Context(), "/empty")
	if err != nil {
		t.Fatal(err)
	}

	for _, network := range []string{"tcp", "unix"} {
		s, err := o.Stream(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		if err := s.Prepare(); err != nil {
			t.Fatal(err)
		}

		dst, receiver := downstreamPair(t, network)
		n, err := s.WriteTo(dst)
		_ = dst.Close()
		body, readErr := io.ReadAll(receiver)
		_ = s.Close()

		if err != nil || readErr != nil || n != 0 || len(body) != 0 || s.Stats() != (TransferStats{}) || len(requests) != 0 {
			t.Fatalf("empty %s stream: n=%d err=%v readErr=%v body=%d stats=%+v requests=%v", network, n, err, readErr, len(body), s.Stats(), requests)
		}
	}

	c = newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("ETag", checksumTag(nil))
			w.Header().Set("Content-Length", "1")

			return
		}

		<-r.Context().Done()
	}), ClientOptions{Timeout: 50 * time.Millisecond})

	o, err = c.Open(t.Context(), "/blocked")
	if err != nil {
		t.Fatal(err)
	}

	s, err := o.Stream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.Prepare(); err == nil {
		t.Fatal("timeout not enforced")
	}
}
