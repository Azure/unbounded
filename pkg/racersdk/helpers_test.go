// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
		if pool.config.Now != nil {
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

	volume, err := ParseVolumeName("test")
	if err != nil {
		t.Fatal(err)
	}

	c, err := newClient(ClientConfig{Volume: volume, MaxConnections: maxConn}, path)
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
	poolConfig := c.bulk.config
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

type transferDiscard struct{ *httptest.ResponseRecorder }

func (transferDiscard) ReadFrom(r io.Reader) (int64, error) { return io.Copy(io.Discard, r) }

func orderedWait(t *testing.T, done <-chan struct{}) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ordered receiver did not reach barrier")
	}
}

func orderedRelease(t *testing.T, reader io.Reader, number uint64, length uint32) bool {
	t.Helper()

	var frame [12]byte
	if _, err := io.ReadFull(reader, frame[:]); err != nil {
		t.Error(err)
		return false
	}

	if binary.BigEndian.Uint64(frame[:8]) != number || binary.BigEndian.Uint32(frame[8:]) != length {
		t.Errorf("release = %x, want page %d length %d", frame, number, length)
		return false
	}

	return true
}

func orderedClean(t *testing.T, v *Value) {
	t.Helper()
	closeBody(v)
	orderedWait(t, v.ordered.done)

	if len(v.ordered.slots) != 0 || len(v.ordered.ready) != 0 || len(v.stream.buffers) != 0 || v.ordered.lease != nil {
		t.Fatal("ordered storage retained after cleanup")
	}

	v.stream.mu.Lock()
	defer v.stream.mu.Unlock()

	if len(v.stream.outstanding) != 0 || v.stream.bytesHeld != 0 || v.client.Stats().ActiveBulk != 0 {
		t.Fatal("ordered credits or admission retained")
	}
}

type orderedReleaseFailureConn struct{ net.Conn }

func (orderedReleaseFailureConn) SetWriteDeadline(time.Time) error {
	return errors.New("release deadline failure")
}

type orderedCloseSignal struct {
	io.Closer
	closed chan struct{}
}

func (b orderedCloseSignal) Close() error {
	err := b.Closer.Close()
	close(b.closed)

	return err
}

type writeFunc func([]byte) (int, error)

func (f writeFunc) Write(p []byte) (int, error) { return f(p) }

type copyDestination struct {
	bytes.Buffer
	readFrom bool
	maxWrite int
	sizes    []int
}

func (w *copyDestination) ReadFrom(io.Reader) (int64, error) {
	w.readFrom = true
	return 0, errors.New("unexpected ReaderFrom")
}

func (w *copyDestination) Write(p []byte) (int, error) {
	w.maxWrite = max(w.maxWrite, len(p))
	w.sizes = append(w.sizes, len(p))

	return w.Buffer.Write(p)
}

// Exercise copying through the supported subscription transport, including page
// verification, ordered delivery, and admission cleanup.
func copyTestValue(t *testing.T, source io.ReadCloser, length int64) *Value {
	t.Helper()
	t.Cleanup(func() { closeBody(source) })
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		streamResponseHead(w, 0, length, length, `"v"`)
		_, _ = io.Copy(w, source)
	}))
	c := testClient(t, path, 1)

	v, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeBody(v) })

	return v
}

func streamTCPPair(t *testing.T) (peer net.Conn, destination *net.TCPConn) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeBody(listener) })

	peer, err = net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeBody(peer) })

	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeBody(connection) })

	return peer, connection.(*net.TCPConn)
}

func streamFakeClient(t *testing.T, origin Origin) *Client {
	t.Helper()

	client, cleanup, err := newFakeClient(t, origin)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)

	return client
}

// benchmarkPeer generates bytes with fixed scratch. The SDK uses subscriptions;
// the stdlib baseline uses origin-style ranges. Neither retains an object fixture.
func benchmarkPeer(b *testing.B, size int64, origin bool) string {
	b.Helper()
	path := socketDir(b) + "/socket"

	listener, err := net.Listen("unix", path)
	if err != nil {
		b.Fatal(err)
	}

	server := &http.Server{ReadHeaderTimeout: time.Second}

	if origin {
		config, err := (OriginConfig{Volume: VolumeName{value: "bench"}}).defaults()
		if err != nil {
			b.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		b.Cleanup(cancel)

		listener = &originListener{Listener: listener, ctx: ctx, config: config, slots: make(chan struct{}, 128)}
		server.BaseContext = func(net.Listener) context.Context { return ctx }
		server.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
			return context.WithValue(ctx, originConnKey{}, conn)
		}
		slots := make(chan struct{}, 64)
		headSlots := make(chan struct{}, config.MaxConcurrentHeadRequests)
		callback := func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
			m := originMeta(ByteLength(size))
			if r.Operation() == OperationHead || size == 0 {
				return m, nil, nil
			}

			first, last, err := r.byteRange.Resolve(m.Size)
			if err != nil {
				return m, nil, err
			}

			return m, io.NopCloser(io.LimitReader(repeatedByte('x'), int64(last-first)+1)), nil
		}
		server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveOperation(w, r, config, callback, slots, headSlots) })
	} else {
		server.Handler = subscriptionHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "HEAD" {
				w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
				w.Header().Set("ETag", `"v"`)
				w.Header().Set("Racer-Expires-At", "0")

				return
			}

			if size == 0 {
				streamResponse(w, 0, 0, 0, `"v"`)
				return
			}

			if r.Method == "POST" {
				selected, err := wire.ParseSubscriptionRequest(r)
				if err != nil {
					b.Error(err)
					return
				}

				streamResponse(w, int64(selected.First), int64(min(selected.End, uint64(size))-selected.First), size, `"v"`)

				return
			}

			requested, err := parseRange(r.Header.Get("Range"))
			if err != nil {
				b.Error(err)
				return
			}

			first, last, err := requested.resolve(ByteLength(size))
			if err != nil {
				b.Error(err)
				return
			}

			streamResponse(w, int64(first), int64(last-first)+1, size, `"v"`)
		}))
	}

	finished := make(chan struct{})

	go func() { defer close(finished); _ = server.Serve(listener) }()

	b.Cleanup(func() { _ = server.Close(); <-finished })

	return path
}

type benchmarkReader struct {
	client *Client
	plain  *http.Transport
	buffer []byte
	copyIO bool
	size   int64
}

func newBenchmarkReader(b *testing.B, path, implementation, mode string, size int64, bufferSize int) *benchmarkReader {
	b.Helper()

	r := &benchmarkReader{size: size, copyIO: mode == "Copy", buffer: make([]byte, bufferSize)}

	if implementation == "sdk" {
		var err error

		r.client, err = newClient(ClientConfig{Volume: VolumeName{value: "bench"}}, path)
		if err != nil {
			b.Fatal(err)
		}

		b.Cleanup(func() { closeBody(r.client) })
	} else {
		r.plain = unixTransport(path)
		b.Cleanup(r.plain.CloseIdleConnections)
	}

	return r
}

func (r *benchmarkReader) copy(dst io.Writer, body io.Reader) (int64, error) {
	if r.copyIO {
		return io.Copy(struct{ io.Writer }{dst}, body)
	}

	return io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{body}, r.buffer)
}

func (r *benchmarkReader) read(dst io.Writer, fresh bool) error {
	if r.client != nil {
		if fresh {
			r.client.closeIdleConnections()
		}

		value, err := r.client.Get(context.Background(), Request{})
		if err != nil {
			return err
		}
		defer closeBody(value)

		n, err := r.copy(dst, value)
		if err == nil && n != r.size {
			return fmt.Errorf("size %d, want %d", n, r.size)
		}

		return err
	}

	if fresh {
		r.plain.CloseIdleConnections()
	}

	var total int64

	for first := int64(0); ; first = int64(PageSize) {
		request, err := http.NewRequest("GET", "http://racer"+objectPrefix+(Key{}).String(), nil)
		if err != nil {
			return err
		}

		request.Header.Set("Range", "bytes=0-16777215")

		if first != 0 {
			request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", first, r.size-1))
			request.Header.Set("If-Match", `"v"`)
		}

		response, err := r.plain.RoundTrip(request)
		if err != nil {
			return err
		}

		n, err := r.copy(dst, response.Body)
		closeBody(response.Body)

		if err != nil {
			return err
		}

		total += n
		if total == r.size {
			return nil
		}

		if first != 0 || total != int64(PageSize) {
			return fmt.Errorf("size %d, want %d", total, r.size)
		}
	}
}

func BenchmarkClientStream(b *testing.B) {
	for _, size := range []int64{0, 4096, int64(PageSize), 1 << 30} {
		b.Run(strconv.FormatInt(size, 10), func(b *testing.B) {
			path := benchmarkPeer(b, size, false)

			for _, implementation := range []string{"stdlib", "sdk"} {
				for _, fresh := range []bool{false, true} {
					for _, mode := range []struct {
						name  string
						bytes int
					}{{"Read4K", 4096}, {"Read32K", 32768}, {"Read256K", 262144}, {"Copy", 0}} {
						b.Run(fmt.Sprintf("%s/fresh=%t/%s", implementation, fresh, mode.name), func(b *testing.B) {
							r := newBenchmarkReader(b, path, implementation, mode.name, size, mode.bytes)
							if err := r.read(io.Discard, false); err != nil {
								b.Fatal(err)
							}

							b.ReportAllocs()
							b.SetBytes(size)
							b.ResetTimer()

							for range b.N {
								if err := r.read(io.Discard, fresh); err != nil {
									b.Fatal(err)
								}
							}
						})
					}
				}
			}
		})
	}
}

// Keep the fixture's generation cost visible independently of SDK transport.
// A scalar fill loop previously dominated BenchmarkClientStream and changed
// throughput with binary layout, even when no SDK code was executed.
func BenchmarkFixtureGenerator(b *testing.B) {
	var p [copyBufferSize]byte
	b.SetBytes(int64(len(p)))
	b.ReportAllocs()

	r := fixtureReader()
	for b.Loop() {
		_, _ = r.Read(p[:])
	}
}

// Match the interface dispatch used by streamResponse's io.CopyN.
//
//go:noinline
func fixtureReader() io.Reader { return repeatedByte('x') }

func BenchmarkOriginStream(b *testing.B) {
	for _, size := range []int64{0, 4096, int64(PageSize)} {
		for _, origin := range []bool{false, true} {
			b.Run(fmt.Sprintf("%d/sdk=%t", size, origin), func(b *testing.B) {
				path := benchmarkPeer(b, size, origin)

				r := newBenchmarkReader(b, path, "stdlib", "Read32K", size, 32768)
				if err := r.read(io.Discard, false); err != nil {
					b.Fatal(err)
				}

				b.ReportAllocs()
				b.SetBytes(size)
				b.ResetTimer()

				for range b.N {
					if err := r.read(io.Discard, false); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkConcurrentStream(b *testing.B) {
	for _, implementation := range []string{"stdlib", "sdk"} {
		for _, concurrency := range []int{1, 16} {
			b.Run(fmt.Sprintf("%s/concurrency=%d", implementation, concurrency), func(b *testing.B) {
				const size = int64(PageSize)

				path := benchmarkPeer(b, size, false)

				readers := make([]*benchmarkReader, concurrency)
				for i := range readers {
					readers[i] = newBenchmarkReader(b, path, implementation, "Read32K", size, 32768)
					if i > 0 {
						readers[i].client, readers[i].plain = readers[0].client, readers[0].plain
					}

					if err := readers[i].read(io.Discard, false); err != nil {
						b.Fatal(err)
					}
				}

				var (
					next    atomic.Int64
					workers sync.WaitGroup
				)

				b.ReportAllocs()
				b.SetBytes(size)
				b.ResetTimer()

				for _, reader := range readers {
					workers.Go(func() {
						for next.Add(1) <= int64(b.N) {
							if err := reader.read(io.Discard, false); err != nil {
								b.Error(err)
								return
							}
						}
					})
				}

				workers.Wait()
			})
		}
	}
}

func BenchmarkClientStat(b *testing.B) {
	path := benchmarkPeer(b, 1<<30, false)

	c, err := newClient(ClientConfig{Volume: VolumeName{value: "bench"}}, path)
	if err != nil {
		b.Fatal(err)
	}
	defer closeBody(c)

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		m, err := c.Stat(context.Background(), Request{})
		if err != nil || m.Size != 1<<30 {
			b.Fatal(m, err)
		}
	}
}

func BenchmarkClientRange(b *testing.B) {
	path := benchmarkPeer(b, 1<<30, false)

	c, err := newClient(ClientConfig{Volume: VolumeName{value: "bench"}}, path)
	if err != nil {
		b.Fatal(err)
	}
	defer closeBody(c)

	b.ReportAllocs()
	b.SetBytes(4096)
	b.ResetTimer()

	for range b.N {
		v, err := c.Get(context.Background(), Request{}, ReadOptions{Offset: ByteOffset(PageSize) + 3, Length: 4096})
		if err != nil {
			b.Fatal(err)
		}

		n, err := v.WriteTo(io.Discard)
		closeBody(v)

		if err != nil || n != 4096 {
			b.Fatal(n, err)
		}
	}
}

// Live-heap sampling is separate from throughput: forced GC and runtime sampling
// intentionally perturb timing. Report absolute process heap and the warmed baseline
// separately: collection of old server work can make a baseline delta negative.
type heapSink struct {
	bytes, next int64
	base, peak  uint64
	first, last uint64
	slow        bool
	nextPause   int64
}

func (w *heapSink) Write(p []byte) (int, error) {
	w.bytes += int64(len(p))
	if w.slow && w.bytes >= w.nextPause {
		w.nextPause = w.bytes + 1<<20

		time.Sleep(time.Millisecond)
	}

	if w.bytes >= w.next {
		w.next = w.bytes + 64<<20

		runtime.GC()

		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)

		w.peak = max(w.peak, stats.HeapAlloc)

		w.last = stats.HeapAlloc
		if w.first == 0 {
			w.first = w.last
		}
	}

	return len(p), nil
}

func BenchmarkStreamingLiveHeap(b *testing.B) {
	for _, implementation := range []string{"stdlib", "sdk"} {
		for _, slow := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/slow=%t", implementation, slow), func(b *testing.B) {
				const size = 1 << 30

				path := benchmarkPeer(b, size, false)

				r := newBenchmarkReader(b, path, implementation, "Read32K", size, 32768)
				if err := r.read(io.Discard, false); err != nil {
					b.Fatal(err)
				}

				runtime.GC()

				var stats runtime.MemStats
				runtime.ReadMemStats(&stats)
				sink := &heapSink{base: stats.HeapAlloc, slow: slow}

				b.ResetTimer()

				for range b.N {
					if err := r.read(sink, false); err != nil {
						b.Fatal(err)
					}
				}

				b.ReportMetric(float64(sink.peak), "peak-live-B")
				b.ReportMetric(float64(sink.base), "baseline-live-B")
				b.ReportMetric(float64(sink.first), "first-live-B")
				b.ReportMetric(float64(sink.last), "last-live-B")
			})
		}
	}
}

// BenchmarkHTTPStream compares the buffered and streaming SDK paths over real
// Unix sockets and plaintext loopback HTTP/1.1. The existing protocol fixture
// generates payloads in bounded scratch, not from the Rust dataplane or disk.
// Requests are sequential with one page credit and a warmed HTTP connection.
// Timing includes generation, UDS dialing/framing, HTTP delivery, and draining.
// Allocations cover the whole process, not just the SDK, and exclude warmup;
// pooled page memory retained by Get is not a per-operation allocation metric.
func BenchmarkHTTPStream(b *testing.B) {
	for _, size := range []int64{16 << 20, 32 << 20} {
		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			path := benchmarkPeer(b, size, false)

			for _, streaming := range []bool{false, true} {
				name := "Get"
				if streaming {
					name = "GetStreaming"
				}

				b.Run(name, func(b *testing.B) {
					benchmarkHTTPStream(b, path, size, streaming)
				})
			}
		})
	}
}

func benchmarkHTTPStream(b *testing.B, path string, size int64, streaming bool) {
	b.Helper()

	c, err := newClient(ClientConfig{Volume: VolumeName{value: "bench"}}, path)
	if err != nil {
		b.Fatal(err)
	}
	defer closeBody(c)

	get := c.Get
	if streaming {
		get = c.GetStreaming
	}

	// Join handler cleanup before starting the next operation, including when
	// the client observes the final Content-Length byte before the handler exits.
	finished := make(chan error, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var transferErr error

		defer func() { finished <- transferErr }()

		v, err := get(r.Context(), Request{}, ReadOptions{PageCredits: 1, ByteCredits: PageSize})
		if err != nil {
			transferErr = err

			http.Error(w, "get failed", http.StatusBadGateway)

			return
		}
		defer closeBody(v)

		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))

		n, err := v.WriteToHTTP(w)
		if err != nil || n != size {
			transferErr = fmt.Errorf("HTTP transfer: bytes=%d, want=%d, error=%v", n, size, err)

			panic(http.ErrAbortHandler)
		}
	}))
	defer server.Close()

	httpClient := server.Client()
	httpClient.Timeout = 15 * time.Second

	consume := func() {
		b.Helper()

		req, err := http.NewRequestWithContext(b.Context(), http.MethodGet, server.URL, nil)
		if err != nil {
			b.Fatal(err)
		}

		res, err := httpClient.Do(req)
		if err != nil {
			b.Fatal(err)
		}

		n, err := io.Copy(io.Discard, res.Body)
		closeBody(res.Body)

		if err != nil || n != size || res.StatusCode != http.StatusOK || res.ProtoMajor != 1 {
			b.Fatalf("response: bytes=%d, want=%d, status=%s, protocol=%s, error=%v", n, size, res.Status, res.Proto, err)
		}

		if err := <-finished; err != nil {
			b.Fatal(err)
		}
	}
	consume()

	b.ReportAllocs()
	b.SetBytes(size)
	b.ResetTimer()

	for range b.N {
		consume()
	}

	b.StopTimer()
}
