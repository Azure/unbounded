// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// copyBufferSize bounds each batch moved by WriteTo and the scratch buffer it
// uses when the destination cannot splice.
const copyBufferSize = 256 * 1024

var copyBuffers = sync.Pool{New: func() any { return new([copyBufferSize]byte) }}

// Object is an in-progress read returned by [Client.Get]. Consume it with
// either [Object.Read] or [Object.WriteTo] and always call [Object.Close].
//
// Read copies data into your buffer, so you can inspect, hash, or transform
// it. WriteTo hands data straight to a destination and, when the destination
// is backed by a file descriptor, never copies it into process memory. Prefer
// WriteTo whenever you only forward bytes; see the package documentation for
// details.
//
// Both methods withhold the last byte of the read until Racer confirms the
// transfer completed, so a successful read is never silently truncated.
//
// One goroutine at a time may call Read or WriteTo. Metadata and Close may be
// called concurrently with them.
type Object struct {
	ctx      context.Context
	cancel   func()      // ends ctx
	stop     func() bool // disarms the close-on-cancel callback
	admitted func()      // returns the admission slot
	conn     *clientConn
	timeout  time.Duration
	metadata Metadata

	mu   sync.Mutex
	err  error // terminal: io.EOF after success
	busy bool
	done bool

	// Owned by the goroutine that is reading (busy).
	seq        *wire.Sequence
	first, end uint64
	pages      uint64
	delivered  uint64
	page       wire.Credit
	inPage     bool
	lastPage   bool
	remaining  int64
	final      [1]byte
	finalReady bool
	completed  bool
}

// Metadata describes the version being read. Size is the whole object, not
// the selected range.
func (o *Object) Metadata() Metadata { return o.metadata }

// Read copies the next bytes of the object into p. It returns [io.EOF] once
// the whole selected range has been read and verified complete.
//
// Every byte is copied from the kernel into p. Reads smaller than 4 KiB are
// staged through an internal buffer and copied twice; use buffers of 32 KiB
// or more for throughput.
func (o *Object) Read(p []byte) (int, error) {
	if err := o.begin(); err != nil {
		return 0, err
	}

	if len(p) == 0 {
		return 0, o.finish("read", nil)
	}

	n, err := o.read(p)

	return n, o.finish("read", err)
}

// WriteTo writes the rest of the object to w and returns the number of bytes
// written. [io.Copy] calls it automatically. A nil error means the whole
// selected range was written and Racer confirmed it complete.
//
// If w implements [io.ReaderFrom] and is backed by a file descriptor, such as
// a [net.Conn], an [*os.File], or an HTTP/1 [http.ResponseWriter], data moves
// from Racer's socket to w inside the kernel with splice(2), with no copy into
// process memory. Other writers receive data through a reused 256 KiB buffer.
//
// When w is an [http.ResponseWriter], WriteTo applies a write deadline to each
// write and interrupts a blocked write if the Get context ends. Set response
// headers, including Content-Length, before calling WriteTo.
func (o *Object) WriteTo(w io.Writer) (int64, error) {
	if err := o.begin(); err != nil {
		if err == io.EOF {
			return 0, nil
		}

		return 0, err
	}

	if w == nil {
		return 0, o.finish("write", invalid("write", errors.New("nil writer")))
	}

	n, err := o.writeTo(w)
	if err == nil {
		err = io.EOF
	}

	if err = o.finish("write", err); err == io.EOF {
		err = nil
	}

	return n, err
}

// Close stops the read and releases its connection. It is safe to call more
// than once and concurrently with Read or WriteTo, which then fail with an
// error wrapping [net.ErrClosed]. Close always returns nil.
func (o *Object) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.err == nil {
		o.err = closedError("read")
	}

	if o.busy {
		// Interrupt the reader; it releases resources when it returns.
		o.cancel()
		closeQuietly(o.conn)

		return nil
	}

	o.cleanupLocked()

	return nil
}

func (o *Object) begin() error {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.err != nil {
		return o.err
	}

	if o.busy {
		return invalid("read", errors.New("concurrent Read or WriteTo"))
	}

	o.busy = true

	return nil
}

// finish ends a Read or WriteTo call. A non-nil err ends the object.
func (o *Object) finish(op string, err error) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.busy = false

	if err == nil && o.err == nil {
		return nil
	}

	if o.err == nil {
		switch {
		case err == io.EOF:
			o.err = io.EOF
		case o.ctx.Err() != nil:
			o.err = contextError(op, o.ctx)
		default:
			o.err = ioFailure(op, err)
		}
	}

	o.cleanupLocked()

	return o.err
}

func (o *Object) cleanupLocked() {
	if o.done {
		return
	}

	o.done = true
	o.stop()
	closeQuietly(o.conn)
	o.cancel()
	o.admitted()
}

func (o *Object) read(p []byte) (int, error) {
	if err := o.advance(); err != nil {
		return 0, err
	}

	if o.finalReady {
		o.finalReady = false
		p[0] = o.final[0]

		return 1, io.EOF
	}

	n := int(min(int64(len(p)), o.remaining))

	if err := o.conn.SetReadDeadline(time.Now().Add(o.timeout)); err != nil {
		return 0, err
	}

	n, err := o.conn.r.Read(p[:n])
	o.remaining -= int64(n)

	// Clearing the deadline must not discard bytes already received.
	_ = o.conn.SetReadDeadline(time.Time{}) //nolint:errcheck // See above.

	if err != nil {
		return n, truncation(err)
	}

	if n == 0 {
		return 0, io.ErrNoProgress
	}

	return n, nil
}

func (o *Object) writeTo(w io.Writer) (int64, error) {
	dst := newDestination(o.ctx, w, o.timeout)
	stop := dst.interruptOnCancel()

	defer stop()

	var (
		written int64
		buffer  *[copyBufferSize]byte
	)

	defer func() {
		if buffer != nil {
			copyBuffers.Put(buffer)
		}
	}()

	for {
		err := o.advance()
		if err == io.EOF {
			// Disarm the interrupt before cleanup ends ctx: an expired HTTP/2
			// write deadline resets the stream even if it is cleared later.
			stop()

			if err := o.ctx.Err(); err != nil {
				return written, err
			}

			return written, nil
		}

		if err != nil {
			return written, err
		}

		if o.finalReady {
			n, err := dst.write(o.final[:])
			written += n

			if err != nil {
				return written, err
			}

			o.finalReady = false

			continue
		}

		raw, unix := o.conn.Conn.(*net.UnixConn)
		if o.conn.r.Buffered() == 0 && unix && dst.rf != nil {
			n, err := o.splice(dst, raw)
			written += n

			if err != nil {
				return written, err
			}

			continue
		}

		if buffer == nil {
			buffer = copyBuffers.Get().(*[copyBufferSize]byte) //nolint:errcheck,forcetypeassert // Pool only holds this type.
		}

		n, err := o.copy(dst, buffer[:])
		written += n

		if err != nil {
			return written, err
		}
	}
}

// splice moves one batch from the socket to dst inside the kernel when
// possible. The read side must have no buffered bytes.
func (o *Object) splice(dst *destination, raw *net.UnixConn) (int64, error) {
	batch := min(o.remaining, copyBufferSize)

	if err := o.conn.SetReadDeadline(time.Now().Add(o.timeout)); err != nil {
		return 0, err
	}

	if err := dst.arm(); err != nil {
		return 0, err
	}

	limited := &io.LimitedReader{R: raw, N: batch}
	n, err := dst.rf.ReadFrom(limited)

	if clearErr := dst.disarm(); err == nil {
		err = clearErr
	}

	_ = o.conn.SetReadDeadline(time.Time{}) //nolint:errcheck // Preserve bytes received before a peer close.

	o.remaining -= batch - limited.N

	if n < 0 || n > batch {
		n = 0

		if err == nil {
			err = io.ErrShortWrite
		}
	}

	if err == nil && limited.N != 0 {
		err = io.ErrUnexpectedEOF
	}

	if err == nil && n != batch {
		err = io.ErrShortWrite
	}

	return n, truncation(err)
}

// copy moves one batch through buffer. It reads once rather than filling the
// buffer, so partially received pages flow to dst promptly.
func (o *Object) copy(dst *destination, buffer []byte) (int64, error) {
	batch := min(o.remaining, int64(len(buffer)))
	if buffered := o.conn.r.Buffered(); buffered > 0 {
		batch = min(batch, int64(buffered))
	}

	if err := o.conn.SetReadDeadline(time.Now().Add(o.timeout)); err != nil {
		return 0, err
	}

	n, readErr := o.conn.r.Read(buffer[:batch])
	o.remaining -= int64(n)

	_ = o.conn.SetReadDeadline(time.Time{}) //nolint:errcheck // Preserve bytes received before a peer close.

	written, err := dst.write(buffer[:n])
	if err != nil {
		return written, err
	}

	if readErr != nil {
		return written, truncation(readErr)
	}

	if n == 0 {
		return 0, io.ErrNoProgress
	}

	return written, nil
}

// advance positions the object at the next unread payload byte, releasing
// page credits and validating frames along the way. It returns io.EOF once the
// completion frame has been validated and every byte delivered.
func (o *Object) advance() error {
	for o.remaining == 0 && !o.finalReady {
		if o.inPage {
			o.inPage = false

			if o.lastPage {
				if err := o.finishLastPage(); err != nil {
					return err
				}

				continue
			}

			if err := o.credit(o.page); err != nil {
				return err
			}
		}

		if o.completed {
			return io.EOF
		}

		if o.delivered == o.pages {
			// An empty range has no pages, only a completion frame.
			if err := o.readComplete(); err != nil {
				return err
			}

			o.completed = true
			closeQuietly(o.conn)

			return io.EOF
		}

		f, err := o.readFrame()
		if err != nil {
			return err
		}

		if f.Kind != wire.PageFrame || f.Length == 0 {
			return failure(wire.ErrorProtocol, "read", errors.New("unexpected frame"))
		}

		o.delivered++
		o.page = wire.Credit{Number: f.Number, Length: f.Length}
		o.inPage = true
		o.lastPage = o.delivered == o.pages

		o.remaining = int64(f.Length)
		if o.lastPage {
			// Withhold the final byte until completion is confirmed.
			o.remaining--
		}
	}

	return nil
}

// finishLastPage reads the withheld final byte and the completion frame.
// Racer may wait for the final credit before sending completion, or send
// completion without reading the credit, so both happen concurrently.
func (o *Object) finishLastPage() error {
	if err := o.readFull(o.final[:]); err != nil {
		return err
	}

	released := make(chan struct{})

	go func() {
		defer close(released)

		// The read side decides whether the transfer completed.
		_ = o.credit(o.page) //nolint:errcheck // See above.
	}()

	err := o.readComplete()

	closeQuietly(o.conn)
	<-released

	if err != nil {
		return err
	}

	o.completed = true
	o.finalReady = true

	return nil
}

func (o *Object) readFrame() (wire.Frame, error) {
	var raw [wire.FrameSize]byte
	if err := o.readFull(raw[:]); err != nil {
		return wire.Frame{}, err
	}

	f := wire.DecodeFrame(raw)
	if err := o.seq.Accept(f, "read"); err != nil {
		return wire.Frame{}, err
	}

	return f, nil
}

func (o *Object) readComplete() error {
	f, err := o.readFrame()
	if err != nil {
		return err
	}

	if f.Kind != wire.CompleteFrame {
		return failure(wire.ErrorProtocol, "read", errors.New("missing completion"))
	}

	return nil
}

func (o *Object) readFull(p []byte) error {
	for len(p) > 0 {
		if err := o.conn.SetReadDeadline(time.Now().Add(o.timeout)); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			return err
		}

		n, err := o.conn.r.Read(p)
		p = p[n:]

		_ = o.conn.SetReadDeadline(time.Time{}) //nolint:errcheck // Preserve bytes received before a peer close.

		if err != nil {
			return truncation(err)
		}

		if n == 0 {
			return io.ErrNoProgress
		}
	}

	return nil
}

// credit returns a consumed page's credit so Racer can send another page.
func (o *Object) credit(page wire.Credit) error {
	frame := page.Encode()

	err := o.conn.SetWriteDeadline(time.Now().Add(o.timeout))
	if err == nil {
		var n int

		n, err = o.conn.Write(frame[:])
		if err == nil && n != len(frame) {
			err = io.ErrShortWrite
		}
	}

	if clearErr := o.conn.SetWriteDeadline(time.Time{}); err == nil {
		err = clearErr
	}

	// Racer may finish and close while a credit is in flight; the next read
	// reports whether the transfer was actually cut short.
	if err != nil && !staleConnectionError(err) && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, net.ErrClosed) {
		return err
	}

	return nil
}

// truncation reports a peer close or reset during a transfer as an unexpected
// EOF, keeping a reset as the underlying cause.
func truncation(err error) error {
	switch {
	case err == nil:
		return nil
	case err == io.EOF:
		return io.ErrUnexpectedEOF
	case staleConnectionError(err):
		return fmt.Errorf("%w: %w", io.ErrUnexpectedEOF, err)
	default:
		return err
	}
}

// destination wraps a WriteTo writer. For an http.ResponseWriter it bounds
// each write with a deadline and can interrupt a blocked write.
type destination struct {
	ctx     context.Context
	w       io.Writer
	rf      io.ReaderFrom
	rc      *http.ResponseController
	timeout time.Duration
	mu      sync.Mutex
}

func newDestination(ctx context.Context, w io.Writer, timeout time.Duration) *destination {
	d := &destination{ctx: ctx, w: w, timeout: timeout}
	if rf, ok := w.(io.ReaderFrom); ok {
		d.rf = rf
	}

	if rw, ok := w.(http.ResponseWriter); ok {
		d.rc = http.NewResponseController(rw)
	}

	return d
}

// interruptOnCancel returns an idempotent stop-and-join function.
func (d *destination) interruptOnCancel() func() {
	if d.rc == nil {
		return func() {}
	}

	done := make(chan struct{})
	stop := context.AfterFunc(d.ctx, func() {
		defer close(done)

		d.mu.Lock()
		defer d.mu.Unlock()

		_ = d.rc.SetWriteDeadline(time.Now()) //nolint:errcheck // Unsupported writers cannot be interrupted.
	})

	return sync.OnceFunc(func() {
		if !stop() {
			<-done
		}
	})
}

func (d *destination) deadline(t time.Time) error {
	if d.rc == nil {
		return nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Never overwrite the immediate deadline set by a cancellation.
	if err := d.ctx.Err(); err != nil {
		return err
	}

	if err := d.rc.SetWriteDeadline(t); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}

	return nil
}

func (d *destination) arm() error { return d.deadline(time.Now().Add(d.timeout)) }

func (d *destination) disarm() error { return d.deadline(time.Time{}) }

func (d *destination) write(p []byte) (int64, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if err := d.arm(); err != nil {
		return 0, err
	}

	n, err := d.w.Write(p)
	if clearErr := d.disarm(); err == nil {
		err = clearErr
	}

	if n < 0 || n > len(p) {
		n = 0

		if err == nil {
			err = io.ErrShortWrite
		}
	}

	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}

	return int64(n), err
}
