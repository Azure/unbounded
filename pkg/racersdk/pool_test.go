// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Explicitly fire even stopped callbacks to model callbacks waiting on Pool.mu.
type poolTestTimer struct {
	delay time.Duration
	fire  func()
}

func (*poolTestTimer) Stop() bool { return true }

type poolTestClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*poolTestTimer
	ages   atomic.Int32
}

func installPoolClock(config *connectionPolicy, age time.Duration) *poolTestClock {
	clock := &poolTestClock{now: time.Now()}
	config.Now = func() time.Time { clock.mu.Lock(); defer clock.mu.Unlock(); return clock.now }
	config.MaxAge = age
	config.Jitter = func(n int64) int64 { clock.ages.Add(1); return n - 1 }
	config.AfterFunc = func(d time.Duration, f func()) poolTimer {
		clock.mu.Lock()
		defer clock.mu.Unlock()

		timer := &poolTestTimer{delay: d, fire: f}
		clock.timers = append(clock.timers, timer)

		return timer
	}

	return clock
}

func (clock *poolTestClock) advance(d time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()

	clock.now = clock.now.Add(d)
}

func (clock *poolTestClock) latest() *poolTestTimer {
	clock.mu.Lock()
	defer clock.mu.Unlock()

	return clock.timers[len(clock.timers)-1]
}

func pipeConnPool(t *testing.T) (*connectionPool, *poolTestClock) {
	t.Helper()

	config := connectionPolicy{IdleTimeout: 90 * time.Second}
	clock := installPoolClock(&config, 4*time.Second)
	config.Dial = func(context.Context, string, string) (net.Conn, error) {
		conn, peer := net.Pipe()

		t.Cleanup(func() { closeQuietly(peer) })

		return conn, nil
	}
	p := newTestPool(config)

	t.Cleanup(func() { closeQuietly(p) })

	return p, clock
}

func checkoutConn(t *testing.T, p *connectionPool) (*pooledConn, bool) {
	t.Helper()

	conn, reused, err := p.Get(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeQuietly(conn) })

	return conn, reused
}

func TestPoolConnectionAgeConfigAndJitter(t *testing.T) {
	for _, maxAge := range []time.Duration{1, 2, 3, 4, 5, 7, 8, 9, time.Minute, math.MaxInt64} {
		t.Run(strconv.FormatInt(int64(maxAge), 10), func(t *testing.T) {
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
				p, clock := pipeConnPool(t)

				conn, reused := checkoutConn(t, p)
				if reused || conn.expiresAt != p.config.Now().Add(4*time.Second) {
					t.Fatal("fresh connection deadline")
				}

				if at == "recycle" {
					clock.advance(4 * time.Second)
					p.Recycle(conn)
				} else {
					if at == "idle" {
						p.config.IdleTimeout = time.Second
					}

					clock.advance(time.Second)
					p.Recycle(conn)

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
						next, reused := checkoutConn(t, p)
						if next == conn || reused {
							t.Fatal("expired connection checked out")
						}

						closeQuietly(next)
					}

					timer.fire()
					timer.fire()
				}

				closeQuietly(conn)

				wantRotations, wantDials := uint64(1), uint64(1)
				if at == "idle" {
					wantRotations = 0
				}

				if at == "checkout" {
					wantDials = 2
				}

				if s := p.Stats(); s.ConnectionRotations != wantRotations || s.Dials != wantDials || s.ConnectionReuses != 0 || s.Connections != 0 || s.IdleConnections != 0 {
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
	for _, state := range []string{"active", "recycled"} {
		t.Run(state, func(t *testing.T) {
			p, clock := pipeConnPool(t)
			conn, _ := checkoutConn(t, p)
			p.Recycle(conn)

			old := clock.latest()
			clock.advance(time.Second)

			next, reused := checkoutConn(t, p)
			if next != conn || !reused {
				t.Fatal("healthy connection not reused")
			}

			if state == "recycled" {
				p.Recycle(next)
				old.fire()

				if s := p.Stats(); s.Connections != 1 || s.IdleConnections != 1 || s.ConnectionRotations != 0 {
					t.Fatal("old timer removed a newer idle generation", s)
				}

				if clock.latest().delay != 3*time.Second || clock.ages.Load() != 1 {
					t.Fatal("reuse renewed lifetime")
				}

				return
			}

			clock.advance(3 * time.Second)
			old.fire()

			if s := p.Stats(); s.Connections != 1 || s.ConnectionRotations != 0 {
				t.Fatal("old timer interrupted active response", s)
			}

			p.Recycle(conn)
			old.fire()

			if s := p.Stats(); s.ConnectionRotations != 1 || s.Connections != 0 || clock.ages.Load() != 1 {
				t.Fatal(s)
			}
		})
	}
}

func TestConnectionAgeCloseTimerRace(t *testing.T) {
	for range 50 {
		p, clock := pipeConnPool(t)
		conn, _ := checkoutConn(t, p)
		p.Recycle(conn)

		timer := clock.latest()
		clock.advance(4 * time.Second)

		var wg sync.WaitGroup
		wg.Go(timer.fire)
		wg.Go(timer.fire)
		wg.Go(func() { closeQuietly(p) })
		wg.Wait()
		timer.fire()

		if s := p.Stats(); s.ConnectionRotations > 1 || s.Connections != 0 || s.IdleConnections != 0 || s.Dials != 1 {
			t.Fatal("close/age race double counted or leaked", s)
		}
	}
}

func TestConnectionAgeUnusableResponsesDoNotRotate(t *testing.T) {
	for _, mode := range []string{"partial", "close", "surplus", "failed"} {
		t.Run(mode, func(t *testing.T) {
			p, clock := pipeConnPool(t)
			conn, _ := checkoutConn(t, p)
			body := newConnectionBody(conn)
			body.SetReusable(true)

			switch mode {
			case "partial", "close", "failed":
				body.SetReusable(false)
			case "surplus":
				conn.Reader.Reset(strings.NewReader("extra"))

				if _, err := conn.Reader.Peek(1); err != nil {
					t.Fatal(err)
				}
			}

			clock.advance(4 * time.Second)
			closeQuietly(body)
			closeQuietly(body)

			if s := p.Stats(); s.ConnectionRotations != 0 || s.Connections != 0 || s.IdleConnections != 0 {
				t.Fatal("unusable response counted as age retirement", s)
			}
		})
	}
}

func TestConnectionAgeCheckoutSkipsExpiredCandidates(t *testing.T) {
	p, clock := pipeConnPool(t)
	first, _ := checkoutConn(t, p)
	second, _ := checkoutConn(t, p)

	clock.advance(time.Second)

	healthy, _ := checkoutConn(t, p)
	p.Recycle(healthy)
	p.Recycle(first)
	p.Recycle(second)
	clock.advance(3 * time.Second)

	next, reused := checkoutConn(t, p)
	if next != healthy || !reused {
		t.Fatal("did not reuse healthy candidate after retiring expired candidates")
	}

	closeQuietly(next)

	if s := p.Stats(); s.ConnectionRotations != 2 || s.Dials != 3 || s.ConnectionReuses != 1 || s.Connections != 0 {
		t.Fatal(s)
	}
}

func TestConnectionAgeRealTimer(t *testing.T) {
	conn, peer := net.Pipe()
	defer closeQuietly(peer)

	p := newTestPool(connectionPolicy{MaxAge: 20 * time.Millisecond, IdleTimeout: 90 * time.Second, Jitter: func(n int64) int64 { return n - 1 }, Dial: func(context.Context, string, string) (net.Conn, error) { return conn, nil }})
	defer closeQuietly(p)

	pooled, _ := checkoutConn(t, p)
	p.Recycle(pooled)

	if err := peer.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatal("idle age timer did not close connection", err)
	}
	// Stats takes Pool.mu, observing the completed timer's accounting.
	if s := p.Stats(); s.ConnectionRotations != 1 || s.Dials != 1 || s.Connections != 0 || s.IdleConnections != 0 {
		t.Fatal(s)
	}
}

func TestBodyRecycleAndClose(t *testing.T) {
	p, clock := pipeConnPool(t)
	conn, _ := checkoutConn(t, p)
	body := newConnectionBody(conn)
	body.SetReusable(true)
	closeQuietly(body)
	closeQuietly(body)

	if s := p.Stats(); s.IdleConnections != 1 || s.Connections != 1 {
		t.Fatal(s)
	}

	timer := clock.latest()

	p.CloseIdle()
	timer.fire()

	if s := p.Stats(); s.IdleConnections != 0 || s.Connections != 0 || s.ConnectionRotations != 0 {
		t.Fatal(s)
	}

	next, reused := checkoutConn(t, p)
	if reused || next == conn {
		t.Fatal("CloseIdle did not permit a fresh checkout")
	}

	closeQuietly(p)
	p.Recycle(next)

	if s := p.Stats(); s.Connections != 0 || s.IdleConnections != 0 {
		t.Fatal(s)
	}

	_, _, err := p.Get(context.Background(), false)
	assertKind(t, err, ErrorClosed)

	if p.Stats().Dials != 2 {
		t.Fatal(err, p.Stats())
	}
}

func TestDialFailureAndCancellation(t *testing.T) {
	for _, mode := range []string{"failure", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			config := connectionPolicy{MaxAge: time.Minute, IdleTimeout: time.Minute}
			clock := installPoolClock(&config, time.Minute)

			var peer net.Conn

			config.Dial = func(context.Context, string, string) (net.Conn, error) {
				if mode == "failure" {
					return nil, io.EOF
				}

				var conn net.Conn

				conn, peer = net.Pipe()

				cancel()

				return conn, nil
			}

			p := newTestPool(config)
			defer closeQuietly(p)

			_, reused, err := p.Get(ctx, false)

			want := io.EOF
			if mode == "canceled" {
				want = context.Canceled

				defer closeQuietly(peer)

				if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
					t.Fatal("canceled socket leaked", err)
				}
			}

			if !errors.Is(err, want) || reused || clock.ages.Load() != 0 || p.Stats().Dials != 1 || p.Stats().Connections != 0 {
				t.Fatal(err, p.Stats())
			}
		})
	}
}

func TestStaleError(t *testing.T) {
	for _, tt := range []struct {
		name  string
		err   error
		stale bool
	}{
		{"wrapped-eof", fmt.Errorf("wrapped: %w", io.EOF), true},
		{"wrapped-reset", fmt.Errorf("wrapped: %w", syscall.ECONNRESET), true},
		{"wrapped-pipe", fmt.Errorf("wrapped: %w", syscall.EPIPE), true},
		{"nil", nil, false},
		{"deadline", context.DeadlineExceeded, false},
		{"canceled", context.Canceled, false},
		{"truncated", io.ErrUnexpectedEOF, false},
		{"closed-socket", net.ErrClosed, false},
		{"closed-pipe", io.ErrClosedPipe, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := staleConnectionError(tt.err); got != tt.stale {
				t.Fatalf("staleConnectionError(%v) = %v, want %v", tt.err, got, tt.stale)
			}
		})
	}
}

func TestFreshCheckoutAndShutdownDuringDial(t *testing.T) {
	t.Run("fresh", func(t *testing.T) {
		p, _ := pipeConnPool(t)
		idle, _ := checkoutConn(t, p)
		p.Recycle(idle)

		fresh, reused, err := p.Get(context.Background(), true)
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuietly(fresh)

		if reused || fresh == idle || p.Stats().IdleConnections != 1 || p.Stats().Dials != 2 {
			t.Fatal(p.Stats())
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})

		conn, peer := net.Pipe()
		defer closeQuietly(peer)

		p := newTestPool(connectionPolicy{MaxAge: time.Minute, IdleTimeout: time.Minute, Dial: func(context.Context, string, string) (net.Conn, error) {
			close(entered)
			<-release

			return conn, nil
		}})
		defer closeQuietly(p)

		done := make(chan *pooledConn, 1)

		go func() {
			lease, _, err := p.Get(context.Background(), false)
			if err != nil {
				t.Error(err)
			}

			done <- lease
		}()

		<-entered
		closeQuietly(p)
		close(release)

		lease := <-done
		if lease == nil {
			t.Fatal("in-flight lease ownership lost")
		}

		p.Recycle(lease)

		if s := p.Stats(); s.Connections != 0 || s.IdleConnections != 0 || s.Dials != 1 || s.ConnectionRotations != 0 {
			t.Fatal(s)
		}
	})
}

type closeErrorConn struct {
	net.Conn
	closes atomic.Int32
}

func (c *closeErrorConn) Close() error {
	c.closes.Add(1)
	closeQuietly(c.Conn)

	return io.ErrClosedPipe
}

func TestCloseErrorAccountingAndClosedBeforeAge(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(strconv.FormatBool(shutdown), func(t *testing.T) {
			conn, peer := net.Pipe()
			defer closeQuietly(peer)

			wrapped := &closeErrorConn{Conn: conn}
			config := connectionPolicy{IdleTimeout: time.Minute, Dial: func(context.Context, string, string) (net.Conn, error) { return wrapped, nil }}
			clock := installPoolClock(&config, time.Second)

			p := newTestPool(config)
			defer closeQuietly(p)

			lease, _ := checkoutConn(t, p)
			body := newConnectionBody(lease)
			body.SetReusable(true)
			clock.advance(time.Second)

			wantRotations := uint64(1)

			if shutdown {
				closeQuietly(p)

				wantRotations = 0
			}

			closeQuietly(body)
			closeQuietly(body)
			body.SetReusable(true)
			closeQuietly(lease)

			if s := p.Stats(); s.ConnectionRotations != wantRotations || s.Connections != 0 || s.IdleConnections != 0 || wrapped.closes.Load() != 1 {
				t.Fatal(s, wrapped.closes.Load())
			}
		})
	}
}

func TestDefaultDialPolicy(t *testing.T) {
	// A canceled context exercises the default dialer without relying on a
	// platform-specific Unix socket backlog to force a deterministic timeout.
	p := newTestPool(connectionPolicy{Path: "unused", DialTimeout: time.Nanosecond, MaxAge: time.Minute, IdleTimeout: time.Minute})
	defer closeQuietly(p)

	if p.Stats().Dials != 0 || p.config.DialTimeout != time.Nanosecond {
		t.Fatal(p.Stats(), p.config)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, reused, err := p.Get(ctx, false)
	if err == nil || reused || p.Stats().Dials != 1 || p.Stats().Connections != 0 {
		t.Fatal(err, p.Stats())
	}
}

func newTestPool(config connectionPolicy) *connectionPool {
	return &connectionPool{config: defaultPoolConfig(config)}
}

func closeQuietly(c io.Closer) { closeBody(c) }

func installConnClock(c *Client, age time.Duration) *poolTestClock {
	config := c.bulk.config
	clock := installPoolClock(&config, age)
	c.configurePools(config)

	return clock
}

func TestConnectionAgeConfigAndJitter(t *testing.T) {
	c := testClient(t, "unused", 1)
	if c.config.MaxConnAge != 5*time.Minute || c.Stats().Dials != 0 {
		t.Fatal("default or eager dial", c.config.MaxConnAge, c.Stats())
	}

	_, err := NewClient(ClientConfig{Volume: VolumeName{value: "test"}, MaxConnAge: -1})
	assertKind(t, err, ErrorInvalidArgument)

	for _, maxAge := range []time.Duration{1, 2, 3, 4, 5, 7, 8, 9, time.Minute, math.MaxInt64} {
		t.Run(strconv.FormatInt(int64(maxAge), 10), func(t *testing.T) {
			configured, err := NewClient(ClientConfig{Volume: VolumeName{value: "test"}, MaxConnAge: maxAge})
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
	config := c.bulk.config

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

// TestRealRuntimeConnectionAgeLoad is driven by the opt-in Rust process fixture.
// Only socket paths differ from deployment; clocks, jitter, dialing, and reuse
// are production SDK behavior. No fake HTTP server or connection is substituted.
func TestRealRuntimeConnectionAgeLoad(t *testing.T) {
	path := os.Getenv("RACER_SDK_AGE_SOCKET")
	if path == "" {
		t.Skip("requires the real_sdk_connection_age_sustained Rust executable fixture")
	}

	age, err := time.ParseDuration(os.Getenv("RACER_SDK_AGE"))
	if err != nil || age <= 0 {
		t.Fatal("invalid RACER_SDK_AGE", err)
	}

	duration := 8 * time.Second
	if text := os.Getenv("RACER_SDK_AGE_DURATION"); text != "" {
		duration, err = time.ParseDuration(text)
		if err != nil || duration < 4*time.Second || duration > 30*time.Second {
			t.Fatal("duration must be between 4s and 30s", err)
		}
	}

	newLoadClient := func(path string) *Client {
		c, err := newClient(ClientConfig{
			Volume: VolumeName{value: "age"}, MaxConnAge: age,
			MaxConnections: 2, MetadataConnections: 1, SmallObjectConnections: 1,
			QueueTimeout: 3 * time.Second,
		}, path)
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { closeBody(c) })

		return c
	}
	bulk, small := newLoadClient(path), newLoadClient(os.Getenv("RACER_SDK_AGE_SMALL_SOCKET"))
	request := Request{Context: FetchContext{authorization: Authorization{value: "fixture-credential"}, metadata: AdapterMetadata{value: "fixture-metadata"}}}

	ctx, cancel := context.WithTimeout(context.Background(), duration+10*time.Second)
	defer cancel()

	type measurement struct {
		Kind    string
		Latency time.Duration
		Bytes   int64
		Err     error
	}

	results := make(chan measurement, 32)
	start := time.Now()
	until := start.Add(duration)

	var wg sync.WaitGroup
	// Four bulk producers compete for two slots. One deliberately holds a live
	// response past the rotating age. Fast metadata/small traffic uses separate
	// reservations; a paced bulk producer adds skew rather than lockstep reuse.
	for worker := range 8 {
		wg.Go(func() {
			kind := "bulk"
			if worker >= 6 {
				kind = "small"
			} else if worker >= 4 {
				kind = "stat"
			}

			for time.Now().Before(until) {
				began := time.Now()

				n, err := ageLoadRequest(ctx, bulk, small, request, worker)
				results <- measurement{kind, time.Since(began), n, err}

				if err != nil {
					return
				}

				pause := 10 * time.Millisecond
				if worker == 3 {
					pause = 100 * time.Millisecond
				}

				time.Sleep(pause)
			}
		})
	}

	go func() { wg.Wait(); close(results) }()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	latencies := map[string][]time.Duration{}

	var (
		bytes                                            int64
		failures, peakQueue, peakActive, peakConnections int
		samples                                          []map[string]any
	)

	for results != nil {
		select {
		case r, ok := <-results:
			if !ok {
				results = nil
				continue
			}

			if r.Err != nil {
				failures++

				t.Errorf("%s: %v", r.Kind, r.Err)
			} else {
				latencies[r.Kind] = append(latencies[r.Kind], r.Latency)
				bytes += r.Bytes
			}
		case <-ticker.C:
			b, s := bulk.Stats(), small.Stats()
			peakQueue = max(peakQueue, b.QueueDepth+s.QueueDepth)
			peakActive = max(peakActive, b.ActiveBulk+b.ActiveMetadata+s.ActiveSmallObjects)

			peakConnections = max(peakConnections, int(b.Connections+s.Connections))
			if len(samples) == 0 || time.Since(start).Seconds() >= float64(len(samples)) {
				samples = append(samples, map[string]any{"seconds": time.Since(start).Seconds(), "bulk": b, "small": s})
			}
		}
	}

	elapsed := time.Since(start).Seconds()
	summary := map[string]any{}
	total := 0

	for kind, values := range latencies {
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		total += len(values)
		summary[kind] = map[string]any{"completed": len(values), "p50_ms": float64(values[(len(values)-1)/2]) / 1e6, "p99_ms": float64(values[(len(values)-1)*99/100]) / 1e6, "max_ms": float64(values[len(values)-1]) / 1e6}
	}

	b, s := bulk.Stats(), small.Stats()

	report, err := json.Marshal(map[string]any{"age": age.String(), "elapsed_seconds": elapsed, "verified_bytes": bytes, "MiB_per_second": float64(bytes) / (1 << 20) / elapsed, "requests_per_second": float64(total) / elapsed, "failures": failures, "latency": summary, "peak_sampled_queue": peakQueue, "peak_sampled_active": peakActive, "peak_sampled_connections": peakConnections, "bulk_stats": b, "small_stats": s, "samples": samples})
	if err != nil {
		t.Fatal(err)
	}

	t.Log("SDK_AGE", string(report))

	if len(latencies) != 3 || peakQueue == 0 || b.QueueWaits == 0 {
		t.Error("mixed traffic or queue pressure not exercised")
	}

	for _, stats := range []Stats{b, s} {
		if stats.Retries != 0 || stats.QueueRejections != 0 || stats.QueueTimeouts != 0 || stats.QueueDepth != 0 || stats.ActiveBulk != 0 || stats.ActiveMetadata != 0 || stats.ActiveSmallObjects != 0 {
			t.Error("unexpected retries, rejection, timeout, or retained admission", stats)
		}

		if age > duration && stats.ConnectionRotations != 0 {
			t.Error("baseline rotated", stats)
		}
	}

	// HEAD connections remain reusable and must still rotate under sustained
	// traffic. POST subscriptions close at completion, including SmallObject;
	// every completed small read must dial once, never reuse or age-rotate.
	if age <= time.Second && b.ConnectionRotations < 3 {
		t.Error("too few metadata rotation cycles", b)
	}

	if b.ConnectionReuses == 0 || b.Dials < uint64(len(latencies["bulk"]))+1 {
		t.Error("metadata reuse or dedicated bulk subscriptions not exercised", b)
	}

	if s.Dials != uint64(len(latencies["small"])) || s.ConnectionReuses != 0 || s.ConnectionRotations != 0 || s.Connections != 0 || s.IdleConnections != 0 {
		t.Error("small subscriptions must use one dedicated connection per read", s)
	}
}

func ageLoadRequest(ctx context.Context, bulk, small *Client, request Request, worker int) (int64, error) {
	if worker == 4 || worker == 5 {
		m, err := bulk.Stat(ctx, request)
		if err == nil && (m.Size != PageSize+113 || m.ETag.String() != `"restart-v1"`) {
			err = fmt.Errorf("incorrect Stat metadata: %+v", m)
		}

		return 0, err
	}

	c, length := bulk, int64(PageSize)+113

	options := ReadOptions{}
	if worker >= 6 {
		c, length, options.SmallObject = small, 113, true
	}
	// Mix whole-object subscriptions with an explicit cross-page range.
	first := int64(0)
	if worker == 3 {
		first, length = int64(PageSize)-41, 97
		options.Offset, options.Length = ByteOffset(first), ByteLength(length)
	}

	v, err := c.Get(ctx, request, options)
	if err != nil {
		return 0, err
	}
	defer closeBody(v)

	wantSize := PageSize + 113
	if worker >= 6 {
		wantSize = 113
	}

	if v.Metadata().Size != wantSize || v.Metadata().ETag.String() != `"restart-v1"` {
		return 0, fmt.Errorf("incorrect Get metadata")
	}

	if worker == 0 {
		// Always exceed the rotating phase's 500 ms maximum, without changing the
		// baseline's workload. A frame remains active, not an idle pooled socket.
		select {
		case <-time.After(1100 * time.Millisecond):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	sink := &agePayloadSink{offset: first}

	n, err := io.Copy(sink, v)
	if err == nil && n != length {
		err = fmt.Errorf("length %d, want %d", n, length)
	}

	return n, err
}

type agePayloadSink struct{ offset int64 }

func TestAgePayloadSinkRejectsCorruption(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		sink := &agePayloadSink{offset: int64(PageSize) - 1}

		data := make([]byte, 3)
		for i := range data {
			offset := sink.offset + int64(i)
			data[i] = byte((offset*31 + offset/int64(PageSize)*17) % 251)
		}

		if corrupt {
			data[1] ^= 1
		}

		n, err := sink.Write(data)
		if corrupt {
			if n != 1 || err == nil {
				t.Fatal("corruption accepted", n, err)
			}
		} else if n != len(data) || err != nil || sink.offset != int64(PageSize)+2 {
			t.Fatal("valid cross-page payload rejected", n, err, sink.offset)
		}
	}
}

func (s *agePayloadSink) Write(p []byte) (int, error) {
	for i, b := range p {
		offset := s.offset + int64(i)

		want := byte((offset*31 + offset/int64(PageSize)*17) % 251)
		if b != want {
			return i, fmt.Errorf("corrupt byte at %d: %d != %d", offset, b, want)
		}
	}

	s.offset += int64(len(p))

	return len(p), nil
}

// This is deliberately separate from the fake dataplane tests. The subprocess
// serves production Rust ClientListeners, Coordinator, RangeStream and Fill over
// a real UDS. Only origin content and initial publication are fixture supplied.
func TestRustSubscriptionInterop(t *testing.T) {
	if os.Getenv("RACER_SUBSCRIPTION_INTEROP") != "1" {
		t.Skip("set RACER_SUBSCRIPTION_INTEROP=1 to run the Rust dataplane fixture")
	}

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	directory := socketDir(t)

	ctx, cancel := context.WithTimeout(t.Context(), 240*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "timeout", "--signal=TERM", "--kill-after=10s", "230s", "cargo", "test", "--locked", "--manifest-path", filepath.Join(root, "cmd/racer-dataplane/Cargo.toml"), "--features", "subscription-interop", "--test", "subscription_interop", "go_sdk_subscription_server", "--", "--exact", "--ignored", "--nocapture")

	cmd.Env = append(os.Environ(), "RACER_SUBSCRIPTION_INTEROP_DIR="+directory)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }

	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { done <- cmd.Wait() }()

	reaped := false

	defer func() {
		if reaped {
			return
		}

		if err := os.WriteFile(filepath.Join(directory, "stop"), nil, 0o600); err != nil {
			t.Error(err)
		}

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Rust fixture: %v", err)
			}
		case <-time.After(15 * time.Second):
			cancel()
			<-done
			t.Error("Rust fixture failed to drain")
		}
	}()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		if _, err := os.Stat(filepath.Join(directory, "ready")); err == nil {
			break
		}

		select {
		case err := <-done:
			reaped = true

			t.Fatalf("Rust fixture exited before ready: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}

	// Keep sun_path short even when the shared worktree has a long absolute path.
	anchor, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(anchor)

	path := fmt.Sprintf("/proc/%d/fd/%d/interop/client/socket", os.Getpid(), anchor.Fd())

	volume, err := ParseVolumeName("interop")
	if err != nil {
		t.Fatal(err)
	}

	client, err := newClient(ClientConfig{Volume: volume, MaxConnections: 1, PageWindow: 1, BodyReadTimeout: 5 * time.Second}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(client)

	for key, size := range []int64{0, 4096, int64(PageSize), 3*int64(PageSize) + 13} {
		t.Run("Stat/"+strconv.FormatInt(size, 10), func(t *testing.T) {
			metadata, err := client.Stat(ctx, Request{Key: Key{byte(key)}})
			if err != nil || metadata.Size != ByteLength(size) {
				t.Fatal("v2 HEAD metadata", metadata, err)
			}
		})
		t.Run("Get/"+strconv.FormatInt(size, 10), func(t *testing.T) {
			value, err := client.Get(ctx, Request{Key: Key{byte(key)}})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(value)

			if !value.stream.ordered || value.Metadata().Size != ByteLength(size) {
				t.Fatal("Get did not negotiate ordered immutable subscription")
			}

			sink := &offsetSink{}

			n, err := io.CopyBuffer(sink, value, make([]byte, 32*1024))
			if err != nil || n != size {
				t.Fatalf("ordered bytes=%d want=%d: %v", n, size, err)
			}
		})
	}

	t.Run("Get/read-ahead-large-bounded", func(t *testing.T) {
		value, err := client.Get(ctx, Request{Key: Key{4}}, ReadOptions{PageCredits: 2, ByteCredits: 2 * PageSize})
		if err != nil {
			t.Fatal(err)
		}
		defer closeBody(value)

		if cap(value.ordered.slots) != 2 {
			t.Fatal("live ordered read did not enable two-buffer read-ahead")
		}

		n, err := io.Copy(&offsetSink{}, value)
		if err != nil || n != 32*int64(PageSize)+13 {
			t.Fatal("large ordered read", n, err)
		}

		orderedClean(t, value)
	})

	for _, credits := range []int{1, 2} {
		t.Run("Get/read-ahead-partial/credits="+strconv.Itoa(credits), func(t *testing.T) {
			options := ReadOptions{Offset: ByteOffset(PageSize - 7), Length: PageSize + 20, PageCredits: credits}

			value, err := client.Get(ctx, Request{Key: Key{3}}, options)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(value)

			n, err := io.Copy(&offsetSink{offset: int64(options.Offset)}, value)
			if err != nil || n != int64(options.Length) {
				t.Fatal("partial ordered read", n, err)
			}

			orderedClean(t, value)
		})
	}

	t.Run("OpenPages/partial-release-final", func(t *testing.T) {
		options := ReadOptions{Offset: ByteOffset(PageSize - 7), Length: 2*PageSize + 20, PageCredits: 1, ByteCredits: PageSize}

		stream, err := client.OpenPages(ctx, Request{Key: Key{3}}, options)
		if err != nil {
			t.Fatal(err)
		}
		defer closeBody(stream)

		if stream.ordered {
			t.Fatal("OpenPages default must negotiate unordered delivery")
		}
		// Split every release into one-byte writes so Rust must retain partial
		// 12-byte control frames across reads rather than assume one recv.
		stream.conn.Conn = &fragmentedReleaseConn{Conn: stream.conn.Conn}

		var total int64

		seen := make(map[uint64]bool)

		for {
			page, err := stream.Next()
			if err == io.EOF {
				break
			}

			if err != nil {
				t.Fatal(err)
			}

			if seen[page.Number] {
				t.Fatal("duplicate page", page.Number)
			}

			seen[page.Number] = true
			checkInteropPage(t, page)
			total += int64(len(page.Data))

			assertInteropAccounting(t, stream)

			if !stream.complete {
				// Bypass SDK credit waiting: the actual Rust socket must be
				// silent while this sole page credit is held by the caller.
				if err := stream.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
					t.Fatal(err)
				}

				_, err := stream.conn.Reader.Peek(1)

				var timeout net.Error
				if !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Fatalf("Rust sent data without released credit: %v", err)
				}

				if err := stream.conn.SetReadDeadline(time.Time{}); err != nil {
					t.Fatal(err)
				}
			}

			if err := page.Release(); err != nil {
				t.Fatal(err)
			}

			if err := page.Release(); err != nil || page.Data != nil {
				t.Fatal("release is not idempotent", err)
			}
		}

		if total != int64(options.Length) || len(seen) != 4 || !stream.complete {
			t.Fatalf("partial range bytes=%d pages=%d complete=%v", total, len(seen), stream.complete)
		}

		assertInteropAccounting(t, stream)
	})

	t.Run("OpenPages/empty", func(t *testing.T) {
		stream, err := client.OpenPages(ctx, Request{})
		if err != nil {
			t.Fatal(err)
		}
		defer closeBody(stream)

		if page, err := stream.Next(); page != nil || err != io.EOF || !stream.complete {
			t.Fatalf("empty terminal frame: %v %v", page, err)
		}

		assertInteropAccounting(t, stream)
	})

	t.Run("OpenPages/byte-credit-and-final-held", func(t *testing.T) {
		stream, err := client.OpenPages(ctx, Request{Key: Key{3}}, ReadOptions{PageCredits: 2, ByteCredits: PageSize, Length: PageSize + 13})
		if err != nil {
			t.Fatal(err)
		}
		defer closeBody(stream)

		first, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}

		checkInteropPage(t, first)
		assertInteropAccounting(t, stream)
		// There is still a page credit, but no byte credit. Inspect the Rust
		// socket, not just the SDK's local gate, to verify both endpoints.
		if err := stream.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}

		_, err = stream.conn.Reader.Peek(1)

		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("Rust bypassed exhausted byte credit: %v", err)
		}

		if err := stream.conn.SetReadDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}

		if err := first.Release(); err != nil {
			t.Fatal(err)
		}

		last, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}

		checkInteropPage(t, last)

		if len(last.Data) != 13 || !stream.complete {
			t.Fatal("final short page must include validated Complete without release")
		}

		if page, err := stream.Next(); page != nil || err != io.EOF {
			t.Fatal("held final lease blocked EOF", err)
		}

		checkInteropPage(t, last)

		if err := last.Release(); err != nil {
			t.Fatal("final release after remote close", err)
		}

		assertInteropAccounting(t, stream)
	})

	t.Run("OpenPages/large-bounded", func(t *testing.T) {
		stream, err := client.OpenPages(ctx, Request{Key: Key{4}}, ReadOptions{PageCredits: 2, ByteCredits: 2 * PageSize})
		if err != nil {
			t.Fatal(err)
		}
		defer closeBody(stream)

		var total int64

		seen := make(map[ByteOffset]bool)

		for {
			page, err := stream.Next()
			if err == io.EOF {
				break
			}

			if err != nil {
				t.Fatal(err)
			}

			if seen[page.Offset] {
				t.Fatal("duplicate offset", page.Offset)
			}

			seen[page.Offset] = true
			checkInteropPage(t, page)
			total += int64(len(page.Data))

			assertInteropAccounting(t, stream)

			if err := page.Release(); err != nil {
				t.Fatal(err)
			}
		}

		if total != 32*int64(PageSize)+13 || len(seen) != 33 {
			t.Fatalf("bytes=%d pages=%d", total, len(seen))
		}

		assertInteropAccounting(t, stream)
	})

	for _, closeStream := range []bool{false, true} {
		t.Run("cancel-held-credit/close="+strconv.FormatBool(closeStream), func(t *testing.T) {
			requestCtx, stop := context.WithCancel(ctx)
			defer stop()

			stream, err := client.OpenPages(requestCtx, Request{Key: Key{5}}, ReadOptions{PageCredits: 1, ByteCredits: PageSize})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(stream)

			page, err := stream.Next()
			if err != nil {
				t.Fatal(err)
			}

			checkInteropPage(t, page)
			assertInteropAccounting(t, stream)

			blocked := make(chan error, 1)

			go func() { _, err := stream.Next(); blocked <- err }()

			select {
			case err := <-blocked:
				t.Fatalf("Next bypassed held credit: %v", err)
			case <-time.After(50 * time.Millisecond):
			}

			if closeStream {
				closeBody(stream)
			} else {
				stop()
			}

			select {
			case err := <-blocked:
				if err == nil || err == io.EOF || !closeStream && !errors.Is(err, context.Canceled) {
					t.Fatal("wrong cancellation result", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancel retained blocked Next")
			}

			_ = page.Release()

			assertInteropAccounting(t, stream)
			// MaxConnections=1 also verifies canceled admission is returned.
			value, err := client.Get(ctx, Request{Key: Key{1}})
			if err != nil {
				t.Fatal("request after cancellation", err)
			}
			defer closeBody(value)

			if n, err := io.Copy(&offsetSink{}, value); err != nil || n != 4096 {
				t.Fatal("read after cancellation", n, err)
			}
		})
	}

	t.Run("OpenPages/writer-failure", func(t *testing.T) {
		sentinel := errors.New("destination failure")

		stream, err := client.OpenPages(ctx, Request{Key: Key{5}})
		if err != nil {
			t.Fatal(err)
		}
		defer closeBody(stream)

		page, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}

		n, err := (writeFunc(func([]byte) (int, error) { return 0, sentinel })).Write(page.Data)
		if n != 0 || !errors.Is(err, sentinel) {
			t.Fatal(n, err)
		}

		closeBody(stream)

		_ = page.Release()
		if page.Data != nil || client.Stats().ActiveBulk != 0 {
			t.Fatal("failed destination retained ownership")
		}

		assertInteropAccounting(t, stream)
	})
}

func checkInteropPage(t *testing.T, page *PageLease) {
	t.Helper()

	if page.Number != uint64(page.Offset)/uint64(PageSize) || len(page.Data) > int(PageSize) {
		t.Fatal("invalid page geometry")
	}

	if _, err := (&offsetSink{offset: int64(page.Offset)}).Write(page.Data); err != nil {
		t.Fatal(err)
	}
}

func assertInteropAccounting(t *testing.T, stream *PageStream) {
	t.Helper()
	stream.mu.Lock()
	defer stream.mu.Unlock()

	var held uint64
	for _, length := range stream.outstanding {
		held += uint64(length)
	}

	if held != stream.bytesHeld || held > stream.byteCredits || len(stream.outstanding) > stream.pageCredits || stream.sequence.Intervals() > 4096 {
		t.Fatalf("unbounded SDK accounting: bytes=%d pages=%d intervals=%d", held, len(stream.outstanding), stream.sequence.Intervals())
	}
}

type fragmentedReleaseConn struct{ net.Conn }

func (c *fragmentedReleaseConn) Write(p []byte) (int, error) {
	for i := range p {
		if _, err := c.Conn.Write(p[i : i+1]); err != nil {
			return i, err
		}

		time.Sleep(time.Millisecond)
	}

	return len(p), nil
}
