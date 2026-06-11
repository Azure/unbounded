// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"encoding/binary"
	"io"
)

// contentReader is a deterministic io.Reader that emits exactly size bytes
// derived from a seed and an object index. The same (seed, index, size)
// always produces the same bytes, with bounded memory usage regardless of
// object size. It is not safe for concurrent use.
type contentReader struct {
	state     uint64
	remaining int64
	buf       [8]byte
	bufLen    int
}

// newContentReader returns a reader that emits size bytes for object index.
func newContentReader(seed, index, size int64) *contentReader {
	// Mix the seed and index so distinct objects get distinct streams while
	// remaining fully deterministic.
	state := mix(uint64(seed)) ^ mix(uint64(index)*0x9E3779B97F4A7C15)

	return &contentReader{state: state, remaining: size}
}

// Read fills p with deterministic bytes until size bytes have been produced.
func (r *contentReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}

	n := 0

	for n < len(p) && r.remaining > 0 {
		if r.bufLen == 0 {
			r.state += 0x9E3779B97F4A7C15
			binary.LittleEndian.PutUint64(r.buf[:], mix(r.state))
			r.bufLen = 8
		}

		// Copy from the tail of buf that is still unconsumed, but never
		// emit more than remaining bytes so we don't write past the limit
		// into the caller's buffer.
		off := 8 - r.bufLen

		avail := r.buf[off:]
		if int64(len(avail)) > r.remaining {
			avail = avail[:r.remaining]
		}

		c := copy(p[n:], avail)

		r.bufLen -= c
		r.remaining -= int64(c)
		n += c
	}

	return n, nil
}
