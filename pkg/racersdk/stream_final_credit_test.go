// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStreamingBufferedCompleteWithoutFinalCredit(t *testing.T) {
	started, closed, peerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	writeResult := make(chan error, 1)
	c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		defer close(peerDone)

		var all bytes.Buffer
		all.WriteString(subscriptionHead(3, 0, 3))
		_ = fakeSubscriptionFrame(&all, 1, 0, 0, 3)
		all.WriteString("abc")
		_ = fakeSubscriptionFrame(&all, 2, 1, 3, 0)
		_, _ = conn.Write(all.Bytes())

		<-closed
	})
	c.config.BodyReadTimeout = time.Minute
	poolConfig := c.bulk.config
	dial := poolConfig.Dial
	poolConfig.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}

		return &finalCreditBlockedConn{Conn: conn, started: started, closed: closed, writeResult: writeResult}, nil
	}
	c.configurePools(poolConfig)

	v, err := c.GetStreaming(t.Context(), Request{}, ReadOptions{PageCredits: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	if v.stream.conn.Reader.Buffered() != 45 {
		t.Fatal("fixture did not read ahead")
	}

	w := httptest.NewRecorder()
	done := make(chan struct{})

	var n int64

	go func() {
		defer close(done)

		n, err = v.WriteToHTTP(w)
	}()

	defer func() {
		closeBody(v)
		orderedWait(t, done)
		orderedWait(t, peerDone)
	}()

	orderedWait(t, done)
	orderedWait(t, peerDone)

	if err != nil || n != 3 || w.Body.String() != "abc" {
		t.Fatal("buffered Complete waited for final credit", n, w.Body.String(), err)
	}

	if c.Stats().BytesRead != 3 || c.Stats().ActiveBulk != 0 || len(c.copySlots) != 0 || v.stream.bytesHeld != 0 || len(v.stream.outstanding) != 0 {
		t.Fatal("retained admission or final credit accounting", c.Stats())
	}
}

func TestStreamingFinalCreditOrderings(t *testing.T) {
	for _, ordering := range []string{"complete first", "credit first"} {
		for _, mode := range []string{"success", "malformed", "missing", "canceled"} {
			t.Run(ordering+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				started, closed, peerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})

				ready, resume := make(chan struct{}), make(chan struct{})
				defer close(resume)

				writeResult := make(chan error, 1)
				c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, head []byte) {
					defer close(peerDone)

					if headHeaders(head).Get("Racer-Page-Credits") != "1" {
						t.Error("not a one-credit subscription")
					}

					_, _ = io.WriteString(conn, subscriptionHead(3, 0, 3))
					_ = fakeSubscriptionFrame(conn, 1, 0, 0, 3)
					_, _ = io.WriteString(conn, "abc")

					select {
					case <-started:
					case <-closed:
						return
					}

					if ordering == "credit first" && !orderedRelease(t, reader, 0, 3) {
						return
					}

					close(ready)

					select {
					case <-resume:
					case <-closed:
						return
					}

					switch mode {
					case "canceled":
						cancel()
					case "missing":
						return
					default:
						length := uint64(3)
						if mode == "malformed" {
							length++
						}

						_ = fakeSubscriptionFrame(conn, 2, 1, length, 0)
					}

					<-closed
				})
				poolConfig := c.bulk.config
				dial := poolConfig.Dial
				poolConfig.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
					conn, err := dial(ctx, network, address)
					if err != nil {
						return nil, err
					}

					return &finalCreditBlockedConn{Conn: conn, started: started, closed: closed, writeResult: writeResult}, nil
				}
				c.configurePools(poolConfig)

				v, err := c.GetStreaming(ctx, Request{}, ReadOptions{PageCredits: 1})
				if err != nil {
					t.Fatal(err)
				}

				w := httptest.NewRecorder()
				done := make(chan struct{})

				var n int64

				go func() {
					defer close(done)

					n, err = v.WriteToHTTP(w)
				}()

				defer func() {
					closeBody(v)
					orderedWait(t, done)
					orderedWait(t, peerDone)
				}()

				orderedWait(t, ready)

				if w.Body.String() != "ab" {
					t.Fatal("final byte exposed before Complete", w.Body.String())
				}

				select {
				case <-done:
					t.Fatal("transfer ended before Complete")
				default:
				}

				resume <- struct{}{}

				orderedWait(t, done)
				orderedWait(t, peerDone)

				select {
				case writeErr := <-writeResult:
					if ordering == "complete first" {
						if !errors.Is(writeErr, io.ErrClosedPipe) && !errors.Is(writeErr, net.ErrClosed) {
							t.Fatal("final credit was not interrupted by closure", writeErr)
						}
					} else if writeErr != nil {
						t.Fatal("final credit failed", writeErr)
					}
				default:
					t.Fatal("final credit writer did not finish before transfer returned")
				}

				if mode == "success" {
					if err != nil || n != 3 || w.Body.String() != "abc" {
						t.Fatal("Complete overridden by final credit", n, w.Body.String(), err)
					}
				} else {
					if n != 2 || w.Body.String() != "ab" {
						t.Fatal("invalid final byte exposed", n, w.Body.String())
					}

					switch mode {
					case "malformed":
						assertKind(t, err, ErrorProtocol)
					case "missing":
						if !errors.Is(err, io.ErrUnexpectedEOF) {
							t.Fatal(err)
						}
					case "canceled":
						if !errors.Is(err, context.Canceled) {
							t.Fatal(err)
						}
					}
				}

				if c.Stats().ActiveBulk != 0 || len(c.copySlots) != 0 || v.stream.bytesHeld != 0 || len(v.stream.outstanding) != 0 {
					t.Fatal("retained admission or final credit accounting", c.Stats())
				}
			})
		}
	}
}
