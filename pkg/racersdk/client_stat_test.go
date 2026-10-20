// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

type statDeadlineGate struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
}

func (c *statDeadlineGate) SetDeadline(deadline time.Time) error {
	if deadline.After(time.Now()) {
		close(c.entered)
		<-c.release
	}

	return c.Conn.SetDeadline(deadline)
}

func TestStatCancellationDuringDeadlineSetup(t *testing.T) {
	for _, clientClose := range []bool{false, true} {
		name := "context"
		if clientClose {
			name = "client close"
		}

		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				local, peer := net.Pipe()
				conn := &statDeadlineGate{Conn: local, entered: make(chan struct{}), release: make(chan struct{})}
				release := sync.OnceFunc(func() { close(conn.release) })

				defer func() {
					release()
					closeQuietly(local)
					closeQuietly(peer)
					synctest.Wait()
				}()

				c := testClient(t, "unused", 1)
				c.idle = []*clientConn{{Conn: conn, r: bufio.NewReader(conn), expires: time.Now().Add(time.Minute), idleSince: time.Now()}}
				result := make(chan error, 1)

				go func() {
					_, err := c.Stat(ctx, Request{})
					result <- err
				}()

				synctest.Wait()

				select {
				case <-conn.entered:
				default:
					t.Fatal("Stat did not reach deadline setup")
				}

				want := context.Canceled

				if clientClose {
					if err := c.Close(); err != nil {
						t.Fatal(err)
					}

					want = net.ErrClosed
				} else {
					cancel()
				}

				// Finish any cancellation callback before allowing the normal
				// deadline to take effect. No sleep or scheduler race is needed.
				synctest.Wait()
				release()
				synctest.Wait()

				select {
				case err := <-result:
					assertIs(t, err, want)
				default:
					t.Fatal("Stat still blocked after cancellation during deadline setup")
				}

				if len(c.stat.slots) != 0 || len(c.stat.queue) != 0 {
					t.Fatal("Stat retained admission after cancellation")
				}

				if len(c.idle) != 0 {
					t.Fatal("Stat pooled a canceled connection")
				}
			})
		})
	}
}

func TestStatBurstLeavesIngressForGet(t *testing.T) {
	const burst = 2 * statIdleConnections

	path := filepath.Join(socketDir(t), "socket")

	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}

	// Like Racer, admit at most burst open connections and hold a slot until
	// the client closes its connection, even while it is idle.
	slots := make(chan struct{}, burst)
	stop := make(chan struct{})
	all := make(chan struct{})

	var (
		heads atomic.Int32
		wg    sync.WaitGroup
	)

	// Hold every Stat response until the whole burst is in flight, so each
	// Stat uses its own connection. Release on stop too, so cleanup does not
	// hang when a regression keeps part of the burst from arriving.
	arrive := func() bool {
		if heads.Add(1) == burst {
			close(all)
		}

		select {
		case <-all:
			return true
		case <-stop:
			return false
		}
	}

	serve := func(conn net.Conn) {
		r := bufio.NewReader(conn)

		for {
			head, err := wire.ReadRawHead(r, false)
			if err != nil {
				return
			}

			if !strings.HasPrefix(string(head), "HEAD ") {
				_, _ = io.WriteString(conn, subscriptionHead(100, 0, 100))
				return
			}

			if !arrive() {
				return
			}

			if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 7\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\n\r\n"); err != nil {
				return
			}
		}
	}

	wg.Go(func() {
		for {
			select {
			case slots <- struct{}{}:
			case <-stop:
				return
			}

			conn, err := l.Accept()
			if err != nil {
				<-slots
				return
			}

			wg.Go(func() {
				defer func() { <-slots }()
				defer closeQuietly(conn)

				serve(conn)
			})
		}
	})

	t.Cleanup(func() {
		close(stop)
		closeQuietly(l)
		wg.Wait()
	})

	// Registered after the server cleanup, so the client closes first and
	// releases the server's connections.
	c := testClient(t, path, burst)

	var stats sync.WaitGroup

	for range burst {
		stats.Go(func() {
			if _, err := c.Stat(t.Context(), Request{}); err != nil {
				t.Error(err)
			}
		})
	}

	stats.Wait()

	if t.Failed() {
		t.FailNow()
	}

	c.mu.Lock()
	idle := len(c.idle)
	c.mu.Unlock()

	if idle != statIdleConnections {
		t.Fatalf("idle connections = %d, want %d", idle, statIdleConnections)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	o, err := c.Get(ctx, Request{})
	if err != nil {
		t.Fatalf("Get after a Stat burst: %v", err)
	}

	closeQuietly(o)
}

func TestStatLaneScalesWithMaxConnections(t *testing.T) {
	for _, tt := range []struct {
		name           string
		maxConnections int
		want           int
		wantIdle       int
	}{
		{"default", 0, defaultMaxConnections, statIdleConnections},
		{"one", 1, 1, 1},
		{"wide", 128, 128, statIdleConnections},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(t, "unused", tt.maxConnections)

			if got := cap(c.stat.slots); got != tt.want {
				t.Fatalf("stat slots = %d, want %d", got, tt.want)
			}

			if got := cap(c.stat.queue); got != statQueue {
				t.Fatalf("stat queue = %d, want %d", got, statQueue)
			}

			peers := make([]net.Conn, 0, tt.wantIdle+1)

			t.Cleanup(func() {
				for _, peer := range peers {
					closeQuietly(peer)
				}
			})

			for range tt.wantIdle + 1 {
				local, peer := net.Pipe()
				peers = append(peers, peer)

				c.recycle(&clientConn{Conn: local, r: bufio.NewReader(local), expires: time.Now().Add(time.Minute)})
			}

			if got := len(c.idle); got != tt.wantIdle {
				t.Fatalf("idle connections = %d, want %d", got, tt.wantIdle)
			}

			// The connection beyond the idle cap is closed rather than pooled.
			if _, err := peers[tt.wantIdle].Read(make([]byte, 1)); err == nil {
				t.Fatal("connection beyond the idle cap was not closed")
			}
		})
	}
}
