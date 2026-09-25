// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

type streamConn struct {
	net.Conn
	reader *bufio.Reader
	header []byte
	parser *bufio.Reader
	idle   time.Time
}

// Raw HTTP sockets are separate from net/http's pool: bypassing its response
// body reader would corrupt its framing and reuse state. Both pools are shared
// by authorization views; credentials are written afresh for every request.
type streamPool struct {
	pipes       splicePipePool
	mu          sync.Mutex
	idle        []*streamConn
	endpoint    string
	limit       int
	timeout     time.Duration
	speculative chan struct{}
}

func (p *streamPool) get(ctx context.Context) (*streamConn, error) {
	p.mu.Lock()
	for len(p.idle) > 0 {
		i := len(p.idle) - 1
		c := p.idle[i]

		p.idle[i] = nil
		p.idle = p.idle[:i]
		p.mu.Unlock()

		if time.Since(c.idle) < 90*time.Second {
			return c, nil
		}

		_ = c.Close() //nolint:errcheck // Expired idle socket cleanup.

		p.mu.Lock()
	}
	p.mu.Unlock()

	c, err := (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, "unix", p.endpoint)
	if err != nil {
		return nil, err
	}

	return &streamConn{Conn: c, reader: bufio.NewReaderSize(c, 8192), parser: bufio.NewReader(nil), header: make([]byte, 0, 8192)}, nil
}

func (p *streamPool) put(c *streamConn) {
	if c.reader.Buffered() != 0 || c.SetDeadline(time.Time{}) != nil {
		_ = c.Close() //nolint:errcheck // Discard an unusable connection.
		return
	}

	p.mu.Lock()

	if len(p.idle) >= p.limit {
		p.mu.Unlock()

		_ = c.Close() //nolint:errcheck // The idle pool is full.

		return
	}

	c.idle = time.Now()
	p.idle = append(p.idle, c)
	p.mu.Unlock()
}

func (p *streamPool) closeIdle() {
	p.pipes.closeIdle()

	p.mu.Lock()
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()

	for _, c := range idle {
		_ = c.Close() //nolint:errcheck // Idle connection cleanup.
	}
}

func (c *streamConn) response(r *http.Request) (*http.Response, error) {
	c.header = c.header[:0]
	for {
		line, err := c.reader.ReadSlice('\n')
		if err != nil {
			return nil, err
		}

		if len(c.header)+len(line) > 8192 {
			return nil, fmt.Errorf("%w: response headers exceed 8192 bytes", ErrProtocol)
		}

		c.header = append(c.header, line...)
		if bytes.Equal(line, []byte("\r\n")) {
			break
		}
	}
	// Parse only the isolated header. Any payload read ahead remains in reader
	// and must be drained before splice can consume the underlying socket.
	c.parser.Reset(bytes.NewReader(c.header))

	resp, err := http.ReadResponse(c.parser, r)
	if err == nil && resp.Proto != "HTTP/1.1" {
		err = fmt.Errorf("%w: expected HTTP/1.1", ErrProtocol)
	}

	return resp, err
}

// Stream reads a pinned snapshot sequentially, issuing one GET per aligned
// 64 MiB page. It allocates no page-sized buffers. Close cancels blocked I/O.
// WriteTo calls are serialized; Close may run concurrently with WriteTo.
type Stream struct {
	mu          sync.Mutex
	object      *Object
	ctx         context.Context
	cancel      context.CancelFunc
	page        *preparedPage
	offset, end int64
	err         error
	closed      bool
	stats       TransferStats
	failure     *StreamFailure
	future      *pageFuture
}

// TransferStats distinguishes actual kernel splice traffic from buffered
// header read-ahead and portable copies. Stats are per stream, not global.
// BufferedBytes counts bytes read through userspace;
// SpliceBytes counts bytes successfully forwarded to the destination socket.
type TransferStats struct {
	SpliceBytes   int64
	SpliceCalls   int64
	BufferedBytes int64
	// PageRequests counts GET write attempts, including retries and failed writes,
	// but excludes HEAD and connection failures before a request can be written.
	PageRequests int64
	// PageRetries counts PageRequests that retry rejected pre-body responses.
	PageRetries int64
	// PageHeaderWait is time spent preparing page headers on the consumption
	// path, including connection setup, validation, retry backoff, and failures.
	// It includes Prepare, but excludes HEAD and caller idle time.
	PageHeaderWait time.Duration
	// ForwardDuration is active WriteTo time excluding PageHeaderWait. It includes
	// upstream body waits, downstream backpressure, and forwarding/cleanup work;
	// it is not a measure of downstream socket blocking alone.
	ForwardDuration time.Duration
}

// Stats returns a cumulative snapshot. It waits for an active Prepare or WriteTo
// to finish, and remains available after failure or Close.
func (s *Stream) Stats() TransferStats {
	s.mu.Lock()
	defer s.mu.Unlock()

	stats := s.stats
	if s.page != nil {
		stats.PageRequests += s.page.requests.Load()
		stats.PageRetries += s.page.retries.Load()
	}

	if s.future != nil {
		stats.PageRequests += s.future.page.requests.Load()
		stats.PageRetries += s.future.page.retries.Load()
	}

	return stats
}

// Prepare fetches and validates the first page's headers without consuming its
// body. Call before committing downstream HTTP headers to surface authorization,
// version, and framing errors while an HTTP error response is still possible.
// Empty streams complete without issuing a GET.
func (s *Stream) Prepare() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if done, err := s.readState(); done || err != nil {
		return err
	}

	return s.prepareRead()
}

// Stream opens the entire snapshot without verifying a content digest.
// It does not assume ETag is a content hash.
func (o *Object) Stream(ctx context.Context) (*Stream, error) {
	return o.ReadRange(ctx, 0, o.meta.Size)
}

// ReadRange opens [offset, offset+length), rejecting out-of-bounds intervals.
// Requests are lazy and pinned by If-Match to Open's HEAD snapshot. Consumption
// is ordered, with at most one additional page prepared when lookahead is enabled.
// Close is required even when a caller stops reading early.
func (o *Object) ReadRange(ctx context.Context, offset, length int64) (*Stream, error) {
	if offset < 0 || length < 0 || offset > o.meta.Size || length > o.meta.Size-offset {
		return nil, fmt.Errorf("racer: invalid stream range")
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var cancel context.CancelFunc
	if timeout := o.client.streamPool.timeout; timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}

	return &Stream{object: o, ctx: ctx, cancel: cancel, offset: offset, end: offset + length, page: newPreparedPage(ctx, o, offset)}, nil
}

func (s *Stream) nextPage() error {
	if err := s.ctx.Err(); err != nil {
		return err
	}

	if s.page.conn != nil && s.offset < s.page.pageEnd {
		return nil
	}

	if s.page.conn != nil && s.page.conn.reader.Buffered() != 0 {
		return fmt.Errorf("%w: bytes beyond response length", ErrProtocol)
	}

	if s.page.conn != nil && s.page.responseClose {
		s.page.release(false)
	}

	started := time.Now()

	defer func() { s.stats.PageHeaderWait += time.Since(started) }()

	if s.future != nil {
		if err := s.takeFuture(); err != nil {
			return err
		}
	} else if err := s.page.preparePageWithRetry(s.offset, s.end); err != nil {
		return err
	}

	s.startFuture()

	return nil
}

func (s *Stream) finish() error {
	s.page.close(!s.page.responseClose)

	return io.EOF
}

func (s *Stream) fail(err error) error {
	if s.failure == nil {
		s.failure = &StreamFailure{Operation: s.page.operation, PageOffset: s.page.pageOffset, Offset: s.offset, StatusCode: s.page.statusCode, Err: err, ContextErr: s.ctx.Err()}
	}

	if cause := s.ctx.Err(); cause != nil {
		err = cause
	}

	s.err = err
	s.page.close(false)
	s.discardFuture()

	return err
}

// readState checks terminal conditions before either preparing or consuming a
// page. In particular, EOF and failures take precedence over a zero-length read.
func (s *Stream) readState() (done bool, err error) {
	if s.closed {
		return false, net.ErrClosed
	}

	if s.err != nil {
		return false, s.err
	}

	if err := s.ctx.Err(); err != nil {
		return false, s.fail(err)
	}

	if s.offset == s.end {
		if err := s.finish(); err != io.EOF {
			return false, err
		}

		return true, nil
	}

	return false, nil
}

// prepareRead advances only when needed and records failures at the page
// operation and offset that produced them. Call after checking readState.
func (s *Stream) prepareRead() error {
	if err := s.nextPage(); err != nil {
		return s.fail(err)
	}

	return nil
}

func (s *Stream) read(p []byte) (int, error) {
	if done, err := s.readState(); err != nil {
		return 0, err
	} else if done {
		return 0, io.EOF
	}

	if len(p) == 0 {
		return 0, nil
	}

	if err := s.prepareRead(); err != nil {
		return 0, err
	}

	s.page.operation = "page_body"
	n, err := s.page.conn.reader.Read(p[:min(int64(len(p)), s.page.pageEnd-s.offset)])
	s.offset += int64(n)

	s.stats.BufferedBytes += int64(n)

	if err != nil && (err != io.EOF || s.offset != s.page.pageEnd) {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}

		return n, s.fail(err)
	}

	if s.offset == s.end {
		err = s.finish()
		if err == io.EOF {
			err = nil
		}

		return n, err
	}

	return n, nil
}

// writeBuffered counts downstream progress separately from the bytes already
// consumed by read. A partial write without an error is still a stream failure.
func (s *Stream) writeBuffered(dst io.Writer, p []byte) (int, error) {
	s.page.operation = "downstream_write"

	n, err := dst.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}

	if err != nil {
		return n, s.fail(err)
	}

	return n, nil
}

// Close cancels the stream, discarding incomplete responses. Successful complete
// responses return their socket to the shared pool. It is safe to call repeatedly.
func (s *Stream) Close() error {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	s.page.close(false)
	s.discardFuture()

	return nil
}

// WriteTo explicitly splices to concrete *net.TCPConn and *net.UnixConn on
// Linux. Other writers (including TLS and http.ResponseWriter) use a bounded
// copy. For HTTP splice, hijack, write/flush headers, then pass the raw conn;
// the caller owns downstream framing and must close it on any error.
func (s *Stream) WriteTo(dst io.Writer) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if dst == nil {
		return 0, fmt.Errorf("racer: nil stream destination")
	}

	if !s.closed && s.err == nil && s.offset < s.end {
		started, headerWait := time.Now(), s.stats.PageHeaderWait

		defer func() {
			s.stats.ForwardDuration += time.Since(started) - (s.stats.PageHeaderWait - headerWait)
		}()
	}

	if n, err, supported := s.spliceTo(dst); supported {
		return n, err
	}

	buf := copyBuffers.Get().(*[]byte) //nolint:errcheck // The private pool only contains *[]byte.
	defer copyBuffers.Put(buf)

	var total int64

	for {
		n, err := s.read(*buf)
		if err != nil && err != io.EOF {
			return total, err
		}

		if n > 0 {
			written, writeErr := s.writeBuffered(dst, (*buf)[:n])

			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
		}

		if err == io.EOF {
			return total, nil
		}
	}
}
