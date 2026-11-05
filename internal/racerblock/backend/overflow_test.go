// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"math"
	"strings"
	"testing"
)

func TestNormalizeCacheBytesOverflow(t *testing.T) {
	maxRA := int64(math.MaxInt64) / MaxChunkSize * MaxChunkSize
	exact := int64(MinCacheChunks*MaxChunkSize) + (math.MaxInt64-MinCacheChunks*MaxChunkSize)/MaxChunkSize*MaxChunkSize

	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"max readahead default chunk", Config{ReadaheadMax: math.MaxInt64}, "overflows int64"},
		{"max readahead max chunk", Config{ChunkSize: MaxChunkSize, ReadaheadMax: math.MaxInt64, CacheBytes: math.MaxInt64}, "overflows int64"},
		{"rounded max readahead", Config{ChunkSize: MaxChunkSize, ReadaheadMax: maxRA, CacheBytes: math.MaxInt64}, "overflows int64"},
		{"largest valid sum", Config{ChunkSize: MaxChunkSize, ReadaheadMax: exact - MinCacheChunks*MaxChunkSize, CacheBytes: exact}, ""},
		{"largest sum one byte short", Config{ChunkSize: MaxChunkSize, ReadaheadMax: exact - MinCacheChunks*MaxChunkSize, CacheBytes: exact - 1}, "must be at least"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.cfg.normalize()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("normalize: %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "at least") {
				t.Fatalf("normalize error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestStreamAdvanceNearMaxInt64(t *testing.T) {
	const size = math.MaxInt64

	maxWindow := int64(math.MaxInt64 / DefaultChunkSize * DefaultChunkSize)
	s := stream{next: size - 10, raEnd: size - 10, window: maxWindow}

	start, stop := s.advance(size, maxWindow)
	if start != size-10 || stop != size {
		t.Fatalf("advance = [%d, %d), want [%d, %d)", start, stop, int64(size-10), int64(size))
	}

	if s.window != maxWindow {
		t.Fatalf("window = %d, want %d", s.window, maxWindow)
	}

	if start, stop = s.advance(size, maxWindow); stop != start {
		t.Fatalf("advance at end = [%d, %d), want empty", start, stop)
	}

	s = stream{window: maxWindow/2 + 1}
	s.advance(size, maxWindow)

	if s.window != maxWindow {
		t.Fatalf("window after doubling = %d, want %d", s.window, maxWindow)
	}
}

func TestStreamAdvanceDoubles(t *testing.T) {
	s := stream{window: 2 * DefaultChunkSize}

	start, stop := s.advance(1<<40, DefaultReadaheadMax)
	if start != 0 || stop != 2*DefaultChunkSize || s.window != 4*DefaultChunkSize {
		t.Fatalf("advance = [%d, %d) window %d", start, stop, s.window)
	}

	s.raEnd = 10 * DefaultChunkSize // well ahead of the reader: no range due
	if start, stop = s.advance(1<<40, DefaultReadaheadMax); stop != start {
		t.Fatalf("advance = [%d, %d), want empty", start, stop)
	}
}
