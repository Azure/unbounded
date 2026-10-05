// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/fakeracer"
	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// Protocol adapters used only by SDK tests and benchmarks.
const objectPrefix = wire.ObjectPrefix

func validateContentType(s string) error {
	return fromWireError(wire.ValidateContentType(s))
}

func statusError(status int) *Error {
	err := fromWireError(wire.StatusError(status))

	var typed *Error
	errors.As(err, &typed)

	return typed
}

func (r Range) resolve(size ByteLength) (ByteOffset, ByteOffset, error) {
	first, last, err := r.wire().Resolve(uint64(size))
	return ByteOffset(first), ByteOffset(last), fromWireError(err)
}

func decimal(s string) (uint64, error) {
	n, err := wire.Decimal(s)
	return n, fromWireError(err)
}

func parseRange(s string) (Range, error) {
	r, err := wire.ParseRange(s)
	return fromWireRange(r), fromWireError(err)
}

func bootstrapRange() Range           { return fromWireRange(wire.BootstrapRange()) }
func validatePageShape(r Range) error { return fromWireError(wire.ValidatePageShape(r.wire())) }

func (c *Client) closeIdleConnections() {
	for _, pool := range []*connectionPool{&c.bulk, &c.metadataPool, &c.smallPool} {
		if pool.Pool != nil {
			pool.CloseIdle()
		}
	}
}

// Keep socket and test scratch paths inside this worktree, including under race.
func socketDir(t testing.TB) string {
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
	return rawClientPeer(t, subscriptionHandler(handler))
}

func rawClientPeer(t *testing.T, handler http.Handler) string {
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
	// Use bulk copies rather than a scalar byte-store loop. The latter can
	// dominate transport benchmarks and is sensitive to linked code alignment.
	if len(p) > 0 {
		p[0] = byte(b)
		for filled := 1; filled < len(p); {
			filled += copy(p[filled:], p[:filled])
		}
	}

	return len(p), nil
}

func streamResponse(w http.ResponseWriter, first, length, size int64, tag string) {
	streamResponseHead(w, first, length, size, tag)
	_, _ = io.CopyN(w, repeatedByte('x'), length)
}

func streamResponseHead(w http.ResponseWriter, first, length, size int64, tag string) {
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", tag)
	w.Header().Set("Racer-Expires-At", "0")

	if length != 0 {
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(first, 10)+"-"+strconv.FormatInt(first+length-1, 10)+"/"+strconv.FormatInt(size, 10))
		w.WriteHeader(206)
	}
}

type shortDestination struct{}

func (shortDestination) Write(p []byte) (int, error) { return len(p) / 2, nil }

// Root white-box tests use the daemon directly, without importing racersdktest
// (which would cycle back to this package under test).
func newFakeClient(t *testing.T, origin Origin) (*Client, func(), error) {
	t.Helper()

	if origin == nil {
		return nil, nil, failure(ErrorInvalidArgument, "fake origin", nil)
	}

	path, cancel, done := startOrigin(t, OriginConfig{}, origin)
	c := originClient(t, path, 64)

	var once sync.Once

	return c, func() { once.Do(func() { closeBody(c); cancel(); <-done }) }, nil
}

func fakeSubscriptionSocket(t *testing.T, client *Client, headers string) (net.Conn, *http.Response) {
	t.Helper()

	conn, _, err := client.bulk.Get(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeBody(conn) })

	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	_, err = fmt.Fprintf(conn, "POST /v2/objects/%s HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n%s\r\n", (Key{}).String(), headers)
	if err != nil {
		t.Fatal(err)
	}

	res, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeBody(res.Body) })

	return conn, res
}

func fakeReadFrame(t *testing.T, reader io.Reader, kind byte, page, offset uint64, length uint32) {
	t.Helper()

	var frame [21]byte
	if _, err := io.ReadFull(reader, frame[:]); err != nil {
		t.Fatal(err)
	}

	if frame[0] != kind || binary.BigEndian.Uint64(frame[1:9]) != page || binary.BigEndian.Uint64(frame[9:17]) != offset || binary.BigEndian.Uint32(frame[17:]) != length {
		t.Fatalf("unexpected frame: %x; want kind=%d page=%d offset=%d length=%d", frame, kind, page, offset, length)
	}

	if length != 0 {
		sink := &offsetSink{offset: int64(offset)}
		if n, err := io.CopyN(sink, reader, int64(length)); err != nil || n != int64(length) {
			t.Fatal("payload", n, err)
		}
	}
}

func fakeRelease(t *testing.T, conn net.Conn, page uint64, length uint32) {
	t.Helper()

	var release [12]byte
	binary.BigEndian.PutUint64(release[:8], page)
	binary.BigEndian.PutUint32(release[8:], length)

	if _, err := conn.Write(release[:]); err != nil {
		t.Fatal(err)
	}
}

func fakeSubscriptionOrigin(t *testing.T, size ByteLength) Origin {
	t.Helper()

	return func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
		m := originMeta(size)

		m.ContentType = "test/example"
		if r.Operation() == OperationHead || size == 0 {
			return m, nil, nil
		}

		page, _ := r.Range()

		first, last, err := page.Resolve(size)
		if err != nil {
			return m, nil, err
		}

		return m, io.NopCloser(io.LimitReader(&offsetStream{offset: int64(first)}, int64(last-first)+1)), nil
	}
}

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

	return fakeracer.NewHandler(transport)
}

// originClient exercises the real Unix origin through the subscription fake.
func originClient(t *testing.T, path string, maxConnections int) *Client {
	t.Helper()
	return testClient(t, rawClientPeer(t, pageForwarder(t, path, 0)), maxConnections)
}

// subscriptionHandler migrates HTTP payload fixtures to the duplex wire. The
// fixture still chooses metadata and payload/failure timing. No request is
// rewritten or retried, and credits gate every page frame on this one socket.
func subscriptionHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			handler.ServeHTTP(w, r)
			return
		}

		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer closeBody(conn)

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		credits := fakeracer.NewCredits()
		go credits.Releases(rw.Reader, cancel)

		s, err := wire.ParseSubscriptionRequest(r)
		if err != nil {
			return
		}

		writer := &subscriptionFixture{header: make(http.Header), writer: rw, conn: conn, ctx: ctx, credits: credits, request: s}
		handler.ServeHTTP(writer, r.WithContext(ctx))
		writer.finish()
	})
}

type subscriptionFixture struct {
	header                              http.Header
	writer                              *bufio.ReadWriter
	conn                                net.Conn
	ctx                                 context.Context
	credits                             *fakeracer.Credits
	request                             wire.SubscriptionRequest
	started                             bool
	err                                 error
	first, end, offset, frameEnd, count uint64
}

func (w *subscriptionFixture) Header() http.Header { return w.header }

func (w *subscriptionFixture) SetWriteDeadline(deadline time.Time) error {
	return w.conn.SetWriteDeadline(deadline)
}

func fixtureRange(t *testing.T, r *http.Request, size int64) (ByteOffset, ByteOffset) {
	t.Helper()

	s, err := wire.ParseSubscriptionRequest(r)
	if err != nil {
		t.Error(err)
		return 0, 0
	}

	return ByteOffset(s.First), ByteOffset(min(s.End, uint64(size)) - 1)
}

func (w *subscriptionFixture) WriteHeader(status int) {
	if w.started {
		return
	}

	w.started = true
	if status == 200 || status == 206 {
		size, err := decimal(w.header.Get("Content-Length"))
		if cr := w.header.Get("Content-Range"); cr != "" {
			parts := strings.Split(cr, "/")
			if len(parts) == 2 {
				size, err = decimal(parts[1])
			}
		}

		if err != nil {
			w.err = err
			return
		}

		w.first, w.end = w.request.First, min(size, w.request.End)
		w.offset = w.first

		pages := uint64(0)
		if w.end > w.first {
			pages = (w.end-1)/uint64(PageSize) - w.first/uint64(PageSize) + 1
		}

		w.header.Del("Content-Range")
		w.header.Set("Content-Type", "application/octet-stream")
		w.header.Set("Racer-Object-Length", strconv.FormatUint(size, 10))
		w.header.Set("Racer-Range-Start", strconv.FormatUint(w.first, 10))
		w.header.Set("Racer-Range-End", strconv.FormatUint(w.end, 10))
		w.header.Set("Content-Length", strconv.FormatUint(w.end-w.first+21*(pages+1), 10))

		status = 200
	}

	w.err = wire.WriteSubscriptionHead(w.writer, status, w.header)
	if w.err == nil {
		w.err = w.writer.Flush()
	}
}

func (w *subscriptionFixture) Write(p []byte) (int, error) {
	if !w.started {
		w.WriteHeader(200)
	}

	n := 0

	for len(p) > 0 && w.err == nil {
		if w.offset == w.end {
			return n, io.ErrShortWrite
		}

		if w.offset == w.frameEnd || w.frameEnd == 0 {
			w.frameEnd = min(w.end, (w.offset/uint64(PageSize)+1)*uint64(PageSize))
			length := uint32(w.frameEnd - w.offset)

			w.err = w.credits.Reserve(w.ctx, w.request, w.offset/uint64(PageSize), length)
			if w.err == nil {
				w.err = fakeSubscriptionFrame(w.writer, 1, w.offset/uint64(PageSize), w.offset, length)
			}

			w.count++
		}

		if w.err != nil {
			break
		}

		part := p[:min(uint64(len(p)), w.frameEnd-w.offset)]

		var written int

		written, w.err = w.writer.Write(part)
		w.offset += uint64(written)
		n += written
		p = p[written:]

		if w.err == nil {
			w.err = w.writer.Flush()
		}
	}

	return n, w.err
}

func (w *subscriptionFixture) Flush() {
	if !w.started {
		w.WriteHeader(200)
	}

	if w.err == nil {
		w.err = w.writer.Flush()
	}
}

func (w *subscriptionFixture) finish() {
	w.Flush()

	if w.err == nil && w.offset == w.end {
		w.err = fakeSubscriptionFrame(w.writer, 2, w.count, w.end-w.first, 0)
		if w.err == nil {
			w.err = w.writer.Flush()
		}
	}
}

func subscriptionHead(size, first, end uint64) string {
	pages := uint64(0)
	if end > first {
		pages = (end-1)/uint64(PageSize) - first/uint64(PageSize) + 1
	}

	return fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\nETag: \"v\"\r\nRacer-Expires-At: 0\r\nRacer-Object-Length: %d\r\nRacer-Range-Start: %d\r\nRacer-Range-End: %d\r\nConnection: close\r\n\r\n", end-first+21*(pages+1), size, first, end)
}

// fakeSubscriptionFrame encodes intentionally valid or malformed test frames
// with the shared codec; sequence validation belongs to the client under test.
func fakeSubscriptionFrame(w io.Writer, kind byte, page, offset uint64, length uint32) error {
	return wire.WriteFrame(w, wire.Frame{Kind: kind, Number: page, Offset: offset, Length: length})
}

func rawSubscriptionClient(t *testing.T, serve func(net.Conn, *bufio.Reader, []byte)) *Client {
	t.Helper()
	c := testClient(t, "unused", 1)
	c.config.BodyReadTimeout = time.Second
	poolConfig := c.bulk.Config()
	poolConfig.Dial = func(context.Context, string, string) (net.Conn, error) {
		client, peer := net.Pipe()

		t.Cleanup(func() { closeBody(peer) })

		go func() {
			defer closeBody(peer)

			r := bufio.NewReader(peer)

			head, err := readRawHead(r, false)
			if err == nil {
				serve(peer, r, head)
			}
		}()

		return client, nil
	}
	c.configurePools(poolConfig)

	return c
}

func assertKind(t *testing.T, err error, kind ErrorKind) {
	t.Helper()

	var typed *Error
	if !errors.As(err, &typed) || typed.Kind() != kind {
		t.Fatalf("error = %v; want kind %v", err, kind)
	}
}

// Raw fixtures shared by root integration tests deliberately bypass validation.
func rawRequest(method, fields string) []byte {
	return []byte(method + " " + objectPrefix + (Key{}).String() + " HTTP/1.1\r\nHost: racer\r\n" + fields + "\r\n")
}

func rawResponse(status int, fields string) []byte {
	return []byte("HTTP/1.1 " + strconv.Itoa(status) + " " + http.StatusText(status) + "\r\n" + fields + "\r\n")
}

type finalErrorReader struct{ err error }

func (r finalErrorReader) Read(p []byte) (int, error) { return copy(p, "abc"), r.err }
