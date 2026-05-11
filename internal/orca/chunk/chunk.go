// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package chunk implements the chunk model: ChunkKey, deterministic
// path encoding, and the range -> chunk-index iterator.
package chunk

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
)

// Key is the immutable identifier for a chunk.
//
// Path encoding:
//
//	LP(s)   = LE64(uint64(len(s))) || s
//	hashKey = sha256(
//	            LP(origin_id) ||
//	            LP(bucket)    ||
//	            LP(key)       ||
//	            LP(etag)      ||
//	            LE64(chunk_size)
//	          )
//	path    = "<origin_id>/<hex(hashKey)>/<chunk_index>"
type Key struct {
	OriginID  string
	Bucket    string
	ObjectKey string
	ETag      string
	ChunkSize int64
	Index     int64
}

// Path returns the canonical on-store path for this ChunkKey.
func (k Key) Path() string {
	h := sha256.New()
	writeLP(h, k.OriginID)
	writeLP(h, k.Bucket)
	writeLP(h, k.ObjectKey)
	writeLP(h, k.ETag)

	var sizeBuf [8]byte
	binary.LittleEndian.PutUint64(sizeBuf[:], uint64(k.ChunkSize))
	h.Write(sizeBuf[:])
	sum := h.Sum(nil)

	return fmt.Sprintf("%s/%s/%d", k.OriginID, hex.EncodeToString(sum), k.Index)
}

// Range returns the byte range [Off, Off+Len) within the origin
// object that this chunk corresponds to.
func (k Key) Range() (off, length int64) {
	off = k.Index * k.ChunkSize
	length = k.ChunkSize

	return off, length
}

// ExpectedLen returns the authoritative number of bytes this chunk
// should contain given the object's total size. For non-tail chunks
// this is k.ChunkSize; for the tail chunk it is the remainder. If
// objectSize is zero or negative (unknown), returns k.ChunkSize. If
// the chunk is entirely past the end of the object, returns 0.
func (k Key) ExpectedLen(objectSize int64) int64 {
	if objectSize <= 0 {
		return k.ChunkSize
	}

	off := k.Index * k.ChunkSize
	if off >= objectSize {
		return 0
	}

	remaining := objectSize - off
	if remaining < k.ChunkSize {
		return remaining
	}

	return k.ChunkSize
}

// String renders the key compactly for logging.
func (k Key) String() string {
	if len(k.ETag) > 8 {
		return fmt.Sprintf("ChunkKey{%s/%s/%s..@%d#%d}",
			k.OriginID, k.Bucket, k.ObjectKey, k.Index, len(k.ETag))
	}

	return fmt.Sprintf("ChunkKey{%s/%s/%s@%d}", k.OriginID, k.Bucket, k.ObjectKey, k.Index)
}

func writeLP(h hash.Hash, s string) {
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(s)))
	h.Write(lenBuf[:])
	h.Write([]byte(s))
}

// IndexRange returns the inclusive [first, last] chunk indices that
// cover the byte range [start, end] of an object whose total size is
// objectSize.
//
// Caller is responsible for clamping start / end against objectSize
// before invoking; if end >= objectSize, end is clamped here. If
// end is negative (e.g. an empty-object [0, -1] degenerate range),
// last is clamped to 0 so the integer-division floor does not
// silently produce a negative chunk index that could leak into
// downstream loop bounds.
func IndexRange(start, end, chunkSize, objectSize int64) (first, last int64) {
	if end >= objectSize {
		end = objectSize - 1
	}

	if end < 0 {
		end = 0
	}

	first = start / chunkSize
	last = end / chunkSize

	return first, last
}

// ChunkSlice returns the [off, len) within a single chunk that
// satisfies the original client byte range [start, end].
//
// chunkIdx is the chunk index. chunkSize is the configured chunk size.
// objectSize is the total origin-object size (used to clamp the last
// chunk if it is partial).
func ChunkSlice(chunkIdx, chunkSize, start, end, objectSize int64) (off, length int64) {
	chunkStart := chunkIdx * chunkSize

	chunkEnd := chunkStart + chunkSize - 1
	if chunkEnd >= objectSize {
		chunkEnd = objectSize - 1
	}

	if start > chunkStart {
		off = start - chunkStart
	}

	sliceEnd := chunkEnd
	if end < chunkEnd {
		sliceEnd = end
	}

	length = sliceEnd - chunkStart - off + 1

	return off, length
}
