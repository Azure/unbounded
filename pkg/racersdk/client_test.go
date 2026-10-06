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
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
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
						case "pin", "size", "range":
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

// A peer that abandons a subscription while a credit release is still unread
// in its receive queue resets the socket instead of closing it cleanly. That
// is still post-header truncation, on both the Read and buffered HTTP paths.
func TestClientPeerResetAfterReleaseIsTruncation(t *testing.T) {
	for _, path := range []string{"read", "http"} {
		t.Run(path, func(t *testing.T) {
			size := uint64(PageSize) + 2
			socket := filepath.Join(socketDir(t), "socket")

			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}

			served := make(chan struct{})

			t.Cleanup(func() { closeBody(listener); <-served })

			go func() {
				defer close(served)

				conn, err := listener.Accept()
				if err != nil {
					t.Error(err)
					return
				}
				defer closeBody(conn)

				if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
					t.Error(err)
					return
				}

				if _, err := io.WriteString(conn, subscriptionHead(size, 0, size)); err != nil {
					t.Error(err)
					return
				}

				if err := fakeSubscriptionFrame(conn, 1, 0, 0, uint32(PageSize)); err != nil {
					t.Error(err)
					return
				}

				if _, err := io.CopyN(conn, repeatedByte('x'), int64(PageSize)); err != nil {
					t.Error(err)
					return
				}

				// Wait until the release is queued, without consuming it, so the
				// close below deterministically resets the client side.
				if err := peekCredit(conn.(*net.UnixConn)); err != nil {
					t.Error(err)
				}
			}()

			c := testClient(t, socket, 1)

			var n int64

			if path == "read" {
				v, getErr := c.Get(context.Background(), Request{})
				if getErr != nil {
					t.Fatal(getErr)
				}

				n, err = io.Copy(io.Discard, v)
			} else {
				v, getErr := c.GetStreaming(context.Background(), Request{})
				if getErr != nil {
					t.Fatal(getErr)
				}

				n, err = v.WriteToHTTP(httptest.NewRecorder())
			}

			if n != int64(PageSize) {
				t.Fatal("prefix bytes", n, err)
			}

			assertKind(t, err, ErrorIO)

			if !errors.Is(err, io.ErrUnexpectedEOF) || !errors.Is(err, syscall.ECONNRESET) {
				t.Fatalf("reset must be truncation with its cause: %#v", errors.Unwrap(err))
			}
		})
	}
}

// peekCredit blocks until one 12-byte credit release is readable, leaving it
// queued.
func peekCredit(conn *net.UnixConn) error {
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}

	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}

	var peekErr error

	buffer := make([]byte, wire.CreditSize)

	err = raw.Read(func(fd uintptr) bool {
		n, _, err := syscall.Recvfrom(int(fd), buffer, syscall.MSG_PEEK)
		if errors.Is(err, syscall.EAGAIN) || err == nil && n > 0 && n < len(buffer) {
			return false
		}

		if err == nil && n == 0 {
			err = io.ErrUnexpectedEOF
		}

		peekErr = err

		return true
	})
	if err != nil {
		return err
	}

	return peekErr
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
	for _, tt := range []struct {
		name   string
		config ClientConfig
	}{
		{"volume", ClientConfig{}},
		{"metadata-connections", ClientConfig{MetadataConnections: -1}},
		{"queued-requests", ClientConfig{MaxQueuedRequests: -1}},
		{"queue-timeout", ClientConfig{QueueTimeout: -1}},
		{"connections", ClientConfig{MaxConnections: -1}},
		{"dial-timeout", ClientConfig{DialTimeout: -1}},
		{"header-timeout", ClientConfig{ResponseHeaderTimeout: -1}},
		{"idle-timeout", ClientConfig{IdleConnTimeout: -1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name != "volume" {
				tt.config.Volume = VolumeName{value: "test"}
			}

			_, err := NewClient(tt.config)
			assertKind(t, err, ErrorInvalidArgument)
		})
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

	c, err := newClient(ClientConfig{Volume: VolumeName{value: "test"}, ResponseHeaderTimeout: 20 * time.Millisecond}, path)
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

func TestRangeBounds(t *testing.T) {
	if first, last, present := (Range{}).Bounds(); first != 0 || last != 0 || present {
		t.Fatal(first, last, present)
	}

	r, err := ClosedRange(7, 29)
	if err != nil {
		t.Fatal(err)
	}

	if first, last, present := r.Bounds(); first != 7 || last != 29 || !present {
		t.Fatal(first, last, present)
	}
}

func TestReadOptionsSnapshot(t *testing.T) {
	for _, explicitPin := range []bool{false, true} {
		t.Run(map[bool]string{false: "implicit", true: "explicit"}[explicitPin], func(t *testing.T) {
			var gets atomic.Int32

			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gets.Add(1)

				if r.Method != "POST" || r.Header.Get("If-Match") != `"v"` || r.Header.Get("Range") != "bytes=1-" {
					t.Error("snapshot did not skip HEAD or pin exact range")
				}

				w.Header().Set("Racer-Content-Type", "text/plain")
				streamResponse(w, 1, 2, 3, `"v"`)
			}))
			c := testClient(t, path, 1)
			m := originMeta(3)
			m.ContentType = "text/plain"
			original := m

			o := ReadOptions{Offset: 1, Metadata: &m}
			if explicitPin {
				o.Pin = m.ETag
			}

			v, err := c.Get(context.Background(), Request{}, o)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(v)

			m = Metadata{} // The admitted stream owns a copy, including its pin.

			if n, err := io.Copy(io.Discard, v); err != nil || n != 2 {
				t.Fatal(n, err)
			}

			if v.Metadata() != original || gets.Load() != 1 {
				t.Fatal("snapshot changed or extra request")
			}
		})
	}
}

func TestReadOptionsInvalidSnapshotDoesNotFetch(t *testing.T) {
	c := testClient(t, "unused", 1)
	poolConfig := c.bulk.config
	poolConfig.Dial = func(context.Context, string, string) (net.Conn, error) {
		t.Error("invalid snapshot performed I/O")
		return nil, errors.New("unexpected dial")
	}
	c.configurePools(poolConfig)

	for _, tt := range []struct {
		name       string
		invalidate func(*Metadata)
	}{
		{"zero", func(m *Metadata) { *m = Metadata{} }},
		{"size", func(m *Metadata) { m.Size = ByteLength(math.MaxUint64) }},
		{"etag", func(m *Metadata) { m.ETag = ETag{} }},
		{"content-type", func(m *Metadata) { m.ContentType = "text/plain\r\nx: y" }},
		{"expiration", func(m *Metadata) { m.ExpiresAt = time.Unix(0, 1) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := originMeta(3)
			tt.invalidate(&m)
			_, err := c.Get(context.Background(), Request{}, ReadOptions{Metadata: &m})
			assertKind(t, err, ErrorInvalidArgument)
		})
	}

	valid := originMeta(3)
	_, err := c.Get(context.Background(), Request{}, ReadOptions{Metadata: &valid, Pin: ETag{value: `"other"`}})
	assertKind(t, err, ErrorInvalidArgument)
	_, err = c.Get(context.Background(), Request{}, ReadOptions{Metadata: &valid, Offset: 4})
	assertKind(t, err, ErrorUnsatisfiableRange)

	empty := originMeta(0)
	c = testClient(t, clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Match") != empty.ETag.String() {
			t.Error("empty snapshot pin lost")
		}

		streamResponse(w, 0, 0, 0, `"v"`)
	})), 1)

	v, err := c.Get(context.Background(), Request{}, ReadOptions{Metadata: &empty})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	if n, err := v.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatal(n, err)
	}

	if stats := c.Stats(); stats.Dials != 1 || stats.ActiveBulk != 0 || stats.ActiveMetadata != 0 {
		t.Fatal(stats)
	}
}

func TestClientStats(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", "3")
			w.Header().Set("ETag", `"v"`)
			w.Header().Set("Racer-Expires-At", "0")

			return
		}

		streamResponse(w, 0, 3, 3, `"v"`)
	}))

	c, err := newClient(ClientConfig{Volume: VolumeName{value: "test"}, MaxConnections: 1, MaxQueuedRequests: 1, QueueTimeout: 100 * time.Millisecond}, path)
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

	done := make(chan error, 1)

	go func() { _, err := c.Get(ctx, Request{}); done <- err }()

	until := time.Now().Add(time.Second)
	for c.Stats().QueueDepth != 1 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}

	_, err = c.Get(context.Background(), Request{})
	assertKind(t, err, ErrorUnavailable)

	if _, err := c.Stat(context.Background(), Request{}); err != nil {
		t.Fatal(err)
	}

	s := c.Stats()
	if s.QueueDepth != 1 || s.QueueWaits != 1 || s.QueueRejections != 1 || s.ActiveBulk != 1 || s.ActiveMetadata != 0 || s.Connections != 2 || s.IdleConnections != 1 || s.Dials != 2 {
		t.Fatal(s)
	}

	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	_, err = c.Get(context.Background(), Request{})
	assertKind(t, err, ErrorDeadline)

	if _, err := io.Copy(io.Discard, v); err != nil {
		t.Fatal(err)
	}

	v, err = c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := io.Copy(io.Discard, v); err != nil {
		t.Fatal(err)
	}

	closeBody(v)

	s = c.Stats()
	if s.QueueDepth != 0 || s.QueueWaits != 2 || s.QueueTimeouts != 1 || s.QueueWaitNanoseconds == 0 || s.ConnectionReuses != 0 || s.BytesRead != 6 || s.ActiveBulk != 0 || s.Retries != 0 {
		t.Fatal(s)
	}

	closeBody(c)

	s = c.Stats()
	if s.Connections != 0 || s.IdleConnections != 0 || s.Dials != 3 || s.BytesRead != 6 {
		t.Fatal("cleanup lost counters or retained gauges", s)
	}
}

// A scripted peer warms one pooled lease, then fails its next exchange. A retry
// is allowed only for an empty EOF/reset, never after any response prefix.
func TestClientStaleRetryBoundary(t *testing.T) {
	for _, method := range []string{"POST", "HEAD"} {
		for _, mode := range []string{"stale", "partial", "malformed", "timeout", "twice", "fresh", "canceled"} {
			t.Run(method+"/"+mode, func(t *testing.T) {
				path := socketDir(t) + "/socket"

				listener, err := net.Listen("unix", path)
				if err != nil {
					t.Fatal(err)
				}
				defer closeBody(listener)

				c, err := newClient(ClientConfig{Volume: VolumeName{value: "test"}, MaxConnections: 1, ResponseHeaderTimeout: 40 * time.Millisecond}, path)
				if err != nil {
					t.Fatal(err)
				}
				defer closeBody(c)

				entered := make(chan struct{})
				serverDone := make(chan struct{})

				var exchanges atomic.Int32

				go func() {
					defer close(serverDone)

					conn, err := listener.Accept()
					if err != nil {
						return
					}
					defer closeBody(conn)

					r := bufio.NewReader(conn)
					respond := func(conn net.Conn) {
						if method == "HEAD" {
							_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 1\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\n\r\n")
							return
						}

						_, _ = io.WriteString(conn, subscriptionHead(1, 0, 1))
						_ = fakeSubscriptionFrame(conn, 1, 0, 0, 1)
						_, _ = io.WriteString(conn, "x")
						_ = fakeSubscriptionFrame(conn, 2, 1, 1, 0)
					}

					if mode != "fresh" {
						if _, err := readRawHead(r, false); err != nil {
							return
						}

						exchanges.Add(1)
						respond(conn)

						if method == "POST" {
							closeBody(conn)

							conn, err = listener.Accept()
							if err != nil {
								return
							}
							defer closeBody(conn)

							r = bufio.NewReader(conn)
						}
					}

					if _, err := readRawHead(r, false); err != nil {
						return
					}

					exchanges.Add(1)
					close(entered)

					switch mode {
					case "partial":
						_, _ = io.WriteString(conn, "H")
					case "malformed":
						_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nContent-Length: 0\r\n\r\n")
					case "timeout", "canceled":
						_, _ = r.ReadByte()
						return
					}

					closeBody(conn)

					if method == "HEAD" && (mode == "stale" || mode == "twice") {
						conn, err := listener.Accept()
						if err != nil {
							return
						}
						defer closeBody(conn)

						if _, err := readRawHead(bufio.NewReader(conn), false); err != nil {
							return
						}

						exchanges.Add(1)

						if mode == "stale" {
							respond(conn)
						}
					}
				}()

				read := func(ctx context.Context) error {
					if method == "HEAD" {
						_, err := c.Stat(ctx, Request{})
						return err
					}

					v, err := c.Get(ctx, Request{})
					if err != nil {
						return err
					}
					defer closeBody(v)

					_, err = io.Copy(io.Discard, v)

					return err
				}
				if mode != "fresh" {
					if err := read(context.Background()); err != nil {
						t.Fatal(err)
					}
				}

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				if mode == "canceled" {
					go func() { <-entered; cancel() }()
				}

				err = read(ctx)
				if method == "HEAD" && mode == "stale" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil {
					t.Fatal("failed exchange succeeded")
				}

				if mode == "partial" && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatal(err)
				}

				if mode == "malformed" {
					assertKind(t, err, ErrorProtocol)
				}

				if mode == "canceled" && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}

				closeBody(listener)
				<-serverDone

				want := uint64(0)
				if method == "HEAD" && (mode == "stale" || mode == "twice") {
					want = 1
				}

				wantDials := 1 + want
				if method == "POST" && mode != "fresh" {
					wantDials = 2
				}

				if s := c.Stats(); s.Retries != want || s.Dials != wantDials {
					t.Fatal("retry boundary", s)
				}

				if want == 1 && exchanges.Load() != 3 {
					t.Fatal("retry was not exactly once")
				}
			})
		}
	}
}

func TestReadOptionsSnapshotResponseMismatch(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { streamResponse(w, 0, 3, 3, `"different"`) }))
	c := testClient(t, path, 1)
	m := originMeta(3)
	_, err := c.Get(context.Background(), Request{}, ReadOptions{Metadata: &m})
	assertKind(t, err, ErrorProtocol)

	if s := c.Stats(); s.BytesRead != 0 || s.Retries != 0 || s.Connections != 0 {
		t.Fatal(s)
	}
}

func TestClientStaleIdlePinnedRetryPreservesRequest(t *testing.T) {
	path := socketDir(t) + "/socket"

	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(l)

	if err := l.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}

	closed, done := make(chan struct{}), make(chan struct{})

	go func() {
		defer close(done)

		for i := range 2 {
			conn, err := l.Accept()
			if err != nil {
				t.Error(err)
				return
			}

			head, err := readRawHead(bufio.NewReader(conn), false)
			if err != nil {
				closeBody(conn)
				t.Error(err)

				return
			}

			h := headHeaders(head)
			if h.Get("If-Match") != `"v"` || h.Get("Range") != "bytes=1-" || h.Get("Authorization") != "secret" || h.Get("Racer-Metadata") != "opaque" {
				t.Error("subscription changed request")
			}

			_, _ = io.WriteString(conn, subscriptionHead(3, 1, 3))
			_ = fakeSubscriptionFrame(conn, 1, 0, 1, 2)
			_, _ = io.WriteString(conn, "xx")
			_ = fakeSubscriptionFrame(conn, 2, 1, 2, 0)
			closeBody(conn)

			if i == 0 {
				close(closed)
			}
		}
	}()

	c := testClient(t, path, 1)
	m := originMeta(3)

	request := Request{Context: FetchContext{authorization: Authorization{value: "secret"}, metadata: AdapterMetadata{value: "opaque"}}}
	for i := range 2 {
		v, err := c.Get(context.Background(), request, ReadOptions{Offset: 1, Metadata: &m})
		if err != nil {
			t.Fatal(err)
		}

		if n, err := io.Copy(io.Discard, v); n != 2 || err != nil {
			t.Fatal(n, err)
		}

		closeBody(v)

		if i == 0 {
			<-closed
		}
	}

	<-done

	s := c.Stats()
	if s.Dials != 2 || s.ConnectionReuses != 0 || s.Retries != 0 || s.BytesRead != 4 {
		t.Fatal(s)
	}
}

func TestClientIdleAndCloseConnectionPolicy(t *testing.T) {
	for _, policy := range []string{"reuse", "idle", "close"} {
		t.Run(policy, func(t *testing.T) {
			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if policy == "close" {
					w.Header().Set("Connection", "close")
				}

				streamResponse(w, 0, 1, 1, `"v"`)
			}))

			config := ClientConfig{Volume: VolumeName{value: "test"}, MaxConnections: 1}
			if policy == "idle" {
				config.IdleConnTimeout = 20 * time.Millisecond
			}

			c, err := newClient(config, path)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(c)

			var dials atomic.Int32

			poolConfig := c.bulk.config
			dial := poolConfig.Dial

			poolConfig.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
				dials.Add(1)
				return dial(ctx, network, address)
			}
			c.configurePools(poolConfig)

			for i := range 2 {
				v, err := c.Get(context.Background(), Request{})
				if err != nil {
					t.Fatal(err)
				}

				if n, err := v.WriteTo(io.Discard); n != 1 || err != nil {
					t.Fatal(n, err)
				}

				closeBody(v)

				if policy == "idle" && i == 0 {
					deadline := time.Now().Add(time.Second)

					for {
						idle := c.bulk.Stats().IdleConnections

						if idle == 0 {
							break
						}

						if time.Now().After(deadline) {
							t.Fatal("idle connection retained")
						}

						time.Sleep(time.Millisecond)
					}
				}
			}

			want := int32(2)

			if dials.Load() != want {
				t.Fatal("connection policy", dials.Load(), want)
			}
		})
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

	c, err := newClient(ClientConfig{Volume: VolumeName{value: "test"}, MaxConnections: 1, MetadataConnections: 1, MaxQueuedRequests: 1, QueueTimeout: 100 * time.Millisecond}, path)
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

	c, err := newClient(ClientConfig{Volume: VolumeName{value: "test"}, MetadataConnections: 2, MetadataQueuedRequests: 3}, path)
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

	c, err := newClient(ClientConfig{Volume: VolumeName{value: "test"}, MaxConnections: 1, MaxQueuedRequests: 1, MetadataConnections: 1, MetadataQueuedRequests: 1, SmallObjectConnections: 1, SmallObjectQueuedRequests: 1}, path)
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
		config.Volume = VolumeName{value: "test"}
		_, err := NewClient(config)
		assertKind(t, err, ErrorInvalidArgument)
	}

	_, err := (OriginConfig{Volume: VolumeName{value: "test"}, MaxConcurrentHeadRequests: -1}).defaults()
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
