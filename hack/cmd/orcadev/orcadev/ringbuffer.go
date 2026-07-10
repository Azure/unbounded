// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import "sync"

// ringBuffer is a fixed-capacity io.Writer that retains the most
// recently written bytes and discards older bytes when the buffer
// fills. Used by spawnPortForward to drain a subprocess's stderr
// for diagnostic-on-failure purposes without leaking unbounded
// memory across long-lived sessions.
type ringBuffer struct {
	mu   sync.RWMutex
	buf  []byte
	pos  int  // next write index
	full bool // true once the buffer has wrapped at least once
}

func newRingBuffer(size int) *ringBuffer {
	if size <= 0 {
		size = 1
	}

	return &ringBuffer{buf: make([]byte, size)}
}

// Write appends p to the ring, discarding bytes from the front of
// the logical stream once capacity is reached. Always returns
// (len(p), nil); writes never fail.
func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	written := len(p)
	size := len(r.buf)

	if len(p) >= size {
		// Only the last `size` bytes of p are retained; everything
		// older is gone.
		copy(r.buf, p[len(p)-size:])
		r.pos = 0
		r.full = true

		return written, nil
	}

	avail := size - r.pos
	if len(p) <= avail {
		copy(r.buf[r.pos:], p)
		r.pos += len(p)

		if r.pos == size {
			r.pos = 0
			r.full = true
		}

		return written, nil
	}

	copy(r.buf[r.pos:], p[:avail])
	copy(r.buf, p[avail:])
	r.pos = len(p) - avail
	r.full = true

	return written, nil
}

// String returns the retained bytes in write order (oldest first).
func (r *ringBuffer) String() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if !r.full {
		return string(r.buf[:r.pos])
	}

	out := make([]byte, 0, len(r.buf))
	out = append(out, r.buf[r.pos:]...)
	out = append(out, r.buf[:r.pos]...)

	return string(out)
}
