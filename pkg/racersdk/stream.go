// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// PageLease owns one verified page slice. Data is valid until Release. Do not
// copy a lease or access Data concurrently with Release. Release is idempotent.
type PageLease struct {
	// Number is the absolute object page number.
	Number uint64
	// Offset is the absolute object offset of the first byte in Data.
	Offset ByteOffset
	// Data is the verified selected slice, valid until Release.
	Data   []byte
	stream *PageStream
	number uint64
	length uint32
	buffer []byte
	once   sync.Once
	err    error
}

// Release returns the page's credits and invalidates Data exactly once.
func (p *PageLease) Release() error {
	if p == nil || p.stream == nil {
		return failure(ErrorInvalidArgument, "page lease", nil)
	}

	p.once.Do(func() {
		p.err = p.stream.release(p.number, p.length, false)
		p.Data = nil
		p.stream.putBuffer(p.buffer)
		p.buffer = nil
	})

	return p.err
}

// PageStream receives page slices, unordered unless Ordered was requested.
// One goroutine calls Next; Release and Close may run concurrently. Call Close
// on every path. Next waits when credits are held by outstanding leases.
type PageStream struct {
	mu               sync.Mutex
	readMu           sync.Mutex
	writeMu          sync.Mutex
	owner            *Value
	conn             *pooledConn
	first, end       uint64
	pages, delivered uint64
	pageCredits      int
	byteCredits      uint64
	bytesHeld        uint64
	outstanding      map[uint64]uint32
	sequence         *wire.Sequence
	ordered          bool
	complete         bool
	notify           chan struct{}
	// Get alone reuses payload storage, within this subscription. Public leases
	// from OpenPages keep their existing synchronous allocation behavior.
	buffers chan []byte
}

// Metadata returns the immutable full-object metadata snapshot.
func (s *PageStream) Metadata() Metadata { return s.owner.metadata }

// Range returns the selected start offset and exclusive end offset.
func (s *PageStream) Range() (ByteOffset, ByteOffset) { return ByteOffset(s.first), ByteOffset(s.end) }

// Close cancels the subscription without draining; it is idempotent.
func (s *PageStream) Close() error { return s.owner.Close() }

// OpenPages resolves metadata and subscribes to the entire selected range with
// one POST. It never performs a bootstrap or continuation request.
func (c *Client) OpenPages(ctx context.Context, request Request, options ...ReadOptions) (*PageStream, error) {
	if ctx == nil || len(options) > 1 {
		return nil, failure(ErrorInvalidArgument, "open pages", nil)
	}

	var o ReadOptions
	if len(options) == 1 {
		o = options[0]
	}

	o, err := c.subscriptionOptions(o)
	if err != nil {
		return nil, err
	}

	r := OriginRequest{key: request.Key, context: request.Context, operation: OperationHead, pin: o.Pin}
	if err := validateRequest(r); err != nil {
		return nil, err
	}

	pool := &c.bulk
	if o.SmallObject {
		pool = &c.smallPool
	}

	lease, err := c.admit(ctx, pool)
	if err != nil {
		return nil, err
	}

	v := &Value{admissionLease: lease}

	s := &PageStream{
		owner: v, pageCredits: o.PageCredits, byteCredits: uint64(o.ByteCredits), ordered: o.Ordered,
		outstanding: make(map[uint64]uint32), notify: make(chan struct{}, 1),
	}
	if err := s.open(r, o); err != nil {
		return nil, s.fail(err)
	}

	return s, nil
}

func (c *Client) subscriptionOptions(o ReadOptions) (ReadOptions, error) {
	if o.PageCredits == 0 {
		o.PageCredits = c.config.PageWindow
	}

	if o.PageCredits == 0 {
		o.PageCredits = 2
	}

	if o.ByteCredits == 0 {
		o.ByteCredits = ByteLength(o.PageCredits) * PageSize
	}

	if o.PageCredits < 1 || o.PageCredits > 64 || o.ByteCredits < PageSize || o.ByteCredits > 64*PageSize ||
		uint64(o.Offset) > math.MaxInt64 || uint64(o.Length) > math.MaxInt64-uint64(o.Offset) {
		return o, failure(ErrorInvalidArgument, "subscription options", nil)
	}

	if o.Metadata != nil {
		m := *o.Metadata
		if err := m.Validate(); err != nil {
			return o, err
		}

		if o.Pin.value != "" && o.Pin != m.ETag {
			return o, failure(ErrorInvalidArgument, "snapshot pin", nil)
		}

		o.Pin = m.ETag

		o.Metadata = &m
		if ByteLength(o.Offset) > m.Size || o.Length > m.Size-ByteLength(o.Offset) {
			return o, failure(ErrorUnsatisfiableRange, "subscription range", nil)
		}

		if o.SmallObject && m.Size > PageSize {
			return o, failure(ErrorInvalidArgument, "small object size", nil)
		}
	}

	return o, nil
}

func (s *PageStream) open(descriptor OriginRequest, o ReadOptions) error {
	v := s.owner

	b, err := wire.SubscriptionHead(descriptor.wire(), o.wire())
	if err != nil {
		return fromWireError(err)
	}

	conn, _, err := v.client.connection(v.ctx, v.pool, true)
	if err != nil {
		return err
	}

	s.conn = conn

	v.mu.Lock()
	if v.terminal != nil {
		err = v.terminal
		v.mu.Unlock()
		closeBody(conn)

		return err
	}

	v.body = newConnectionBody(conn)
	v.mu.Unlock()

	if err := conn.SetDeadline(time.Now().Add(v.client.config.ResponseHeaderTimeout)); err != nil {
		return ioFailure("subscription deadline", err)
	}

	if n, err := conn.Write(b); err != nil {
		return ioFailure("subscription request", err)
	} else if n != len(b) {
		return ioFailure("subscription request", io.ErrShortWrite)
	}

	head, err := readRawHead(conn.Reader, true)
	if err != nil {
		return err
	}

	if err := s.parseHead(head, o); err != nil {
		return err
	}

	if err := conn.SetDeadline(time.Time{}); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		return ioFailure("subscription deadline", err)
	}

	return v.err()
}

func (s *PageStream) parseHead(head []byte, o ReadOptions) error {
	r, err := wire.ParseSubscriptionResponse(head, o.wire())
	if err != nil {
		return fromWireError(err)
	}

	s.first, s.end, s.pages = r.First, r.End, r.Pages
	s.owner.metadata = fromWireMetadata(r.Metadata)

	return nil
}

func (s *PageStream) fail(err error) error {
	if err == io.EOF {
		err = ioFailure("subscription", io.ErrUnexpectedEOF)
	}

	if s.owner.ctx.Err() != nil {
		err = ioFailure("subscription", s.owner.ctx.Err())
	}

	s.owner.finish(err)

	return s.owner.err()
}

// Next returns io.EOF only after validating the terminal frame. Lease payload
// allocations and outstanding accounting are bounded by negotiated credits.
func (s *PageStream) Next() (*PageLease, error) {
	p, err := s.next()
	if err != nil {
		if err == io.EOF {
			s.owner.finish(io.EOF)
		} else {
			err = s.fail(err)
		}
	}

	return p, err
}

// next leaves terminal publication to its consumer. Get must consume already
// verified pages before reporting a later receive failure.
func (s *PageStream) next() (*PageLease, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()

	if err := s.owner.err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	complete := s.complete
	s.mu.Unlock()

	if complete {
		return nil, io.EOF
	}

	if err := s.waitForCredits(); err != nil {
		return nil, err
	}

	var frame [wire.FrameSize]byte
	if err := s.readFull(frame[:]); err != nil {
		return nil, err
	}

	f := wire.DecodeFrame(frame)
	number, offset, length := f.Number, f.Offset, f.Length

	if err := s.validateFrame(f, "subscription frame"); err != nil {
		return nil, err
	}

	if f.Kind == wire.CompleteFrame {
		s.mu.Lock()
		s.complete = true
		s.mu.Unlock()

		return nil, io.EOF
	}

	if err := s.reservePage(number, length, "subscription frame"); err != nil {
		return nil, err
	}

	buffer := s.getBuffer(int(length))
	data := buffer[:length:length]
	validPayload := false

	defer func() {
		if !validPayload {
			s.mu.Lock()
			if _, held := s.outstanding[number]; held {
				delete(s.outstanding, number)
				s.bytesHeld -= uint64(length)
			}
			s.mu.Unlock()
			s.putBuffer(buffer)
		}
	}()

	if err := s.readPayload(data); err != nil {
		return nil, err
	}

	s.delivered++

	if s.delivered == s.pages {
		released := make(chan error, 1)

		go func() { released <- s.release(number, length, true) }()

		err := s.readComplete("subscription frame")
		closeBody(s.conn)
		<-released

		if err != nil {
			return nil, err
		}

		if err := s.owner.ctx.Err(); err != nil {
			return nil, ioFailure("subscription", err)
		}

		if err := s.owner.err(); err != nil {
			return nil, err
		}
	}

	validPayload = true

	return &PageLease{Number: number, Offset: ByteOffset(offset), Data: data, stream: s, number: number, length: length, buffer: buffer}, nil
}

func (s *PageStream) waitForCredits() error {
	for {
		s.mu.Lock()
		ready := s.delivered == s.pages || len(s.outstanding) < s.pageCredits && s.byteCredits-s.bytesHeld >= uint64(PageSize)
		s.mu.Unlock()

		if ready {
			return nil
		}

		select {
		case <-s.notify:
		case <-s.owner.ctx.Done():
			return ioFailure("subscription", s.owner.ctx.Err())
		}
	}
}

func (s *PageStream) reservePage(number uint64, length uint32, operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.outstanding) >= s.pageCredits || uint64(length) > s.byteCredits-s.bytesHeld {
		return failure(ErrorProtocol, operation, nil)
	}

	s.outstanding[number] = length
	s.bytesHeld += uint64(length)

	return nil
}

func (s *PageStream) readComplete(operation string) error {
	var frame [wire.FrameSize]byte
	if err := s.readFull(frame[:]); err != nil {
		return err
	}

	if err := s.validateFrame(wire.DecodeFrame(frame), operation); err != nil {
		return err
	}

	s.mu.Lock()
	s.complete = true
	s.mu.Unlock()

	return nil
}

func (s *PageStream) getBuffer(length int) []byte {
	if s.buffers == nil {
		return make([]byte, length)
	}

	select {
	case b := <-s.buffers:
		return b
	default:
		return make([]byte, int(min(uint64(PageSize), s.end-s.first)))
	}
}

func (s *PageStream) putBuffer(buffer []byte) {
	if s.buffers != nil {
		select {
		case s.buffers <- buffer:
		default:
		}
	}
}

func (s *PageStream) validateFrame(f wire.Frame, operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sequence == nil {
		s.sequence = wire.NewSequence(s.first, s.end, s.ordered)
	}

	return fromWireError(s.sequence.Accept(f, operation))
}

func (s *PageStream) readFull(p []byte) error {
	return s.readBytes(p, false)
}

// truncation classifies a read failure after the response head. A clean peer
// close is truncation. So is a peer reset: a dataplane that abandons a
// subscription while client credit releases are still unread in its receive
// queue resets the stream (ECONNRESET on Linux AF_UNIX) instead of closing it
// cleanly. The reset stays in the chain as the underlying cause.
func truncation(err error) error {
	switch {
	case err == io.EOF:
		return io.ErrUnexpectedEOF
	case errors.Is(err, syscall.ECONNRESET):
		return fmt.Errorf("%w: %w", io.ErrUnexpectedEOF, err)
	default:
		return err
	}
}

func (s *PageStream) readPayload(p []byte) error {
	return s.readBytes(p, true)
}

func (s *PageStream) readBytes(p []byte, payload bool) error {
	for len(p) > 0 {
		if err := s.conn.SetReadDeadline(time.Now().Add(s.owner.client.config.BodyReadTimeout)); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			return ioFailure("subscription deadline", err)
		}

		n, err := s.conn.Reader.Read(p)
		if payload {
			s.owner.client.stats.bytesRead.Add(uint64(n))
		}

		p = p[n:]

		err = truncation(err)
		// A peer may close immediately after the final bytes. Clearing an old
		// deadline must not discard bytes that were successfully received.
		_ = s.conn.SetReadDeadline(time.Time{}) //nolint:errcheck // Best-effort cleanup preserves successfully received bytes after peer close.

		if err != nil {
			return ioFailure("subscription read", err)
		}

		if n == 0 {
			return ioFailure("subscription read", io.ErrNoProgress)
		}
	}

	return nil
}

func (s *PageStream) release(number uint64, length uint32, terminalRead bool) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	if _, held := s.outstanding[number]; !held {
		s.mu.Unlock()
		return nil
	}

	complete := s.complete
	delete(s.outstanding, number)
	s.bytesHeld -= uint64(length)
	s.mu.Unlock()

	if !complete {
		if err := s.owner.err(); err != nil {
			if err == io.EOF {
				return nil
			}

			return err
		}

		frame := (wire.Credit{Number: number, Length: length}).Encode()

		err := s.conn.SetWriteDeadline(time.Now().Add(s.owner.client.config.BodyReadTimeout))
		if err == nil {
			var n int

			n, err = s.conn.Write(frame[:])
			if err == nil && n != len(frame) {
				err = io.ErrShortWrite
			}
		}

		if clearErr := s.conn.SetWriteDeadline(time.Time{}); err == nil {
			err = clearErr
		}
		// The peer may already have sent Complete and closed while this lease
		// was held. The read side, not a racing release write, decides whether
		// the subscription completed or was truncated.
		if err != nil && !staleConnectionError(err) && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, net.ErrClosed) {
			if terminalRead || s.buffers != nil {
				return ioFailure("subscription release", err)
			}

			return s.fail(ioFailure("subscription release", err))
		}
	}

	select {
	case s.notify <- struct{}{}:
	default:
	}

	return nil
}

const copyBufferSize = 256 * 1024

// Value is an immutable full-object or selected-range stream. One goroutine may
// consume it using Read or WriteTo (including io.Copy); Metadata and Close may be called
// concurrently. A Value must not be copied. Construct it with Client.Get, or
// Client.GetStreaming for exclusive WriteToHTTP consumption.
type Value struct {
	*admissionLease
	stream    *PageStream
	streaming bool
	ordered   *orderedRead
	metadata  Metadata
}

// Metadata returns the immutable total-size/tag/expiry snapshot, never a
// remaining length.
func (v *Value) Metadata() Metadata { return v.metadata }

// Read consumes ordered page leases. An incomplete page is never exposed.
// A terminal error never restarts the version or opens another subscription.
// GetStreaming Values reject Read, including zero-length reads.
func (v *Value) Read(p []byte) (int, error) {
	if v.streaming {
		return 0, failure(ErrorInvalidArgument, "streaming value requires WriteToHTTP", nil)
	}

	if err := v.err(); err != nil {
		// Cancellation publishes the error before joining the receiver. A
		// consumer observing it must also wait for admission/storage cleanup.
		v.finish(err)
		return 0, err
	}

	if v.stream == nil {
		return 0, failure(ErrorClosed, "value", nil)
	}

	if err := v.ctx.Err(); err != nil {
		return 0, v.stream.fail(ioFailure("read", err))
	}

	if len(p) == 0 {
		return 0, nil
	}

	n, err := v.ordered.read(p)
	if err != nil {
		if err == io.EOF {
			v.finish(io.EOF)
		} else {
			err = v.stream.fail(err)
		}
	}

	return n, err
}

// Close cancels in-flight subscription reads and closes without draining. It is
// idempotent. Later consumption reports a typed closed error, except that an
// already observed clean EOF remains EOF.
func (v *Value) Close() error {
	v.finish(failure(ErrorClosed, "value", nil))
	return nil
}

// WriteTo streams into w using 256 KiB scratch without invoking w.ReadFrom.
// The returned count includes only bytes accepted by w. The caller must still
// Close, including on writer failure. Copy buffers are bounded independently to
// MaxConnections for bulk and SmallObjectConnections for small objects per client,
// including canceled copies still blocked in caller-owned Write.
// If those blocked writers exhaust scratch admission, WriteTo returns
// ErrorUnavailable. Cancellation cannot interrupt an arbitrary destination Write.
// GetStreaming Values reject WriteTo without consuming the subscription.
func (v *Value) WriteTo(w io.Writer) (int64, error) {
	if _, err := v.Read(nil); err != nil {
		if err == io.EOF {
			return 0, nil
		}

		return 0, err
	}

	buf, release, err := v.client.admitCopy(v.pool)
	if err != nil {
		return 0, err
	}
	defer release()

	var written int64

	for {
		buffer := buf[:]

		n, readErr := v.Read(buffer)
		if n > 0 {
			nw, writeErr := w.Write(buf[:n])
			if nw < 0 || nw > n {
				nw = 0

				if writeErr == nil {
					writeErr = io.ErrShortWrite
				}
			}

			written += int64(nw)
			if writeErr != nil {
				return written, writeErr
			}

			if nw != n {
				return written, io.ErrShortWrite
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				return written, nil
			}

			return written, readErr
		}
	}
}

// orderedRead owns at most two page-sized buffers, including the current lease,
// queued leases and an in-flight receive. Negotiated credits can reduce this to
// one. No buffer crosses request boundaries. Only Release makes storage reusable.
// The connection admission slot stays held until consumption ends or cancellation
// has joined the receiver and dropped its buffers, not merely until wire EOF.
type orderedRead struct {
	mu     sync.Mutex
	stream *PageStream
	ready  chan *PageLease
	slots  chan struct{}
	done   chan struct{}
	err    error // Published by closing ready; read only after ready is drained.
	lease  *PageLease
	offset int
}

func (v *Value) startOrdered(s *PageStream) {
	limit := min(2, s.pageCredits, int(s.byteCredits/uint64(PageSize)))
	s.buffers = make(chan []byte, limit)
	r := &orderedRead{stream: s, ready: make(chan *PageLease, limit), slots: make(chan struct{}, limit), done: make(chan struct{})}

	v.mu.Lock()
	defer v.mu.Unlock()

	if v.terminal != nil {
		return
	}

	v.ordered = r

	v.cleanup = r.shutdown
	go r.receive()
}

func (r *orderedRead) receive() {
	defer close(r.done)
	defer close(r.ready)

	for {
		select {
		case r.slots <- struct{}{}:
		case <-r.stream.owner.ctx.Done():
			r.err = ioFailure("subscription", r.stream.owner.ctx.Err())
			return
		}

		p, err := r.stream.next()
		if err != nil {
			<-r.slots
			r.err = err

			return
		}
		// ready has room for every occupied slot, even if cancellation wins.
		r.ready <- p

		r.stream.mu.Lock()
		complete := r.stream.complete
		r.stream.mu.Unlock()

		if complete {
			r.err = io.EOF
			return
		}
	}
}

// shutdown runs after cancel and socket close. It never waits on caller-owned
// Write, which only sees the separately bounded WriteTo scratch buffer.
func (r *orderedRead) shutdown() {
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.lease != nil {
		_ = r.lease.Release() //nolint:errcheck // Terminal cleanup drops local ownership; the closed socket needs no credit return.
		r.lease = nil
		<-r.slots
	}

	for p := range r.ready {
		_ = p.Release() //nolint:errcheck // Preserve the already published terminal error during cleanup.

		<-r.slots
	}

	for len(r.stream.buffers) > 0 {
		<-r.stream.buffers
	}
}

func (r *orderedRead) read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.stream.owner.err(); err != nil {
		return 0, err
	}

	if r.lease == nil {
		select {
		case lease, ok := <-r.ready:
			if !ok {
				return 0, r.err
			}

			r.lease, r.offset = lease, 0
		case <-r.stream.owner.ctx.Done():
			return 0, ioFailure("read", r.stream.owner.ctx.Err())
		}
	}

	n := copy(p, r.lease.Data[r.offset:])

	r.offset += n
	if r.offset == len(r.lease.Data) {
		err := r.lease.Release()
		r.lease = nil
		<-r.slots

		return n, err
	}

	return n, nil
}

// WriteToHTTP delivers a Value through the server's normal response lifecycle.
// The caller sets headers, including the selected range's Content-Length, and
// owns Close. Get Values retain buffered page validation. GetStreaming Values
// validate frame headers and forward bounded payload slices, using the writer's
// ReadFrom when a raw Unix socket is available. The final byte is withheld until
// Complete is validated; an incomplete page prefix can be exposed on failure.
// No connection is hijacked. Empty ranges validate Complete before returning,
// but cannot withhold a byte; callers must avoid committing empty responses early.
// Destination deadlines are used where supported for streaming transfers.
// Arbitrary blocked writers cannot be interrupted, but retain a separately
// bounded copy admission slot.
// A streaming transfer failure is terminal; a handler that has committed headers
// must abort its response rather than emit an error body or report success.
func (v *Value) WriteToHTTP(w http.ResponseWriter) (int64, error) {
	if v.streaming {
		return v.writeStreamingHTTP(w)
	}

	return v.WriteTo(w)
}

// streamingHTTP owns receive framing exclusively. It never creates page buffers
// or a background receiver. Scratch admission remains held through destination
// calls even after cancellation releases the connection admission slot.
type streamingHTTP struct {
	value      *Value
	writer     http.ResponseWriter
	buffer     *[copyBufferSize]byte
	controller *http.ResponseController
	deadlineMu sync.Mutex
}

func (v *Value) writeStreamingHTTP(w http.ResponseWriter) (written int64, result error) {
	if w == nil {
		return 0, failure(ErrorInvalidArgument, "HTTP destination", nil)
	}

	if err := v.err(); err != nil {
		v.finish(err)

		if err == io.EOF {
			return 0, nil
		}

		return 0, err
	}

	buffer, release, err := v.client.admitCopy(v.pool)
	if err != nil {
		return 0, err
	}
	defer release()

	h := &streamingHTTP{value: v, writer: w, buffer: buffer, controller: http.NewResponseController(w)}
	stopCancellation := h.interruptOnCancel()

	defer func() {
		stopCancellation()

		_ = h.controller.SetWriteDeadline(time.Time{}) //nolint:errcheck // Best-effort cleanup after destination closure.
	}()

	written, result = h.transfer()
	if result != nil {
		return written, v.stream.fail(result)
	}
	// Successful finish cancels the Value context for resource cleanup. Disarm
	// and join destination cancellation first: an expired HTTP/2 deadline resets
	// the stream irreversibly, even if cleanup immediately clears that deadline.
	// The deferred call also joins the callback on failures, exactly once.
	stopCancellation()

	if err := v.ctx.Err(); err != nil {
		return written, v.stream.fail(ioFailure("HTTP transfer", err))
	}

	v.finish(io.EOF)

	if err := v.err(); err != io.EOF {
		return written, err
	}

	return written, nil
}

// interruptOnCancel returns an idempotent stop-and-join function. Success must
// call it before finishing the Value, so cleanup cannot reset an HTTP/2 stream.
func (h *streamingHTTP) interruptOnCancel() func() {
	done := make(chan struct{})
	stop := context.AfterFunc(h.value.ctx, func() {
		defer close(done)

		h.deadlineMu.Lock()
		defer h.deadlineMu.Unlock()

		_ = h.controller.SetWriteDeadline(time.Now()) //nolint:errcheck // Unsupported arbitrary writers remain bounded by copy admission.
	})

	return sync.OnceFunc(func() {
		if !stop() {
			<-done
		}
	})
}

func (h *streamingHTTP) writeDeadline() error {
	h.deadlineMu.Lock()
	defer h.deadlineMu.Unlock()

	if err := h.value.ctx.Err(); err != nil {
		return ioFailure("HTTP write", err)
	}

	err := h.controller.SetWriteDeadline(time.Now().Add(h.value.client.config.BodyReadTimeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}

	return err
}

func (h *streamingHTTP) write(p []byte) (int64, error) {
	if err := h.writeDeadline(); err != nil {
		return 0, err
	}

	n, err := h.writer.Write(p)
	if clearErr := h.clearWriteDeadline(); err == nil {
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

// Clear only an ordinary operation deadline. Cancellation uses the same lock,
// so cleanup cannot erase its immediate interrupt, even if the callback has
// already run. Final cleanup may clear unconditionally after joining it.
func (h *streamingHTTP) clearWriteDeadline() error {
	h.deadlineMu.Lock()
	defer h.deadlineMu.Unlock()

	if err := h.value.ctx.Err(); err != nil {
		return ioFailure("HTTP write", err)
	}

	err := h.controller.SetWriteDeadline(time.Time{})
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}

	return err
}

func (h *streamingHTTP) transfer() (int64, error) {
	s := h.value.stream

	var (
		written  int64
		final    [1]byte
		released chan error
	)

	for s.delivered < s.pages {
		number, length, err := s.streamingFrame()
		if err != nil {
			return written, err
		}

		last := s.delivered+1 == s.pages

		remaining := int64(length)
		if last {
			remaining--
		}

		n, err := h.payload(remaining)

		written += n
		if err != nil {
			return written, err
		}

		if last {
			if err := s.readPayload(final[:]); err != nil {
				return written, err
			}
		}

		s.delivered++
		// The peer may wait for final credit or send Complete without reading it.
		// Read Complete concurrently so either ordering can make progress.
		if last {
			released = make(chan error, 1)

			go func() { released <- s.release(number, length, true) }()
		} else if err := s.release(number, length, false); err != nil {
			return written, err
		}
	}

	err := s.readComplete("subscription complete")
	if released != nil {
		closeBody(s.conn)
		<-released
	}

	if err != nil {
		return written, err
	}

	if s.end != s.first {
		n, err := h.write(final[:])
		return written + n, err
	}

	return written, nil
}

// streamingFrame reserves exactly one ordered page using negotiated credits.
// Payload is not hashed or buffered for whole-page validation in this mode.
func (s *PageStream) streamingFrame() (uint64, uint32, error) {
	var frame [wire.FrameSize]byte
	if err := s.readFull(frame[:]); err != nil {
		return 0, 0, err
	}

	f := wire.DecodeFrame(frame)
	if err := s.validateFrame(f, "subscription frame"); err != nil {
		return 0, 0, err
	}

	number, length := f.Number, f.Length

	if err := s.reservePage(number, length, "subscription credits"); err != nil {
		return 0, 0, err
	}

	return number, length, nil
}

func (h *streamingHTTP) payload(remaining int64) (int64, error) {
	s := h.value.stream

	var written int64

	for remaining > 0 {
		batch := min(remaining, int64(copyBufferSize))
		// Never bypass read-ahead: it may contain payload and later frame headers.
		buffered := s.conn.Reader.Buffered()
		raw, unix := s.conn.Conn.(*net.UnixConn)

		rf, fast := h.writer.(io.ReaderFrom)
		if buffered == 0 && unix && fast {
			n, err := h.transferFromSocket(raw, rf, batch)

			written += n
			if err != nil {
				return written, err
			}

			remaining -= batch

			continue
		}

		if buffered > 0 {
			batch = min(batch, int64(buffered))
		}
		// Read once rather than ReadFull, so incomplete page prefixes have the
		// same streaming semantics with and without ReaderFrom.
		if err := s.conn.SetReadDeadline(time.Now().Add(h.value.client.config.BodyReadTimeout)); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			return written, ioFailure("subscription deadline", err)
		}

		n, readErr := s.conn.Reader.Read(h.buffer[:batch])
		_ = s.conn.SetReadDeadline(time.Time{}) //nolint:errcheck // Preserve successful bytes after peer closure.

		h.value.client.stats.bytesRead.Add(uint64(n))

		if n > 0 {
			nw, err := h.write(h.buffer[:n])

			written += nw
			if err != nil {
				return written, err
			}

			remaining -= int64(n)
		}

		if readErr != nil {
			return written, ioFailure("subscription payload", truncation(readErr))
		}

		if n == 0 {
			return written, ioFailure("subscription payload", io.ErrNoProgress)
		}
	}

	return written, nil
}

func (h *streamingHTTP) transferFromSocket(raw *net.UnixConn, destination io.ReaderFrom, batch int64) (int64, error) {
	s := h.value.stream
	if err := s.conn.SetReadDeadline(time.Now().Add(h.value.client.config.BodyReadTimeout)); err != nil {
		return 0, ioFailure("subscription deadline", err)
	}

	if err := h.writeDeadline(); err != nil {
		return 0, err
	}

	limited := &io.LimitedReader{R: raw, N: batch}

	n, err := destination.ReadFrom(limited)
	if clearErr := h.clearWriteDeadline(); err == nil {
		err = clearErr
	}

	_ = s.conn.SetReadDeadline(time.Time{}) //nolint:errcheck // Preserve successful reads after peer closure.
	// With stdlib splice, LimitedReader.N advances by bytes delivered.
	// A destination failure can leave additional source bytes in the kernel pipe.
	// No public syscall accounting API exposes that loss, so BytesRead may
	// undercount source consumption on this error path.
	consumed := batch - limited.N
	if consumed >= 0 && consumed <= batch {
		h.value.client.stats.bytesRead.Add(uint64(consumed))
	}

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

	return n, ioFailure("subscription payload", truncation(err))
}
