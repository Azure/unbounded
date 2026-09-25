// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Keep socket and test scratch paths inside this worktree, including under race.
func socketDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("../../tmp", "sdk-")
	if err != nil {
		t.Fatal(err)
	}

	path, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := os.RemoveAll(path); err != nil {
			t.Error(err)
		}
	})

	return path
}

func testClient(t *testing.T, path string, maxConn int) *Client {
	t.Helper()

	cache, err := ParseCacheName("test")
	if err != nil {
		t.Fatal(err)
	}

	c, err := newClient(ClientConfig{Cache: cache, MaxConnections: maxConn}, path)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})

	return c
}

func clientPeer(t *testing.T, handler http.Handler) string {
	t.Helper()
	path := filepath.Join(socketDir(t), "socket")

	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}

	s := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	done := make(chan struct{})

	go func() { defer close(done); _ = s.Serve(l) }()

	t.Cleanup(func() { _ = s.Close(); <-done })

	return path
}

type repeatedByte byte

func (b repeatedByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}

	return len(p), nil
}

func streamResponse(w http.ResponseWriter, first, length, size int64, tag string) {
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", tag)
	w.Header().Set("Racer-Expires-At", "0")

	if length != 0 {
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(first, 10)+"-"+strconv.FormatInt(first+length-1, 10)+"/"+strconv.FormatInt(size, 10))
		w.WriteHeader(206)
	}

	_, _ = io.CopyN(w, repeatedByte('x'), length)
}

func TestClientStreamingTranscript(t *testing.T) {
	for _, size := range []int64{0, 1, 4096, int64(PageSize), 3*int64(PageSize) + 1} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			var calls atomic.Int32

			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)

				if r.Method != "GET" || r.Host != "racer" || r.Header.Get("Accept-Encoding") != "" || r.Header.Get("Authorization") != "secret\xff" {
					t.Error("request envelope")
				}

				first, length := int64(0), min(size, int64(PageSize))

				if call == 1 {
					if r.Header.Get("If-Match") != "" || r.Header.Get("Range") != "bytes=0-16777215" {
						t.Error("bootstrap")
					}
				} else {
					first, length = int64(PageSize), size-int64(PageSize)
					if call != 2 || r.Header.Get("If-Match") != `"v"` || r.Header.Get("Range") != "bytes=16777216-"+strconv.FormatInt(size-1, 10) {
						t.Error("continuation")
					}
				}

				streamResponse(w, first, length, size, `"v"`)
			}))
			c := testClient(t, path, 1)

			v, err := c.Get(context.Background(), Request{Context: FetchContext{authorization: Authorization{value: "secret\xff"}}})
			if err != nil {
				t.Fatal(err)
			}

			if v.Metadata().Size != ByteLength(size) || calls.Load() != 1 {
				t.Fatal("metadata or eager continuation")
			}

			n, err := v.WriteTo(io.Discard)
			if err != nil || n != size {
				t.Fatalf("stream: %d %v", n, err)
			}

			if err := v.Close(); err != nil {
				t.Fatal(err)
			}

			if _, err := v.Read(make([]byte, 1)); err != io.EOF {
				t.Fatal("EOF lost", err)
			}

			want := int32(1)
			if size > int64(PageSize) {
				want = 2
			}

			if calls.Load() != want {
				t.Fatal("request count", calls.Load())
			}
		})
	}
}

func TestClientHeadersWithoutBodyAndClose(t *testing.T) {
	arrived := make(chan struct{})
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "16777216")
		w.Header().Set("Content-Range", "bytes 0-16777215/16777217")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", `"v"`)
		w.Header().Set("Racer-Expires-At", "0")
		w.WriteHeader(206)

		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}

		close(arrived)
		<-r.Context().Done()
	}))
	c := testClient(t, path, 1)

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	<-arrived

	readDone := make(chan error, 1)

	go func() { _, err := v.Read(make([]byte, 1)); readDone <- err }()

	getDone := make(chan error, 1)

	go func() { _, err := c.Get(context.Background(), Request{}); getDone <- err }()

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	for _, done := range []chan error{readDone, getDone} {
		select {
		case err := <-done:
			assertKind(t, err, ErrorClosed)
		case <-time.After(3 * time.Second):
			t.Fatal("close blocked")
		}
	}
}

func TestClientPendingHeadersCanceled(t *testing.T) {
	arrived := make(chan struct{})
	path := clientPeer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { close(arrived); <-r.Context().Done() }))
	c := testClient(t, path, 1)
	done := make(chan error, 1)

	go func() { _, err := c.Get(context.Background(), Request{}); done <- err }()

	<-arrived

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		assertKind(t, err, ErrorClosed)
	case <-time.After(3 * time.Second):
		t.Fatal("pending Get not canceled")
	}
}

func TestClientRawResponses(t *testing.T) {
	valid := string(rawResponse(206, "Content-Length: 1\r\nContent-Range: bytes 0-0/1\r\nContent-Type: application/octet-stream\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\n"))
	for _, wire := range []string{
		strings.Replace(valid, "Content-Length: 1", "Content-Length: 1\r\nContent-Length: 1", 1),
		strings.Replace(valid, "Content-Length: 1", "Content-Length: 1\r\nTransfer-Encoding: chunked", 1),
		strings.Replace(valid, "Content-Length: 1", "Content-Length: 1\r\nContent-Encoding: identity", 1),
		string(rawResponse(100, "")) + valid,
		string(rawResponse(302, "Content-Length: 0\r\nLocation: http://example.com/\r\n")),
		strings.Replace(valid, "ETag: \"v\"", "ETag: W/\"v\"", 1),
		strings.Replace(valid, "Content-Length: 1", "X: "+strings.Repeat("x", maxHeadBytes)+"\r\nContent-Length: 1", 1),
	} {
		t.Run(strconv.Itoa(len(wire))+wire[:12], func(t *testing.T) {
			path := filepath.Join(socketDir(t), "socket")

			l, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(l)

			done := make(chan struct{})

			go func() {
				defer close(done)

				conn, err := l.Accept()
				if err != nil {
					return
				}
				defer closeBody(conn)

				if _, err := readRawHead(bufio.NewReader(conn), false); err != nil {
					return
				}

				_, _ = io.WriteString(conn, wire+"x")
			}()

			c := testClient(t, path, 1)
			_, err = c.Get(context.Background(), Request{})
			assertKind(t, err, ErrorProtocol)
			<-done
		})
	}
}

type shortDestination struct{}

func (shortDestination) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestClientFailuresAndCapacity(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { streamResponse(w, 0, 10, 10, `"v"`) }))
	c := testClient(t, path, 1)

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = c.Get(ctx, Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	n, err := v.WriteTo(shortDestination{})
	if n != 5 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write: %d %v", n, err)
	}

	v, err = c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal("capacity leaked", err)
	}

	if err := v.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = v.Read(make([]byte, 1))
	assertKind(t, err, ErrorClosed)
}

func TestClientGetCloseRace(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { streamResponse(w, 0, 1, 1, `"v"`) }))
	for range 30 {
		c := testClient(t, path, 2)

		var wg sync.WaitGroup
		for range 5 {
			wg.Go(func() {
				v, err := c.Get(context.Background(), Request{})
				if err == nil {
					_, _ = v.WriteTo(io.Discard)
					_ = v.Close()
				}
			})
		}

		if err := c.Close(); err != nil {
			t.Fatal(err)
		}

		wg.Wait()
		c.mu.Lock()
		count := len(c.active)
		c.mu.Unlock()

		if count != 0 || len(c.slots) != 0 {
			t.Fatal("live resources after close")
		}
	}
}

func TestClientContinuationFailures(t *testing.T) {
	for _, mode := range []string{"pin", "size", "412", "503", "short"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32

			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					streamResponse(w, 0, int64(PageSize), int64(PageSize)+2, `"v"`)
					return
				}

				switch mode {
				case "pin":
					streamResponse(w, int64(PageSize), 2, int64(PageSize)+2, `"other"`)
				case "size":
					streamResponse(w, int64(PageSize), 2, int64(PageSize)+3, `"v"`)
				case "412", "503":
					status, _ := strconv.Atoi(mode)

					w.Header().Set("Content-Length", "0")
					w.WriteHeader(status)
				case "short":
					w.Header().Set("Content-Length", "2")
					w.Header().Set("Content-Range", "bytes 16777216-16777217/16777218")
					w.Header().Set("Content-Type", "application/octet-stream")
					w.Header().Set("ETag", `"v"`)
					w.Header().Set("Racer-Expires-At", "1")
					w.WriteHeader(206)
					_, _ = w.Write([]byte("x"))
				}
			}))
			c := testClient(t, path, 1)

			v, err := c.Get(context.Background(), Request{})
			if err != nil {
				t.Fatal(err)
			}

			n, err := v.WriteTo(io.Discard)
			if err == nil || n < int64(PageSize) || calls.Load() != 2 {
				t.Fatalf("failure %d %v", n, err)
			}

			switch mode {
			case "pin", "size":
				assertKind(t, err, ErrorProtocol)
			case "412":
				assertKind(t, err, ErrorVersionUnavailable)
			case "503":
				assertKind(t, err, ErrorUnavailable)
			case "short":
				if n != int64(PageSize)+1 || !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatal("truncation", n, err)
				}
			}

			if v.Metadata().ExpiresAt.UnixMilli() != 0 {
				t.Fatal("snapshot changed")
			}
		})
	}
}
