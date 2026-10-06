// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
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
)

// Origin atomically selects metadata and opens the requested immutable version.
// It is called once per operation, including HEAD, and may be called concurrently.
// HEAD requires a nil body; GET returns exactly the resolved page bytes (an empty
// bootstrap may use nil). Every nonnil body transfers to the SDK even on error.
// The callback must honor ctx, and body.Close must promptly interrupt Read and be
// safe concurrently with it. The SDK closes each body once, probes EOF, and aborts
// late failures. Panics are recovered without logging their values. Origins must
// support repeated read operations because HTTP clients can replay failed reads.
type Origin func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error)

// OriginConfig selects the canonical volume endpoint and bounds server resources.
// Zero numeric fields select defaults; negative values are invalid. The endpoint
// directory must already exist and its ancestors must not be symlinks or writable
// by untrusted peers. The SDK does not create or change parent directories.
type OriginConfig struct {
	// Volume selects the canonical origin endpoint.
	Volume VolumeName
	// MaxConnections includes idle accepted connections (default 128).
	MaxConnections int
	// MaxConcurrentRequests bounds GET callbacks and bodies, with empty 503 on overload (default 64).
	MaxConcurrentRequests int
	// MaxConcurrentHeadRequests reserves HEAD callback lifetimes (default 4).
	// Canceled callbacks that ignore context retain their slot until returning.
	MaxConcurrentHeadRequests int
	// ReadHeaderTimeout bounds each raw head (default 5 seconds).
	ReadHeaderTimeout time.Duration
	// RequestTimeout bounds callback, body, EOF probe, and final success write
	// (default 60 seconds). Expiry before headers sends empty 503 with a fresh
	// WriteTimeout allowance; it never extends a successful stream's deadline.
	RequestTimeout time.Duration
	// WriteTimeout bounds each blocked bounded write (default 30 seconds).
	WriteTimeout time.Duration
	// IdleTimeout bounds waiting for a subsequent request (default 30 seconds).
	IdleTimeout time.Duration
	// SocketMode contains only permission bits and defaults to 0600.
	SocketMode os.FileMode
	// RecoverStaleSocket opts into exclusive endpoint ownership using persistent
	// lock and socket witness files. Only sockets created in this mode are recovered.
	// The directory must be owned by this user and not group/world writable.
	RecoverStaleSocket bool
}

func (c OriginConfig) defaults() (OriginConfig, error) {
	if _, err := ParseVolumeName(c.Volume.value); err != nil {
		return c, err
	}

	if c.MaxConnections < 0 || c.MaxConcurrentRequests < 0 || c.MaxConcurrentHeadRequests < 0 ||
		c.ReadHeaderTimeout < 0 || c.RequestTimeout < 0 || c.WriteTimeout < 0 || c.IdleTimeout < 0 ||
		c.SocketMode & ^os.FileMode(0o777) != 0 {
		return c, failure(ErrorInvalidArgument, "origin config", nil)
	}

	defaultIfZero(&c.MaxConnections, 128)
	defaultIfZero(&c.MaxConcurrentRequests, 64)
	defaultIfZero(&c.MaxConcurrentHeadRequests, 4)
	defaultIfZero(&c.ReadHeaderTimeout, 5*time.Second)
	defaultIfZero(&c.RequestTimeout, 60*time.Second)
	defaultIfZero(&c.WriteTimeout, 30*time.Second)
	defaultIfZero(&c.IdleTimeout, 30*time.Second)
	defaultIfZero(&c.SocketMode, 0o600)

	return c, nil
}

// ServeOrigin binds /run/racer/<volume>/origin/socket and serves until cancellation
// or a listener failure. Existing paths (including stale sockets) are refused
// unless RecoverStaleSocket explicitly enables recovery of an owned endpoint.
// Cleanup removes only this invocation's socket inode, preserving replacements.
// Cancellation closes connections and bodies and returns ctx.Err() without waiting
// for noncooperative callbacks; a late-returned body is still closed. Callbacks
// that ignore cancellation continue occupying their bounded admission slot.
func ServeOrigin(ctx context.Context, config OriginConfig, origin Origin) error {
	return serveOrigin(ctx, config, origin, "/run/racer/"+config.Volume.value+"/origin/socket")
}

type originConnKey struct{}

func serveOrigin(ctx context.Context, config OriginConfig, origin Origin, path string) error {
	if ctx == nil || origin == nil {
		return failure(ErrorInvalidArgument, "origin", nil)
	}

	config, err := config.defaults()
	if err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	listen := listenOrigin
	if config.RecoverStaleSocket {
		listen = listenOwnedOrigin
	}

	l, cleanup, err := listen(path, config.SocketMode)
	if err != nil {
		return err
	}
	defer cleanup()

	lifetime, cancel := context.WithCancel(ctx)
	defer cancel()

	listener := &originListener{Listener: l, ctx: lifetime, slots: make(chan struct{}, config.MaxConnections), config: config}
	server := &http.Server{
		ReadHeaderTimeout: config.ReadHeaderTimeout, IdleTimeout: config.IdleTimeout,
		WriteTimeout:   config.WriteTimeout,
		MaxHeaderBytes: maxHeadBytes, ErrorLog: log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context { return lifetime },
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, originConnKey{}, c)
		},
	}
	slots := make(chan struct{}, config.MaxConcurrentRequests)
	headSlots := make(chan struct{}, config.MaxConcurrentHeadRequests)
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveOperation(w, r, config, origin, slots, headSlots) })

	stop := context.AfterFunc(lifetime, func() { closeBody(server) })
	defer stop()

	err = server.Serve(listener)
	closeBody(server)

	if ctx.Err() != nil {
		return ctx.Err()
	}

	return ioFailure("origin serve", err)
}

type originResult struct {
	metadata Metadata
	body     io.ReadCloser
	err      error
}

func callOrigin(ctx context.Context, origin Origin, request OriginRequest) (result originResult) {
	defer func() {
		if recover() != nil {
			result.err = NewOriginError(ErrorInternal, nil)
		}
	}()

	result.metadata, result.body, result.err = origin(ctx, request)

	return result
}

func serveOperation(w http.ResponseWriter, r *http.Request, config OriginConfig, origin Origin, slots, headSlots chan struct{}) {
	writeOriginError := func(w http.ResponseWriter, status int, metadata Metadata) {
		// Once a request expires, only the empty error response gets a fresh,
		// bounded write opportunity. Success writes never extend its deadline.
		if status == 503 {
			if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(config.WriteTimeout)); err != nil {
				return
			}
		}

		writeOriginErrorResponse(w, status, metadata)
	}
	committed := false

	defer func() {
		if recover() != nil {
			if committed {
				panic(http.ErrAbortHandler)
			}

			writeOriginError(w, 500, Metadata{})
		}
	}()

	conn, ok := r.Context().Value(originConnKey{}).(*originConn)
	if !ok {
		writeOriginError(w, 500, Metadata{})
		return
	}

	head := conn.takeHead()
	if head.err != nil {
		writeOriginError(w, originHeadStatus(head.err), Metadata{})

		return
	}

	ctx, cancel := context.WithDeadline(r.Context(), head.at.Add(config.RequestTimeout))
	defer cancel()

	if ctx.Err() != nil {
		writeOriginError(w, 503, Metadata{})
		return
	}

	controller := http.NewResponseController(w)

	deadline, _ := ctx.Deadline()
	if err := controller.SetWriteDeadline(minTime(deadline, time.Now().Add(config.WriteTimeout))); err != nil {
		panic(http.ErrAbortHandler)
	}

	if head.request.operation == OperationHead {
		slots = headSlots
	}

	select {
	case slots <- struct{}{}:
	default:
		writeOriginError(w, 503, Metadata{})
		return
	}

	result, received := awaitOrigin(ctx, origin, head.request, slots)
	if !received {
		writeOriginError(w, 503, Metadata{})
		return
	}

	defer func() { <-slots }()

	body := &onceBody{body: result.body}
	defer body.close()

	stop := context.AfterFunc(ctx, body.close)
	defer stop()

	if ctx.Err() != nil {
		writeOriginError(w, 503, Metadata{})
		return
	}

	if result.err != nil {
		status := callbackStatus(result.err, head.request.pin.value != "")
		if status == 416 && (result.metadata.Validate() != nil || head.request.pin.value != "" && head.request.pin != result.metadata.ETag) {
			status = 502
		}

		writeOriginError(w, status, result.metadata)

		return
	}

	response, err := originResponse(head.request, result.metadata)
	if err != nil {
		writeOriginError(w, callbackStatus(err, head.request.pin.value != ""), result.metadata)
		return
	}

	if head.request.operation == OperationHead && result.body != nil || response.length != 0 && result.body == nil {
		writeOriginError(w, 502, Metadata{})
		return
	}

	if response.length == 0 && result.body != nil {
		if err := probeEOF(result.body); err != nil {
			status := 502
			if ctx.Err() != nil {
				status = 503
			}

			writeOriginError(w, status, Metadata{})

			return
		}
	}

	if ctx.Err() != nil {
		writeOriginError(w, 503, Metadata{})
		return
	}

	status, err := prepareOriginHeaders(w, head.request, result.metadata, response)
	if err != nil {
		writeOriginError(w, 502, Metadata{})
		return
	}

	if err := controller.SetWriteDeadline(minTime(deadline, time.Now().Add(config.WriteTimeout))); err != nil {
		panic(http.ErrAbortHandler)
	}

	committed = true

	w.WriteHeader(status)

	if err := controller.Flush(); err != nil {
		panic(http.ErrAbortHandler)
	}

	if response.length != 0 {
		if err := copyOrigin(ctx, controller, w, result.body, response.length, config.WriteTimeout); err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

func originHeadStatus(err error) int {
	status := 400

	var typed *Error
	if errors.As(err, &typed) {
		if typed.Kind() == ErrorHeaderLimit {
			status = 431
		}

		if typed.StatusCode() == 405 {
			status = 405
		}
	}

	return status
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

func prepareOriginHeaders(w http.ResponseWriter, request OriginRequest, metadata Metadata, response wireResponse) (int, error) {
	h, err := metadataHeaders(metadata)
	if err != nil {
		return 0, err
	}

	for name, values := range h {
		w.Header()[name] = values
	}

	length := response.length
	if request.operation == OperationHead {
		length = int64(metadata.Size)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}

	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))

	status := 200

	if response.length != 0 {
		cr, err := contentRangeValue(response.first, response.last, metadata.Size)
		if err != nil {
			panic(http.ErrAbortHandler)
		}

		w.Header().Set("Content-Range", cr)

		status = 206
	}

	return status, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}

	return b
}

func writeOriginErrorResponse(w http.ResponseWriter, status int, metadata Metadata) {
	for name := range w.Header() {
		w.Header().Del(name)
	}

	w.Header().Set("Content-Length", "0")

	if status == 405 {
		w.Header().Set("Allow", "HEAD, GET")
	}

	if status == 416 {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatUint(uint64(metadata.Size), 10))
	}

	w.WriteHeader(status)
}

func probeEOF(body io.Reader) error {
	var one [1]byte
	for range 100 {
		n, err := body.Read(one[:])
		if n != 0 {
			return failure(ErrorBadGateway, "origin excess body", nil)
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
	request OriginRequest
	err     error
	at      time.Time
}

type originConn struct {
	net.Conn
	config  OriginConfig
	release func()
	once    sync.Once
	reader  *bufio.Reader
	head    []byte
	first   bool
	mu      sync.Mutex
	pending []originHead
	failed  bool
}

// Close closes the socket and returns its admission slot exactly once.
func (c *originConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)

	return err
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
			if err := c.SetReadDeadline(time.Now().Add(c.config.ReadHeaderTimeout)); err != nil {
				return 0, err
			}
		}

		if _, err := c.reader.Peek(1); err != nil {
			return 0, err
		}

		if !first {
			if err := c.SetReadDeadline(time.Now().Add(c.config.ReadHeaderTimeout)); err != nil {
				return 0, err
			}
		}

		raw, err := readHeadBytes(c.reader, false)
		at := time.Now()

		var request OriginRequest
		if err == nil {
			request, err = parseRequestHead(raw, true)
		}

		entry := originHead{request: request, err: err, at: at}
		if err == nil {
			c.head, err = requestHead(request)
			if err != nil {
				return 0, err
			}

			if connectionClose(headHeaders(raw)) {
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
	config OriginConfig
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

	return &originConn{Conn: c, config: l.config, release: func() { <-l.slots }}, nil
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

		closeBody(b.body)
	})
}

// Retain at most 2 MiB across origin servers. Active buffers remain owned by the
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
			return failure(ErrorBadGateway, "origin read count", nil)
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
		return nil, nil, failure(ErrorInvalidArgument, "socket path", nil)
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
			return nil, nil, failure(ErrorInvalidArgument, "socket directory", nil)
		}
	}

	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		if err == nil {
			err = os.ErrExist
		}

		return nil, nil, ioFailure("socket exists", err)
	}

	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, nil, ioFailure("socket bind", err)
	}

	l.SetUnlinkOnClose(false)

	info, err := os.Lstat(path)
	if err != nil {
		closeBody(l)
		return nil, nil, ioFailure("socket identity", err)
	}

	cleanup := func() {
		closeBody(l)

		current, err := os.Lstat(path)
		if err == nil && current.Mode()&os.ModeSocket != 0 && os.SameFile(info, current) && info.ModTime().Equal(current.ModTime()) {
			if err := os.Remove(path); err != nil {
				return
			}
		}
	}
	if err := os.Chmod(path, mode); err != nil {
		cleanup()
		return nil, nil, ioFailure("socket mode", err)
	}

	return l, cleanup, nil
}

// Never unlink the lock file: waiters must always contend on the same inode.
// A hard-linked socket witness pins identity across crashes and inode reuse.
func listenOwnedOrigin(path string, mode os.FileMode) (*net.UnixListener, func(), error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > socketPathLimit {
		return nil, nil, failure(ErrorInvalidArgument, "socket path", nil)
	}

	dir, err := openOriginDirectory(filepath.Dir(path))
	if err != nil {
		return nil, nil, ioFailure("owned socket directory", err)
	}

	keepDir := false

	defer func() {
		if !keepDir {
			closeBody(dir)
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
			closeBody(lock)
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
			defer closeBody(lock)
			defer closeBody(dir)

			closeBody(l)

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
			closeBody(lock)
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
		closeBody(dir)

		if err != nil {
			return nil, err
		}

		dir = os.NewFile(uintptr(fd), part)
	}

	info, err := dir.Stat()
	if err != nil {
		closeBody(dir)
		return nil, err
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0o022 != 0 {
		closeBody(dir)
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
		closeBody(conn)
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
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, nil, ioFailure("socket bind", err)
	}

	l.SetUnlinkOnClose(false)

	identity, err := os.Lstat(path)
	if err != nil {
		closeBody(l)
		return nil, nil, ioFailure("witness identity", err)
	}

	cleanup := func() {
		closeBody(l)

		if current, err := os.Lstat(path); err == nil && os.SameFile(identity, current) {
			if err := os.Remove(path); err != nil {
				return
			}
		}
	}
	if err := os.Chmod(path, mode); err != nil {
		cleanup()
		return nil, nil, ioFailure("socket mode", err)
	}

	return l, cleanup, nil
}
