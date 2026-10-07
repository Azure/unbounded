// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// Origin loads objects for Racer on a cache miss. It is called concurrently,
// once per request, and must honor ctx.
//
// For a metadata request (Head is set) return the current metadata and a nil
// body. Otherwise return the metadata of the version you are serving and a
// body that yields exactly the bytes in [Offset, min(Offset+Length, Size)).
// When that range is empty, return a nil body. If ETag is set, serve only
// that version and return [ErrVersionMismatch] if it is gone; returning
// metadata with a different ETag has the same effect.
//
// Report failures by wrapping one of the package errors, for example
// fmt.Errorf("%w: %w", racersdk.ErrNotFound, err). Any other error is
// reported to Racer as an internal error.
//
// The SDK takes ownership of a non-nil body even when err is non-nil, and
// closes it exactly once, possibly concurrently with Read to abort a transfer.
// Panics are recovered.
type Origin func(ctx context.Context, request OriginRequest) (Metadata, io.ReadCloser, error)

// OriginRequest is a request from Racer to an [Origin]. The embedded Request
// carries the key and the opaque values a client passed to [Client.Get] or
// [Client.Stat].
type OriginRequest struct {
	Request
	// Head asks for metadata only.
	Head bool
	// ETag, when set, is the only version that may be served.
	ETag string
	// Offset is the first byte to return. It is always a multiple of
	// [PageSize].
	Offset int64
	// Length is the maximum number of bytes to return, at most [PageSize].
	// The range may extend past the end of the object.
	Length int64
}

// Format prints the request with Metadata and Authorization redacted.
func (r OriginRequest) Format(s fmt.State, _ rune) {
	// fmt.State cannot usefully report a write error back through Format.
	_, _ = fmt.Fprintf(s, "OriginRequest{%v, Head: %t, ETag: %q, Offset: %d, Length: %d}", r.Request, r.Head, r.ETag, r.Offset, r.Length) //nolint:errcheck // See above.
}

// OriginConfig configures [ServeOrigin]. Only Volume is required.
type OriginConfig struct {
	// Volume names the Racer volume. The origin listens on
	// /run/racer/<Volume>/origin/socket. The directory must already exist and
	// its ancestors must not be symlinks.
	Volume string
	// MaxConcurrentRequests bounds concurrent content requests; Racer receives
	// a retryable error beyond it. Zero means 64. Metadata requests have a
	// small separate limit.
	MaxConcurrentRequests int
	// RecoverStaleSocket lets ServeOrigin replace a socket left behind by an
	// earlier ServeOrigin with this option that exited without cleanup, such
	// as after a crash. The directory must be owned by this user and must
	// not be writable by group or others. Without it, an existing socket
	// path is an error.
	RecoverStaleSocket bool
}

type originLimits struct {
	maxConnections    int
	maxRequests       int
	maxHeadRequests   int
	readHeaderTimeout time.Duration
	requestTimeout    time.Duration
	writeTimeout      time.Duration
	idleTimeout       time.Duration
	socketMode        os.FileMode
	recoverStale      bool
}

func (c OriginConfig) limits() (originLimits, error) {
	if err := validateVolume(c.Volume); err != nil {
		return originLimits{}, err
	}

	if c.MaxConcurrentRequests < 0 {
		return originLimits{}, invalid("origin config", errors.New("negative MaxConcurrentRequests"))
	}

	l := originLimits{
		maxConnections:    128,
		maxRequests:       64,
		maxHeadRequests:   4,
		readHeaderTimeout: 5 * time.Second,
		requestTimeout:    60 * time.Second,
		writeTimeout:      30 * time.Second,
		idleTimeout:       30 * time.Second,
		socketMode:        0o600,
		recoverStale:      c.RecoverStaleSocket,
	}
	if c.MaxConcurrentRequests != 0 {
		l.maxRequests = c.MaxConcurrentRequests
	}

	return l, nil
}

// ServeOrigin serves origin to Racer until ctx is canceled, then closes all
// connections and bodies and returns ctx.Err(). It does not wait for
// callbacks that ignore cancellation. The socket is created with mode 0600,
// so the origin must run as the same user as the Racer dataplane, usually
// root. On return it removes the socket it created.
func ServeOrigin(ctx context.Context, config OriginConfig, origin Origin) error {
	return serveOrigin(ctx, config, origin, "/run/racer/"+config.Volume+"/origin/socket")
}

type originConnKey struct{}

func serveOrigin(ctx context.Context, config OriginConfig, origin Origin, path string) error {
	if ctx == nil || origin == nil {
		return invalid("origin", errors.New("nil context or origin"))
	}

	limits, err := config.limits()
	if err != nil {
		return err
	}

	return serveOriginLimits(ctx, limits, origin, path)
}

// serveOriginLimits serves with explicit limits so tests can shorten them.
func serveOriginLimits(ctx context.Context, limits originLimits, origin Origin, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	listen := listenOrigin
	if limits.recoverStale {
		listen = listenOwnedOrigin
	}

	l, cleanup, err := listen(path, limits.socketMode)
	if err != nil {
		return err
	}
	defer cleanup()

	lifetime, cancel := context.WithCancel(ctx)
	defer cancel()

	listener := &originListener{Listener: l, ctx: lifetime, slots: make(chan struct{}, limits.maxConnections), limits: limits}
	server := &http.Server{
		ReadHeaderTimeout: limits.readHeaderTimeout,
		IdleTimeout:       limits.idleTimeout,
		WriteTimeout:      limits.writeTimeout,
		MaxHeaderBytes:    wire.MaxHeadBytes,
		ErrorLog:          log.New(io.Discard, "", 0),
		BaseContext:       func(net.Listener) context.Context { return lifetime },
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, originConnKey{}, c)
		},
	}
	slots := make(chan struct{}, limits.maxRequests)
	headSlots := make(chan struct{}, limits.maxHeadRequests)
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveOperation(w, r, limits, origin, slots, headSlots)
	})

	stop := context.AfterFunc(lifetime, func() { closeQuietly(server) })
	defer stop()

	err = server.Serve(listener)
	closeQuietly(server)

	if ctx.Err() != nil {
		return ctx.Err()
	}

	return ioFailure("origin serve", err)
}

func newOriginRequest(r wire.Request) OriginRequest {
	o := OriginRequest{
		Request: Request{Key: r.Key, Metadata: r.AdapterMetadata, Authorization: r.Authorization},
		ETag:    r.Pin,
	}

	switch r.Operation {
	case wire.OperationHead:
		o.Head = true
	case wire.OperationBootstrap:
		o.Length = PageSize
	case wire.OperationPinned:
		o.Offset = int64(r.Range.First)
		o.Length = int64(r.Range.Last - r.Range.First + 1)
	}

	return o
}

type originResult struct {
	metadata Metadata
	body     io.ReadCloser
	err      error
}

func callOrigin(ctx context.Context, origin Origin, request OriginRequest) (result originResult) {
	defer func() {
		if recover() != nil {
			result.err = failure(wire.ErrorInternal, "origin", errors.New("panic"))
		}
	}()

	result.metadata, result.body, result.err = origin(ctx, request)

	return result
}

func serveOperation(w http.ResponseWriter, r *http.Request, limits originLimits, origin Origin, slots, headSlots chan struct{}) {
	writeOriginError := func(w http.ResponseWriter, status int, size uint64) {
		// Once a request expires, only the empty error response gets a fresh,
		// bounded write opportunity. Success writes never extend its deadline.
		if status == http.StatusServiceUnavailable {
			if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(limits.writeTimeout)); err != nil {
				return
			}
		}

		writeOriginErrorResponse(w, status, size)
	}
	committed := false

	defer func() {
		if recover() != nil {
			if committed {
				panic(http.ErrAbortHandler)
			}

			writeOriginError(w, http.StatusInternalServerError, 0)
		}
	}()

	conn, ok := r.Context().Value(originConnKey{}).(*originConn)
	if !ok {
		writeOriginError(w, http.StatusInternalServerError, 0)
		return
	}

	head := conn.takeHead()
	if head.err != nil {
		writeOriginError(w, originHeadStatus(head.err), 0)
		return
	}

	ctx, cancel := context.WithDeadline(r.Context(), head.at.Add(limits.requestTimeout))
	defer cancel()

	if ctx.Err() != nil {
		writeOriginError(w, http.StatusServiceUnavailable, 0)
		return
	}

	controller := http.NewResponseController(w)

	deadline, _ := ctx.Deadline()
	if err := controller.SetWriteDeadline(minTime(deadline, time.Now().Add(limits.writeTimeout))); err != nil {
		panic(http.ErrAbortHandler)
	}

	request := head.request
	pinned := request.Pin != ""

	if request.Operation == wire.OperationHead {
		slots = headSlots
	}

	select {
	case slots <- struct{}{}:
	default:
		writeOriginError(w, http.StatusServiceUnavailable, 0)
		return
	}

	result, received := awaitOrigin(ctx, origin, newOriginRequest(request), slots)
	if !received {
		writeOriginError(w, http.StatusServiceUnavailable, 0)
		return
	}

	defer func() { <-slots }()

	body := &onceBody{body: result.body}
	defer body.close()

	stop := context.AfterFunc(ctx, body.close)
	defer stop()

	if ctx.Err() != nil {
		writeOriginError(w, http.StatusServiceUnavailable, 0)
		return
	}

	metadata := result.metadata.wire()

	if result.err != nil {
		status := callbackStatus(result.err, pinned)
		if status == http.StatusRequestedRangeNotSatisfiable && (metadata.Validate() != nil || pinned && request.Pin != metadata.ETag) {
			status = http.StatusBadGateway
		}

		writeOriginError(w, status, metadata.Size)

		return
	}

	if pinned && metadata.Validate() == nil && metadata.ETag != request.Pin {
		// The origin no longer has the pinned version.
		writeOriginError(w, http.StatusPreconditionFailed, 0)
		return
	}

	response, err := wire.OriginResponse(request, metadata)
	if err != nil {
		writeOriginError(w, callbackStatus(err, pinned), metadata.Size)
		return
	}

	if request.Operation == wire.OperationHead && result.body != nil {
		// Metadata requests carry no body; discard one if returned.
		body.close()
		result.body = nil
	}

	if response.Length != 0 && result.body == nil {
		writeOriginError(w, http.StatusBadGateway, 0)
		return
	}

	if response.Length == 0 && result.body != nil {
		if err := probeEOF(result.body); err != nil {
			status := http.StatusBadGateway
			if ctx.Err() != nil {
				status = http.StatusServiceUnavailable
			}

			writeOriginError(w, status, 0)

			return
		}
	}

	if ctx.Err() != nil {
		writeOriginError(w, http.StatusServiceUnavailable, 0)
		return
	}

	status, err := prepareOriginHeaders(w, request, metadata, response)
	if err != nil {
		writeOriginError(w, http.StatusBadGateway, 0)
		return
	}

	if err := controller.SetWriteDeadline(minTime(deadline, time.Now().Add(limits.writeTimeout))); err != nil {
		panic(http.ErrAbortHandler)
	}

	committed = true

	w.WriteHeader(status)

	if err := controller.Flush(); err != nil {
		panic(http.ErrAbortHandler)
	}

	if response.Length != 0 {
		if err := copyOrigin(ctx, controller, w, result.body, response.Length, limits.writeTimeout); err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

// callbackStatus maps an origin or SDK error to the HTTP status sent to
// Racer. A missing pinned version is a version mismatch.
func callbackStatus(err error, pinned bool) int {
	var w *wire.Error
	if errors.As(err, &w) {
		err = &sdkError{kind: w.Kind, op: w.Operation, status: w.Status, err: w.Err}
	}

	var typed *sdkError
	if errors.As(err, &typed) {
		switch typed.kind {
		case wire.ErrorHeaderLimit:
			return http.StatusRequestHeaderFieldsTooLarge
		case wire.ErrorInternal:
			return http.StatusInternalServerError
		case wire.ErrorBadGateway, wire.ErrorProtocol:
			return http.StatusBadGateway
		case wire.ErrorCanceled, wire.ErrorDeadline, wire.ErrorClosed:
			return http.StatusServiceUnavailable
		}
	}

	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable
	case errors.Is(err, ErrInvalidRequest):
		return http.StatusBadRequest
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrNotFound):
		if pinned {
			return http.StatusPreconditionFailed
		}

		return http.StatusNotFound
	case errors.Is(err, ErrVersionMismatch):
		return http.StatusPreconditionFailed
	case errors.Is(err, ErrRangeNotSatisfiable):
		return http.StatusRequestedRangeNotSatisfiable
	case errors.Is(err, ErrUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func originHeadStatus(err error) int {
	var w *wire.Error
	if errors.As(err, &w) {
		switch {
		case w.Kind == wire.ErrorHeaderLimit:
			return http.StatusRequestHeaderFieldsTooLarge
		case w.Status == http.StatusMethodNotAllowed:
			return http.StatusMethodNotAllowed
		}
	}

	var typed *sdkError
	if errors.As(err, &typed) && typed.kind == wire.ErrorHeaderLimit {
		return http.StatusRequestHeaderFieldsTooLarge
	}

	return http.StatusBadRequest
}

// An unbuffered handoff gives exactly one owner of a late callback result.
// The receiver owns the body and slot on success; cancellation leaves them with
// the callback until it returns, even when the callback ignores its context.
func awaitOrigin(ctx context.Context, origin Origin, request OriginRequest, slots chan struct{}) (originResult, bool) {
	results := make(chan originResult)

	go func() {
		result := callOrigin(ctx, origin, request)
		select {
		case results <- result:
		case <-ctx.Done():
			(&onceBody{body: result.body}).close()
			<-slots
		}
	}()

	select {
	case result := <-results:
		return result, true
	case <-ctx.Done():
		return originResult{}, false
	}
}

func prepareOriginHeaders(w http.ResponseWriter, request wire.Request, metadata wire.Metadata, response wire.Response) (int, error) {
	h, err := wire.MetadataHeaders(metadata)
	if err != nil {
		return 0, err
	}

	for name, values := range h {
		w.Header()[name] = values
	}

	length := response.Length
	if request.Operation == wire.OperationHead {
		length = int64(metadata.Size)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}

	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))

	status := http.StatusOK

	if response.Length != 0 {
		cr, err := wire.ContentRangeValue(response.First, response.Last, metadata.Size)
		if err != nil {
			return 0, err
		}

		w.Header().Set("Content-Range", cr)

		status = http.StatusPartialContent
	}

	return status, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}

	return b
}

func writeOriginErrorResponse(w http.ResponseWriter, status int, size uint64) {
	for name := range w.Header() {
		w.Header().Del(name)
	}

	w.Header().Set("Content-Length", "0")

	if status == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", "HEAD, GET")
	}

	if status == http.StatusRequestedRangeNotSatisfiable {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatUint(size, 10))
	}

	w.WriteHeader(status)
}

func probeEOF(body io.Reader) error {
	var one [1]byte
	for range 100 {
		n, err := body.Read(one[:])
		if n != 0 {
			return failure(wire.ErrorBadGateway, "origin", errors.New("body longer than range"))
		}

		if err == io.EOF {
			return nil
		}

		if err != nil {
			return err
		}
	}

	return io.ErrNoProgress
}

type originHead struct {
	request wire.Request
	err     error
	at      time.Time
}

type originConn struct {
	net.Conn
	limits         originLimits
	release        func()
	once           sync.Once
	reader         *bufio.Reader
	head           []byte
	first          bool
	mu             sync.Mutex
	pending        []originHead
	failed         bool
	raw            []byte
	headerDeadline time.Time
	interrupted    bool
}

// Close closes the socket and returns its admission slot exactly once.
func (c *originConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)

	return err
}

func (c *originConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.interrupted = !deadline.IsZero() && deadline.Before(time.Now())

	return c.Conn.SetReadDeadline(deadline)
}

func (c *originConn) setHeaderDeadline() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.interrupted {
		return nil
	}

	return c.Conn.SetReadDeadline(c.headerDeadline)
}

func (c *originConn) readHead() ([]byte, error) {
	for len(c.raw) < wire.MaxHeadBytes {
		if !time.Now().Before(c.headerDeadline) {
			return c.raw, os.ErrDeadlineExceeded
		}

		line, err := c.reader.ReadSlice('\n')
		if len(line) > wire.MaxHeadBytes-len(c.raw) {
			return c.raw, failure(wire.ErrorHeaderLimit, "wire head", nil)
		}

		c.raw = append(c.raw, line...)

		if err == bufio.ErrBufferFull {
			continue
		}

		if err != nil {
			if err == io.EOF && len(c.raw) != 0 {
				err = io.ErrUnexpectedEOF
			}

			return c.raw, err
		}

		if bytes.HasSuffix(c.raw, []byte("\r\n\r\n")) {
			return c.raw, nil
		}
	}

	return c.raw, failure(wire.ErrorHeaderLimit, "wire head", nil)
}

// Read passes only validated canonical requests to net/http. Unknown fields are
// discarded after counting toward raw limits. Malformed requests become a private
// error operation so the handler, rather than net/http's text error writer, sends
// the empty response. All reads retain this same reader, including background
// reads used by net/http to detect peer disconnects.
func (c *originConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if len(c.head) == 0 {
		if c.failed {
			return 0, io.EOF
		}

		if c.reader == nil {
			c.reader = bufio.NewReader(c.Conn)
		}

		first := !c.first
		if first {
			c.first = true

			c.headerDeadline = time.Now().Add(c.limits.readHeaderTimeout)
			if err := c.setHeaderDeadline(); err != nil {
				return 0, err
			}
		}

		if len(c.raw) == 0 {
			if _, err := c.reader.Peek(1); err != nil {
				return 0, err
			}

			if !first {
				c.headerDeadline = time.Now().Add(c.limits.readHeaderTimeout)
			}
		}

		if err := c.setHeaderDeadline(); err != nil {
			return 0, err
		}

		raw, err := c.readHead()
		at := time.Now()

		if err != nil {
			var timeout net.Error

			c.mu.Lock()
			interrupted := c.interrupted
			c.mu.Unlock()

			if errors.As(err, &timeout) && timeout.Timeout() && interrupted && at.Before(c.headerDeadline) {
				return 0, timeout
			}
		}

		c.raw = nil

		var request wire.Request
		if err == nil {
			request, err = wire.ParseRequestHead(raw, true)
		}

		entry := originHead{request: request, err: err, at: at}
		if err == nil {
			c.head, err = wire.RequestHead(request)
			if err != nil {
				return 0, err
			}

			if wire.ConnectionClose(wire.HeadHeaders(raw)) {
				c.head = append(c.head[:len(c.head)-2], []byte("Connection: close\r\n\r\n")...)
			}
		} else {
			c.failed = true
			c.head = []byte("HEAD / HTTP/1.1\r\nHost: racer\r\nConnection: close\r\n\r\n")
		}

		c.mu.Lock()
		// net/http permits at most one background byte read, so only the current
		// and next head can be resident, independent of peer pipelining volume.
		c.pending = append(c.pending, entry)
		c.mu.Unlock()
	}

	n := copy(p, c.head)

	c.head = c.head[n:]
	if len(c.head) == 0 {
		c.head = nil
	}

	return n, nil
}

func (c *originConn) takeHead() originHead {
	c.mu.Lock()
	defer c.mu.Unlock()

	h := c.pending[0]
	c.pending[0] = originHead{}
	c.pending = c.pending[1:]

	return h
}

// Admission happens before Accept, so net/http never spawns an unbounded set of
// connection goroutines waiting on the limit. Close releases each slot once.
type originListener struct {
	net.Listener
	ctx    context.Context
	slots  chan struct{}
	limits originLimits
}

// Accept reserves capacity before accepting a connection.
func (l *originListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.ctx.Done():
		return nil, l.ctx.Err()
	}

	c, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}

	return &originConn{Conn: c, limits: l.limits, release: func() { <-l.slots }}, nil
}

type onceBody struct {
	body interface{ Close() error }
	once sync.Once
}

// Callback Close is external code: suppress panic values, including during
// cancellation and late-return cleanup where net/http cannot recover them.
func (b *onceBody) close() {
	b.once.Do(func() {
		defer func() {
			if recover() != nil {
				return
			}
		}()

		closeQuietly(b.body)
	})
}

// Retain at most 16 MiB across origin servers. Active buffers remain owned by the
// admitted callback until copying returns, including noncooperative readers.
var originCopyBuffers = make(chan *[copyBufferSize]byte, 64)

func acquireOriginBuffer() *[copyBufferSize]byte {
	select {
	case buffer := <-originCopyBuffers:
		return buffer
	default:
		return new([copyBufferSize]byte)
	}
}

func releaseOriginBuffer(buffer *[copyBufferSize]byte) {
	clear(buffer[:])

	select {
	case originCopyBuffers <- buffer:
	default:
	}
}

// Keep the final byte private until the callback proves EOF. Every write is
// bounded by both the request deadline and a fresh blocked-write deadline.
func copyOrigin(ctx context.Context, controller *http.ResponseController, w io.Writer, body io.Reader, remaining int64, timeout time.Duration) error {
	buffer := acquireOriginBuffer()
	defer releaseOriginBuffer(buffer)

	buf := buffer[:]
	deadline, _ := ctx.Deadline()
	empty := 0

	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}

		n, err := body.Read(buf[:min(int64(len(buf)), remaining)])
		if n < 0 || int64(n) > min(int64(len(buf)), remaining) {
			return failure(wire.ErrorBadGateway, "origin read count", nil)
		}

		remaining -= int64(n)
		if err != nil && (err != io.EOF || remaining != 0) {
			return err
		}

		if n == 0 {
			empty++
			if empty == 100 {
				return io.ErrNoProgress
			}

			continue
		}

		empty = 0

		if remaining == 0 {
			if err == nil {
				if err := probeEOF(body); err != nil {
					return err
				}
			}
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		if err := controller.SetWriteDeadline(minTime(deadline, time.Now().Add(timeout))); err != nil {
			return err
		}

		written, err := w.Write(buf[:n])
		if err != nil {
			return err
		}

		if written != n {
			return io.ErrShortWrite
		}

		if err := controller.Flush(); err != nil {
			return err
		}
	}

	return nil
}

func listenOrigin(path string, mode os.FileMode) (*net.UnixListener, func(), error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > socketPathLimit {
		return nil, nil, failure(wire.ErrorInvalidArgument, "socket path", nil)
	}

	parent := string(filepath.Separator)

	parts := strings.Split(strings.TrimPrefix(path, parent), parent)
	for _, part := range parts[:len(parts)-1] {
		parent = filepath.Join(parent, part)

		info, err := os.Lstat(parent)
		if err != nil {
			return nil, nil, ioFailure("socket directory", err)
		}

		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, failure(wire.ErrorInvalidArgument, "socket directory", nil)
		}
	}

	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		if err == nil {
			err = os.ErrExist
		}

		return nil, nil, ioFailure("socket exists", err)
	}

	l, err := bindOriginSocket(path, mode)
	if err != nil {
		return nil, nil, ioFailure("socket bind", err)
	}

	l.SetUnlinkOnClose(false)

	info, err := os.Lstat(path)
	if err != nil {
		closeQuietly(l)
		return nil, nil, ioFailure("socket identity", err)
	}

	cleanup := func() {
		closeQuietly(l)

		current, err := os.Lstat(path)
		if err == nil && current.Mode()&os.ModeSocket != 0 && os.SameFile(info, current) && info.ModTime().Equal(current.ModTime()) {
			if err := os.Remove(path); err != nil {
				return
			}
		}
	}

	return l, cleanup, nil
}

// Never unlink the lock file: waiters must always contend on the same inode.
// A hard-linked socket witness pins identity across crashes and inode reuse.
func listenOwnedOrigin(path string, mode os.FileMode) (*net.UnixListener, func(), error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > socketPathLimit {
		return nil, nil, failure(wire.ErrorInvalidArgument, "socket path", nil)
	}

	dir, err := openOriginDirectory(filepath.Dir(path))
	if err != nil {
		return nil, nil, ioFailure("owned socket directory", err)
	}

	keepDir := false

	defer func() {
		if !keepDir {
			closeQuietly(dir)
		}
	}()

	base := fmt.Sprintf("/proc/self/fd/%d/", dir.Fd())

	lock, err := lockOriginSocket(base + ".racer-origin.lock")
	if err != nil {
		return nil, nil, err
	}

	keep := false

	defer func() {
		if !keep {
			closeQuietly(lock)
		}
	}()

	socket := base + filepath.Base(path)

	witness := base + ".racer-origin.socket"
	if err := recoverOriginSocket(socket, witness); err != nil {
		return nil, nil, ioFailure("recover origin socket", err)
	}

	l, cleanupWitness, err := listenOriginAtWitness(witness, mode)
	if err != nil {
		return nil, nil, err
	}

	identity, err := os.Lstat(witness)
	if err != nil {
		cleanupWitness()
		return nil, nil, ioFailure("socket identity", err)
	}

	if err := os.Link(witness, socket); err != nil {
		cleanupWitness()
		return nil, nil, ioFailure("publish origin socket", err)
	}

	keep = true
	keepDir = true

	var once sync.Once

	return l, func() {
		once.Do(func() {
			defer closeQuietly(lock)
			defer closeQuietly(dir)

			closeQuietly(l)

			for _, name := range []string{filepath.Base(path), ".racer-origin.socket"} {
				entry := base + name
				if current, err := os.Lstat(entry); err == nil && os.SameFile(identity, current) {
					if err := os.Remove(entry); err != nil {
						// Keep the witness if canonical cleanup failed, for recovery.
						return
					}
				}
			}
		})
	}, nil
}

func lockOriginSocket(path string) (*os.File, error) {
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, ioFailure("socket lock", err)
	}

	keep := false

	defer func() {
		if !keep {
			closeQuietly(lock)
		}
	}()

	info, err := lock.Stat()
	if err != nil || !safeOriginFile(info) {
		return nil, ioFailure("unsafe socket lock", os.ErrPermission)
	}

	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, ioFailure("socket owner active", err)
	}

	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return nil, ioFailure("socket lock replaced", os.ErrPermission)
	}

	keep = true

	return lock, nil
}

func safeOriginFile(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return false
	}

	stat, ok := info.Sys().(*syscall.Stat_t)

	return ok && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1
}

func openOriginDirectory(path string) (*os.File, error) {
	dir, err := os.Open("/")
	if err != nil {
		return nil, err
	}

	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		fd, err := unix.Openat(int(dir.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		closeQuietly(dir)

		if err != nil {
			return nil, err
		}

		dir = os.NewFile(uintptr(fd), part)
	}

	info, err := dir.Stat()
	if err != nil {
		closeQuietly(dir)
		return nil, err
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0o022 != 0 {
		closeQuietly(dir)
		return nil, os.ErrPermission
	}

	return dir, nil
}

func recoverOriginSocket(socket, witness string) error {
	owned, err := os.Lstat(witness)
	if os.IsNotExist(err) {
		if _, err := os.Lstat(socket); !os.IsNotExist(err) {
			return os.ErrExist
		}

		return nil
	}

	if err != nil {
		return err
	}

	if owned.Mode()&os.ModeSocket == 0 {
		return os.ErrPermission
	}

	current, err := os.Lstat(socket)
	if err == nil && !os.SameFile(owned, current) {
		return os.ErrExist
	}

	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// A listener without our lock (for example an inherited descriptor) is live.
	conn, probeErr := net.DialTimeout("unix", witness, time.Second)
	if probeErr == nil {
		closeQuietly(conn)
		return os.ErrExist
	}

	if !errors.Is(probeErr, unix.ECONNREFUSED) {
		return probeErr
	}

	if err == nil {
		if err := os.Remove(socket); err != nil {
			return err
		}
	}

	return os.Remove(witness)
}

func listenOriginAtWitness(path string, mode os.FileMode) (*net.UnixListener, func(), error) {
	// The caller pinned and validated the directory; /proc/self/fd is intentional.
	l, err := bindOriginSocket(path, mode)
	if err != nil {
		return nil, nil, ioFailure("socket bind", err)
	}

	l.SetUnlinkOnClose(false)

	identity, err := os.Lstat(path)
	if err != nil {
		closeQuietly(l)
		return nil, nil, ioFailure("witness identity", err)
	}

	cleanup := func() {
		closeQuietly(l)

		if current, err := os.Lstat(path); err == nil && os.SameFile(identity, current) {
			if err := os.Remove(path); err != nil {
				return
			}
		}
	}

	return l, cleanup, nil
}

func bindOriginSocket(path string, mode os.FileMode) (*net.UnixListener, error) {
	// Bind behind a private directory so even a permissive umask cannot expose
	// the socket before chmod. A hard link publishes without replacing a path.
	stage, err := os.MkdirTemp(filepath.Dir(path), ".racer-origin-")
	if err != nil {
		return nil, err
	}

	defer func() {
		if err := os.RemoveAll(stage); err != nil {
			return
		}
	}()

	dir, err := os.Open(stage)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(dir)

	// Use the directory FD to avoid consuming the public socket's path budget.
	private := fmt.Sprintf("/proc/self/fd/%d/socket", dir.Fd())

	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: private, Net: "unix"})
	if err != nil {
		return nil, err
	}

	l.SetUnlinkOnClose(false)

	if err := os.Chmod(private, mode); err != nil {
		closeQuietly(l)
		return nil, err
	}

	if err := os.Link(private, path); err != nil {
		closeQuietly(l)
		return nil, err
	}

	return l, nil
}
