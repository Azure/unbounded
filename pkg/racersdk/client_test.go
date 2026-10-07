// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// writerOnly hides io.ReaderFrom so WriteTo takes its buffered path.
type writerOnly struct{ io.Writer }

// readerOnly hides io.WriterTo so io.Copy takes the Read path.
type readerOnly struct{ io.Reader }

func TestNewClientValidation(t *testing.T) {
	for _, config := range []ClientConfig{
		{},
		{Cache: "Upper"},
		{Cache: "-bad"},
		{Cache: "bad-"},
		{Cache: "a..b"},
		{Cache: "under_score"},
		{Cache: strings.Repeat("a", 64)},
		{Cache: strings.Repeat("a.", 60) + "a"},
		{Cache: "ok", MaxConnections: -1},
	} {
		if _, err := NewClient(config); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("NewClient(%+v) = %v; want ErrInvalidRequest", config, err)
		}
	}

	c, err := NewClient(ClientConfig{Cache: "cache.example-1"})
	if err != nil {
		t.Fatal(err)
	}

	if c.path != "/run/racer/cache.example-1/client/socket" || cap(c.bulk.slots) != defaultMaxConnections {
		t.Fatalf("client path %q, %d connections", c.path, cap(c.bulk.slots))
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGetValidation(t *testing.T) {
	c := testClient(t, filepath.Join(socketDir(t), "missing"), 1)
	ctx := t.Context()

	for name, call := range map[string]func() error{
		"two options": func() error {
			_, err := c.Get(ctx, Request{}, ReadOptions{}, ReadOptions{})
			return err
		},
		"negative offset": func() error {
			_, err := c.Get(ctx, Request{}, ReadOptions{Offset: -1})
			return err
		},
		"negative length": func() error {
			_, err := c.Get(ctx, Request{}, ReadOptions{Length: -1})
			return err
		},
		"overflowing range": func() error {
			_, err := c.Get(ctx, Request{}, ReadOptions{Offset: 2, Length: 1<<63 - 2})
			return err
		},
		"weak etag": func() error {
			_, err := c.Get(ctx, Request{}, ReadOptions{ETag: `W/"v"`})
			return err
		},
		"control metadata": func() error {
			_, err := c.Get(ctx, Request{Metadata: "a\nb"})
			return err
		},
		"control authorization": func() error {
			_, err := c.Stat(ctx, Request{Authorization: "a\x00b"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { assertIs(t, call(), ErrInvalidRequest) })
	}

	// Validation happens before connecting, so Racer being absent is
	// reported only for valid requests.
	_, err := c.Get(ctx, Request{})
	assertIs(t, err, ErrUnavailable)

	_, err = c.Stat(ctx, Request{})
	assertIs(t, err, ErrUnavailable)
}

func TestGetRanges(t *testing.T) {
	const size = PageSize + 1000

	c := fakeClient(t, offsetOrigin(size))

	for _, tc := range []struct {
		name           string
		offset, length int64
	}{
		{"whole", 0, 0},
		{"tail", 10, 0},
		{"within first page", 7, 100},
		{"across pages", PageSize - 3, 10},
		{"second page", PageSize, 0},
		{"last byte", size - 1, 1},
		{"exact length", 0, size},
	} {
		want := tc.length
		if want == 0 {
			want = size - tc.offset
		}

		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"read", "write"} {
				o, err := c.Get(t.Context(), Request{}, ReadOptions{Offset: tc.offset, Length: tc.length})
				if err != nil {
					t.Fatal(err)
				}

				if m := o.Metadata(); m.Size != size || m.ETag != `"v"` {
					t.Fatalf("metadata %+v", m)
				}

				sink := &offsetSink{offset: tc.offset}

				var n int64
				if mode == "read" {
					n, err = io.CopyBuffer(writerOnly{sink}, readerOnly{o}, make([]byte, 64<<10))
				} else {
					n, err = o.WriteTo(writerOnly{sink})
				}

				if err != nil || n != want {
					t.Fatalf("%s: n=%d err=%v; want %d bytes", mode, n, err, want)
				}

				if err := o.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestGetEmptyObject(t *testing.T) {
	c := fakeClient(t, offsetOrigin(0))

	o, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(o)

	if n, err := o.Read(make([]byte, 8)); n != 0 || err != io.EOF {
		t.Fatalf("Read = %d, %v; want EOF", n, err)
	}

	if n, err := o.WriteTo(io.Discard); n != 0 || err != nil {
		t.Fatalf("WriteTo after EOF = %d, %v", n, err)
	}
}

func TestGetSelectionErrors(t *testing.T) {
	versioned := func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
		m := originMeta(10)
		if r.ETag != "" && r.ETag != m.ETag {
			return m, nil, ErrVersionMismatch
		}

		return offsetOrigin(10)(context.Background(), r)
	}
	c := fakeClient(t, versioned)
	plain := fakeClient(t, offsetOrigin(10))
	big := fakeClient(t, offsetOrigin(PageSize+1))

	for name, tc := range map[string]struct {
		c    *Client
		o    ReadOptions
		want error
	}{
		"other version":        {c, ReadOptions{ETag: `"other"`}, ErrVersionMismatch},
		"ignored pin":          {plain, ReadOptions{ETag: `"other"`}, ErrVersionMismatch},
		"ignored later pin":    {plain, ReadOptions{ETag: `"other"`, Offset: 5}, ErrVersionMismatch},
		"offset past end":      {c, ReadOptions{Offset: 11}, ErrRangeNotSatisfiable},
		"length past end":      {c, ReadOptions{Offset: 5, Length: 6}, ErrRangeNotSatisfiable},
		"large small object":   {big, ReadOptions{SmallObject: true}, ErrInvalidRequest},
		"matching version":     {c, ReadOptions{ETag: `"v"`}, nil},
		"offset at end":        {c, ReadOptions{Offset: 10}, ErrRangeNotSatisfiable},
		"small object in page": {c, ReadOptions{SmallObject: true}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			o, err := tc.c.Get(t.Context(), Request{}, tc.o)
			if tc.want != nil {
				assertIs(t, err, tc.want)
				return
			}

			if err != nil {
				t.Fatal(err)
			}

			defer closeQuietly(o)

			if _, err := io.Copy(io.Discard, o); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStat(t *testing.T) {
	var calls atomic.Int32

	c := fakeClient(t, func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
		calls.Add(1)

		if !r.Head || r.Key != (Key{1}) || r.Metadata != "meta" || r.Authorization != "auth" {
			t.Errorf("origin request %+v", r)
		}

		m := originMeta(42)
		m.ContentType = "text/plain"

		return m, nil, nil
	})

	for range 3 {
		m, err := c.Stat(t.Context(), Request{Key: Key{1}, Metadata: "meta", Authorization: "auth"})
		if err != nil {
			t.Fatal(err)
		}

		if m.Size != 42 || m.ETag != `"v"` || m.ContentType != "text/plain" || !m.ExpiresAt.Equal(time.UnixMilli(0)) {
			t.Fatalf("metadata %+v", m)
		}
	}

	c.mu.Lock()
	idle := len(c.idle)
	c.mu.Unlock()

	if idle != 1 {
		t.Fatalf("%d idle connections; want the one connection reused", idle)
	}

	if calls.Load() == 0 {
		t.Fatal("origin not called")
	}
}

func TestStatRetriesStaleConnection(t *testing.T) {
	c := rawServer(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 7\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\n\r\n")
		// Returning closes the connection the client just pooled.
	})

	for range 3 {
		m, err := c.Stat(t.Context(), Request{})
		if err != nil {
			t.Fatal(err)
		}

		if m.Size != 7 {
			t.Fatalf("size %d", m.Size)
		}
	}
}

func TestErrorStatuses(t *testing.T) {
	for status, want := range map[int]error{
		400: ErrInvalidRequest,
		401: ErrUnauthorized,
		403: ErrForbidden,
		404: ErrNotFound,
		412: ErrVersionMismatch,
		416: ErrRangeNotSatisfiable,
		503: ErrUnavailable,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			fields := "Content-Length: 0\r\nConnection: close\r\n"
			if status == 416 {
				fields += "Content-Range: bytes */10\r\n"
			}

			c := rawServer(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
				_, _ = conn.Write(rawResponse(status, fields))
			})

			_, err := c.Get(t.Context(), Request{})
			assertIs(t, err, want)

			_, err = c.Stat(t.Context(), Request{})
			assertIs(t, err, want)
		})
	}

	c := rawServer(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		_, _ = conn.Write(rawResponse(502, "Content-Length: 0\r\nConnection: close\r\n"))
	})

	_, err := c.Get(t.Context(), Request{})
	assertNoSentinel(t, err)
}

// assertNoSentinel checks an error a caller would report as a bad gateway.
func assertNoSentinel(t *testing.T, err error) {
	t.Helper()

	if err == nil {
		t.Fatal("no error")
	}

	for _, target := range []error{ErrInvalidRequest, ErrUnauthorized, ErrForbidden, ErrNotFound, ErrVersionMismatch, ErrRangeNotSatisfiable, ErrUnavailable, context.Canceled, net.ErrClosed} {
		if errors.Is(err, target) {
			t.Fatalf("error %v matches %v", err, target)
		}
	}
}

// serveOnePage answers a subscription for a 100-byte object with one page,
// then calls finish to write the rest.
func serveOnePage(t *testing.T, finish func(conn net.Conn, r *bufio.Reader)) *Client {
	t.Helper()

	return rawServer(t, func(conn net.Conn, r *bufio.Reader, _ []byte) {
		if _, err := io.WriteString(conn, subscriptionHead(100, 0, 100)); err != nil {
			return
		}

		if err := writeFrame(conn, wire.PageFrame, 0, 0, 100); err != nil {
			return
		}

		finish(conn, r)
	})
}

func TestObjectCompletion(t *testing.T) {
	payload := strings.Repeat("x", 100)

	t.Run("credit before completion", func(t *testing.T) {
		c := serveOnePage(t, func(conn net.Conn, r *bufio.Reader) {
			_, _ = io.WriteString(conn, payload)
			// Racer may hold completion until the last credit arrives.
			if err := readCredit(r, 0, 100); err != nil {
				t.Error(err)
				return
			}

			_ = writeFrame(conn, wire.CompleteFrame, 1, 100, 0)
		})

		for _, mode := range []string{"read", "write"} {
			o, err := c.Get(t.Context(), Request{})
			if err != nil {
				t.Fatal(err)
			}

			var b strings.Builder
			if mode == "read" {
				_, err = io.Copy(writerOnly{&b}, readerOnly{o})
			} else {
				_, err = o.WriteTo(&b)
			}

			if err != nil || b.String() != payload {
				t.Fatalf("%s: %d bytes, %v", mode, b.Len(), err)
			}

			closeQuietly(o)
		}
	})

	for name, tc := range map[string]struct {
		finish func(conn net.Conn)
		want   error
	}{
		"truncated payload":  {func(conn net.Conn) { _, _ = io.WriteString(conn, payload[:50]) }, io.ErrUnexpectedEOF},
		"missing completion": {func(conn net.Conn) { _, _ = io.WriteString(conn, payload) }, io.ErrUnexpectedEOF},
		"wrong completion": {func(conn net.Conn) {
			_, _ = io.WriteString(conn, payload)
			_ = writeFrame(conn, wire.CompleteFrame, 1, 99, 0)
		}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			c := serveOnePage(t, func(conn net.Conn, _ *bufio.Reader) { tc.finish(conn) })

			for _, mode := range []string{"read", "write"} {
				o, err := c.Get(t.Context(), Request{})
				if err != nil {
					t.Fatal(err)
				}

				var b strings.Builder
				if mode == "read" {
					_, err = io.Copy(writerOnly{&b}, readerOnly{o})
				} else {
					_, err = o.WriteTo(&b)
				}

				// The final byte is never released without completion.
				if b.Len() >= 100 {
					t.Fatalf("%s delivered %d bytes without completion", mode, b.Len())
				}

				if tc.want != nil {
					assertIs(t, err, tc.want)
					assertIs(t, err, ErrUnavailable)
				} else {
					assertNoSentinel(t, err)
				}

				closeQuietly(o)
			}
		})
	}
}

func TestObjectMalformedFrame(t *testing.T) {
	c := rawServer(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, subscriptionHead(100, 0, 100))
		_ = writeFrame(conn, wire.PageFrame, 0, 0, 0)
	})

	o, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(o)

	_, err = io.ReadAll(o)
	assertNoSentinel(t, err)
}

func TestObjectReadEdges(t *testing.T) {
	c := fakeClient(t, offsetOrigin(3))

	o, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	if n, err := o.Read(nil); n != 0 || err != nil {
		t.Fatalf("empty Read = %d, %v", n, err)
	}

	p := make([]byte, 1)
	for want := range 2 {
		if n, err := o.Read(p); n != 1 || err != nil || p[0] != byte(want) {
			t.Fatalf("Read = %d, %v, %d", n, err, p[0])
		}
	}

	// The last byte arrives with io.EOF once completion is validated.
	if n, err := o.Read(p); n != 1 || err != io.EOF || p[0] != 2 {
		t.Fatalf("last Read = %d, %v, %d", n, err, p[0])
	}

	if n, err := o.Read(p); n != 0 || err != io.EOF {
		t.Fatalf("Read after EOF = %d, %v", n, err)
	}

	if err := o.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := o.Read(p); err != io.EOF {
		t.Fatalf("Read after EOF and Close = %v", err)
	}

	o, err = c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	_ = o.Close()

	_, err = o.Read(p)
	assertIs(t, err, net.ErrClosed)

	_, err = o.WriteTo(io.Discard)
	assertIs(t, err, net.ErrClosed)
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestObjectWriteToErrors(t *testing.T) {
	c := fakeClient(t, offsetOrigin(1000))

	o, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(o)

	_, err = o.WriteTo(shortWriter{})
	assertIs(t, err, io.ErrShortWrite)

	o, err = c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(o)

	_, err = o.WriteTo(nil)
	assertIs(t, err, ErrInvalidRequest)
}

// blockedObject returns an object whose server sends a page header and then
// stalls until the test ends.
func blockedObject(t *testing.T, ctx context.Context) (*Client, *Object) {
	t.Helper()

	stall := make(chan struct{})

	t.Cleanup(func() { close(stall) })

	c := serveOnePage(t, func(net.Conn, *bufio.Reader) { <-stall })

	o, err := c.Get(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeQuietly(o) })

	return c, o
}

func TestObjectInterrupts(t *testing.T) {
	for name, tc := range map[string]struct {
		interrupt func(context.CancelFunc, *Client, *Object)
		want      error
	}{
		"object close": {func(_ context.CancelFunc, _ *Client, o *Object) { _ = o.Close() }, net.ErrClosed},
		"client close": {func(_ context.CancelFunc, c *Client, _ *Object) { _ = c.Close() }, net.ErrClosed},
		"context":      {func(cancel context.CancelFunc, _ *Client, _ *Object) { cancel() }, context.Canceled},
	} {
		for _, mode := range []string{"read", "write"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				c, o := blockedObject(t, ctx)
				errs := make(chan error, 1)

				go func() {
					var err error
					if mode == "read" {
						_, err = o.Read(make([]byte, 10))
					} else {
						_, err = o.WriteTo(io.Discard)
					}

					errs <- err
				}()

				time.Sleep(20 * time.Millisecond)
				tc.interrupt(cancel, c, o)

				select {
				case err := <-errs:
					assertIs(t, err, tc.want)
				case <-time.After(5 * time.Second):
					t.Fatal("read not interrupted")
				}
			})
		}
	}
}

func TestAdmission(t *testing.T) {
	stall := make(chan struct{})

	t.Cleanup(func() { close(stall) })

	c := serveOnePage(t, func(net.Conn, *bufio.Reader) { <-stall })
	c.limits.queueTimeout = 20 * time.Millisecond

	o, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	// The client allows one connection, so a second Get waits and times out.
	_, err = c.Get(t.Context(), Request{})
	assertIs(t, err, ErrUnavailable)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = c.Get(ctx, Request{})
	assertIs(t, err, context.Canceled)

	// Closing the object frees its slot.
	_ = o.Close()

	o, err = c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	_ = o.Close()

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = c.Get(t.Context(), Request{})
	assertIs(t, err, net.ErrClosed)

	_, err = c.Stat(t.Context(), Request{})
	assertIs(t, err, net.ErrClosed)
}

// TestWriteToDestinations covers the splice and HTTP paths of WriteTo.
func TestWriteToDestinations(t *testing.T) {
	const size = PageSize + 4096

	c := fakeClient(t, offsetOrigin(size))

	verify := func(t *testing.T, r io.Reader) {
		t.Helper()

		sink := &offsetSink{}
		if n, err := io.Copy(sink, r); err != nil || n != size {
			t.Fatalf("copied %d bytes, %v", n, err)
		}
	}

	get := func(t *testing.T) *Object {
		t.Helper()

		o, err := c.Get(t.Context(), Request{})
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { closeQuietly(o) })

		return o
	}

	t.Run("file", func(t *testing.T) {
		f, err := os.Create(filepath.Join(t.TempDir(), "object"))
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuietly(f)

		if n, err := get(t).WriteTo(f); err != nil || n != size {
			t.Fatalf("WriteTo = %d, %v", n, err)
		}

		if _, err := f.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}

		verify(t, f)
	})

	t.Run("tcp", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuietly(l)

		received := make(chan error, 1)

		go func() {
			conn, err := l.Accept()
			if err != nil {
				received <- err
				return
			}
			defer closeQuietly(conn)

			sink := &offsetSink{}

			n, err := io.Copy(sink, conn)
			if err == nil && n != size {
				err = fmt.Errorf("received %d bytes", n)
			}

			received <- err
		}()

		conn, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}

		if n, err := get(t).WriteTo(conn); err != nil || n != size {
			t.Fatalf("WriteTo = %d, %v", n, err)
		}

		closeQuietly(conn)

		if err := <-received; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("http", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			o, err := c.Get(r.Context(), Request{})
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer closeQuietly(o)

			w.Header().Set("Content-Length", fmt.Sprint(o.Metadata().Size))

			if _, err := o.WriteTo(w); err != nil {
				panic(http.ErrAbortHandler)
			}
		}))
		defer server.Close()

		response, err := http.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer closeQuietly(response.Body)

		verify(t, response.Body)
	})
}

// BenchmarkGet measures the SDK alone against a raw peer that streams
// prebuilt pages as fast as the socket allows.
func BenchmarkGet(b *testing.B) {
	const size = 4 * PageSize

	page := make([]byte, PageSize)

	c := rawServer(b, func(conn net.Conn, r *bufio.Reader, _ []byte) {
		go func() { _, _ = io.Copy(io.Discard, r) }()

		if _, err := io.WriteString(conn, subscriptionHead(size, 0, size)); err != nil {
			return
		}

		for number := range uint64(size / PageSize) {
			if writeFrame(conn, wire.PageFrame, number, number*PageSize, PageSize) != nil {
				return
			}

			if _, err := conn.Write(page); err != nil {
				return
			}
		}

		_ = writeFrame(conn, wire.CompleteFrame, size/PageSize, size, 0)
		_, _ = io.Copy(io.Discard, r)
	})

	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		b.Fatal(err)
	}

	b.Cleanup(func() { closeQuietly(null) })

	buffer := make([]byte, 256<<10)

	for name, consume := range map[string]func(*Object) error{
		"Read": func(o *Object) error {
			_, err := io.CopyBuffer(writerOnly{io.Discard}, readerOnly{o}, buffer)
			return err
		},
		"WriteTo/buffered": func(o *Object) error {
			_, err := o.WriteTo(writerOnly{io.Discard})
			return err
		},
		"WriteTo/splice": func(o *Object) error {
			_, err := o.WriteTo(null)
			return err
		},
	} {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(size)
			b.ReportAllocs()

			for b.Loop() {
				o, err := c.Get(b.Context(), Request{})
				if err != nil {
					b.Fatal(err)
				}

				if err := consume(o); err != nil {
					b.Fatal(err)
				}

				closeQuietly(o)
			}
		})
	}
}
