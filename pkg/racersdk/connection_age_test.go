// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Timers are fired explicitly, including stopped callbacks to model an already
// runnable callback waiting on Client.mu. No test changes the process clock.
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
	c.connNow = func() time.Time {
		clock.mu.Lock()
		defer clock.mu.Unlock()

		return clock.now
	}
	c.connAge = func() time.Duration {
		clock.ages.Add(1)
		return age
	}
	c.connAfterFunc = func(d time.Duration, f func()) connectionTimer {
		clock.mu.Lock()
		defer clock.mu.Unlock()

		timer := &connTestTimer{delay: d, fire: f}
		clock.timers = append(clock.timers, timer)

		return timer
	}

	return clock
}

func (clock *connTestClock) advance(d time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()

	clock.now = clock.now.Add(d)
}

func (clock *connTestClock) latest() *connTestTimer {
	clock.mu.Lock()
	defer clock.mu.Unlock()

	return clock.timers[len(clock.timers)-1]
}

func pipeConnClient(t *testing.T) (*Client, *connTestClock) {
	t.Helper()
	c := testClient(t, "unused", 1)
	clock := installConnClock(c, 4*time.Second)
	c.dial = func(context.Context, string, string) (net.Conn, error) {
		conn, peer := net.Pipe()

		t.Cleanup(func() { closeBody(peer) })

		return conn, nil
	}

	return c, clock
}

func checkoutConn(t *testing.T, c *Client, pool *connectionPool) (*pooledConn, bool) {
	t.Helper()

	conn, reused, err := c.connection(context.Background(), pool, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeBody(conn) })

	return conn, reused
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

			for _, upper := range []bool{false, true} {
				age := jitteredConnAge(maxAge, func(n int64) int64 {
					if n != int64(maxAge/4)+1 || n <= 0 {
						t.Fatal("invalid uniform random bound", n)
					}

					if upper {
						return n - 1
					}

					return 0
				})

				want := maxAge - maxAge/4
				if upper {
					want = maxAge
				}

				if age != want || age <= 0 {
					t.Fatal("jitter endpoint", age, want)
				}
			}
		})
	}
}

func TestConnectionAgeLifecycle(t *testing.T) {
	for _, poolName := range []string{"bulk", "metadata", "small"} {
		for _, at := range []string{"checkout", "recycle", "timer", "idle"} {
			t.Run(poolName+"/"+at, func(t *testing.T) {
				c, clock := pipeConnClient(t)
				pool := map[string]*connectionPool{"bulk": &c.bulk, "metadata": &c.metadataPool, "small": &c.smallPool}[poolName]

				conn, reused := checkoutConn(t, c, pool)
				if reused || conn.expiresAt != c.connNow().Add(4*time.Second) {
					t.Fatal("fresh connection deadline")
				}

				if at == "recycle" {
					clock.advance(4 * time.Second)
					c.recycle(pool, conn)
				} else {
					if at == "idle" {
						c.config.IdleConnTimeout = time.Second
					}

					clock.advance(time.Second)
					c.recycle(pool, conn)

					timer := clock.latest()

					wantDelay := 3 * time.Second
					if at == "idle" {
						wantDelay = time.Second
					}

					if timer.delay != wantDelay {
						t.Fatal("idle timer ignores remaining lifetime", timer.delay)
					}

					clock.advance(wantDelay)

					if at == "checkout" {
						next, reused := checkoutConn(t, c, pool)
						if next == conn || reused {
							t.Fatal("expired connection checked out")
						}

						closeBody(next)
					}

					timer.fire()
					timer.fire()
				}

				closeBody(conn)

				wantRotations, wantDials := uint64(1), uint64(1)
				if at == "idle" {
					wantRotations = 0
				}

				if at == "checkout" {
					wantDials = 2
				}

				if s := c.Stats(); s.ConnectionRotations != wantRotations || s.Dials != wantDials || s.Retries != 0 || s.ConnectionReuses != 0 || s.Connections != 0 || s.IdleConnections != 0 {
					t.Fatal(s)
				}

				if clock.ages.Load() != int32(wantDials) {
					t.Fatal("age not chosen once per successful dial")
				}
			})
		}
	}
}

func TestConnectionAgeStaleTimerGeneration(t *testing.T) {
	c, clock := pipeConnClient(t)
	conn, _ := checkoutConn(t, c, &c.bulk)
	c.recycle(&c.bulk, conn)

	old := clock.latest()
	clock.advance(time.Second)

	next, reused := checkoutConn(t, c, &c.bulk)
	if next != conn || !reused {
		t.Fatal("healthy connection not reused")
	}

	clock.advance(3 * time.Second)
	old.fire() // Even an expired connection belongs to its active response.

	if s := c.Stats(); s.Connections != 1 || s.ConnectionRotations != 0 {
		t.Fatal("old timer interrupted active response", s)
	}

	c.recycle(&c.bulk, conn)
	old.fire()

	if s := c.Stats(); s.ConnectionRotations != 1 || s.Connections != 0 || clock.ages.Load() != 1 {
		t.Fatal(s)
	}
}

func TestConnectionAgeOldTimerAfterRecycling(t *testing.T) {
	c, clock := pipeConnClient(t)
	conn, _ := checkoutConn(t, c, &c.bulk)
	c.recycle(&c.bulk, conn)

	old := clock.latest()
	clock.advance(time.Second)

	conn, _ = checkoutConn(t, c, &c.bulk)
	c.recycle(&c.bulk, conn)
	old.fire()

	if s := c.Stats(); s.Connections != 1 || s.IdleConnections != 1 || s.ConnectionRotations != 0 {
		t.Fatal("old timer removed a newer idle generation", s)
	}

	if clock.latest().delay != 3*time.Second || clock.ages.Load() != 1 {
		t.Fatal("reuse renewed lifetime")
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
	c, clock := pipeConnClient(t)
	conn, _ := checkoutConn(t, c, &c.metadataPool)
	c.recycle(&c.metadataPool, conn)
	clock.advance(4 * time.Second)

	c.dial = func(context.Context, string, string) (net.Conn, error) { return nil, io.EOF }

	_, err := c.Stat(context.Background(), Request{})
	if !errors.Is(err, io.EOF) {
		t.Fatal("dial failure lost", err)
	}

	if s := c.Stats(); s.ConnectionRotations != 1 || s.Dials != 2 || s.Retries != 0 || s.ConnectionReuses != 0 || s.Connections != 0 || s.ActiveBulk != 0 || clock.ages.Load() != 1 {
		t.Fatal("rotation became a retry or retained capacity", s)
	}
}

func TestConnectionAgeCloseTimerRace(t *testing.T) {
	for range 50 {
		c, clock := pipeConnClient(t)
		conn, _ := checkoutConn(t, c, &c.bulk)
		c.recycle(&c.bulk, conn)

		timer := clock.latest()
		clock.advance(4 * time.Second)

		var wg sync.WaitGroup
		wg.Go(timer.fire)
		wg.Go(timer.fire)
		wg.Go(func() { closeBody(c) })
		wg.Wait()
		timer.fire()

		if s := c.Stats(); s.ConnectionRotations > 1 || s.Connections != 0 || s.IdleConnections != 0 || s.Retries != 0 || s.Dials != 1 {
			t.Fatal("close/age race double counted or leaked", s)
		}
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

func TestConnectionAgeUnusableResponsesDoNotRotate(t *testing.T) {
	for _, mode := range []string{"partial", "close", "surplus", "failed"} {
		t.Run(mode, func(t *testing.T) {
			c, clock := pipeConnClient(t)
			conn, _ := checkoutConn(t, c, &c.bulk)
			body := &responseBody{client: c, pool: &c.bulk, conn: conn, reusable: true}

			switch mode {
			case "partial":
				// An incomplete HEAD never marks its connection reusable.
				body.reusable = false
			case "close", "failed":
				body.reusable = false
			case "surplus":
				conn.reader.Reset(strings.NewReader("extra"))

				if _, err := conn.reader.Peek(1); err != nil {
					t.Fatal(err)
				}
			}

			clock.advance(4 * time.Second)
			closeBody(body)
			closeBody(body)

			if s := c.Stats(); s.ConnectionRotations != 0 || s.Connections != 0 || s.IdleConnections != 0 {
				t.Fatal("unusable response counted as age retirement", s)
			}
		})
	}
}

func TestConnectionAgeCheckoutSkipsExpiredCandidates(t *testing.T) {
	c, clock := pipeConnClient(t)
	first, _ := checkoutConn(t, c, &c.bulk)
	second, _ := checkoutConn(t, c, &c.bulk)

	clock.advance(time.Second)

	healthy, _ := checkoutConn(t, c, &c.bulk)
	c.recycle(&c.bulk, healthy)
	c.recycle(&c.bulk, first)
	c.recycle(&c.bulk, second)
	clock.advance(3 * time.Second)

	next, reused := checkoutConn(t, c, &c.bulk)
	if next != healthy || !reused {
		t.Fatal("did not reuse healthy candidate after retiring expired candidates")
	}

	closeBody(next)

	if s := c.Stats(); s.ConnectionRotations != 2 || s.Dials != 3 || s.ConnectionReuses != 1 || s.Retries != 0 || s.Connections != 0 {
		t.Fatal(s)
	}
}

func TestConnectionAgeRealTimer(t *testing.T) {
	c := testClient(t, "unused", 1)
	// Keep the production monotonic clock and timer. Only fix the sampled age.
	c.connAge = func() time.Duration { return 20 * time.Millisecond }

	conn, peer := net.Pipe()
	defer closeBody(peer)

	c.dial = func(context.Context, string, string) (net.Conn, error) { return conn, nil }
	pooled, _ := checkoutConn(t, c, &c.bulk)
	c.recycle(&c.bulk, pooled)

	if err := peer.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal("idle age timer did not close connection", err)
	}
	// Stats takes Client.mu, so it observes the completed timer's accounting.
	if s := c.Stats(); s.ConnectionRotations != 1 || s.Dials != 1 || s.Connections != 0 || s.IdleConnections != 0 {
		t.Fatal(s)
	}
}
