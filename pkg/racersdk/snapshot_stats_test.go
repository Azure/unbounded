// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestRangeBounds(t *testing.T) {
	if first, last, present := (Range{}).Bounds(); first != 0 || last != 0 || present {
		t.Fatal(first, last, present)
	}

	r, err := ClosedRange(7, 29)
	if err != nil {
		t.Fatal(err)
	}

	if first, last, present := r.Bounds(); first != 7 || last != 29 || !present {
		t.Fatal(first, last, present)
	}
}

func TestReadOptionsSnapshot(t *testing.T) {
	for _, explicitPin := range []bool{false, true} {
		t.Run(map[bool]string{false: "implicit", true: "explicit"}[explicitPin], func(t *testing.T) {
			var gets atomic.Int32

			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gets.Add(1)

				if r.Method != "POST" || r.Header.Get("If-Match") != `"v"` || r.Header.Get("Range") != "bytes=1-" {
					t.Error("snapshot did not skip HEAD or pin exact range")
				}

				w.Header().Set("Racer-Content-Type", "text/plain")
				streamResponse(w, 1, 2, 3, `"v"`)
			}))
			c := testClient(t, path, 1)
			m := originMeta(3)
			m.ContentType = "text/plain"
			original := m

			o := ReadOptions{Offset: 1, Metadata: &m}
			if explicitPin {
				o.Pin = m.ETag
			}

			v, err := c.Get(context.Background(), Request{}, o)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(v)

			m = Metadata{} // The admitted stream owns a copy, including its pin.

			if n, err := io.Copy(io.Discard, v); err != nil || n != 2 {
				t.Fatal(n, err)
			}

			if v.Metadata() != original || gets.Load() != 1 {
				t.Fatal("snapshot changed or extra request")
			}
		})
	}
}

func TestReadOptionsInvalidSnapshotDoesNotFetch(t *testing.T) {
	c := testClient(t, "unused", 1)
	poolConfig := c.bulk.Config()
	poolConfig.Dial = func(context.Context, string, string) (net.Conn, error) {
		t.Error("invalid snapshot performed I/O")
		return nil, errors.New("unexpected dial")
	}
	c.configurePools(poolConfig)

	valid := originMeta(3)
	invalid := []Metadata{{}, valid, valid, valid, valid}
	invalid[1].Size = ByteLength(math.MaxUint64)
	invalid[2].ETag = ETag{}
	invalid[3].ContentType = "text/plain\r\nx: y"

	invalid[4].ExpiresAt = time.Unix(0, 1)
	for _, m := range invalid {
		_, err := c.Get(context.Background(), Request{}, ReadOptions{Metadata: &m})
		assertKind(t, err, ErrorInvalidArgument)
	}

	_, err := c.Get(context.Background(), Request{}, ReadOptions{Metadata: &valid, Pin: ETag{value: `"other"`}})
	assertKind(t, err, ErrorInvalidArgument)
	_, err = c.Get(context.Background(), Request{}, ReadOptions{Metadata: &valid, Offset: 4})
	assertKind(t, err, ErrorUnsatisfiableRange)

	empty := originMeta(0)
	c = testClient(t, clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Match") != empty.ETag.String() {
			t.Error("empty snapshot pin lost")
		}

		streamResponse(w, 0, 0, 0, `"v"`)
	})), 1)

	v, err := c.Get(context.Background(), Request{}, ReadOptions{Metadata: &empty})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	if n, err := v.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatal(n, err)
	}

	if stats := c.Stats(); stats.Dials != 1 || stats.ActiveBulk != 0 || stats.ActiveMetadata != 0 {
		t.Fatal(stats)
	}
}

func TestClientStats(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", "3")
			w.Header().Set("ETag", `"v"`)
			w.Header().Set("Racer-Expires-At", "0")

			return
		}

		streamResponse(w, 0, 3, 3, `"v"`)
	}))

	c, err := newClient(ClientConfig{Cache: CacheName{value: "test"}, MaxConnections: 1, MaxQueuedRequests: 1, QueueTimeout: 100 * time.Millisecond}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(c)

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)

	go func() { _, err := c.Get(ctx, Request{}); done <- err }()

	until := time.Now().Add(time.Second)
	for c.Stats().QueueDepth != 1 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}

	_, err = c.Get(context.Background(), Request{})
	assertKind(t, err, ErrorUnavailable)

	if _, err := c.Stat(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}

	s := c.Stats()
	if s.QueueDepth != 1 || s.QueueWaits != 1 || s.QueueRejections != 1 || s.ActiveBulk != 1 || s.ActiveMetadata != 0 || s.Connections != 2 || s.IdleConnections != 1 || s.Dials != 2 {
		t.Fatal(s)
	}

	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	_, err = c.Get(context.Background(), Request{})
	assertKind(t, err, ErrorDeadline)

	if _, err := io.Copy(io.Discard, v); err != nil {
		t.Fatal(err)
	}

	v, err = c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := io.Copy(io.Discard, v); err != nil {
		t.Fatal(err)
	}

	closeBody(v)

	s = c.Stats()
	if s.QueueDepth != 0 || s.QueueWaits != 2 || s.QueueTimeouts != 1 || s.QueueWaitNanoseconds == 0 || s.ConnectionReuses != 0 || s.BytesRead != 6 || s.ActiveBulk != 0 || s.Retries != 0 {
		t.Fatal(s)
	}

	closeBody(c)

	s = c.Stats()
	if s.Connections != 0 || s.IdleConnections != 0 || s.Dials != 3 || s.BytesRead != 6 {
		t.Fatal("cleanup lost counters or retained gauges", s)
	}
}

// A scripted peer warms one pooled lease, then fails its next exchange. A retry
// is allowed only for an empty EOF/reset, never after any response prefix.
func TestClientStaleRetryBoundary(t *testing.T) {
	for _, method := range []string{"POST", "HEAD"} {
		for _, mode := range []string{"stale", "partial", "malformed", "timeout", "twice", "fresh", "canceled"} {
			t.Run(method+"/"+mode, func(t *testing.T) {
				path := socketDir(t) + "/socket"

				listener, err := net.Listen("unix", path)
				if err != nil {
					t.Fatal(err)
				}
				defer closeBody(listener)

				c, err := newClient(ClientConfig{Cache: CacheName{value: "test"}, MaxConnections: 1, ResponseHeaderTimeout: 40 * time.Millisecond}, path)
				if err != nil {
					t.Fatal(err)
				}
				defer closeBody(c)

				entered := make(chan struct{})
				serverDone := make(chan struct{})

				var exchanges atomic.Int32

				go func() {
					defer close(serverDone)

					conn, err := listener.Accept()
					if err != nil {
						return
					}
					defer closeBody(conn)

					r := bufio.NewReader(conn)
					respond := func(conn net.Conn) {
						if method == "HEAD" {
							_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 1\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\n\r\n")
							return
						}

						_, _ = io.WriteString(conn, subscriptionHead(1, 0, 1))
						_ = fakeSubscriptionFrame(conn, 1, 0, 0, 1)
						_, _ = io.WriteString(conn, "x")
						_ = fakeSubscriptionFrame(conn, 2, 1, 1, 0)
					}

					if mode != "fresh" {
						if _, err := readRawHead(r, false); err != nil {
							return
						}

						exchanges.Add(1)
						respond(conn)

						if method == "POST" {
							closeBody(conn)

							conn, err = listener.Accept()
							if err != nil {
								return
							}
							defer closeBody(conn)

							r = bufio.NewReader(conn)
						}
					}

					if _, err := readRawHead(r, false); err != nil {
						return
					}

					exchanges.Add(1)
					close(entered)

					switch mode {
					case "partial":
						_, _ = io.WriteString(conn, "H")
					case "malformed":
						_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nContent-Length: 0\r\n\r\n")
					case "timeout", "canceled":
						_, _ = r.ReadByte()
						return
					}

					closeBody(conn)

					if method == "HEAD" && (mode == "stale" || mode == "twice") {
						conn, err := listener.Accept()
						if err != nil {
							return
						}
						defer closeBody(conn)

						if _, err := readRawHead(bufio.NewReader(conn), false); err != nil {
							return
						}

						exchanges.Add(1)

						if mode == "stale" {
							respond(conn)
						}
					}
				}()

				read := func(ctx context.Context) error {
					if method == "HEAD" {
						_, err := c.Stat(ctx, Request{})
						return err
					}

					v, err := c.Get(ctx, Request{})
					if err != nil {
						return err
					}
					defer closeBody(v)

					_, err = io.Copy(io.Discard, v)

					return err
				}
				if mode != "fresh" {
					if err := read(context.Background()); err != nil {
						t.Fatal(err)
					}
				}

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				if mode == "canceled" {
					go func() { <-entered; cancel() }()
				}

				err = read(ctx)
				if method == "HEAD" && mode == "stale" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil {
					t.Fatal("failed exchange succeeded")
				}

				if mode == "partial" && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatal(err)
				}

				if mode == "malformed" {
					assertKind(t, err, ErrorProtocol)
				}

				if mode == "canceled" && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}

				closeBody(listener)
				<-serverDone

				want := uint64(0)
				if method == "HEAD" && (mode == "stale" || mode == "twice") {
					want = 1
				}

				wantDials := 1 + want
				if method == "POST" && mode != "fresh" {
					wantDials = 2
				}

				if s := c.Stats(); s.Retries != want || s.Dials != wantDials {
					t.Fatal("retry boundary", s)
				}

				if want == 1 && exchanges.Load() != 3 {
					t.Fatal("retry was not exactly once")
				}
			})
		}
	}
}

func TestReadOptionsSnapshotResponseMismatch(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { streamResponse(w, 0, 3, 3, `"different"`) }))
	c := testClient(t, path, 1)
	m := originMeta(3)
	_, err := c.Get(context.Background(), Request{}, ReadOptions{Metadata: &m})
	assertKind(t, err, ErrorProtocol)

	if s := c.Stats(); s.BytesRead != 0 || s.Retries != 0 || s.Connections != 0 {
		t.Fatal(s)
	}
}

func TestClientStaleIdlePinnedRetryPreservesRequest(t *testing.T) {
	path := socketDir(t) + "/socket"

	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(l)

	if err := l.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}

	closed, done := make(chan struct{}), make(chan struct{})

	go func() {
		defer close(done)

		for i := range 2 {
			conn, err := l.Accept()
			if err != nil {
				t.Error(err)
				return
			}

			head, err := readRawHead(bufio.NewReader(conn), false)
			if err != nil {
				closeBody(conn)
				t.Error(err)

				return
			}

			h := headHeaders(head)
			if h.Get("If-Match") != `"v"` || h.Get("Range") != "bytes=1-" || h.Get("Authorization") != "secret" || h.Get("Racer-Metadata") != "opaque" {
				t.Error("subscription changed request")
			}

			_, _ = io.WriteString(conn, subscriptionHead(3, 1, 3))
			_ = fakeSubscriptionFrame(conn, 1, 0, 1, 2)
			_, _ = io.WriteString(conn, "xx")
			_ = fakeSubscriptionFrame(conn, 2, 1, 2, 0)
			closeBody(conn)

			if i == 0 {
				close(closed)
			}
		}
	}()

	c := testClient(t, path, 1)
	m := originMeta(3)

	request := Request{Context: FetchContext{authorization: Authorization{value: "secret"}, metadata: AdapterMetadata{value: "opaque"}}}
	for i := range 2 {
		v, err := c.Get(context.Background(), request, ReadOptions{Offset: 1, Metadata: &m})
		if err != nil {
			t.Fatal(err)
		}

		if n, err := io.Copy(io.Discard, v); n != 2 || err != nil {
			t.Fatal(n, err)
		}

		closeBody(v)

		if i == 0 {
			<-closed
		}
	}

	<-done

	s := c.Stats()
	if s.Dials != 2 || s.ConnectionReuses != 0 || s.Retries != 0 || s.BytesRead != 4 {
		t.Fatal(s)
	}
}
