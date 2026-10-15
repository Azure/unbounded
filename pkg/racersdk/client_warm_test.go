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
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/fakeracer"
	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

func TestWarmRequestAndResult(t *testing.T) {
	for _, tt := range []struct {
		ttl     time.Duration
		seconds string
	}{
		{time.Nanosecond, "1"},
		{time.Second, "1"},
		{time.Second + time.Nanosecond, "2"},
		{time.Duration(math.MaxInt64/int64(time.Second)) * time.Second, "9223372036"},
	} {
		t.Run(tt.ttl.String(), func(t *testing.T) {
			c := rawServer(t, func(conn net.Conn, _ *bufio.Reader, head []byte) {
				h := wire.HeadHeaders(head)
				if !strings.HasPrefix(string(head), "HEAD /v2/objects/") || h.Get("Cache-Control") != "max-age="+tt.seconds || h.Get("If-Match") != `"v"` || h.Get("Authorization") != "secret" || h.Get("Racer-Metadata") != "opaque" {
					t.Error("wrong warm request")
				}

				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nETag: \"v\"\r\nRacer-Expires-At: 4567\r\nRacer-Retain-Until: 1234\r\nConnection: close\r\n\r\n")
			})

			got, err := c.Warm(context.Background(), Request{Metadata: "opaque", Authorization: "secret"}, tt.ttl, WarmOptions{ETag: `"v"`})
			if err != nil || got.Metadata.Size != 3 || got.Metadata.ETag != `"v"` || got.Metadata.ExpiresAt.UnixMilli() != 4567 || got.RetainUntil.UnixMilli() != 1234 {
				t.Fatalf("Warm: %+v, %v", got, err)
			}

			if len(c.stat.slots) != 0 {
				t.Fatal("warm retained admission")
			}
		})
	}
}

func TestWarmInvalidArguments(t *testing.T) {
	c := testClient(t, "unused", 1)
	for _, ttl := range []time.Duration{-1, 0, math.MaxInt64} {
		_, err := c.Warm(context.Background(), Request{}, ttl, WarmOptions{})
		assertIs(t, err, ErrInvalidRequest)
	}

	for _, tag := range []string{"*", `W/"v"`, `"a", "b"`, "unquoted"} {
		_, err := c.Warm(context.Background(), Request{}, time.Second, WarmOptions{ETag: tag})
		assertIs(t, err, ErrInvalidRequest)
	}

	_, err := c.Warm(context.Background(), Request{Authorization: "bad\r\n"}, time.Second, WarmOptions{})
	assertIs(t, err, ErrInvalidRequest)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = c.Warm(ctx, Request{}, time.Second, WarmOptions{})
	assertIs(t, err, context.Canceled)
	closeQuietly(c)
	_, err = c.Warm(context.Background(), Request{}, time.Second, WarmOptions{})
	assertIs(t, err, net.ErrClosed)
}

func TestWarmFailures(t *testing.T) {
	for _, tt := range []struct {
		status int
		fields string
		want   error
	}{
		{401, "", ErrUnauthorized},
		{403, "", ErrForbidden},
		{404, "", ErrNotFound},
		{412, "", ErrVersionMismatch},
		{503, "", ErrUnavailable},
		{200, "ETag: \"v\"\r\nRacer-Expires-At: 0\r\nRacer-Retain-Until: -1\r\n", nil},
		{200, "ETag: \"v\"\r\nRacer-Expires-At: 0\r\nRacer-Retain-Until: 1\r\nracer-retain-until: 1\r\n", nil},
	} {
		t.Run(strconv.Itoa(tt.status)+tt.fields, func(t *testing.T) {
			c := rawServer(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
				_, _ = io.WriteString(conn, "HTTP/1.1 "+strconv.Itoa(tt.status)+" Result\r\nContent-Length: 0\r\n"+tt.fields+"\r\n")
			})

			got, err := c.Warm(context.Background(), Request{}, time.Second, WarmOptions{})
			if err == nil || got != (WarmResult{}) || tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("Warm: %+v, %v", got, err)
			}
		})
	}
}

func TestWarmFakeSuccessRefusalAndError(t *testing.T) {
	origin := rawClientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" || r.Header.Get("Cache-Control") != "" || r.Header.Get("If-Match") != `"v"` {
			t.Error("unexpected origin request")
		}

		w.Header().Set("Content-Length", "3")
		w.Header().Set("ETag", `"v"`)
		w.Header().Set("Racer-Expires-At", "4567")
		w.Header().Set("Racer-Retain-Until", "9999")
	}))
	transport := unixTransport(origin)
	t.Cleanup(transport.CloseIdleConnections)

	for _, mode := range []string{"accept", "refuse", "error", "default"} {
		t.Run(mode, func(t *testing.T) {
			var policy fakeracer.WarmPolicy
			if mode != "default" {
				policy = func(_ context.Context, r wire.Request, m wire.Metadata) (time.Time, error) {
					if r.RetainSeconds != 2 || r.Pin != `"v"` || m.Size != 3 {
						t.Error("wrong warm policy input")
					}

					if mode == "error" {
						return time.Time{}, wire.StatusError(503)
					}

					if mode == "accept" {
						return time.UnixMilli(1234), nil
					}

					return time.Time{}, nil
				}
			}

			c := testClient(t, rawClientPeer(t, fakeracer.NewHandlerWithWarm(transport, policy)), 1)

			got, err := c.Warm(context.Background(), Request{}, 1500*time.Millisecond, WarmOptions{ETag: `"v"`})
			if mode == "error" {
				assertIs(t, err, ErrUnavailable)
				return
			}

			if err != nil || got.Metadata.Size != 3 || mode == "accept" && got.RetainUntil.UnixMilli() != 1234 || mode != "accept" && !got.RetainUntil.IsZero() {
				t.Fatalf("Warm: %+v, %v", got, err)
			}
		})
	}
}

func TestStatDoesNotWarm(t *testing.T) {
	c := rawServer(t, func(conn net.Conn, _ *bufio.Reader, head []byte) {
		if wire.HeadHeaders(head).Get("Cache-Control") != "" {
			t.Error("Stat emitted Cache-Control")
		}

		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\n\r\n")
	})
	if _, err := c.Stat(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}
}

func TestWarmCancellationReleasesAdmission(t *testing.T) {
	for _, clientClose := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			local, peer := net.Pipe()
			defer closeQuietly(local)
			defer closeQuietly(peer)

			c := testClient(t, "unused", 1)
			c.idle = []*clientConn{{Conn: local, r: bufio.NewReader(local), expires: time.Now().Add(time.Minute), idleSince: time.Now()}}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			result := make(chan error, 1)

			go func() {
				_, err := c.Warm(ctx, Request{}, time.Second, WarmOptions{})
				result <- err
			}()

			synctest.Wait()

			if len(c.stat.slots) != 1 {
				t.Fatal("warm did not enter HEAD lane")
			}

			want := context.Canceled

			if clientClose {
				closeQuietly(c)

				want = net.ErrClosed
			} else {
				cancel()
			}

			synctest.Wait()
			assertIs(t, <-result, want)

			if len(c.stat.slots) != 0 || len(c.stat.queue) != 0 || len(c.idle) != 0 {
				t.Fatal("canceled warm retained resources")
			}
		})
	}
}
