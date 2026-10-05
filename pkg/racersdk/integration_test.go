// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
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
			client, cleanup, err := newFakeClient(t, fakeSubscriptionOrigin(t, test.size))
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

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
			client, cleanup, err := newFakeClient(t, fakeSubscriptionOrigin(t, 3*PageSize))
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			conn, res := fakeSubscriptionSocket(t, client, test.headers)
			for page := range test.window {
				fakeReadFrame(t, res.Body, 1, page, page*uint64(PageSize), uint32(PageSize))
			}

			if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}

			var b [1]byte

			_, err = res.Body.Read(b[:])

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
			client, cleanup, err := newFakeClient(t, fakeSubscriptionOrigin(t, 2*PageSize))
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)
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
	client, cleanup, err := newFakeClient(t, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
		t.Error("invalid subscription reached origin")
		return Metadata{}, nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)

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
			client, cleanup, err := newFakeClient(t, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
				return Metadata{}, nil, NewOriginError(kind, nil)
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			_, res := fakeSubscriptionSocket(t, client, "")
			if statusError(res.StatusCode).kind != kind || res.ContentLength != 0 || !res.Close {
				t.Fatal("origin error changed", res.Status, res.Header)
			}
		})
	}

	for _, size := range []ByteLength{0, 13} {
		t.Run("unsatisfiable/"+strconv.FormatUint(uint64(size), 10), func(t *testing.T) {
			client, cleanup, err := newFakeClient(t, fakeSubscriptionOrigin(t, size))
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			_, res := fakeSubscriptionSocket(t, client, "Range: bytes=13-\r\n")
			if res.StatusCode != 416 || res.ContentLength != 0 || res.Header.Get("Content-Range") != "bytes */"+strconv.FormatUint(uint64(size), 10) {
				t.Fatal(res.Status, res.Header)
			}
		})
	}
}

func TestFakeSubscriptionDuplicateRelease(t *testing.T) {
	client, cleanup, err := newFakeClient(t, fakeSubscriptionOrigin(t, 3*PageSize))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)
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
	client, cleanup, err := newFakeClient(t, fakeSubscriptionOrigin(t, PageSize+3))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)
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
	testHTTPTransferKeepsConnectionsAndRanges(t, 0)
}

func TestHTTPTransferWindowKeepsConnectionsAndRanges(t *testing.T) {
	testHTTPTransferKeepsConnectionsAndRanges(t, 2)
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
	poolConfig := client.bulk.Config()
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

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(listener)

	peer, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(peer)

	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(connection)

	if err := peer.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() {
		_, err := io.CopyN(&offsetSink{}, peer, size)
		done <- err
	}()

	sink := &tcpTransferWriter{TCPConn: connection.(*net.TCPConn)}

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

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(listener)

	peer, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(peer)

	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(connection)

	if err := connection.(*net.TCPConn).SetWriteBuffer(4096); err != nil {
		t.Fatal(err)
	}

	if err := peer.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	sink := &tcpTransferWriter{TCPConn: connection.(*net.TCPConn)}

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
