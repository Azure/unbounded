// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
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

// OriginConfig selects the canonical cache endpoint and bounds server resources.
// Zero numeric fields select defaults; negative values are invalid. The endpoint
// directory must already exist and its ancestors must not be symlinks or writable
// by untrusted peers. The SDK does not create or change parent directories.
type OriginConfig struct {
	Cache CacheName
	// MaxConnections includes idle accepted connections (default 128).
	MaxConnections int
	// MaxConcurrentRequests bounds callbacks and bodies, with empty 503 on overload (default 64).
	MaxConcurrentRequests int
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
}

func (c OriginConfig) defaults() (OriginConfig, error) {
	if _, err := ParseCacheName(c.Cache.value); err != nil {
		return c, err
	}

	if c.MaxConnections < 0 || c.MaxConcurrentRequests < 0 || c.ReadHeaderTimeout < 0 || c.RequestTimeout < 0 || c.WriteTimeout < 0 || c.IdleTimeout < 0 || c.SocketMode & ^os.FileMode(0o777) != 0 {
		return c, failure(ErrorInvalidArgument, "origin config", nil)
	}

	if c.MaxConnections == 0 {
		c.MaxConnections = 128
	}

	if c.MaxConcurrentRequests == 0 {
		c.MaxConcurrentRequests = 64
	}

	if c.ReadHeaderTimeout == 0 {
		c.ReadHeaderTimeout = 5 * time.Second
	}

	if c.RequestTimeout == 0 {
		c.RequestTimeout = 60 * time.Second
	}

	if c.WriteTimeout == 0 {
		c.WriteTimeout = 30 * time.Second
	}

	if c.IdleTimeout == 0 {
		c.IdleTimeout = 30 * time.Second
	}

	if c.SocketMode == 0 {
		c.SocketMode = 0o600
	}

	return c, nil
}

// ServeOrigin binds /run/racer/<cache>/origin/socket and serves until cancellation
// or a listener failure. Existing paths (including stale sockets) are refused.
// Cleanup removes only this invocation's socket inode, preserving replacements.
// Cancellation closes connections and bodies and returns ctx.Err() without waiting
// for noncooperative callbacks; a late-returned body is still closed. Callbacks
// that ignore cancellation continue occupying their bounded admission slot.
func ServeOrigin(ctx context.Context, config OriginConfig, origin Origin) error {
	return serveOrigin(ctx, config, origin, "/run/racer/"+config.Cache.value+"/origin/socket")
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

	l, cleanup, err := listenOrigin(path, config.SocketMode)
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
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveOperation(w, r, config, origin, slots) })

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

func serveOperation(w http.ResponseWriter, r *http.Request, config OriginConfig, origin Origin, slots chan struct{}) {
	writeOriginError := func(w http.ResponseWriter, status int, metadata Metadata) {
		// Once a request expires, only the empty error response gets a fresh,
		// bounded write opportunity. Success writes never extend its deadline.
		if status == 503 {
			if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(config.WriteTimeout)); err != nil {
				return
			}
		}

		writeOriginError(w, status, metadata)
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
		status := 400

		var typed *Error
		if errors.As(head.err, &typed) {
			if typed.Kind() == ErrorHeaderLimit {
				status = 431
			}

			if typed.StatusCode() == 405 {
				status = 405
			}
		}

		writeOriginError(w, status, Metadata{})

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

	select {
	case slots <- struct{}{}:
	default:
		writeOriginError(w, 503, Metadata{})
		return
	}
	// An unbuffered handoff gives exactly one owner of a late callback result.
	results := make(chan originResult)

	go func() {
		result := callOrigin(ctx, origin, head.request)
		select {
		case results <- result:
		case <-ctx.Done():
			(&onceBody{body: result.body}).close()
			<-slots
		}
	}()

	var result originResult
	select {
	case result = <-results:
		defer func() { <-slots }()
	case <-ctx.Done():
		writeOriginError(w, 503, Metadata{})

		return
	}

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

	h, err := metadataHeaders(result.metadata)
	if err != nil {
		writeOriginError(w, 502, Metadata{})
		return
	}

	for name, values := range h {
		w.Header()[name] = values
	}

	length := response.length
	if head.request.operation == OperationHead {
		length = int64(result.metadata.Size)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}

	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))

	status := 200

	if response.length != 0 {
		cr, err := contentRangeValue(response.first, response.last, result.metadata.Size)
		if err != nil {
			panic(http.ErrAbortHandler)
		}

		w.Header().Set("Content-Range", cr)

		status = 206
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

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}

	return b
}

func writeOriginError(w http.ResponseWriter, status int, metadata Metadata) {
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

// Keep the final byte private until the callback proves EOF. Every write is
// bounded by both the request deadline and a fresh blocked-write deadline.
func copyOrigin(ctx context.Context, controller *http.ResponseController, w io.Writer, body io.Reader, remaining int64, timeout time.Duration) error {
	buf := make([]byte, copyBufferSize)
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
