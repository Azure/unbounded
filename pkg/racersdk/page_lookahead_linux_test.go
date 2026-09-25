// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestLookaheadSpliceAndPipeLifetime(t *testing.T) {
	for _, network := range []string{"tcp", "unix"} {
		t.Run(network, func(t *testing.T) {
			const size = PageSize + 137

			c := newTestClient(t, lookaheadOrigin(t, size, nil), ClientOptions{PageLookahead: true})

			s := lookaheadRange(t, c, "/object", 0, size)
			if err := s.Prepare(); err != nil {
				t.Fatal(err)
			}

			if len(c.streamPool.pipes.idle) != 0 {
				t.Fatal("Prepare acquired pipe")
			}

			dst, receiver := downstreamPair(t, network)
			done := make(chan error, 1)

			go func() {
				w := &patternWriter{}

				_, err := io.Copy(w, receiver)
				done <- err
			}()

			n, err := s.WriteTo(dst)
			_ = dst.Close()
			readErr := <-done

			stats := s.Stats()
			if err != nil || readErr != nil || n != size || stats.PageRequests != 2 || stats.SpliceBytes < PageSize-16384 || stats.SpliceCalls == 0 || stats.BufferedBytes+stats.SpliceBytes != size {
				t.Fatal(n, err, readErr, stats)
			}

			if len(c.streamPool.pipes.idle) != 1 || c.streamPool.pipes.idle[0].buffered != 0 || len(c.streamPool.speculative) != 0 {
				t.Fatal("invalid pipe or future lifetime")
			}

			fds := c.streamPool.pipes.idle[0].fd
			c.CloseIdleConnections()
			assertPipeClosed(t, fds)
		})
	}
}

func TestLookaheadSpliceInterrupted(t *testing.T) {
	for _, action := range []string{"close", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			seen := make(chan int64, 2)
			c := newTestClient(t, lookaheadOrigin(t, 2*PageSize, func(_ *http.Request, off int64) { seen <- off }), ClientOptions{PageLookahead: true})
			s := lookaheadRange(t, c, "/object", 0, 2*PageSize)
			dst, receiver := downstreamPair(t, "tcp")
			_ = dst.(*net.TCPConn).SetWriteBuffer(4096)
			_ = receiver.(*net.TCPConn).SetReadBuffer(4096)
			done := make(chan error, 1)

			go func() { _, err := s.WriteTo(dst); done <- err }()

			for range 2 {
				select {
				case <-seen:
				case <-time.After(time.Second):
					t.Fatal("lookahead did not start")
				}
			}

			if action == "close" {
				_ = s.Close()
			} else {
				_ = receiver.Close()
			}

			select {
			case err := <-done:
				if err == nil {
					t.Fatal("interrupted stream succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("splice or speculative worker leaked")
			}

			if s.future != nil || s.conn != nil || len(c.streamPool.speculative) != 0 || len(c.streamPool.pipes.idle) != 0 || len(c.streamPool.idle) != 0 {
				t.Fatal("interrupted resources pooled or retained")
			}
		})
	}
}
