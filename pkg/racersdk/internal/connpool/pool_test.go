// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package connpool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Explicitly fire even stopped callbacks to model callbacks waiting on Pool.mu.
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

func installConnClock(config *Config, age time.Duration) *connTestClock {
	clock := &connTestClock{now: time.Now()}
	config.Now = func() time.Time { clock.mu.Lock(); defer clock.mu.Unlock(); return clock.now }
	config.MaxAge = age
	config.Jitter = func(n int64) int64 { clock.ages.Add(1); return n - 1 }
	config.AfterFunc = func(d time.Duration, f func()) Timer {
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

func pipeConnPool(t *testing.T) (*Pool, *connTestClock) {
	t.Helper()

	config := Config{IdleTimeout: 90 * time.Second}
	clock := installConnClock(&config, 4*time.Second)
	config.Dial = func(context.Context, string, string) (net.Conn, error) {
		conn, peer := net.Pipe()

		t.Cleanup(func() { closeQuietly(peer) })

		return conn, nil
	}
	p := New(config)

	t.Cleanup(func() { closeQuietly(p) })

	return p, clock
}

func checkoutConn(t *testing.T, p *Pool) (*Conn, bool) {
	t.Helper()

	conn, reused, err := p.Get(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeQuietly(conn) })

	return conn, reused
}

func TestConnectionAgeConfigAndJitter(t *testing.T) {
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
	p, clock := pipeConnPool(t)
	conn, _ := checkoutConn(t, p)
	p.Recycle(conn)

	old := clock.latest()
	clock.advance(time.Second)

	next, reused := checkoutConn(t, p)
	if next != conn || !reused {
		t.Fatal("healthy connection not reused")
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
}

func TestConnectionAgeOldTimerAfterRecycling(t *testing.T) {
	p, clock := pipeConnPool(t)
	conn, _ := checkoutConn(t, p)
	p.Recycle(conn)

	old := clock.latest()
	clock.advance(time.Second)

	conn, _ = checkoutConn(t, p)
	p.Recycle(conn)
	old.fire()

	if s := p.Stats(); s.Connections != 1 || s.IdleConnections != 1 || s.ConnectionRotations != 0 {
		t.Fatal("old timer removed a newer idle generation", s)
	}

	if clock.latest().delay != 3*time.Second || clock.ages.Load() != 1 {
		t.Fatal("reuse renewed lifetime")
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
			body := NewBody(conn)
			body.SetReusable(true)

			switch mode {
			case "partial":
				body.SetReusable(false)
			case "close", "failed":
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

	p := New(Config{MaxAge: 20 * time.Millisecond, IdleTimeout: 90 * time.Second, Jitter: func(n int64) int64 { return n - 1 }, Dial: func(context.Context, string, string) (net.Conn, error) { return conn, nil }})
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
	body := NewBody(conn)
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
	if !errors.Is(err, ErrClosed) || p.Stats().Dials != 2 {
		t.Fatal(err, p.Stats())
	}
}

func TestDialFailureAndCancellation(t *testing.T) {
	for _, mode := range []string{"failure", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			config := Config{MaxAge: time.Minute, IdleTimeout: time.Minute}
			clock := installConnClock(&config, time.Minute)

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

			p := New(config)
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
	for _, err := range []error{io.EOF, syscall.ECONNRESET, syscall.EPIPE} {
		if !StaleError(fmt.Errorf("wrapped: %w", err)) {
			t.Fatal(err)
		}
	}

	for _, err := range []error{nil, context.DeadlineExceeded, context.Canceled, io.ErrUnexpectedEOF, net.ErrClosed, io.ErrClosedPipe} {
		if StaleError(err) {
			t.Fatal(err)
		}
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

		p := New(Config{MaxAge: time.Minute, IdleTimeout: time.Minute, Dial: func(context.Context, string, string) (net.Conn, error) {
			close(entered)
			<-release

			return conn, nil
		}})
		defer closeQuietly(p)

		done := make(chan *Conn, 1)

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
			config := Config{IdleTimeout: time.Minute, Dial: func(context.Context, string, string) (net.Conn, error) { return wrapped, nil }}
			clock := installConnClock(&config, time.Second)

			p := New(config)
			defer closeQuietly(p)

			lease, _ := checkoutConn(t, p)
			body := NewBody(lease)
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
	p := New(Config{Path: "unused", DialTimeout: time.Nanosecond, MaxAge: time.Minute, IdleTimeout: time.Minute})
	defer closeQuietly(p)

	if p.Stats().Dials != 0 || p.Config().DialTimeout != time.Nanosecond {
		t.Fatal(p.Stats(), p.Config())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, reused, err := p.Get(ctx, false)
	if err == nil || reused || p.Stats().Dials != 1 || p.Stats().Connections != 0 {
		t.Fatal(err, p.Stats())
	}
}
