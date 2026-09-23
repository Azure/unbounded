// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// ErrDigestMismatch means a full stream did not match the caller's SHA-256.
var ErrDigestMismatch = errors.New("racer: SHA-256 digest mismatch")

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
	mu       sync.Mutex
	idle     []*streamConn
	endpoint string
	limit    int
	timeout  time.Duration
}

func (p *streamPool) get(ctx context.Context) (*streamConn, error) {
	p.mu.Lock()
	for len(p.idle) > 0 {
		i := len(p.idle) - 1
		c := p.idle[i]

		p.idle = p.idle[:i]
		if time.Since(c.idle) < 90*time.Second {
			p.mu.Unlock()
			return c, nil
		}

		_ = c.Close() //nolint:errcheck // Expired idle socket cleanup.
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
	defer p.mu.Unlock()

	if len(p.idle) >= p.limit {
		_ = c.Close() //nolint:errcheck // The idle pool is full.
		return
	}

	c.idle = time.Now()
	p.idle = append(p.idle, c)
}

func (p *streamPool) closeIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, c := range p.idle {
		_ = c.Close() //nolint:errcheck // Idle connection cleanup.
	}

	p.idle = nil
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
// Read and WriteTo are serialized; Close may run concurrently with either.
type Stream struct {
	mu                   sync.Mutex
	object               *Object
	ctx                  context.Context
	cancel               context.CancelFunc
	conn                 *streamConn
	stop                 func() bool
	stopped              chan struct{}
	offset, end, pageEnd int64
	responseClose        bool
	err                  error
	closed               bool
	digest               hash.Hash
	expected             [32]byte
	verified             bool
	stats                TransferStats
}

// TransferStats distinguishes actual kernel splice/tee traffic from buffered
// header read-ahead and portable copies. Stats are per stream, not global.
// BufferedBytes counts bytes read, including a withheld verification suffix;
// SpliceBytes counts bytes successfully forwarded to the destination socket.
type TransferStats struct {
	SpliceBytes   int64
	SpliceCalls   int64
	TeeBytes      int64
	TeeCalls      int64
	BufferedBytes int64
}

func (s *Stream) Stats() TransferStats {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.stats
}

// Prepare fetches and validates the first page's headers without consuming its
// body. Call before committing downstream HTTP headers to surface authorization,
// version, and framing errors while an HTTP error response is still possible.
// Empty streams verify their digest here without issuing a GET.
func (s *Stream) Prepare() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return net.ErrClosed
	}

	if s.err != nil {
		return s.err
	}

	if err := s.ctx.Err(); err != nil {
		return s.fail(err)
	}

	if s.offset == s.end {
		if err := s.finish(); err != io.EOF {
			return err
		}

		return nil
	}

	if err := s.nextPage(); err != nil {
		return s.fail(err)
	}

	return nil
}

// Stream opens the entire snapshot. It does not assume ETag is a content hash.
func (o *Object) Stream(ctx context.Context) (*Stream, error) {
	return o.ReadRange(ctx, 0, o.meta.Size)
}

// StreamVerified verifies the complete object against an explicit SHA-256,
// independent of Racer's opaque version ETag. WriteTo withholds the final byte
// until verification succeeds, so a digest mismatch cannot complete a framed
// HTTP body. Verification requires hashing a tee copy in userspace, while the
// forwarding leg still uses splice. Partial ranges cannot verify a full digest.
func (o *Object) StreamVerified(ctx context.Context, expected [32]byte) (*Stream, error) {
	s, err := o.Stream(ctx)
	if err != nil {
		return nil, err
	}

	s.digest, s.expected = sha256.New(), expected

	return s, nil
}

// ReadRange opens [offset, offset+length), rejecting out-of-bounds intervals.
// Requests are lazy, sequential, and pinned by If-Match to Open's HEAD snapshot.
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

	return &Stream{object: o, ctx: ctx, cancel: cancel, offset: offset, end: offset + length}, nil
}

func (s *Stream) release(reuse bool) {
	if s.conn == nil {
		return
	}

	if !s.stop() {
		<-s.stopped

		reuse = false
	}

	if reuse && s.ctx.Err() == nil {
		s.object.client.streamPool.put(s.conn)
	} else {
		_ = s.conn.Close() //nolint:errcheck // Preserve the transfer error.
	}

	s.conn = nil
}

func (s *Stream) nextPage() error {
	if err := s.ctx.Err(); err != nil {
		return err
	}

	if s.conn != nil && s.offset < s.pageEnd {
		return nil
	}

	if s.conn != nil && s.conn.reader.Buffered() != 0 {
		return fmt.Errorf("%w: bytes beyond response length", ErrProtocol)
	}

	if s.conn != nil && s.responseClose {
		s.release(false)
	}

	if s.conn == nil {
		c, err := s.object.client.streamPool.get(s.ctx)
		if err != nil {
			return err
		}

		s.conn = c
		s.stopped = make(chan struct{})
		done := s.stopped

		s.stop = context.AfterFunc(s.ctx, func() {
			_ = c.Close() //nolint:errcheck // Cancellation interrupts socket I/O.

			close(done)
		})
		if deadline, ok := s.ctx.Deadline(); ok {
			if err := c.SetDeadline(deadline); err != nil {
				return err
			}
		}
	}

	s.pageEnd = s.offset + min(PageSize-s.offset%PageSize, s.end-s.offset)

	r, err := s.object.client.request(s.ctx, http.MethodGet, s.object.target)
	if err != nil {
		return err
	}

	r.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", s.offset, s.pageEnd-1))
	r.Header.Set("If-Match", s.object.meta.ETag)

	if err := r.Write(s.conn); err != nil {
		return err
	}

	resp, err := s.conn.response(r)
	if err != nil {
		return err
	}

	if err := s.object.validatePage(resp, s.offset, s.pageEnd-1); err != nil {
		return err
	}

	s.responseClose = resp.Close

	return nil
}

func (s *Stream) finish() error {
	if s.digest != nil && !s.verified {
		var sum [32]byte
		if !bytes.Equal(s.digest.Sum(sum[:0]), s.expected[:]) {
			return s.fail(ErrDigestMismatch)
		}

		s.verified = true
	}

	s.release(!s.responseClose)

	return io.EOF
}

func (s *Stream) fail(err error) error {
	if cause := s.ctx.Err(); cause != nil {
		err = cause
	}

	s.err = err
	s.release(false)

	return err
}

func (s *Stream) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.read(p)
}

func (s *Stream) read(p []byte) (int, error) {
	if s.closed {
		return 0, net.ErrClosed
	}

	if s.err != nil {
		return 0, s.err
	}

	if err := s.ctx.Err(); err != nil {
		return 0, s.fail(err)
	}

	if s.offset == s.end {
		return 0, s.finish()
	}

	if len(p) == 0 {
		return 0, nil
	}

	if err := s.nextPage(); err != nil {
		return 0, s.fail(err)
	}

	n, err := s.conn.reader.Read(p[:min(int64(len(p)), s.pageEnd-s.offset)])
	s.offset += int64(n)

	s.stats.BufferedBytes += int64(n)
	if s.digest != nil {
		_, _ = s.digest.Write(p[:n])
	}

	if err != nil && (err != io.EOF || s.offset != s.pageEnd) {
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

// Close cancels the stream, discarding incomplete responses. Successful complete
// responses return their socket to the shared pool. It is safe to call repeatedly.
func (s *Stream) Close() error {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	s.release(false)

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
			written, writeErr := dst.Write((*buf)[:n])

			total += int64(written)
			if writeErr == nil && written != n {
				writeErr = io.ErrShortWrite
			}

			if writeErr != nil {
				return total, s.fail(writeErr)
			}
		}

		if err == io.EOF {
			return total, nil
		}
	}
}
