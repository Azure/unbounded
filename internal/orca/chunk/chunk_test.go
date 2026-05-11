// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chunk

import (
	"strings"
	"testing"
)

// TestKey_ExpectedLen covers the per-chunk expected length given an
// object size: full chunks for non-tail, remainder for the tail, 0 for
// past-end, k.ChunkSize when objectSize is unknown (<= 0).
func TestKey_ExpectedLen(t *testing.T) {
	t.Parallel()

	const cs = int64(1024)

	tests := []struct {
		name       string
		k          Key
		objectSize int64
		want       int64
	}{
		{"full chunk 0", Key{ChunkSize: cs, Index: 0}, 4096, cs},
		{"full chunk 2", Key{ChunkSize: cs, Index: 2}, 4096, cs},
		{"tail chunk partial", Key{ChunkSize: cs, Index: 3}, 3500, 3500 - 3072},
		{"chunk exactly fills object", Key{ChunkSize: cs, Index: 3}, 4096, cs},
		{"chunk past end returns 0", Key{ChunkSize: cs, Index: 5}, 3500, 0},
		{"objectSize 0 -> ChunkSize (unknown)", Key{ChunkSize: cs, Index: 0}, 0, cs},
		{"objectSize negative -> ChunkSize", Key{ChunkSize: cs, Index: 7}, -1, cs},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.k.ExpectedLen(tc.objectSize)
			if got != tc.want {
				t.Errorf("ExpectedLen=%d want %d", got, tc.want)
			}
		})
	}
}

// TestKey_Path_Deterministic verifies that the same inputs always
// produce the same path and that meaningful input differences
// (OriginID, Bucket, ObjectKey, ETag, ChunkSize, Index) produce
// distinct paths. The path encoding is part of orca's design
// contract: any change here invalidates previously cached chunks.
func TestKey_Path_Deterministic(t *testing.T) {
	t.Parallel()

	base := Key{
		OriginID:  "origin-a",
		Bucket:    "bucket",
		ObjectKey: "key",
		ETag:      "etag1",
		ChunkSize: 1024,
		Index:     0,
	}
	// Same inputs -> same path. Compare two equally-constructed Keys
	// (calling Path() on the same receiver tautologically passes).
	dup := base
	if base.Path() != dup.Path() {
		t.Fatalf("Path() not deterministic for identical key")
	}

	other := base
	otherPath := other.Path()

	mutations := []struct {
		name string
		mut  func(k *Key)
	}{
		{"different origin", func(k *Key) { k.OriginID = "origin-b" }},
		{"different bucket", func(k *Key) { k.Bucket = "other-bucket" }},
		{"different key", func(k *Key) { k.ObjectKey = "other-key" }},
		{"different etag", func(k *Key) { k.ETag = "etag2" }},
		{"different chunk size", func(k *Key) { k.ChunkSize = 2048 }},
		{"different index", func(k *Key) { k.Index = 1 }},
	}

	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			mutated := base
			m.mut(&mutated)

			got := mutated.Path()
			if got == otherPath {
				t.Errorf("path collision after %s mutation: %q", m.name, got)
			}
		})
	}
}

// TestKey_Path_Format asserts the documented path shape:
// "<origin_id>/<hex(sha256)>/<chunk_index>".
func TestKey_Path_Format(t *testing.T) {
	t.Parallel()

	k := Key{
		OriginID:  "origin-a",
		Bucket:    "b",
		ObjectKey: "k",
		ETag:      "e",
		ChunkSize: 1024,
		Index:     7,
	}

	path := k.Path()

	parts := strings.Split(path, "/")
	if len(parts) != 3 {
		t.Fatalf("path %q has %d segments, want 3", path, len(parts))
	}

	if parts[0] != "origin-a" {
		t.Errorf("origin segment=%q want %q", parts[0], "origin-a")
	}

	if len(parts[1]) != 64 {
		t.Errorf("hex segment len=%d want 64 (sha256)", len(parts[1]))
	}

	for _, c := range parts[1] {
		isDigit := c >= '0' && c <= '9'
		isLowerHex := c >= 'a' && c <= 'f'

		if !isDigit && !isLowerHex {
			t.Errorf("hex segment contains non-hex char %q", c)
			break
		}
	}

	if parts[2] != "7" {
		t.Errorf("index segment=%q want %q", parts[2], "7")
	}
}

// TestKey_Range verifies (off, length) = (Index*ChunkSize, ChunkSize).
func TestKey_Range(t *testing.T) {
	t.Parallel()

	k := Key{ChunkSize: 1 << 20, Index: 3}

	off, length := k.Range()
	if off != 3<<20 {
		t.Errorf("off=%d want %d", off, 3<<20)
	}

	if length != 1<<20 {
		t.Errorf("length=%d want %d", length, 1<<20)
	}
}

// TestIndexRange covers the chunk-index span computed from a byte
// range plus the end clamping to objectSize.
func TestIndexRange(t *testing.T) {
	t.Parallel()

	const chunkSize = int64(1024)

	tests := []struct {
		name       string
		start, end int64
		objectSize int64
		wantFirst  int64
		wantLast   int64
	}{
		{"aligned full chunk", 0, 1023, 1024, 0, 0},
		{"aligned two chunks", 0, 2047, 4096, 0, 1},
		{"start mid-chunk, end mid-chunk same", 100, 500, 1024, 0, 0},
		{"start mid-chunk, end mid-next-chunk", 100, 1500, 4096, 0, 1},
		{"end clamped to objectSize", 0, 9999, 2048, 0, 1},
		{"single byte", 5, 5, 1024, 0, 0},
		{"last partial chunk", 1024, 1500, 1500, 1, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, last := IndexRange(tt.start, tt.end, chunkSize, tt.objectSize)
			if first != tt.wantFirst {
				t.Errorf("first=%d want %d", first, tt.wantFirst)
			}

			if last != tt.wantLast {
				t.Errorf("last=%d want %d", last, tt.wantLast)
			}
		})
	}
}

// TestChunkSlice covers the (off, length) within a single chunk that
// satisfies the original byte range. Critical for cross-chunk
// streamSlice copies.
func TestChunkSlice(t *testing.T) {
	t.Parallel()

	const chunkSize = int64(1024)

	tests := []struct {
		name       string
		chunkIdx   int64
		start      int64
		end        int64
		objectSize int64
		wantOff    int64
		wantLen    int64
	}{
		{"entirely within chunk 0", 0, 100, 199, 4096, 100, 100},
		{"start at chunk 0 boundary", 0, 0, 99, 4096, 0, 100},
		{"end at chunk 0 boundary", 0, 0, 1023, 4096, 0, 1024},
		{"chunk 1, range covers full chunk", 1, 1024, 2047, 4096, 0, 1024},
		{"chunk spans range start", 1, 500, 1500, 4096, 0, 477}, // [1024..1500]
		{"chunk spans range end", 1, 1500, 2500, 4096, 476, 548},
		{"last partial chunk", 3, 3000, 3500, 3500, 0, 428},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			off, length := ChunkSlice(tt.chunkIdx, chunkSize, tt.start, tt.end, tt.objectSize)
			if off != tt.wantOff {
				t.Errorf("off=%d want %d", off, tt.wantOff)
			}

			if length != tt.wantLen {
				t.Errorf("length=%d want %d", length, tt.wantLen)
			}
		})
	}
}

// TestKey_String covers both formatting branches (short ETag + long
// ETag).
func TestKey_String(t *testing.T) {
	t.Parallel()

	short := Key{
		OriginID:  "o",
		Bucket:    "b",
		ObjectKey: "k",
		ETag:      "abc",
		Index:     5,
	}
	if s := short.String(); !strings.Contains(s, "@5") {
		t.Errorf("short ETag string=%q does not contain @5", s)
	}

	long := Key{
		OriginID:  "o",
		Bucket:    "b",
		ObjectKey: "k",
		ETag:      "abcdefghi", // 9 chars > 8
		Index:     5,
	}

	s := long.String()
	if !strings.Contains(s, "..@") {
		t.Errorf("long ETag string=%q does not contain truncation marker '..@'", s)
	}

	if !strings.Contains(s, "#9") {
		t.Errorf("long ETag string=%q does not contain length suffix '#9'", s)
	}
}
