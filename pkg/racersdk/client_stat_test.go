// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"
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

func TestStatLaneScalesWithMaxConnections(t *testing.T) {
	for _, tt := range []struct {
		name           string
		maxConnections int
		want           int
	}{
		{"default", 0, defaultMaxConnections},
		{"one", 1, 1},
		{"wide", 128, 128},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(t, "unused", tt.maxConnections)

			if got := cap(c.stat.slots); got != tt.want {
				t.Fatalf("stat slots = %d, want %d", got, tt.want)
			}

			if got := cap(c.stat.queue); got != statQueue {
				t.Fatalf("stat queue = %d, want %d", got, statQueue)
			}

			peers := make([]net.Conn, 0, tt.want+1)

			t.Cleanup(func() {
				for _, peer := range peers {
					closeQuietly(peer)
				}
			})

			for range tt.want + 1 {
				local, peer := net.Pipe()
				peers = append(peers, peer)

				c.recycle(&clientConn{Conn: local, r: bufio.NewReader(local), expires: time.Now().Add(time.Minute)})
			}

			if got := len(c.idle); got != tt.want {
				t.Fatalf("idle connections = %d, want %d", got, tt.want)
			}

			// The connection beyond the idle cap is closed rather than pooled.
			if _, err := peers[tt.want].Read(make([]byte, 1)); err == nil {
				t.Fatal("connection beyond the idle cap was not closed")
			}
		})
	}
}
