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
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/connpool"
)

type connTestTimer struct {
	delay time.Duration
	fire  func()
}

func (*connTestTimer) Stop() bool { return true }

type connTestClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*connTestTimer
	ages   atomic.Int32
}

func installConnClock(c *Client, age time.Duration) *connTestClock {
	clock := &connTestClock{now: time.Now()}
	config := c.bulk.Config()
	config.Now = func() time.Time { clock.mu.Lock(); defer clock.mu.Unlock(); return clock.now }
	config.MaxAge = age
	config.Jitter = func(n int64) int64 { clock.ages.Add(1); return n - 1 }
	config.AfterFunc = func(d time.Duration, f func()) connpool.Timer {
		clock.mu.Lock()
		defer clock.mu.Unlock()

		timer := &connTestTimer{delay: d, fire: f}
		clock.timers = append(clock.timers, timer)

		return timer
	}
	c.configurePools(config)

	return clock
}

func (clock *connTestClock) advance(d time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()

	clock.now = clock.now.Add(d)
}

func TestConnectionAgeConfigAndJitter(t *testing.T) {
	c := testClient(t, "unused", 1)
	if c.config.MaxConnAge != 5*time.Minute || c.Stats().Dials != 0 {
		t.Fatal("default or eager dial", c.config.MaxConnAge, c.Stats())
	}

	_, err := NewClient(ClientConfig{Cache: CacheName{value: "test"}, MaxConnAge: -1})
	assertKind(t, err, ErrorInvalidArgument)

	for _, maxAge := range []time.Duration{1, 2, 3, 4, 5, 7, 8, 9, time.Minute, math.MaxInt64} {
		t.Run(strconv.FormatInt(int64(maxAge), 10), func(t *testing.T) {
			configured, err := NewClient(ClientConfig{Cache: CacheName{value: "test"}, MaxConnAge: maxAge})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(configured)

			if configured.config.MaxConnAge != maxAge {
				t.Fatal("positive duration changed")
			}
		})
	}
}

func TestConnectionAgeBusyPools(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", "1")
			w.Header().Set("ETag", `"v"`)
			w.Header().Set("Racer-Expires-At", "0")

			return
		}

		streamResponse(w, 0, 1, 1, `"v"`)
	}))
	c := testClient(t, path, 1)
	clock := installConnClock(c, 4*time.Second)

	for range 12 {
		for _, small := range []bool{false, true} {
			v, err := c.Get(context.Background(), Request{}, ReadOptions{SmallObject: small})
			if err != nil {
				t.Fatal(err)
			}

			if n, err := io.Copy(io.Discard, v); err != nil || n != 1 {
				t.Fatal(n, err)
			}

			closeBody(v)
		}

		if _, err := c.Stat(context.Background(), Request{}); err != nil {
			t.Fatal(err)
		}

		clock.advance(time.Second)
	}

	if s := c.Stats(); s.Dials != 27 || s.ConnectionRotations != 2 || s.ConnectionReuses != 9 || s.Retries != 0 || s.Connections != 1 || s.ActiveBulk != 0 || s.ActiveMetadata != 0 || s.ActiveSmallObjects != 0 {
		t.Fatal("busy reuse prevented rotation or changed admission", s)
	}
}

func TestConnectionAgeActiveResponseCompletes(t *testing.T) {
	release := make(chan struct{})

	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamResponseHead(w, 0, 3, 3, `"v"`)

		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}

		select {
		case <-release:
			_, _ = io.WriteString(w, "abc")
		case <-r.Context().Done():
		}
	}))
	c := testClient(t, path, 1)
	clock := installConnClock(c, 4*time.Second)

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	clock.advance(time.Hour)

	if s := c.Stats(); s.ConnectionRotations != 0 || s.Connections != 1 || s.ActiveBulk != 1 || len(clock.timers) != 0 {
		t.Fatal("age interrupted an active body", s)
	}

	releaseOnce.Do(func() { close(release) })

	b, err := io.ReadAll(v)
	if err != nil || string(b) != "abc" {
		t.Fatal(string(b), err)
	}

	closeBody(v)

	if s := c.Stats(); s.ConnectionRotations != 0 || s.Dials != 1 || s.Retries != 0 || s.Connections != 0 || s.ActiveBulk != 0 {
		t.Fatal(s)
	}
}

func TestConnectionAgeDialFailureAfterRotation(t *testing.T) {
	c := testClient(t, "unused", 1)
	config := c.bulk.Config()

	var fail atomic.Bool

	config.Dial = func(context.Context, string, string) (net.Conn, error) {
		if fail.Load() {
			return nil, io.EOF
		}

		conn, peer := net.Pipe()

		t.Cleanup(func() { closeBody(peer) })

		return conn, nil
	}
	c.configurePools(config)
	clock := installConnClock(c, 4*time.Second)

	conn, _, err := c.metadataPool.Get(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}

	c.metadataPool.Recycle(conn)
	clock.advance(4 * time.Second)
	fail.Store(true)

	_, err = c.Stat(context.Background(), Request{})
	if !errors.Is(err, io.EOF) {
		t.Fatal("dial failure lost", err)
	}

	if s := c.Stats(); s.ConnectionRotations != 1 || s.Dials != 2 || s.Retries != 0 || s.ConnectionReuses != 0 || s.Connections != 0 || s.ActiveBulk != 0 || clock.ages.Load() != 1 {
		t.Fatal("rotation became a retry or retained capacity", s)
	}
}

func TestConnectionAgeCancelRecycleRace(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { streamResponse(w, 0, 1, 1, `"v"`) }))
	for range 50 {
		c := testClient(t, path, 1)
		clock := installConnClock(c, 4*time.Second)
		ctx, cancel := context.WithCancel(context.Background())

		v, err := c.Get(ctx, Request{})
		if err != nil {
			cancel()
			t.Fatal(err)
		}

		clock.advance(4 * time.Second)

		var wg sync.WaitGroup
		wg.Go(func() { _, _ = io.Copy(io.Discard, v) })
		wg.Go(cancel)
		wg.Go(func() { closeBody(v) })
		wg.Wait()
		closeBody(c)

		if s := c.Stats(); s.ConnectionRotations > 1 || s.Connections != 0 || s.ActiveBulk != 0 || s.Retries != 0 || s.Dials != 1 {
			t.Fatal("cancel/recycle race double counted or leaked", s)
		}
	}
}

func TestConnectionAgeBootstrapContinuation(t *testing.T) {
	const size = int64(PageSize) + 37

	var calls atomic.Int32

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		if r.Method != "POST" || r.URL.Path != "/v2/objects/"+(Key{7}).String() || r.Header.Get("Authorization") != "secret\xff" || r.Header.Get("Racer-Metadata") != "opaque  bytes" {
			t.Error("rotation changed request envelope")
		}

		first, length := int64(0), size

		if r.Header.Get("If-Match") != "" || r.Header.Get("Range") != "" {
			t.Error("unexpected pin or range")
		}

		streamResponseHead(w, first, length, size, `"v"`)
		_, _ = io.CopyN(w, &offsetStream{offset: first}, length)
	}))
	c := testClient(t, path, 1)
	clock := installConnClock(c, 4*time.Second)
	request := Request{Key: Key{7}, Context: FetchContext{authorization: Authorization{value: "secret\xff"}, metadata: AdapterMetadata{value: "opaque  bytes"}}}

	v, err := c.Get(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}

	snapshot := v.Metadata()

	sink := &offsetSink{}
	if n, err := io.CopyN(sink, v, int64(PageSize)); n != int64(PageSize) || err != nil {
		t.Fatal(n, err)
	}

	clock.advance(4 * time.Second)

	if c.Stats().Dials != 1 || c.Stats().ActiveBulk != 1 || calls.Load() != 1 {
		t.Fatal("eager continuation or lost admission")
	}

	if n, err := io.Copy(sink, v); n != 37 || err != nil {
		t.Fatal("continuation bytes changed", n, err)
	}

	closeBody(v)

	if s := c.Stats(); s.Dials != 1 || s.ConnectionRotations != 0 || s.Retries != 0 || s.BytesRead != uint64(size) || s.ActiveBulk != 0 || calls.Load() != 1 || v.Metadata() != snapshot {
		t.Fatal("rotation changed continuation contract", s)
	}
}

func TestConnectionAgeContinuationKeepsContext(t *testing.T) {
	entered := make(chan struct{})
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamResponseHead(w, 0, int64(PageSize)+1, int64(PageSize)+1, `"v"`)
		_, _ = io.CopyN(w, repeatedByte('x'), int64(PageSize))

		close(entered)
		<-r.Context().Done()
	}))
	c := testClient(t, path, 1)
	clock := installConnClock(c, 4*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	v, err := c.Get(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := io.CopyN(io.Discard, v, int64(PageSize)); err != nil {
		t.Fatal(err)
	}

	clock.advance(4 * time.Second)

	done := make(chan error, 1)

	go func() { _, err := io.Copy(io.Discard, v); done <- err }()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("rotated continuation not opened")
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("continuation lost original context", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("rotated continuation ignored cancellation")
	}

	closeBody(v)

	if s := c.Stats(); s.ConnectionRotations != 0 || s.Dials != 1 || s.Retries != 0 || s.Connections != 0 || s.ActiveBulk != 0 {
		t.Fatal(s)
	}
}

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

func TestClientIdleAndCloseConnectionPolicy(t *testing.T) {
	for _, policy := range []string{"reuse", "idle", "close"} {
		t.Run(policy, func(t *testing.T) {
			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if policy == "close" {
					w.Header().Set("Connection", "close")
				}

				streamResponse(w, 0, 1, 1, `"v"`)
			}))

			config := ClientConfig{Cache: CacheName{value: "test"}, MaxConnections: 1}
			if policy == "idle" {
				config.IdleConnTimeout = 20 * time.Millisecond
			}

			c, err := newClient(config, path)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(c)

			var dials atomic.Int32

			poolConfig := c.bulk.Config()
			dial := poolConfig.Dial

			poolConfig.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
				dials.Add(1)
				return dial(ctx, network, address)
			}
			c.configurePools(poolConfig)

			for i := range 2 {
				v, err := c.Get(context.Background(), Request{})
				if err != nil {
					t.Fatal(err)
				}

				if n, err := v.WriteTo(io.Discard); n != 1 || err != nil {
					t.Fatal(n, err)
				}

				closeBody(v)

				if policy == "idle" && i == 0 {
					deadline := time.Now().Add(time.Second)

					for {
						idle := c.bulk.Stats().IdleConnections

						if idle == 0 {
							break
						}

						if time.Now().After(deadline) {
							t.Fatal("idle connection retained")
						}

						time.Sleep(time.Millisecond)
					}
				}
			}

			want := int32(2)

			if dials.Load() != want {
				t.Fatal("connection policy", dials.Load(), want)
			}
		})
	}
}
