// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"math"
	"testing"
)

// prefetchAndWait runs b.prefetch and waits for its fetches to settle.
func prefetchAndWait(b *Backend, start, stop int64) {
	b.prefetch(start, stop)
	b.wg.Wait()
}

func TestPrefetchFinalPartialChunkNearMaxInt64(t *testing.T) {
	const cs = MinChunkSize

	f := newFake(math.MaxInt64 - 511)
	cfg := testConfig()
	cfg.ReadaheadMax = 4 * cs
	b := mustOpen(t, f, cfg)

	size := b.Size()
	last := (size - 1) / cs

	prefetchAndWait(b, (last-2)*cs, size)

	b.mu.Lock()

	n := len(b.chunks)
	for idx, c := range b.chunks {
		if idx < last-2 || idx > last {
			t.Errorf("chunk %d outside [%d, %d]", idx, last-2, last)
		}

		if want := b.chunkLen(idx); int64(len(c.buf)) != want {
			t.Errorf("chunk %d len = %d, want %d", idx, len(c.buf), want)
		}
	}
	b.mu.Unlock()

	if n != 3 {
		t.Fatalf("prefetched %d chunks, want 3", n)
	}

	for _, c := range f.openCalls() {
		if c.off < 0 || c.length <= 0 || c.off > size-c.length {
			t.Fatalf("open out of range: %+v", c)
		}
	}

	opens := len(f.openCalls())

	checkRead(t, b, size-100, 100)
	checkRead(t, b, last*cs, size-last*cs)

	if got := len(f.openCalls()); got != opens {
		t.Fatalf("tail reads opened %d more ranges, want 0", got-opens)
	}
}

func TestPrefetchBounds(t *testing.T) {
	const cs = MinChunkSize

	tests := []struct {
		name        string
		start, stop int64
		want        int
	}{
		{"empty", 3 * cs, 3 * cs, 0},
		{"inverted", 3 * cs, 2 * cs, 0},
		{"one byte", 3*cs + 7, 3*cs + 8, 1},
		{"chunk aligned", 2 * cs, 4 * cs, 2},
		{"unaligned", 2*cs + 1, 4*cs + 1, 3},
		{"to end", 8 * cs, 10*cs - 5, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(10*cs - 5)
			cfg := testConfig()
			cfg.ReadaheadMax = 4 * cs
			b := mustOpen(t, f, cfg)

			prefetchAndWait(b, tt.start, tt.stop)

			b.mu.Lock()
			n := len(b.chunks)
			b.mu.Unlock()

			if n != tt.want {
				t.Fatalf("prefetched %d chunks, want %d", n, tt.want)
			}

			for _, c := range f.openCalls() {
				if c.off+c.length > b.Size() {
					t.Fatalf("open past end: %+v", c)
				}
			}
		})
	}
}

func TestStreamMatchNearMaxInt64(t *testing.T) {
	const cs = MinChunkSize

	f := newFake(math.MaxInt64 - 511)
	cfg := testConfig()
	cfg.ReadaheadMax = 4 * cs
	b := mustOpen(t, f, cfg)

	size := b.Size()

	// Sequential reads within streamSlack of MaxInt64 must still be
	// recognized as one stream and trigger read-ahead.
	for off := size - streamSlack; off < size-cs; off += cs / 4 {
		checkRead(t, b, off, cs/4)
	}

	if s := b.Stats(); s.ReadaheadFetches == 0 {
		t.Fatalf("no read-ahead near MaxInt64: %+v", s)
	}
}
