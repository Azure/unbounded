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
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// offsetStream makes wrong page offsets observable without allocating an object.
type offsetStream struct{ offset int64 }

func (r *offsetStream) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte((r.offset + int64(i)) % 251)
	}

	r.offset += int64(len(p))

	return len(p), nil
}

type offsetSink struct{ offset int64 }

func (w *offsetSink) Write(p []byte) (int, error) {
	for i, b := range p {
		if b != byte((w.offset+int64(i))%251) {
			return i, fmt.Errorf("wrong byte at offset %d", w.offset+int64(i))
		}
	}

	w.offset += int64(len(p))

	return len(p), nil
}

func unixTransport(path string) *http.Transport {
	return &http.Transport{
		DisableCompression: true, MaxConnsPerHost: 16, MaxIdleConnsPerHost: 16,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}
}

// pageForwarder is a deliberately sequential fake dataplane: it splits a client
// range into origin pages. It establishes SDK integration, not Rust compatibility.
func pageForwarder(t *testing.T, path string, size int64) http.Handler {
	t.Helper()

	transport := unixTransport(path)
	t.Cleanup(transport.CloseIdleConnections)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested, err := parseRange(r.Header.Get("Range"))
		if err != nil {
			t.Error(err)
			return
		}

		first, last, err := requested.Resolve(ByteLength(size))
		if err != nil {
			t.Error(err)
			return
		}

		for start := int64(first); start <= int64(last); start += int64(PageSize) {
			end := min(start+int64(PageSize)-1, int64(last))

			req, err := http.NewRequestWithContext(r.Context(), "GET", "http://racer"+r.URL.Path, nil)
			if err != nil {
				t.Error(err)
				return
			}

			req.Header = r.Header.Clone()
			if r.Header.Get("If-Match") != "" {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
			}

			res, err := transport.RoundTrip(req)
			if err != nil {
				t.Error(err)
				panic(http.ErrAbortHandler)
			}

			if res.StatusCode != 206 || res.ContentLength != end-start+1 {
				closeBody(res.Body)
				t.Errorf("origin response: %d length %d", res.StatusCode, res.ContentLength)
				panic(http.ErrAbortHandler)
			}

			if start == int64(first) {
				for name, values := range res.Header {
					w.Header()[name] = values
				}

				w.Header().Set("Content-Length", strconv.FormatInt(int64(last-first)+1, 10))
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, size))
				w.WriteHeader(206)
			}

			_, err = io.Copy(w, res.Body)
			closeBody(res.Body)

			if err != nil {
				t.Error(err)
				panic(http.ErrAbortHandler)
			}
		}
	})
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

			if size > 2*int64(PageSize) {
				path = clientPeer(t, pageForwarder(t, path, size))
			}

			client := testClient(t, path, 1)

			value, err := client.Get(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(value)

			snapshot := value.Metadata()
			sink := &offsetSink{}

			n, err := value.WriteTo(sink)
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
	client := testClient(t, path, 1)

	var connections []net.Conn

	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { connections = append(connections, info.Conn) }}

	ctx := httptrace.WithClientTrace(context.Background(), trace)
	for range 3 {
		v, err := client.Get(ctx, Request{})
		if err != nil {
			t.Fatal(err)
		}

		if n, err := io.CopyN(io.Discard, v, int64(PageSize)); err != nil || n != int64(PageSize) {
			t.Fatal(n, err)
		}

		closeBody(v)
	}

	if len(connections) != 3 || connections[0] != connections[1] || connections[1] != connections[2] {
		t.Fatal("fully consumed bootstrap connection was not reused")
	}
}

func TestIntegrationCancelBlockedDial(t *testing.T) {
	client := testClient(t, "unused", 1)
	entered, stopped := make(chan struct{}), make(chan struct{})
	client.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(entered)
		<-ctx.Done()
		close(stopped)

		return nil, ctx.Err()
	}
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

func TestIntegrationResponseHeadBoundary(t *testing.T) {
	for _, size := range []int{maxHeadBytes, maxHeadBytes + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			path := socketDir(t) + "/socket"

			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(listener)

			finished := make(chan struct{})

			go func() {
				defer close(finished)

				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer closeBody(conn)

				_, err = readRawHead(bufio.NewReader(conn), false)
				if err != nil {
					return
				}

				fields := "Content-Length: 0\r\nContent-Type: application/octet-stream\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\nX: \r\n"
				padding := strings.Repeat("x", size-len(rawResponse(200, fields)))
				_, _ = conn.Write(rawResponse(200, strings.Replace(fields, "X: ", "X: "+padding, 1)))
			}()

			client := testClient(t, path, 1)

			v, err := client.Get(context.Background(), Request{})
			if size == maxHeadBytes {
				if err != nil {
					t.Fatal("exact limit rejected", err)
				}

				closeBody(v)
			} else {
				assertKind(t, err, ErrorProtocol)
			}

			<-finished
		})
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

			client := testClient(t, path, 1)

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

	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { connections = append(connections, info.Conn) }}

	ctx := httptrace.WithClientTrace(context.Background(), trace)
	for range 2 {
		value, err := client.Get(ctx, Request{})
		if err != nil {
			t.Fatal(err)
		}

		closeBody(value)
	}

	if len(connections) != 2 || connections[0] == connections[1] {
		t.Fatal("unread response connection reused")
	}
}

func TestIntegrationExpiredBootstrapDoesNotCacheVersion(t *testing.T) {
	var calls atomic.Int32

	path, cancel, done := startOrigin(t, OriginConfig{}, func(_ context.Context, request OriginRequest) (Metadata, io.ReadCloser, error) {
		if _, pinned := request.Pin(); pinned || request.Operation() != OperationBootstrap {
			t.Error("fresh Get reused an old pin")
		}

		version := strconv.Itoa(int(calls.Add(1)))
		metadata := originMeta(1)
		metadata.ETag = ETag{value: `"` + version + `"`}

		return metadata, io.NopCloser(strings.NewReader(version)), nil
	})

	defer func() { cancel(); <-done }()

	client := testClient(t, path, 1)
	for _, want := range []string{"1", "2"} {
		value, err := client.Get(context.Background(), Request{})
		if err != nil {
			t.Fatal(err)
		}

		data, err := io.ReadAll(value)
		closeBody(value)

		if err != nil || string(data) != want || value.Metadata().ETag.String() != `"`+want+`"` {
			t.Fatal("fresh read failed to select a new immutable version", err)
		}
	}

	if calls.Load() != 2 {
		t.Fatal("unexpected bootstrap count")
	}
}
