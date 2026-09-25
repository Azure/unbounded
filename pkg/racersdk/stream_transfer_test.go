// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
)

type streamResultWriter struct {
	written int
	err     error
	cancel  context.CancelFunc
	calls   int
	input   string
}

func (w *streamResultWriter) Write(p []byte) (int, error) {
	w.calls++

	w.input += string(p)
	if w.cancel != nil {
		w.cancel()
	}

	return w.written, w.err
}

func newTransferTestStream(t *testing.T, ctx context.Context) *Stream {
	t.Helper()

	c := generatedStreamClient(t, PageSize, nil)

	o, err := c.Open(ctx, "/object")
	if err != nil {
		t.Fatal(err)
	}

	s, err := o.ReadRange(ctx, PageSize-8, 8)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })

	return s
}

func TestStreamBufferedWriteResults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		written int
		err     error
		cancel  bool
		wantErr error
	}{
		{name: "complete", written: 8},
		{name: "short", written: 3, wantErr: io.ErrShortWrite},
		{name: "no_progress", wantErr: io.ErrShortWrite},
		{name: "partial_error", written: 3, err: io.ErrClosedPipe, wantErr: io.ErrClosedPipe},
		{name: "complete_error", written: 8, err: io.ErrClosedPipe, wantErr: io.ErrClosedPipe},
		{name: "canceled_partial_error", written: 3, err: io.ErrClosedPipe, cancel: true, wantErr: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			s := newTransferTestStream(t, ctx)

			dst := &streamResultWriter{written: tc.written, err: tc.err}
			if tc.cancel {
				dst.cancel = cancel
			}

			n, err := s.WriteTo(dst)
			if n != int64(tc.written) || !errors.Is(err, tc.wantErr) || dst.calls != 1 || dst.input != string(make([]byte, 8)) {
				t.Fatalf("WriteTo = %d, %v; writer = %+v", n, err, dst)
			}

			stats := s.Stats()
			if stats.BufferedBytes != 8 || stats.SpliceBytes != 0 || stats.SpliceCalls != 0 || stats.PageRequests != 1 {
				t.Fatalf("read and forwarded byte accounting: %+v", stats)
			}

			// The complete upstream response is reusable even if forwarding its
			// final buffered bytes fails. The downstream owns its own framing.
			pool := s.object.client.streamPool
			if len(pool.idle) != 1 || s.page.conn != nil {
				t.Fatal("fully consumed response was not returned to the pool")
			}

			f := s.Failure()
			if tc.wantErr == nil {
				if f != nil {
					t.Fatalf("successful transfer recorded failure: %+v", f)
				}

				return
			}

			original := tc.err
			if original == nil {
				original = io.ErrShortWrite
			}

			if f == nil || f.Operation != "downstream_write" || f.PageOffset != PageSize-8 || f.Offset != PageSize || f.StatusCode != 206 || !errors.Is(f.Err, original) || !errors.Is(f.ContextErr, ctx.Err()) {
				t.Fatalf("lost downstream failure evidence: %+v", f)
			}

			if n, err := s.WriteTo(io.Discard); n != 0 || !errors.Is(err, tc.wantErr) {
				t.Fatalf("terminal WriteTo = %d, %v", n, err)
			}

			if err := s.Prepare(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("terminal Prepare = %v", err)
			}

			_ = s.Close()
			if got := s.Failure(); *got != *f {
				t.Fatalf("cleanup changed failure: %+v -> %+v", f, got)
			}
		})
	}
}

func TestStreamZeroLengthReadStateOrdering(t *testing.T) {
	for _, tc := range []struct {
		name    string
		end     int64
		closed  bool
		cancel  bool
		err     error
		wantErr error
	}{
		{name: "active", end: 1},
		{name: "eof", wantErr: io.EOF},
		{name: "canceled_before_eof", cancel: true, wantErr: context.Canceled},
		{name: "failure_before_cancellation", cancel: true, err: io.ErrClosedPipe, wantErr: io.ErrClosedPipe},
		{name: "closed_before_failure", closed: true, cancel: true, err: io.ErrClosedPipe, wantErr: net.ErrClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			if tc.cancel {
				cancel()
			}

			s := &Stream{ctx: ctx, cancel: cancel, page: newPreparedPage(ctx, nil, 0), end: tc.end, closed: tc.closed, err: tc.err}
			defer s.Close()

			if n, err := s.read(nil); n != 0 || !errors.Is(err, tc.wantErr) {
				t.Fatalf("read(nil) = %d, %v; want 0, %v", n, err, tc.wantErr)
			}

			if stats := s.Stats(); stats != (TransferStats{}) {
				t.Fatalf("zero-length read performed transfer work: %+v", stats)
			}
		})
	}
}

func TestStreamFinalBufferedReadEOFAndReuse(t *testing.T) {
	s := newTransferTestStream(t, t.Context())
	if n, err := s.read(nil); n != 0 || err != nil || s.Stats().PageRequests != 0 {
		t.Fatalf("zero-length read prepared a page: %d, %v", n, err)
	}

	var buf [8]byte
	if n, err := s.read(buf[:]); n != len(buf) || err != nil || buf != [8]byte{} {
		t.Fatalf("final read = %d, %v, %q", n, err, buf)
	}

	if len(s.object.client.streamPool.idle) != 1 || s.page.conn != nil {
		t.Fatal("final read did not release its socket")
	}

	for _, p := range [][]byte{nil, buf[:]} {
		if n, err := s.read(p); n != 0 || err != io.EOF {
			t.Fatalf("completed read = %d, %v", n, err)
		}
	}

	if err := s.Prepare(); err != nil {
		t.Fatalf("completed Prepare = %v", err)
	}

	if n, err := s.WriteTo(io.Discard); n != 0 || err != nil {
		t.Fatalf("completed WriteTo = %d, %v", n, err)
	}

	if stats := s.Stats(); stats.PageRequests != 1 || stats.BufferedBytes != 8 || s.Failure() != nil {
		t.Fatalf("completion changed accounting: %+v, %+v", stats, s.Failure())
	}
}
