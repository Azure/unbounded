// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"fmt"
	"io"
)

// ReadAhead is an opt-in, bounded forward window over an immutable Object.
// Construct it with Object.ReadAhead. Reads are serialized, including cache hits;
// waiting and I/O use each ReadAt call's context. It owns no background work or
// connections and needs no Close. It must not be copied.
type ReadAhead struct {
	object   *Object
	gate     chan struct{}
	maxBytes int
	buf      []byte
	offset   int64
}

// ReadAhead creates an independent read-ahead adapter with a positive payload
// storage limit in bytes. Storage is allocated lazily, at most min(maxBytes,
// object size). Transport buffers and caller-owned output are additional.
func (o *Object) ReadAhead(maxBytes int) (*ReadAhead, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("racer: read-ahead limit must be positive")
	}

	return &ReadAhead{object: o, gate: make(chan struct{}, 1), maxBytes: int(min(int64(maxBytes), o.meta.Size))}, nil
}

// ReadAt follows Object.ReadAt's bounds, EOF, and contiguous-prefix rules.
// On a miss, reads no larger than the limit fetch a forward window starting at
// off, clipped at EOF; larger reads use exact Object.ReadAt without speculation.
// A fill must succeed in full before any of it is cached. Speculative failures
// are returned even if the requested prefix was read in full. No version retry
// occurs. Cached bytes retain the Object's version and are not revalidated.
func (r *ReadAhead) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("racer: negative offset")
	}

	if len(p) == 0 {
		return 0, ctx.Err()
	}

	if off >= r.object.meta.Size {
		return 0, io.EOF
	}

	select {
	case r.gate <- struct{}{}:
		defer func() { <-r.gate }()
	case <-ctx.Done():
		return 0, context.Cause(ctx)
	}

	if err := context.Cause(ctx); err != nil {
		return 0, err
	}

	length := min(int64(len(p)), r.object.meta.Size-off)
	if off >= r.offset && off-r.offset <= int64(len(r.buf)) && length <= int64(len(r.buf))-(off-r.offset) {
		n := copy(p, r.buf[off-r.offset:off-r.offset+length])
		if n < len(p) {
			return n, io.EOF
		}

		return n, nil
	}

	if length > int64(r.maxBytes) {
		return r.object.ReadAt(ctx, p, off)
	}

	if cap(r.buf) == 0 {
		r.buf = make([]byte, 0, r.maxBytes)
	}
	// Invalidate before reusing the sole allocation: a failed fill must never
	// expose partial or unchecked bytes through a subsequent cache hit.
	r.buf = r.buf[:0]
	window := r.buf[:min(int64(cap(r.buf)), r.object.meta.Size-off)]
	n, err := r.object.ReadAt(ctx, window, off)
	n = copy(p, window[:n])

	if err == nil {
		err = context.Cause(ctx)
	}

	if err != nil {
		return n, err
	}

	r.buf = window
	r.offset = off

	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}
