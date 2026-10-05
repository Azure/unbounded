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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStatAdmissionLeaseLifecycle(t *testing.T) {
	for _, action := range []string{"success", "protocol", "context", "client"} {
		t.Run(action, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			defer close(release)

			c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, head []byte) {
				if !strings.HasPrefix(string(head), "HEAD /v2/objects/") {
					t.Error("Stat did not issue HEAD")
				}

				close(entered)
				<-release

				if action == "protocol" {
					_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 1\r\n\r\n")
				} else {
					_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 7\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\n\r\n")
					_, _ = io.Copy(io.Discard, reader)
				}
			})

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			done := make(chan error, 1)

			go func() {
				m, err := c.Stat(ctx, Request{})
				if err == nil && m.Size != 7 {
					t.Error("wrong Stat metadata", m)
				}

				done <- err
			}()

			<-entered
			c.mu.Lock()

			active := len(c.active)
			for lease := range c.active {
				if lease.pool != &c.metadataPool || lease.cleanup != nil {
					t.Error("Stat lease attached value consumption state")
				}
			}
			c.mu.Unlock()

			if active != 1 || len(c.slots) != 0 || c.Stats().ActiveMetadata != 1 {
				t.Fatal("wrong in-flight Stat accounting", active, c.Stats())
			}

			switch action {
			case "context":
				cancel()
			case "client":
				closeBody(c)
			default:
				release <- struct{}{}
			}

			err := <-done

			switch action {
			case "success":
				if err != nil {
					t.Fatal(err)
				}
			case "protocol":
				assertKind(t, err, ErrorProtocol)
			case "context":
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case "client":
				assertKind(t, err, ErrorClosed)
			}

			c.mu.Lock()
			active = len(c.active)
			c.mu.Unlock()

			if active != 0 || c.Stats().ActiveMetadata != 0 || c.Stats().BytesRead != 0 {
				t.Fatal("Stat retained lease or counted body bytes", active, c.Stats())
			}
		})
	}
}

func TestRepeatedByteFillsOnlyDestination(t *testing.T) {
	for _, value := range []byte{0, 'x', 255} {
		for _, size := range []int{0, 1, 2, 3, 7, 31, 32, 33, copyBufferSize - 1, copyBufferSize} {
			buffer := make([]byte, size+2)
			buffer[0], buffer[len(buffer)-1] = 42, 43
			p := buffer[1 : len(buffer)-1]

			n, err := repeatedByte(value).Read(p)
			if n != size || err != nil || buffer[0] != 42 || buffer[len(buffer)-1] != 43 {
				t.Fatalf("value=%d size=%d: count=%d err=%v or guard changed", value, size, n, err)
			}

			for i, got := range p {
				if got != value {
					t.Fatalf("value=%d size=%d: byte[%d]=%d", value, size, i, got)
				}
			}
		}
	}

	if n, err := repeatedByte('x').Read(nil); n != 0 || err != nil {
		t.Fatal(n, err)
	}
}

func TestClientStreamingTranscript(t *testing.T) {
	for _, size := range []int64{0, 1, 4096, int64(PageSize), 2 * int64(PageSize), 3*int64(PageSize) + 1} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			var calls atomic.Int32

			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)

				if r.Method != "POST" || r.Host != "racer" || r.Header.Get("Accept-Encoding") != "" || r.Header.Get("Authorization") != "secret\xff" || r.Header.Get("Racer-Ordered") != "1" {
					t.Error("request envelope")
				}

				first, length := int64(0), size

				if r.Header.Get("If-Match") != "" || r.Header.Get("Range") != "" {
					t.Error("unexpected pin/range")
				}

				streamResponseHead(w, first, length, size, `"v"`)
				_, _ = io.CopyN(w, &offsetStream{offset: first}, length)
			}))
			c := testClient(t, path, 1)

			v, err := c.Get(context.Background(), Request{Context: FetchContext{authorization: Authorization{value: "secret\xff"}}})
			if err != nil {
				t.Fatal(err)
			}

			if v.Metadata().Size != ByteLength(size) || calls.Load() != 1 {
				t.Fatal("metadata or eager continuation")
			}

			n, err := io.Copy(&offsetSink{}, v)
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

			if calls.Load() != want {
				t.Fatal("request count", calls.Load())
			}

			if len(c.slots) != 0 {
				t.Fatal("EOF retained capacity")
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
		"HTTP/1.1 206 Partial Content\r\nContent-Length: 1\r\n",
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
			if !strings.HasSuffix(wire, "\r\n\r\n") {
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatal("truncated head cause lost", err)
				}
			} else {
				assertKind(t, err, ErrorProtocol)
			}

			<-done
		})
	}
}

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

	n, err := io.Copy(shortDestination{}, v)
	if n != 5 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write: %d %v", n, err)
	}
	// The caller owns the Value, including after a destination failure.
	if err := v.Close(); err != nil {
		t.Fatal(err)
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
					_, _ = io.Copy(io.Discard, v)
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
	for _, prefixPages := range []int64{1, 2} {
		t.Run(strconv.FormatInt(prefixPages, 10), func(t *testing.T) {
			for _, mode := range []string{"pin", "size", "range", "length", "412", "503", "short", "empty"} {
				t.Run(mode, func(t *testing.T) {
					var calls atomic.Int32

					first := int64(PageSize)
					size := prefixPages*int64(PageSize) + 2
					remainder := size - first

					path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						calls.Add(1)
						streamResponseHead(w, 0, size, size, `"v"`)
						_, _ = io.CopyN(w, repeatedByte('x'), first)

						switch mode {
						case "pin":
							w.(*subscriptionFixture).err = io.ErrUnexpectedEOF
						case "size":
							w.(*subscriptionFixture).err = io.ErrUnexpectedEOF
						case "range":
							w.(*subscriptionFixture).err = io.ErrUnexpectedEOF
						case "length":
							w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(first, 10)+"-"+strconv.FormatInt(size-1, 10)+"/"+strconv.FormatInt(size, 10))
							w.Header().Set("Content-Length", "1")
							w.Header().Set("Content-Type", "application/octet-stream")
							w.Header().Set("ETag", `"v"`)
							w.Header().Set("Racer-Expires-At", "0")
							w.WriteHeader(206)
							_, _ = w.Write([]byte("x"))
						case "412", "503":
							status, _ := strconv.Atoi(mode)

							w.Header().Set("Content-Length", "0")
							w.WriteHeader(status)
						case "short", "empty":
							streamResponseHead(w, first, remainder, size, `"v"`)

							if mode == "short" {
								_, _ = w.Write([]byte("x"))
							}
						}
					}))
					c := testClient(t, path, 1)

					v, err := c.Get(context.Background(), Request{})
					if err != nil {
						t.Fatal(err)
					}

					n, err := io.Copy(io.Discard, v)
					if err == nil || n != first || calls.Load() != 1 {
						t.Fatalf("failure %d %v", n, err)
					}

					if !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatal("post-header failures must truncate", err)
					}

					if mode != "short" && n != first {
						t.Fatal("invalid response bytes exposed", n)
					}

					if next, terminal := v.Read(make([]byte, 1)); next != 0 || terminal != err || calls.Load() != 1 {
						t.Fatal("failure was not terminal", next, terminal)
					}

					if len(c.slots) != 0 {
						t.Fatal("failure retained capacity")
					}

					if v.Metadata().ExpiresAt.UnixMilli() != 0 {
						t.Fatal("snapshot changed")
					}
				})
			}
		})
	}
}

func TestClientContinuationCloseAndContext(t *testing.T) {
	for _, closeClient := range []bool{false, true} {
		t.Run(strconv.FormatBool(closeClient), func(t *testing.T) {
			entered := make(chan struct{})
			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				streamResponseHead(w, 0, int64(PageSize)+1, int64(PageSize)+1, `"v"`)
				_, _ = io.CopyN(w, repeatedByte('x'), int64(PageSize))

				close(entered)
				<-r.Context().Done()
			}))
			c := testClient(t, path, 1)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			v, err := c.Get(ctx, Request{})
			if err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)

			go func() { _, err := io.Copy(io.Discard, v); done <- err }()

			<-entered

			if closeClient {
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}

			select {
			case err := <-done:
				if closeClient {
					assertKind(t, err, ErrorClosed)
				} else if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("continuation blocked")
			}
		})
	}
}

func TestClientConfigAndValidation(t *testing.T) {
	for _, config := range []ClientConfig{{Cache: CacheName{value: "test"}, MetadataConnections: -1}, {Cache: CacheName{value: "test"}, MaxQueuedRequests: -1}, {Cache: CacheName{value: "test"}, QueueTimeout: -1}} {
		_, err := NewClient(config)
		assertKind(t, err, ErrorInvalidArgument)
	}

	for _, config := range []ClientConfig{{}, {Cache: CacheName{value: "test"}, MaxConnections: -1}, {Cache: CacheName{value: "test"}, DialTimeout: -1}, {Cache: CacheName{value: "test"}, ResponseHeaderTimeout: -1}, {Cache: CacheName{value: "test"}, IdleConnTimeout: -1}} {
		_, err := NewClient(config)
		assertKind(t, err, ErrorInvalidArgument)
	}

	c := testClient(t, filepath.Join(socketDir(t), "missing"), 1)
	_, err := c.Get(nil, Request{}) //nolint:staticcheck // Exercise the public nil-context validation contract.
	assertKind(t, err, ErrorInvalidArgument)
	_, err = c.Get(context.Background(), Request{Context: FetchContext{authorization: Authorization{value: "bad\nvalue"}}})
	assertKind(t, err, ErrorInvalidArgument)
	_, err = c.Get(context.Background(), Request{})
	assertKind(t, err, ErrorIO)

	var value Value

	_, err = value.Read(make([]byte, 1))
	assertKind(t, err, ErrorClosed)
}

func TestClientHeaderTimeout(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))

	c, err := newClient(ClientConfig{Cache: CacheName{value: "test"}, ResponseHeaderTimeout: 20 * time.Millisecond}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(c)

	_, err = c.Get(context.Background(), Request{})
	if err == nil {
		t.Fatal("header timeout ignored")
	}

	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatal("timeout cause lost", err)
	}
}

func TestClientConcurrentCloseWaitsForCleanup(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { streamResponse(w, 0, 1, 1, `"v"`) }))
	for range 20 {
		c := testClient(t, path, 1)

		v, err := c.Get(context.Background(), Request{})
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Go(func() { _ = v.Close() })
		wg.Go(func() { _ = c.Close() })

		if err := c.Close(); err != nil {
			t.Fatal(err)
		}

		if len(c.slots) != 0 {
			t.Error("Close returned before capacity release")
		}

		wg.Wait()
	}
}

func TestClientQueueBoundsAndMetadataReservation(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", "1")
			w.Header().Set("ETag", `"v"`)
			w.Header().Set("Racer-Expires-At", "0")

			return
		}

		streamResponse(w, 0, 1, 1, `"v"`)
	}))

	c, err := newClient(ClientConfig{Cache: CacheName{value: "test"}, MaxConnections: 1, MetadataConnections: 1, MaxQueuedRequests: 1, QueueTimeout: 100 * time.Millisecond}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(c)

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	for _, cancelWait := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)

		go func() { _, err := c.Get(ctx, Request{}); result <- err }()

		until := time.Now().Add(time.Second)
		for len(c.queued) != 1 && time.Now().Before(until) {
			time.Sleep(time.Millisecond)
		}

		if len(c.queued) != 1 {
			t.Fatal("waiter not admitted")
		}

		c.mu.Lock()
		active := len(c.active)
		c.mu.Unlock()

		if active != 1 {
			t.Fatal("queued request allocated active state")
		}

		_, err := c.Get(context.Background(), Request{})
		assertKind(t, err, ErrorUnavailable)

		m, err := c.Stat(context.Background(), Request{})
		if err != nil || m.Size != 1 {
			t.Fatal("bulk queue starved reserved metadata", err)
		}

		if cancelWait {
			cancel()
		}

		err = <-result
		if cancelWait {
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		} else {
			assertKind(t, err, ErrorDeadline)
		}

		cancel()

		if len(c.queued) != 0 {
			t.Fatal("queue slot leaked")
		}
	}

	closeBody(v)

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			v, err := c.Get(context.Background(), Request{})
			if err != nil {
				var typed *Error
				if !errors.As(err, &typed) || typed.Kind() != ErrorUnavailable {
					t.Error(err)
				}

				return
			}
			defer closeBody(v)

			if n, err := io.Copy(io.Discard, v); err != nil || n != 1 {
				t.Error(n, err)
			}
		})
	}

	wg.Wait()

	if len(c.slots) != 0 || len(c.metadataPool.slots) != 0 || len(c.queued) != 0 {
		t.Fatal("concurrent calls leaked admission")
	}
}

func TestClientReservedPoolFiniteAdmissionAndCancellation(t *testing.T) {
	entered := make(chan struct{}, 2)
	path := clientPeer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}

		<-r.Context().Done()
	}))

	c, err := newClient(ClientConfig{Cache: CacheName{value: "test"}, MetadataConnections: 2, MetadataQueuedRequests: 3}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(c)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	results := make(chan error, 5)

	for range 2 {
		go func() { _, err := c.Stat(ctx, Request{}); results <- err }()
	}

	for range 2 {
		<-entered
	}

	for range 3 {
		go func() { _, err := c.Stat(ctx, Request{}); results <- err }()
	}

	deadline := time.Now().Add(time.Second)
	for len(c.metadataPool.queued) != 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if len(c.metadataPool.queued) != 3 || len(c.metadataPool.slots) != 2 {
		t.Fatal("metadata admission not bounded")
	}

	_, err = c.Stat(context.Background(), Request{})
	assertKind(t, err, ErrorUnavailable)
	cancel()

	for range 5 {
		select {
		case err := <-results:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("Stat cancellation blocked")
		}
	}

	if len(c.metadataPool.slots) != 0 || len(c.metadataPool.queued) != 0 {
		t.Fatal("Stat retained admission")
	}
}

func TestIndependentQueuesAndSmallObjectAdmission(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})

	var heads atomic.Int32

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			if heads.Add(1) == 1 {
				close(entered)

				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}

			w.Header().Set("Content-Length", "1")
			w.Header().Set("ETag", `"v"`)
			w.Header().Set("Racer-Expires-At", "0")

			return
		}

		streamResponse(w, 0, 1, 1, `"v"`)
	}))

	c, err := newClient(ClientConfig{Cache: CacheName{value: "test"}, MaxConnections: 1, MaxQueuedRequests: 1, MetadataConnections: 1, MetadataQueuedRequests: 1, SmallObjectConnections: 1, SmallObjectQueuedRequests: 1}, path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(c)

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bulkDone := make(chan error, 1)

	go func() { _, err := c.Get(ctx, Request{}); bulkDone <- err }()

	waitDepth := func(want int) {
		t.Helper()

		until := time.Now().Add(time.Second)
		for c.Stats().QueueDepth != want && time.Now().Before(until) {
			time.Sleep(time.Millisecond)
		}

		if c.Stats().QueueDepth != want {
			t.Fatal("queue depth", c.Stats())
		}
	}
	waitDepth(1)

	headDone := make(chan error, 2)

	go func() { _, err := c.Stat(ctx, Request{}); headDone <- err }()

	<-entered

	go func() { _, err := c.Stat(ctx, Request{}); headDone <- err }()

	waitDepth(2)

	small, err := c.Get(ctx, Request{}, ReadOptions{SmallObject: true})
	if err != nil {
		t.Fatal("bulk/HEAD saturation blocked small GET", err)
	}
	defer closeBody(small)

	smallDone := make(chan error, 1)

	go func() { _, err := c.Get(ctx, Request{}, ReadOptions{SmallObject: true}); smallDone <- err }()

	waitDepth(3)

	s := c.Stats()
	if s.BulkQueueDepth != 1 || s.MetadataQueueDepth != 1 || s.SmallObjectQueueDepth != 1 || s.ActiveSmallObjects != 1 || s.Connections != 3 {
		t.Fatal(s)
	}

	_, err = c.Get(ctx, Request{}, ReadOptions{SmallObject: true})
	assertKind(t, err, ErrorUnavailable)
	close(release)

	for range 2 {
		if err := <-headDone; err != nil {
			t.Fatal("reserved metadata queue rejected HEAD", err)
		}
	}

	cancel()

	for _, done := range []chan error{bulkDone, smallDone} {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}

	closeBody(small)
	closeBody(v)

	if s := c.Stats(); s.QueueDepth != 0 || s.ActiveBulk != 0 || s.ActiveMetadata != 0 || s.ActiveSmallObjects != 0 {
		t.Fatal(s)
	}
}

func TestReservedAdmissionConfigValidation(t *testing.T) {
	for _, config := range []ClientConfig{{SmallObjectConnections: -1}, {SmallObjectQueuedRequests: -1}, {MetadataQueuedRequests: -1}} {
		config.Cache = CacheName{value: "test"}
		_, err := NewClient(config)
		assertKind(t, err, ErrorInvalidArgument)
	}

	_, err := (OriginConfig{Cache: CacheName{value: "test"}, MaxConcurrentHeadRequests: -1}).defaults()
	assertKind(t, err, ErrorInvalidArgument)
}

func TestSmallObjectDefaultQueueAcceptsSynchronizedBurst(t *testing.T) {
	release := make(chan struct{})
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}

		streamResponse(w, 0, 1, 1, `"v"`)
	}))

	c := testClient(t, path, 1)
	if cap(c.smallPool.slots) != 4 || cap(c.smallPool.queued) != 128 {
		t.Fatal("unexpected small-object defaults", c.config)
	}

	start := make(chan struct{})
	results := make(chan error, 64)

	for range 64 {
		go func() {
			<-start

			v, err := c.Get(context.Background(), Request{}, ReadOptions{SmallObject: true})
			if err == nil {
				_, err = v.WriteTo(io.Discard)
				closeBody(v)
			}

			results <- err
		}()
	}

	close(start)

	deadline := time.Now().Add(3 * time.Second)
	for c.Stats().SmallObjectQueueDepth != 60 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	s := c.Stats()

	close(release)

	if s.ActiveSmallObjects != 4 || s.SmallObjectQueueDepth != 60 || s.QueueRejections != 0 {
		t.Error("burst was not bounded and queued", s)
	}

	for range 64 {
		select {
		case err := <-results:
			if err != nil {
				t.Error("burst request failed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("burst did not complete")
		}
	}

	if s := c.Stats(); s.ActiveSmallObjects != 0 || s.SmallObjectQueueDepth != 0 || s.QueueRejections != 0 || s.BytesRead != 64 {
		t.Fatal("burst leaked admission or lost bytes", s)
	}
}

func TestContentTypeExactCompatibilityAndRawWhitespace(t *testing.T) {
	for _, initial := range []string{"", "text/plain"} {
		for _, current := range []string{"", "text/plain", "application/json"} {
			m := originMeta(3)
			m.ContentType = initial
			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if current != "" {
					w.Header().Set("Racer-Content-Type", current)
				}

				streamResponse(w, 0, 3, 3, `"v"`)
			}))
			c := testClient(t, path, 1)

			v, err := c.Get(context.Background(), Request{}, ReadOptions{Metadata: &m})
			if initial != current {
				assertKind(t, err, ErrorProtocol)
				continue
			}

			if err != nil {
				t.Fatal(initial, current, err)
			}

			if n, err := v.WriteTo(io.Discard); err != nil || n != 3 {
				t.Fatal(n, err)
			}

			closeBody(v)

			if v.Metadata() != m {
				t.Fatal("initial metadata changed")
			}
		}
	}

	r := OriginRequest{operation: OperationBootstrap, byteRange: bootstrapRange()}

	for _, value := range []string{"text/plain", "  text/plain", "\ttext/plain", " text/plain ", " text/plain\t", " text/plain;\tcharset=utf-8"} {
		head := rawResponse(200, "Content-Length: 0\r\nContent-Type: application/octet-stream\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\nRacer-Content-Type:"+value+"\r\n")
		if _, err := parseResponseHead(head, r, nil); err == nil {
			t.Fatal("normalized invalid raw MIME", value)
		}
	}

	for _, value := range []string{" text/plain", " text/plain; charset=utf-8", " text/plain; x=\"a b\""} {
		head := rawResponse(200, "Content-Length: 0\r\nContent-Type: application/octet-stream\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\nRacer-Content-Type:"+value+"\r\n")

		result, err := parseResponseHead(head, r, nil)
		if err != nil || result.metadata.ContentType != strings.TrimPrefix(value, " ") {
			t.Fatal(value, err)
		}
	}
}
