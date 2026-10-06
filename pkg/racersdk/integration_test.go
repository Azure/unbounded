// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFakeClientPages(t *testing.T) {
	for _, size := range []ByteLength{0, 1, PageSize, PageSize + 13, 3*PageSize + 13} {
		t.Run(strconv.FormatUint(uint64(size), 10), func(t *testing.T) {
			var (
				calls atomic.Int32
				seen  sync.Map
			)

			request := Request{Key: Key{1, 2, 3}, Context: FetchContext{
				metadata:      AdapterMetadata{value: "opaque, interior  spaces\\\xff"},
				authorization: Authorization{value: "Scheme opaque,credential\x80"},
			}}

			client, cleanup, err := newFakeClient(t, func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
				call := calls.Add(1)

				if r.Key() != request.Key || r.Context() != request.Context {
					t.Error("key or context changed")
				}

				m := originMeta(size)

				page, ok := r.Range()
				if !ok || validatePageShape(page) != nil {
					t.Error("origin received a non-page range")
				}

				if call == 1 {
					if _, pinned := r.Pin(); pinned || r.Operation() != OperationBootstrap {
						t.Error("bootstrap changed")
					}
				} else {
					m.ExpiresAt = time.UnixMilli(int64(call))

					pin, ok := r.Pin()
					if !ok || pin != m.ETag || r.Operation() != OperationPinned {
						t.Error("continuation lost immutable pin")
					}
				}

				if size == 0 {
					return m, nil, nil
				}

				first, last, err := page.Resolve(size)
				if err != nil {
					return m, nil, err
				}

				if _, duplicate := seen.LoadOrStore(first, true); duplicate || uint64(first)%uint64(PageSize) != 0 {
					t.Error("duplicate or unaligned page scheduling")
				}

				return m, io.NopCloser(io.LimitReader(&offsetStream{offset: int64(first)}, int64(last-first)+1)), nil
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			value, err := client.Get(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(value)

			if calls.Load() != 1 {
				t.Fatal("Get eagerly fetched a continuation")
			}

			sink := &offsetSink{}

			n, err := io.Copy(sink, value)
			if err != nil || n != int64(size) || sink.offset != int64(size) {
				t.Fatal("stream mismatch", n, err)
			}

			m := value.Metadata()
			if calls.Load() != int32(max(1, (size+PageSize-1)/PageSize)) || m.Size != size || m.ETag != originMeta(size).ETag || m.ExpiresAt.UnixMilli() != 0 {
				t.Fatal("page count or metadata snapshot changed")
			}
		})
	}
}

func TestFakeSubscriptionFrames(t *testing.T) {
	for _, test := range []struct {
		name    string
		size    ByteLength
		headers string
		first   uint64
		end     uint64
	}{
		{name: "empty"},
		{name: "whole", size: 13, end: 13},
		{name: "open", size: PageSize + 13, headers: "Range: bytes=16777211-\r\n", first: uint64(PageSize) - 5, end: uint64(PageSize) + 13},
		{name: "closed", size: 2 * PageSize, headers: "Range: bytes=16777211-16777219\r\nRacer-Ordered: 0\r\n", first: uint64(PageSize) - 5, end: uint64(PageSize) + 4},
		{name: "clamped", size: 13, headers: "Range: bytes=3-20\r\n", first: 3, end: 13},
		{name: "pinned", size: 13, headers: "If-Match: " + originMeta(13).ETag.String() + "\r\nRacer-Ordered: 1\r\n", end: 13},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := streamFakeClient(t, fakeSubscriptionOrigin(t, test.size))

			_, res := fakeSubscriptionSocket(t, client, test.headers)
			if res.StatusCode != 200 || !res.Close || res.Header.Get("ETag") != originMeta(test.size).ETag.String() || res.Header.Get("Racer-Expires-At") != "0" || res.Header.Get("Racer-Content-Type") != "test/example" {
				t.Fatal("response", res.Status, res.Header)
			}

			for name, want := range map[string]uint64{"Racer-Object-Length": uint64(test.size), "Racer-Range-Start": test.first, "Racer-Range-End": test.end} {
				if res.Header.Get(name) != strconv.FormatUint(want, 10) {
					t.Fatal(name, res.Header.Get(name), want)
				}
			}

			pages := uint64(0)

			for offset := test.first; offset < test.end; {
				end := min((offset/uint64(PageSize)+1)*uint64(PageSize), test.end)
				fakeReadFrame(t, res.Body, 1, offset/uint64(PageSize), offset, uint32(end-offset))
				offset = end
				pages++
			}

			fakeReadFrame(t, res.Body, 2, pages, test.end-test.first, 0)

			if res.ContentLength != int64(test.end-test.first+21*(pages+1)) {
				t.Fatal("content length", res.ContentLength)
			}

			if _, err := res.Body.Read(make([]byte, 1)); err != io.EOF {
				t.Fatal("missing EOF", err)
			}
		})
	}
}

func TestFakeSubscriptionCredits(t *testing.T) {
	for _, test := range []struct {
		name    string
		headers string
		window  uint64
	}{
		{name: "defaults", window: 2},
		{name: "pages", headers: "Racer-Page-Credits: 1\r\n", window: 1},
		{name: "bytes", headers: "Racer-Page-Credits: 64\r\nRacer-Byte-Credits: 16777216\r\n", window: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := streamFakeClient(t, fakeSubscriptionOrigin(t, 3*PageSize))

			conn, res := fakeSubscriptionSocket(t, client, test.headers)
			for page := range test.window {
				fakeReadFrame(t, res.Body, 1, page, page*uint64(PageSize), uint32(PageSize))
			}

			if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}

			var b [1]byte

			_, err := res.Body.Read(b[:])

			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() {
				t.Fatal("stream advanced without credits", err)
			}

			if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}

			for page := test.window; page < 3; page++ {
				fakeRelease(t, conn, page-test.window, uint32(PageSize))
				fakeReadFrame(t, res.Body, 1, page, page*uint64(PageSize), uint32(PageSize))
			}

			fakeReadFrame(t, res.Body, 2, 3, 3*uint64(PageSize), 0)
		})
	}
}

func TestFakeSubscriptionInvalidRelease(t *testing.T) {
	for _, test := range []struct {
		name   string
		page   uint64
		length uint32
	}{
		{name: "unknown", page: 4, length: uint32(PageSize)},
		{name: "length", length: uint32(PageSize) - 1},
		{name: "zero"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := streamFakeClient(t, fakeSubscriptionOrigin(t, 2*PageSize))
			conn, res := fakeSubscriptionSocket(t, client, "Racer-Page-Credits: 1\r\n")
			fakeReadFrame(t, res.Body, 1, 0, 0, uint32(PageSize))
			fakeRelease(t, conn, test.page, test.length)

			if _, err := res.Body.Read(make([]byte, 1)); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal("invalid release did not close the subscription", err)
			}
		})
	}
}

func TestFakeSubscriptionInvalidHeaders(t *testing.T) {
	client := streamFakeClient(t, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
		t.Error("invalid subscription reached origin")
		return Metadata{}, nil, nil
	})

	for _, headers := range []string{
		"Racer-Page-Credits: 0\r\n", "Racer-Page-Credits: 65\r\n",
		"Racer-Byte-Credits: 16777215\r\n", "Racer-Byte-Credits: 1073741825\r\n",
		"Racer-Ordered: 2\r\n", "Racer-Ordered: \r\n",
		"Range: bytes=-5\r\n", "Range: bytes=2-1\r\n", "Range: bytes=0-1,3-4\r\n",
		"Racer-Page-Credits: 1\r\nRacer-Page-Credits: 2\r\n", "If-Match: W/\"weak\"\r\n",
	} {
		t.Run(strings.TrimSpace(headers), func(t *testing.T) {
			_, res := fakeSubscriptionSocket(t, client, headers)
			if res.StatusCode != 400 {
				t.Fatal(res.Status)
			}
		})
	}
}

func TestFakeSubscriptionCancellation(t *testing.T) {
	for _, action := range []string{"disconnect", "cleanup"} {
		t.Run(action, func(t *testing.T) {
			body := &blockedBody{done: make(chan struct{}), first: true}

			client, cleanup, err := newFakeClient(t, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
				return originMeta(1), body, nil
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			conn, res := fakeSubscriptionSocket(t, client, "")
			if res.StatusCode != 200 {
				t.Fatal(res.Status)
			}

			if action == "disconnect" {
				closeBody(conn)
			} else {
				cleanup()
			}

			select {
			case <-body.done:
			case <-time.After(5 * time.Second):
				t.Fatal("subscription retained origin body")
			}
		})
	}
}

func TestFakeSubscriptionErrors(t *testing.T) {
	for _, kind := range []ErrorKind{ErrorUnauthorized, ErrorForbidden, ErrorNotFound, ErrorVersionUnavailable, ErrorUnavailable, ErrorInternal} {
		t.Run(kind.String(), func(t *testing.T) {
			client := streamFakeClient(t, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
				return Metadata{}, nil, NewOriginError(kind, nil)
			})

			_, res := fakeSubscriptionSocket(t, client, "")
			if statusError(res.StatusCode).kind != kind || res.ContentLength != 0 || !res.Close {
				t.Fatal("origin error changed", res.Status, res.Header)
			}
		})
	}

	for _, size := range []ByteLength{0, 13} {
		t.Run("unsatisfiable/"+strconv.FormatUint(uint64(size), 10), func(t *testing.T) {
			client := streamFakeClient(t, fakeSubscriptionOrigin(t, size))

			_, res := fakeSubscriptionSocket(t, client, "Range: bytes=13-\r\n")
			if res.StatusCode != 416 || res.ContentLength != 0 || res.Header.Get("Content-Range") != "bytes */"+strconv.FormatUint(uint64(size), 10) {
				t.Fatal(res.Status, res.Header)
			}
		})
	}
}

func TestFakeSubscriptionDuplicateRelease(t *testing.T) {
	client := streamFakeClient(t, fakeSubscriptionOrigin(t, 3*PageSize))
	conn, res := fakeSubscriptionSocket(t, client, "Racer-Page-Credits: 1\r\n")
	fakeReadFrame(t, res.Body, 1, 0, 0, uint32(PageSize))
	fakeRelease(t, conn, 0, uint32(PageSize))
	fakeReadFrame(t, res.Body, 1, 1, uint64(PageSize), uint32(PageSize))
	fakeRelease(t, conn, 0, uint32(PageSize))

	if _, err := res.Body.Read(make([]byte, 1)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("duplicate release did not close the subscription", err)
	}
}

func TestFakeSubscriptionPartialPageRelease(t *testing.T) {
	client := streamFakeClient(t, fakeSubscriptionOrigin(t, PageSize+3))
	conn, res := fakeSubscriptionSocket(t, client, "Range: bytes=16777214-\r\nRacer-Page-Credits: 1\r\n")
	fakeReadFrame(t, res.Body, 1, 0, uint64(PageSize)-2, 2)
	fakeRelease(t, conn, 0, 2)
	fakeReadFrame(t, res.Body, 1, 1, uint64(PageSize), 3)
	fakeReadFrame(t, res.Body, 2, 2, 5, 0)
}

func TestFakeSubscriptionPendingCallbackCancellation(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})

	client, cleanup, err := newFakeClient(t, func(ctx context.Context, _ OriginRequest) (Metadata, io.ReadCloser, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)

		return Metadata{}, nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)

	conn, _, err := client.bulk.Get(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(conn)

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := fmt.Fprintf(conn, "POST /v2/objects/%s HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n\r\n", (Key{}).String()); err != nil {
		t.Fatal(err)
	}

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("origin not called")
	}

	closeBody(conn)

	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect did not cancel pending origin callback")
	}
}

func TestStreamingHTTP2Success(t *testing.T) {
	for _, size := range []int64{0, 1, 2*copyBufferSize + 17} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				streamResponseHead(w, 0, size, size, `"v"`)
				_, _ = io.CopyN(w, &offsetStream{}, size)
			}))
			c := testClient(t, path, 1)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 2 {
					t.Error("HTTP/2 not negotiated")
				}

				v, err := c.GetStreaming(r.Context(), Request{})
				if err != nil {
					t.Error(err)
					http.Error(w, "get failed", http.StatusBadGateway)

					return
				}
				defer closeBody(v)

				w.Header().Set("Content-Length", strconv.FormatInt(size, 10))

				if n, err := v.WriteToHTTP(w); n != size || err != nil {
					t.Error(n, err)
				}
			}))
			server.EnableHTTP2 = true
			server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}

			server.StartTLS()
			defer server.Close()
			// Exercise successive streams on the same HTTP/2 connection. Reading
			// to EOF observes END_STREAM and catches resets after the final Write.
			for range 5 {
				r, err := server.Client().Get(server.URL)
				if err != nil {
					t.Fatal(err)
				}

				if r.ProtoMajor != 2 || r.StatusCode != http.StatusOK {
					closeBody(r.Body)
					t.Fatal(r.Proto, r.Status)
				}

				n, err := io.Copy(&offsetSink{}, r.Body)
				closeBody(r.Body)

				if n != size || err != nil {
					t.Fatal(n, err)
				}
			}

			if c.Stats().BytesRead != uint64(5*size) || c.Stats().ActiveBulk != 0 {
				t.Fatal(c.Stats())
			}
		})
	}
}

type transferResponse struct {
	http.ResponseWriter
	fast *atomic.Int64
}

func (w transferResponse) ReadFrom(r io.Reader) (int64, error) {
	if lr, ok := r.(*io.LimitedReader); ok {
		if _, ok := lr.R.(*net.UnixConn); ok {
			w.fast.Add(lr.N)
		}
	}

	return w.ResponseWriter.(io.ReaderFrom).ReadFrom(r)
}

func TestHTTPTransferKeepsConnectionsAndRanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		window int
	}{
		{"default window", 0},
		{"two-page window", 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			testHTTPTransferKeepsConnectionsAndRanges(t, test.window)
		})
	}
}

func testHTTPTransferKeepsConnectionsAndRanges(t *testing.T, window int) {
	t.Helper()

	const size = 2*int64(PageSize) + 173

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first, last := fixtureRange(t, r, size)

		streamResponseHead(w, int64(first), int64(last-first)+1, size, `"v"`)
		_, _ = io.CopyN(w, &offsetStream{offset: int64(first)}, int64(last-first)+1)
	}))
	c := testClient(t, path, 2)
	c.config.PageWindow = window

	var fast, connections atomic.Int64

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		options := ReadOptions{}

		if offset != 0 {
			metadata := Metadata{Size: ByteLength(size), ETag: ETag{value: `"v"`}, ExpiresAt: time.Unix(2000000000, 0)}
			options = ReadOptions{Offset: ByteOffset(offset), Pin: metadata.ETag, Metadata: &metadata}
		}

		v, err := c.Get(r.Context(), Request{}, options)
		if err != nil {
			t.Error(err)
			http.Error(w, "get failed", http.StatusBadGateway)

			return
		}
		defer closeBody(v)

		w.Header().Set("Content-Length", strconv.FormatInt(size-offset, 10))

		if offset != 0 {
			w.WriteHeader(http.StatusPartialContent)
		}

		if n, err := v.WriteToHTTP(transferResponse{w, &fast}); err != nil || n != size-offset {
			t.Errorf("transfer %d: %v", n, err)
		}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}

	server.Start()
	defer server.Close()

	for _, offset := range []int64{0, int64(PageSize) + 7, 0} {
		response, err := server.Client().Get(server.URL + "?offset=" + strconv.FormatInt(offset, 10))
		if err != nil {
			t.Fatal(err)
		}

		n, err := io.Copy(&offsetSink{offset: offset}, response.Body)
		closeBody(response.Body)

		if err != nil || n != size-offset {
			t.Fatal(n, err)
		}
	}

	if connections.Load() != 1 || fast.Load() != 0 {
		t.Fatal("keep-alive/fast path", connections.Load(), fast.Load())
	}

	if c.Stats().ActiveBulk != 0 {
		t.Fatal("retained admission")
	}
}

func TestHTTPTransferFallbackAndTruncation(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(strconv.FormatBool(truncated), func(t *testing.T) {
			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				streamResponseHead(w, 0, 8192, 8192, `"v"`)

				length := int64(8192)
				if truncated {
					length--
				}

				_, _ = io.CopyN(w, &offsetStream{}, length)
			}))
			c := testClient(t, path, 1)

			v, err := c.Get(context.Background(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(v)

			n, err := v.WriteToHTTP(httptest.NewRecorder())
			if truncated != (err != nil) || !truncated && n != 8192 {
				t.Fatal(n, err)
			}
		})
	}
}

func TestIntegrationOriginRoundTrip(t *testing.T) {
	for _, size := range []int64{0, 4096, int64(PageSize), int64(PageSize) + 13, 3*int64(PageSize) + 13} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			var calls atomic.Int32

			metadata := "opaque, interior  spaces\\\xff"
			authorization := "Scheme opaque,credential\x80"
			request := Request{Key: Key{1, 2, 3}, Context: FetchContext{
				metadata: AdapterMetadata{value: metadata}, authorization: Authorization{value: authorization},
			}}
			path, cancel, done := startOrigin(t, OriginConfig{}, func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
				call := calls.Add(1)

				if r.Key() != request.Key || r.Context().Metadata().ForOrigin() != metadata || r.Context().Authorization().ForOrigin() != authorization {
					t.Error("key or context changed on origin request")
				}

				m := originMeta(ByteLength(size))
				// Both are expired: zero-TTL revalidation admits bootstrap and pins
				// remain valid after expiry. The Value must retain the first snapshot.
				if call > 1 {
					m.ExpiresAt = time.UnixMilli(1)
					if pin, ok := r.Pin(); !ok || pin != m.ETag || r.Operation() != OperationPinned {
						t.Error("continuation lost its immutable pin")
					}
				}

				if size == 0 {
					return m, nil, nil
				}

				byteRange, _ := r.Range()

				first, last, err := byteRange.Resolve(m.Size)
				if err != nil {
					return Metadata{}, nil, err
				}

				return m, io.NopCloser(io.LimitReader(&offsetStream{offset: int64(first)}, int64(last-first)+1)), nil
			})

			defer func() { cancel(); <-done }()

			client := originClient(t, path, 1)

			value, err := client.Get(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(value)

			snapshot := value.Metadata()
			sink := &offsetSink{}

			n, err := io.Copy(sink, value)
			if err != nil || n != size || sink.offset != size {
				t.Fatalf("round trip: %d %v", n, err)
			}

			wantCalls := max(1, (size+int64(PageSize)-1)/int64(PageSize))
			if calls.Load() != int32(wantCalls) || value.Metadata() != snapshot || snapshot.ExpiresAt.UnixMilli() != 0 {
				t.Fatal("page count or immutable metadata snapshot changed")
			}
		})
	}
}

func TestIntegrationClientConnectionReuse(t *testing.T) {
	path, cancel, done := startOrigin(t, OriginConfig{}, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
		return originMeta(PageSize + 1), io.NopCloser(io.LimitReader(repeatedByte('x'), int64(PageSize))), nil
	})

	defer func() { cancel(); <-done }()
	// Only consume bootstrap, then close: a completely consumed HTTP frame can be
	// reused even though the lazy full-object continuation was never requested.
	client := originClient(t, path, 1)

	var connections []net.Conn

	ctx := context.Background()
	for range 3 {
		v, err := client.Get(ctx, Request{})
		if err != nil {
			t.Fatal(err)
		}

		connections = append(connections, v.stream.conn.Conn)

		if n, err := io.CopyN(io.Discard, v, int64(PageSize)); err != nil || n != int64(PageSize) {
			t.Fatal(n, err)
		}

		closeBody(v)
	}

	if len(connections) != 3 || connections[0] == connections[1] || connections[1] == connections[2] {
		t.Fatal("subscription socket must not be reused")
	}
}

func TestIntegrationCancelBlockedDial(t *testing.T) {
	client := testClient(t, "unused", 1)
	entered, stopped := make(chan struct{}), make(chan struct{})
	poolConfig := client.bulk.config
	poolConfig.Dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(entered)
		<-ctx.Done()
		close(stopped)

		return nil, ctx.Err()
	}
	client.configurePools(poolConfig)

	result := make(chan error, 1)

	go func() { _, err := client.Get(context.Background(), Request{}); result <- err }()

	<-entered
	closeBody(client)

	select {
	case err := <-result:
		assertKind(t, err, ErrorClosed)
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock pending dial Get")
	}

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Close retained the detached transport dial")
	}
}

func TestIntegrationValueCloseBlockedBody(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelContext), func(t *testing.T) {
			body := &blockedBody{done: make(chan struct{}), first: true}
			path, stop, done := startOrigin(t, OriginConfig{}, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
				return originMeta(1), body, nil
			})

			defer func() { stop(); <-done }()

			client := originClient(t, path, 1)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			v, err := client.Get(ctx, Request{})
			if err != nil {
				t.Fatal(err)
			}

			result := make(chan error, 1)

			go func() { _, err := v.Read(make([]byte, 1)); result <- err }()

			if cancelContext {
				cancel()
			} else {
				closeBody(v)
			}

			select {
			case err := <-result:
				if cancelContext {
					if !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				} else {
					assertKind(t, err, ErrorClosed)
				}
			case <-time.After(time.Second):
				t.Fatal("blocked Read retained")
			}

			select {
			case <-body.done:
			case <-time.After(time.Second):
				t.Fatal("origin body not canceled by disconnect")
			}

			if body.closed.Load() != 1 {
				t.Fatal("body close count", body.closed.Load())
			}
		})
	}
}

func TestIntegrationAbortedConnectionNotReused(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		streamResponse(w, 0, int64(PageSize), int64(PageSize), `"v"`)
	}))
	client := testClient(t, path, 1)

	var connections []net.Conn

	ctx := context.Background()
	for range 2 {
		value, err := client.Get(ctx, Request{})
		if err != nil {
			t.Fatal(err)
		}

		connections = append(connections, value.stream.conn.Conn)

		closeBody(value)
	}

	if len(connections) != 2 || connections[0] == connections[1] {
		t.Fatal("unread response connection reused")
	}
}

func TestValueWriteToSplicesOrderedHTTPBodies(t *testing.T) {
	const size = 2*int64(PageSize) + 173

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first, last := fixtureRange(t, r, size)

		streamResponseHead(w, int64(first), int64(last-first)+1, size, `"v"`)
		_, _ = io.CopyN(w, &offsetStream{offset: int64(first)}, int64(last-first)+1)
	}))
	c := testClient(t, path, 3)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	v, err := c.Get(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	peer, connection := streamTCPPair(t)

	if err := peer.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() {
		_, err := io.CopyN(&offsetSink{}, peer, size)
		done <- err
	}()

	sink := &tcpTransferWriter{TCPConn: connection}

	n, err := v.WriteTo(sink)
	if err != nil || n != size {
		t.Fatal(n, err)
	}

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if sink.readFrom != 0 {
		t.Fatal("buffered subscription bypassed page validation", sink.readFrom)
	}

	if len(c.slots) != 0 {
		t.Fatal("body permits leaked")
	}
	// A later response still goes through Transport parsing and body lifecycle.
	other, err := c.Get(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}

	closeBody(other)

	streaming, err := c.GetStreaming(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(streaming)

	go func() {
		_, err := io.CopyN(&offsetSink{}, peer, size)
		done <- err
	}()

	if n, err := streaming.WriteToHTTP(sink); err != nil || n != size {
		t.Fatal(n, err)
	}

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if sink.readFrom == 0 || c.Stats().ActiveBulk != 0 {
		t.Fatal("streaming transfer did not dispatch ReaderFrom or release admission")
	}
}

func TestValueWriteToSpliceCancellation(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamResponse(w, 0, int64(PageSize), int64(PageSize), `"v"`)
	}))
	c := testClient(t, path, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	v, err := c.GetStreaming(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	peer, connection := streamTCPPair(t)

	if err := connection.SetWriteBuffer(4096); err != nil {
		t.Fatal(err)
	}

	if err := peer.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	sink := &tcpTransferWriter{TCPConn: connection}

	done := make(chan error, 1)

	go func() { _, err := v.WriteToHTTP(sink); done <- err }()
	// Consume a prefix, then leave the destination blocked and cancel the Value.
	if _, err := io.CopyN(io.Discard, peer, 64*1024); err != nil {
		t.Fatal(err)
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("splice did not cancel")
	}

	if len(c.slots) != 0 {
		t.Fatal("canceled splice retained page")
	}
}

// tcpTransferWriter exposes the real TCP ReaderFrom and deadline implementations
// without HTTP buffering, so transfers exercise Unix-to-TCP dispatch directly.
type tcpTransferWriter struct {
	*net.TCPConn
	readFrom int
}

func (w *tcpTransferWriter) Header() http.Header { return make(http.Header) }

func (w *tcpTransferWriter) WriteHeader(int) {}

func (w *tcpTransferWriter) ReadFrom(r io.Reader) (int64, error) {
	w.readFrom++
	return w.TCPConn.ReadFrom(r)
}

// Model a destination with a shorter write budget than the source read budget.
// Real HTTP/2 expires that budget by resetting the stream, even when no Write
// is in progress. Clearing the deadline must reach the actual HTTP/2 writer.
type streamingShortDeadline struct{ http.ResponseWriter }

func (w streamingShortDeadline) SetWriteDeadline(d time.Time) error {
	if !d.IsZero() && d.After(time.Now()) {
		d = time.Now().Add(50 * time.Millisecond)
	}

	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(d)
}

func TestStreamingHTTP2UpstreamWaits(t *testing.T) {
	for _, phase := range []string{"first", "body", "page", "Complete"} {
		t.Run(phase, func(t *testing.T) {
			const size = uint64(PageSize) + 3

			pause := func(at string) {
				if at == phase {
					time.Sleep(200 * time.Millisecond)
				}
			}
			c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
				_, _ = io.WriteString(conn, subscriptionHead(size, 0, size))

				pause("first")

				_ = fakeSubscriptionFrame(conn, 1, 0, 0, uint32(PageSize))
				_, _ = io.CopyN(conn, &offsetStream{}, copyBufferSize)

				pause("body")

				_, _ = io.CopyN(conn, &offsetStream{offset: copyBufferSize}, int64(PageSize)-copyBufferSize)
				if !orderedRelease(t, reader, 0, uint32(PageSize)) {
					return
				}

				pause("page")

				_ = fakeSubscriptionFrame(conn, 1, 1, uint64(PageSize), 3)
				_, _ = io.CopyN(conn, &offsetStream{offset: int64(PageSize)}, 3)

				if !orderedRelease(t, reader, 1, 3) {
					return
				}

				pause("Complete")

				_ = fakeSubscriptionFrame(conn, 2, 2, size, 0)
			})
			c.config.BodyReadTimeout = 3 * time.Second
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				v, err := c.GetStreaming(r.Context(), Request{}, ReadOptions{PageCredits: 1})
				if err != nil {
					t.Error(err)
					return
				}
				defer closeBody(v)

				w.Header().Set("Content-Length", strconv.FormatUint(size, 10))

				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Error(err)
				}

				if n, err := v.WriteToHTTP(streamingShortDeadline{w}); n != int64(size) || err != nil {
					t.Errorf("stream: bytes=%d err=%v", n, err)
					panic(http.ErrAbortHandler)
				}
			}))
			server.EnableHTTP2 = true
			server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}

			server.StartTLS()
			defer server.Close()

			server.Client().Timeout = 10 * time.Second

			resp, err := server.Client().Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(resp.Body)

			n, err := io.Copy(&offsetSink{}, resp.Body)
			if resp.ProtoMajor != 2 || n != int64(size) || err != nil {
				t.Fatalf("proto=%s bytes=%d err=%v", resp.Proto, n, err)
			}
		})
	}
}

type streamingDeadlineState struct {
	http.ResponseWriter
	mu       sync.Mutex
	deadline time.Time
}

func (w *streamingDeadlineState) SetWriteDeadline(d time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.deadline = d

	return nil
}

func TestStreamingOperationClearPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	dst := &streamingDeadlineState{ResponseWriter: httptest.NewRecorder()}
	h := &streamingHTTP{value: &Value{admissionLease: &admissionLease{ctx: ctx}}, controller: http.NewResponseController(dst)}
	interrupt := time.Now().Add(-time.Second)

	cancel()

	if err := dst.SetWriteDeadline(interrupt); err != nil {
		t.Fatal(err)
	}

	if err := h.clearWriteDeadline(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	if !dst.deadline.Equal(interrupt) {
		t.Fatal("operation cleanup erased cancellation deadline")
	}
}

type streamingHTTP2BlockedWriter struct {
	http.ResponseWriter
	active  atomic.Bool
	written atomic.Int64
}

func (w *streamingHTTP2BlockedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *streamingHTTP2BlockedWriter) Write(p []byte) (int, error) {
	w.active.Store(true)
	defer w.active.Store(false)

	n, err := w.ResponseWriter.Write(p)
	w.written.Add(int64(n))

	return n, err
}

func TestStreamingHTTP2BlockedDestinationCancellation(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		streamResponse(w, 0, int64(PageSize), int64(PageSize), `"v"`)
	}))
	c := testClient(t, path, 1)
	c.config.BodyReadTimeout = 10 * time.Second

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	destination := make(chan *streamingHTTP2BlockedWriter, 1)
	done := make(chan error, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		v, err := c.GetStreaming(ctx, Request{})
		if err != nil {
			done <- err
			return
		}
		defer closeBody(v)

		w.Header().Set("Content-Length", strconv.FormatUint(uint64(PageSize), 10))

		if err := http.NewResponseController(w).Flush(); err != nil {
			done <- err
			return
		}

		dst := &streamingHTTP2BlockedWriter{ResponseWriter: w}
		destination <- dst

		_, err = v.WriteToHTTP(dst)
		done <- err

		if err != nil {
			panic(http.ErrAbortHandler)
		}
	}))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}

	server.StartTLS()
	defer server.Close()

	server.Client().Timeout = 5 * time.Second

	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(resp.Body)

	if resp.ProtoMajor != 2 {
		t.Fatal("HTTP/2 not negotiated")
	}

	var dst *streamingHTTP2BlockedWriter
	select {
	case dst = <-destination:
	case <-time.After(3 * time.Second):
		t.Fatal("destination not started")
	}
	// Do not consume the body: exhaust the client's stream flow-control window.
	// Require an actual Write to stay active with no progress before canceling.
	deadline := time.Now().Add(3 * time.Second)
	blocked := false

	for time.Now().Before(deadline) {
		before := dst.written.Load()

		time.Sleep(100 * time.Millisecond)

		if before > 0 && dst.active.Load() && dst.written.Load() == before {
			blocked = true
			break
		}
	}

	if !blocked {
		t.Fatal("destination never blocked in HTTP/2 Write")
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked HTTP/2 destination did not cancel")
	}

	if c.Stats().ActiveBulk != 0 || len(c.copySlots) != 0 {
		t.Fatal("canceled HTTP/2 transfer retained admission")
	}
}

func TestStreamingFinalGateAndIncompletePages(t *testing.T) {
	for _, mode := range []string{"success", "short payload", "missing complete", "bad complete"} {
		t.Run(mode, func(t *testing.T) {
			released := make(chan struct{})

			resume := make(chan struct{})
			defer close(resume)

			c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, head []byte) {
				if headHeaders(head).Get("Racer-Ordered") != "1" {
					t.Error("streaming did not force ordering")
				}

				_, _ = io.WriteString(conn, subscriptionHead(3, 0, 3))

				_ = fakeSubscriptionFrame(conn, 1, 0, 0, 3)
				if mode == "short payload" {
					_, _ = io.WriteString(conn, "a")
					return
				}

				_, _ = io.WriteString(conn, "abc")

				if !orderedRelease(t, reader, 0, 3) {
					return
				}

				close(released)
				<-resume

				if mode == "missing complete" {
					return
				}

				length := uint64(3)
				if mode == "bad complete" {
					length++
				}

				_ = fakeSubscriptionFrame(conn, 2, 1, length, 0)
			})

			v, err := c.GetStreaming(t.Context(), Request{}, ReadOptions{PageCredits: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(v)

			if v.ordered != nil || v.stream.buffers != nil {
				t.Fatal("streaming allocated page receiver")
			}

			if _, err := v.Read(nil); err == nil {
				t.Fatal("streaming Read accepted")
			} else {
				assertKind(t, err, ErrorInvalidArgument)
			}

			if _, err := v.WriteTo(io.Discard); err == nil {
				t.Fatal("streaming WriteTo accepted")
			}

			dst := httptest.NewRecorder()

			type result struct {
				n   int64
				err error
			}

			done := make(chan result, 1)

			go func() { n, err := v.WriteToHTTP(dst); done <- result{n, err} }()

			if mode != "short payload" {
				orderedWait(t, released)
				// The release is emitted only after both prefix writes have returned.
				if dst.Body.String() != "ab" {
					t.Fatal("final byte escaped before Complete", dst.Body.String())
				}

				resume <- struct{}{}
			}

			r := <-done

			want := "ab"
			if mode == "success" {
				want = "abc"
			}

			if mode == "short payload" {
				want = "a"
			}

			if dst.Body.String() != want || r.n != int64(len(want)) || (r.err == nil) != (mode == "success") {
				t.Fatal(dst.Body.String(), r)
			}

			if mode == "bad complete" {
				assertKind(t, r.err, ErrorProtocol)
			}

			if mode == "short payload" || mode == "missing complete" {
				if !errors.Is(r.err, io.ErrUnexpectedEOF) {
					t.Fatal(r.err)
				}
			}

			wantRead := uint64(3)
			if mode == "short payload" {
				wantRead = 1
			}

			if c.Stats().BytesRead != wantRead || c.Stats().ActiveBulk != 0 {
				t.Fatal(c.Stats())
			}
		})
	}
}

func TestStreamingEmptyAndMalformedFrames(t *testing.T) {
	for _, mode := range []string{"empty", "one byte", "empty bad complete", "kind", "number", "offset", "length", "early complete"} {
		t.Run(mode, func(t *testing.T) {
			size := uint64(3)
			if mode == "empty" || mode == "empty bad complete" {
				size = 0
			}

			if mode == "one byte" {
				size = 1
			}

			c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
				_, _ = io.WriteString(conn, subscriptionHead(size, 0, size))
				if size == 0 {
					n := uint64(0)
					if mode == "empty bad complete" {
						n++
					}

					_ = fakeSubscriptionFrame(conn, 2, n, 0, 0)

					return
				}

				kind, number, offset, length := byte(1), uint64(0), uint64(0), uint32(size)

				switch mode {
				case "kind":
					kind = 3
				case "number":
					number++
				case "offset":
					offset++
				case "length":
					length++
				case "early complete":
					kind = 2
				}

				_ = fakeSubscriptionFrame(conn, kind, number, offset, length)
				if mode == "one byte" {
					_, _ = io.WriteString(conn, "x")
					if orderedRelease(t, reader, 0, 1) {
						_ = fakeSubscriptionFrame(conn, 2, 1, 1, 0)
					}
				}
			})

			v, err := c.GetStreaming(t.Context(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(v)

			dst := httptest.NewRecorder()

			n, err := v.WriteToHTTP(dst)
			if mode == "empty" || mode == "one byte" {
				if n != int64(size) || err != nil {
					t.Fatal(n, err)
				}
			} else {
				assertKind(t, err, ErrorProtocol)

				if n != 0 || dst.Body.Len() != 0 {
					t.Fatal("invalid payload exposed")
				}
			}
		})
	}
}

type streamingResponse struct {
	http.ResponseWriter
	fast *atomic.Int64
}

func (w streamingResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w streamingResponse) ReadFrom(r io.Reader) (int64, error) {
	lr, ok := r.(*io.LimitedReader)
	if !ok || lr.N <= 0 || lr.N > copyBufferSize {
		return 0, errors.New("unbounded transfer")
	}

	if _, ok := lr.R.(*net.UnixConn); !ok {
		return 0, errors.New("wrapped source")
	}

	w.fast.Add(lr.N)

	return w.ResponseWriter.(io.ReaderFrom).ReadFrom(r)
}

func TestStreamingHTTPFastPathRangesAndFallback(t *testing.T) {
	const size = int64(PageSize) + copyBufferSize + 17

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first, last := fixtureRange(t, r, size)
		streamResponseHead(w, int64(first), int64(last-first)+1, size, `"v"`)
		_, _ = io.CopyN(w, &offsetStream{offset: int64(first)}, int64(last-first)+1)
	}))
	c := testClient(t, path, 2)

	var fast, connections atomic.Int64

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)

		v, err := c.GetStreaming(r.Context(), Request{}, ReadOptions{Offset: ByteOffset(offset), PageCredits: 1})
		if err != nil {
			t.Error(err)
			return
		}
		defer closeBody(v)

		w.Header().Set("Content-Length", strconv.FormatInt(size-offset, 10))

		if offset != 0 {
			w.WriteHeader(http.StatusPartialContent)
		}

		n, err := v.WriteToHTTP(streamingResponse{w, &fast})
		if n != size-offset || err != nil {
			t.Error(n, err)
		}
	}))
	server.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			connections.Add(1)
		}
	}

	server.Start()
	defer server.Close()

	for _, offset := range []int64{0, int64(PageSize) - 3, size - 1} {
		r, err := server.Client().Get(server.URL + "?offset=" + strconv.FormatInt(offset, 10))
		if err != nil {
			t.Fatal(err)
		}

		n, err := io.Copy(&offsetSink{offset: offset}, r.Body)
		closeBody(r.Body)

		if n != size-offset || err != nil {
			t.Fatal(n, err)
		}
	}

	if fast.Load() == 0 || connections.Load() != 1 {
		t.Fatal(fast.Load(), connections.Load())
	}
	// A writer without ReaderFrom must retain the same payload framing.
	v, err := c.GetStreaming(t.Context(), Request{}, ReadOptions{Offset: ByteOffset(size - 100)})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	dst := httptest.NewRecorder()
	if n, err := v.WriteToHTTP(dst); n != 100 || err != nil {
		t.Fatal(n, err)
	}

	if _, err := io.Copy(&offsetSink{offset: size - 100}, dst.Body); err != nil {
		t.Fatal(err)
	}
}

type streamingWriter struct {
	http.ResponseWriter
	write func([]byte) (int, error)
}

func (w streamingWriter) Write(p []byte) (int, error) { return w.write(p) }

func TestStreamingBlockedWriterCapacityAndCancellation(t *testing.T) {
	c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, subscriptionHead(3, 0, 3))
		_ = fakeSubscriptionFrame(conn, 1, 0, 0, 3)
		_, _ = io.WriteString(conn, "abc")
		_, _ = io.Copy(io.Discard, reader)
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	v, err := c.GetStreaming(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}

	entered, resume := make(chan struct{}), make(chan struct{})
	defer close(resume)

	done := make(chan error, 1)

	go func() {
		_, err := v.WriteToHTTP(streamingWriter{httptest.NewRecorder(), func(p []byte) (int, error) {
			close(entered)
			<-resume

			if string(p) != "ab" {
				t.Error("blocked scratch changed", string(p))
			}

			return len(p), nil
		}})
		done <- err
	}()

	orderedWait(t, entered)
	cancel()
	orderedWait(t, v.finished)

	if c.Stats().ActiveBulk != 0 || len(c.copySlots) != 1 {
		t.Fatal("wrong canceled admission", c.Stats())
	}

	next, err := c.GetStreaming(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(next)

	_, err = next.WriteToHTTP(httptest.NewRecorder())
	assertKind(t, err, ErrorUnavailable)

	resume <- struct{}{}

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	if len(c.copySlots) != 0 {
		t.Fatal("copy slot leaked")
	}
}

type streamingTCPResponse struct{ *net.TCPConn }

func (w streamingTCPResponse) Header() http.Header { return make(http.Header) }

func (w streamingTCPResponse) WriteHeader(int) {}

func TestStreamingFastDestinationCancellation(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		streamResponse(w, 0, int64(PageSize), int64(PageSize), `"v"`)
	}))
	c := testClient(t, path, 1)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	v, err := c.GetStreaming(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	peer, destination := streamTCPPair(t)

	if err := destination.SetWriteBuffer(4096); err != nil {
		t.Fatal(err)
	}

	if err := peer.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { _, err := v.WriteToHTTP(streamingTCPResponse{destination}); done <- err }()

	if _, err := io.CopyN(io.Discard, peer, 64*1024); err != nil {
		t.Fatal(err)
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("socket transfer did not cancel")
	}

	if c.Stats().ActiveBulk != 0 || len(c.copySlots) != 0 {
		t.Fatal("canceled transfer retained admission")
	}
}

type streamingBadReaderFrom struct {
	http.ResponseWriter
	mode string
}

func (w streamingBadReaderFrom) ReadFrom(r io.Reader) (int64, error) {
	lr := r.(*io.LimitedReader)

	switch w.mode {
	case "negative":
		return -1, nil
	case "excess":
		return lr.N + 1, nil
	case "short":
		_, err := io.Copy(io.Discard, lr)
		return 0, err
	}

	return 0, nil
}

func TestStreamingReaderFromFailures(t *testing.T) {
	for _, mode := range []string{"negative", "excess", "short", "no progress"} {
		t.Run(mode, func(t *testing.T) {
			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				streamResponse(w, 0, 2*copyBufferSize, 2*copyBufferSize, `"v"`)
			}))
			c := testClient(t, path, 1)

			v, err := c.GetStreaming(t.Context(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(v)

			n, err := v.WriteToHTTP(streamingBadReaderFrom{httptest.NewRecorder(), mode})
			if err == nil || n >= 2*copyBufferSize {
				t.Fatal("bad ReaderFrom accepted", n, err)
			}

			if mode == "no progress" {
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, io.ErrShortWrite) {
				t.Fatal(err)
			}

			if c.Stats().ActiveBulk != 0 {
				t.Fatal("failed transfer retained admission")
			}
		})
	}
}

func TestStreamingTLSFallback(t *testing.T) {
	const size = 2*copyBufferSize + 17

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		streamResponseHead(w, 0, size, size, `"v"`)
		_, _ = io.CopyN(w, &offsetStream{}, size)
	}))
	c := testClient(t, path, 1)

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, err := c.GetStreaming(r.Context(), Request{})
		if err != nil {
			t.Error(err)
			return
		}
		defer closeBody(v)

		w.Header().Set("Content-Length", strconv.Itoa(size))

		if n, err := v.WriteToHTTP(w); n != size || err != nil {
			t.Error(n, err)
		}
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}

	server.StartTLS()
	defer server.Close()

	r, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(r.Body)

	if n, err := io.Copy(&offsetSink{}, r.Body); n != size || err != nil {
		t.Fatal(n, err)
	}

	if c.Stats().BytesRead != size {
		t.Fatal(c.Stats())
	}
}

type streamingDeadlineRecorder struct {
	http.ResponseWriter
	expired atomic.Int64
}

func (w *streamingDeadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() && !deadline.After(time.Now()) {
		w.expired.Add(1)
	}

	return nil
}

func TestStreamingSuccessDoesNotExpireDestination(t *testing.T) {
	c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, subscriptionHead(0, 0, 0))
		_ = fakeSubscriptionFrame(conn, 2, 0, 0, 0)
	})

	v, err := c.GetStreaming(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	dst := &streamingDeadlineRecorder{ResponseWriter: httptest.NewRecorder()}
	if n, err := v.WriteToHTTP(dst); n != 0 || err != nil {
		t.Fatal(n, err)
	}

	if dst.expired.Load() != 0 {
		t.Fatal("successful completion expired destination deadline")
	}
}

func completeTestValue(t *testing.T, size uint64, completion string) *Value {
	t.Helper()
	c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, subscriptionHead(size, 0, size))
		for first := uint64(0); first < size; {
			n := min(uint64(PageSize), size-first)
			_ = fakeSubscriptionFrame(conn, 1, first/uint64(PageSize), first, uint32(n))
			_, _ = io.CopyN(conn, &offsetStream{offset: int64(first)}, int64(n))
			first += n
		}

		if completion != "missing" {
			pages := (size + uint64(PageSize) - 1) / uint64(PageSize)
			if completion == "bad" {
				pages++
			}

			_ = fakeSubscriptionFrame(conn, 2, pages, size, 0)
		}
	})

	v, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeBody(v) })

	return v
}

func TestWriteToHTTPCompleteBeforeEmptySuccessAndAbortAfterCommit(t *testing.T) {
	for _, mode := range []string{"http", "tls", "nonhijackable"} {
		for _, size := range []uint64{0, 4, uint64(PageSize) + 1} {
			for _, completion := range []string{"good", "bad", "missing"} {
				t.Run(mode+"/"+strconv.FormatUint(size, 10)+"/"+completion, func(t *testing.T) {
					v := completeTestValue(t, size, completion)
					handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						defer closeBody(v)

						w.Header().Set("Content-Length", strconv.FormatUint(size, 10))

						n, err := v.WriteToHTTP(w)
						if err != nil {
							if n > 0 {
								panic(http.ErrAbortHandler)
							}

							w.Header().Del("Content-Length")
							http.Error(w, "value unavailable", http.StatusBadGateway)
						}
					})

					if mode == "nonhijackable" {
						w := httptest.NewRecorder()

						var caught any

						func() {
							defer func() { caught = recover() }()

							handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
						}()

						if completion == "good" {
							if caught != nil || w.Code != 200 || uint64(w.Body.Len()) != size {
								t.Fatal(w.Code, w.Body.Len(), caught)
							}
						} else if size > uint64(PageSize) {
							if caught != http.ErrAbortHandler {
								t.Fatal("committed response did not abort", caught)
							}
						} else if caught != nil || w.Code < 400 {
							t.Fatal("failure before commit reported success", w.Code, caught)
						}

						return
					}

					server := httptest.NewUnstartedServer(handler)

					server.Config.ErrorLog = log.New(io.Discard, "", 0)
					if mode == "tls" {
						server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
						server.StartTLS()
					} else {
						server.Start()
					}
					defer server.Close()

					response, err := server.Client().Get(server.URL)
					if err != nil {
						if completion == "good" || size == 0 {
							t.Fatal(err)
						}

						return
					}
					defer closeBody(response.Body)

					if mode == "tls" && response.TLS.Version != tls.VersionTLS13 {
						t.Fatal("TLS 1.3 required")
					}

					count, readErr := io.Copy(io.Discard, response.Body)
					if completion == "good" {
						if response.StatusCode != 200 || count != int64(size) || readErr != nil {
							t.Fatal(response.Status, count, readErr)
						}
					} else if size == 0 {
						if response.StatusCode < 400 {
							t.Fatal("empty response committed before Complete", response.Status)
						}
					} else if response.StatusCode < 400 && readErr == nil {
						t.Fatal("incomplete response reported success", count)
					}
				})
			}
		}
	}
}
