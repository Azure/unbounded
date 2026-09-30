// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PageLease owns one verified page slice. Data is valid until Release. Do not
// copy a lease or access Data concurrently with Release. Release is idempotent.
type PageLease struct {
	Number uint64
	Offset ByteOffset
	Data   []byte
	stream *PageStream
	number uint64
	length uint32
	buffer []byte
	once   sync.Once
	err    error
}

func (p *PageLease) Release() error {
	if p == nil || p.stream == nil {
		return failure(ErrorInvalidArgument, "page lease", nil)
	}

	p.once.Do(func() {
		p.err = p.stream.release(p.number, p.length)
		p.Data = nil
		p.stream.putBuffer(p.buffer)
		p.buffer = nil
	})

	return p.err
}

type pageInterval struct{ first, end uint64 }

// PageStream receives page slices, unordered unless Ordered was requested.
// One goroutine calls Next; Release and Close may run concurrently. Call Close
// on every path. Next waits when credits are held by outstanding leases.
type PageStream struct {
	mu                               sync.Mutex
	readMu                           sync.Mutex
	writeMu                          sync.Mutex
	owner                            *Value
	conn                             *pooledConn
	first, end                       uint64
	pages, delivered, deliveredBytes uint64
	pageCredits                      int
	byteCredits                      uint64
	bytesHeld                        uint64
	outstanding                      map[uint64]uint32
	intervals                        []pageInterval
	ordered                          bool
	complete                         bool
	notify                           chan struct{}
	// Get alone reuses payload storage, within this subscription. Public leases
	// from OpenPages keep their existing synchronous allocation behavior.
	buffers chan []byte
}

func (s *PageStream) Metadata() Metadata              { return s.owner.metadata }
func (s *PageStream) Range() (ByteOffset, ByteOffset) { return ByteOffset(s.first), ByteOffset(s.end) }
func (s *PageStream) Close() error                    { return s.owner.Close() }

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

	if o.PageCredits == 0 {
		o.PageCredits = c.config.PageWindow
	}

	if o.PageCredits == 0 {
		o.PageCredits = 2
	}

	if o.ByteCredits == 0 {
		o.ByteCredits = ByteLength(o.PageCredits) * PageSize
	}

	if o.PageCredits < 1 || o.PageCredits > 64 || o.ByteCredits < PageSize || o.ByteCredits > 64*PageSize || uint64(o.Offset) > math.MaxInt64 || uint64(o.Length) > math.MaxInt64-uint64(o.Offset) {
		return nil, failure(ErrorInvalidArgument, "subscription options", nil)
	}

	if o.Metadata != nil {
		m := *o.Metadata
		if err := m.Validate(); err != nil {
			return nil, err
		}

		if o.Pin.value != "" && o.Pin != m.ETag {
			return nil, failure(ErrorInvalidArgument, "snapshot pin", nil)
		}

		o.Pin = m.ETag

		o.Metadata = &m
		if ByteLength(o.Offset) > m.Size || o.Length > m.Size-ByteLength(o.Offset) {
			return nil, failure(ErrorUnsatisfiableRange, "subscription range", nil)
		}

		if o.SmallObject && m.Size > PageSize {
			return nil, failure(ErrorInvalidArgument, "small object size", nil)
		}
	}

	r := OriginRequest{key: request.Key, context: request.Context, operation: OperationHead, pin: o.Pin}
	if err := validateRequest(r); err != nil {
		return nil, err
	}

	pool := &c.bulk
	if o.SmallObject {
		pool = &c.smallPool
	}

	v, err := c.admit(ctx, pool)
	if err != nil {
		return nil, err
	}

	s := &PageStream{owner: v, pageCredits: o.PageCredits, byteCredits: uint64(o.ByteCredits), ordered: o.Ordered, outstanding: make(map[uint64]uint32), notify: make(chan struct{}, 1)}
	if err := s.open(request, r, o); err != nil {
		return nil, s.fail(err)
	}

	return s, nil
}

func (s *PageStream) open(request Request, descriptor OriginRequest, o ReadOptions) error {
	v := s.owner

	var b strings.Builder
	fmt.Fprintf(&b, "POST /v2/objects/%s HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nRacer-Page-Credits: %d\r\nRacer-Byte-Credits: %d\r\nRacer-Ordered: %d\r\n", request.Key.String(), o.PageCredits, o.ByteCredits, boolNumber(o.Ordered))

	if o.Offset != 0 || o.Length != 0 {
		fmt.Fprintf(&b, "Range: bytes=%d-", o.Offset)

		if o.Length != 0 {
			fmt.Fprintf(&b, "%d", uint64(o.Offset)+uint64(o.Length)-1)
		}

		b.WriteString("\r\n")
	}

	for name, values := range requestHeaders(descriptor) {
		fmt.Fprintf(&b, "%s: %s\r\n", name, values[0])
	}

	b.WriteString("\r\n")

	if err := validateRawHead([]byte(b.String()), false); err != nil {
		return err
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

	v.body = &responseBody{conn: conn, client: v.client, pool: v.pool}
	v.mu.Unlock()

	if err := conn.SetDeadline(time.Now().Add(v.client.config.ResponseHeaderTimeout)); err != nil {
		return ioFailure("subscription deadline", err)
	}

	if n, err := io.WriteString(conn, b.String()); err != nil {
		return ioFailure("subscription request", err)
	} else if n != b.Len() {
		return ioFailure("subscription request", io.ErrShortWrite)
	}

	head, err := readRawHead(conn.reader, true)
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

func boolNumber(b bool) int {
	if b {
		return 1
	}

	return 0
}

func (s *PageStream) parseHead(head []byte, o ReadOptions) error {
	bad := failure(ErrorProtocol, "subscription response", nil)

	if err := validateRawHead(head, true); err != nil {
		return err
	}

	h := headHeaders(head)

	line := string(head[:bytes.Index(head, []byte("\r\n"))])
	if len(line) < 12 || !strings.HasPrefix(line, "HTTP/1.1 ") || len(line) > 12 && line[12] != ' ' || forbiddenHeaders(h) {
		return bad
	}

	status, err := strconv.Atoi(line[9:12])
	if err != nil {
		return bad
	}

	length, err := decimal(h.Get("Content-Length"))
	if err != nil {
		return bad
	}

	if status != 200 {
		if length != 0 {
			return bad
		}

		if status == 416 {
			value := h.Get("Content-Range")
			if !strings.HasPrefix(value, "bytes */") {
				return bad
			}

			if _, err := decimal(strings.TrimPrefix(value, "bytes */")); err != nil {
				return bad
			}
		} else if h.Get("Content-Range") != "" {
			return bad
		}

		return statusError(status)
	}

	if !connectionClose(h) || h.Get("Content-Range") != "" || h.Get("Content-Type") != "application/octet-stream" {
		return bad
	}

	size, err := decimal(h.Get("Racer-Object-Length"))
	if err != nil {
		return bad
	}

	first, err := decimal(h.Get("Racer-Range-Start"))
	if err != nil {
		return bad
	}

	end, err := decimal(h.Get("Racer-Range-End"))
	if err != nil {
		return bad
	}

	expiry, err := decimal(h.Get("Racer-Expires-At"))
	if err != nil {
		return bad
	}

	tag, err := ParseETag(h.Get("ETag"))
	if err != nil {
		return bad
	}

	m := Metadata{Size: ByteLength(size), ETag: tag, ExpiresAt: time.UnixMilli(int64(expiry)).UTC(), ContentType: h.Get("Racer-Content-Type")}
	if m.Validate() != nil || first > end || end > size || first != uint64(o.Offset) || o.Length == 0 && end != size || o.Pin.value != "" && o.Pin != tag {
		return bad
	}

	if o.Length != 0 && end-first != uint64(o.Length) {
		if end == size && uint64(o.Length) > end-first {
			return failure(ErrorUnsatisfiableRange, "subscription range", nil)
		}

		return bad
	}

	if o.Metadata != nil && (o.Metadata.Size != m.Size || o.Metadata.ETag != m.ETag || o.Metadata.ContentType != "" && m.ContentType != "" && o.Metadata.ContentType != m.ContentType) {
		return bad
	}

	if o.SmallObject && m.Size > PageSize {
		return failure(ErrorInvalidArgument, "small object size", nil)
	}

	s.first, s.end = first, end
	if end > first {
		s.pages = (end-1)/uint64(PageSize) - first/uint64(PageSize) + 1
	}

	if s.pages+1 > (math.MaxInt64-(end-first))/21 || length != end-first+21*(s.pages+1) {
		return bad
	}

	s.owner.metadata = m
	if o.Metadata != nil {
		s.owner.metadata = *o.Metadata
	}

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

	for {
		s.mu.Lock()
		ready := s.delivered == s.pages || len(s.outstanding) < s.pageCredits && s.byteCredits-s.bytesHeld >= uint64(PageSize)
		s.mu.Unlock()

		if ready {
			break
		}

		select {
		case <-s.notify:
		case <-s.owner.ctx.Done():
			return nil, ioFailure("subscription", s.owner.ctx.Err())
		}
	}

	var frame [21]byte
	if err := s.readFull(frame[:]); err != nil {
		return nil, err
	}

	number := binary.BigEndian.Uint64(frame[1:9])
	offset := binary.BigEndian.Uint64(frame[9:17])
	length := binary.BigEndian.Uint32(frame[17:21])
	bad := failure(ErrorProtocol, "subscription frame", nil)

	if frame[0] == 2 {
		if number != s.pages || s.delivered != s.pages || offset != s.end-s.first || s.deliveredBytes != offset || length != 0 {
			return nil, bad
		}

		s.mu.Lock()
		s.complete = true
		s.mu.Unlock()

		return nil, io.EOF
	}

	if frame[0] != 1 || length == 0 || offset < s.first || offset >= s.end || number != offset/uint64(PageSize) {
		return nil, bad
	}

	start := max(s.first, number*uint64(PageSize))

	end := min(s.end, (number+1)*uint64(PageSize))
	if offset != start || uint64(length) != end-start || s.ordered && number != s.first/uint64(PageSize)+s.delivered {
		return nil, bad
	}

	s.mu.Lock()

	valid := len(s.outstanding) < s.pageCredits && uint64(length) <= s.byteCredits-s.bytesHeld
	if valid {
		valid = s.record(number)
	}

	if valid {
		s.outstanding[number] = length
		s.bytesHeld += uint64(length)
	}
	s.mu.Unlock()

	if !valid {
		return nil, bad
	}

	buffer := s.getBuffer(int(length))
	data := buffer[:length:length]
	validPayload := false

	defer func() {
		if !validPayload {
			s.mu.Lock()
			delete(s.outstanding, number)
			s.bytesHeld -= uint64(length)
			s.mu.Unlock()
			s.putBuffer(buffer)
		}
	}()

	if err := s.readPayload(data); err != nil {
		return nil, err
	}

	s.delivered++

	s.deliveredBytes += uint64(length)
	if s.delivered == s.pages {
		if err := s.readFull(frame[:]); err != nil {
			return nil, err
		}

		if frame[0] != 2 || binary.BigEndian.Uint64(frame[1:9]) != s.pages || binary.BigEndian.Uint64(frame[9:17]) != s.end-s.first || binary.BigEndian.Uint32(frame[17:21]) != 0 || s.deliveredBytes != s.end-s.first {
			return nil, bad
		}

		s.mu.Lock()
		s.complete = true
		s.mu.Unlock()
	}

	validPayload = true

	return &PageLease{Number: number, Offset: ByteOffset(offset), Data: data, stream: s, number: number, length: length, buffer: buffer}, nil
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

// record merges adjacent page intervals, avoiding an object-sized bitmap. The
// hard interval limit bounds memory even for an adversarial fragmented stream.
func (s *PageStream) record(n uint64) bool {
	i := 0
	for i < len(s.intervals) && s.intervals[i].end <= n {
		i++
	}

	if i < len(s.intervals) && s.intervals[i].first <= n {
		return false
	}

	if i > 0 && s.intervals[i-1].end == n {
		s.intervals[i-1].end++
		if i < len(s.intervals) && s.intervals[i].first == n+1 {
			s.intervals[i-1].end = s.intervals[i].end
			s.intervals = append(s.intervals[:i], s.intervals[i+1:]...)
		}

		return true
	}

	if i < len(s.intervals) && s.intervals[i].first == n+1 {
		s.intervals[i].first = n
		return true
	}

	if len(s.intervals) == 4096 {
		return false
	}

	s.intervals = append(s.intervals, pageInterval{})
	copy(s.intervals[i+1:], s.intervals[i:])
	s.intervals[i] = pageInterval{n, n + 1}

	return true
}

func (s *PageStream) readFull(p []byte) error {
	return s.readBytes(p, false)
}

func (s *PageStream) readPayload(p []byte) error {
	return s.readBytes(p, true)
}

func (s *PageStream) readBytes(p []byte, payload bool) error {
	for len(p) > 0 {
		if err := s.conn.SetReadDeadline(time.Now().Add(s.owner.client.config.BodyReadTimeout)); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			return ioFailure("subscription deadline", err)
		}

		n, err := s.conn.reader.Read(p)
		if payload {
			s.owner.client.stats.bytesRead.Add(uint64(n))
		}

		p = p[n:]

		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
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

func (s *PageStream) release(number uint64, length uint32) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
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

		var frame [12]byte
		binary.BigEndian.PutUint64(frame[:8], number)
		binary.BigEndian.PutUint32(frame[8:], length)

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
			if s.buffers != nil {
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

// DownloadTo writes page slices at absolute object offsets. It releases every
// lease after WriteAt returns and closes the subscription on all exit paths.
// An arbitrary caller-owned WriterAt cannot be interrupted by cancellation.
func (c *Client) DownloadTo(ctx context.Context, request Request, w io.WriterAt, options ...ReadOptions) (int64, error) {
	if w == nil {
		return 0, failure(ErrorInvalidArgument, "download destination", nil)
	}

	s, err := c.OpenPages(ctx, request, options...)
	if err != nil {
		return 0, err
	}
	defer closeBody(s)

	var written int64

	for {
		p, err := s.Next()
		if err == io.EOF {
			return written, nil
		}

		if err != nil {
			return written, err
		}

		n, err := w.WriteAt(p.Data, int64(p.Offset))
		if n < 0 || n > len(p.Data) {
			n = 0

			if err == nil {
				err = io.ErrShortWrite
			}
		}

		written += int64(n)
		if err == nil && n != len(p.Data) {
			err = io.ErrShortWrite
		}

		releaseErr := p.Release()

		if err != nil {
			return written, err
		}

		if releaseErr != nil {
			return written, releaseErr
		}
	}
}
