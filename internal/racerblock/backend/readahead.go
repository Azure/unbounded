// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package backend

import "sync"

const (
	// numStreams is how many sequential streams are tracked at once.
	numStreams = 8
	// streamSlack is how far a read may start from the end of the previous
	// read of a stream and still continue it. Block queues reorder nearby
	// requests, so exact adjacency is too strict.
	streamSlack = 256 << 10
	// streamMinHits is how many reads must continue a stream before
	// read-ahead starts, so random reads that happen to land near each
	// other rarely trigger it.
	streamMinHits = 2
)

type stream struct {
	next   int64  // end of the furthest read in the stream
	hits   int    // reads that continued the stream
	window int64  // next read-ahead window, 0 until read-ahead starts
	raEnd  int64  // end of the prefetched region
	used   uint64 // last-use tick for replacement
}

type readahead struct {
	mu      sync.Mutex
	tick    uint64
	streams [numStreams]stream
}

// noteAccess records a read of [off, end) and starts read-ahead when it
// continues a sequential stream.
//
// Heuristic: a read continues a stream when it starts within streamSlack of
// the stream's end. After streamMinHits continuations, read-ahead begins
// with a window of 2 chunks just past the read, aligned to chunks. Whenever
// the reader comes within half a window of the prefetched end, the next
// window is prefetched and the window doubles, up to ReadaheadMax. Streams
// that stop are replaced least recently used first.
func (b *Backend) noteAccess(off, end int64) {
	maxWindow := b.cfg.ReadaheadMax
	if maxWindow <= 0 {
		return
	}

	ra := &b.ra

	ra.mu.Lock()

	ra.tick++

	var s *stream

	for i := range ra.streams {
		c := &ra.streams[i]
		if c.used != 0 && off >= c.next-streamSlack && off <= c.next+streamSlack {
			s = c
			break
		}
	}

	if s == nil {
		s = &ra.streams[0]
		for i := range ra.streams {
			if ra.streams[i].used < s.used {
				s = &ra.streams[i]
			}
		}

		*s = stream{next: end, used: ra.tick}
		ra.mu.Unlock()

		return
	}

	s.used = ra.tick
	s.next = max(s.next, end)
	s.hits++

	if s.hits < streamMinHits {
		ra.mu.Unlock()
		return
	}

	cs := b.chunkSize
	if s.window == 0 {
		s.window = min(2*cs, maxWindow)
	}

	// Start at the chunk holding the stream's end, so a demand miss for it
	// joins the read-ahead fetch instead of opening its own.
	s.raEnd = max(s.raEnd, s.next/cs*cs)

	start, stop := s.advance(b.size, maxWindow)

	ra.mu.Unlock()

	if stop > start {
		b.prefetch(start, stop)
	}
}

// advance returns the next read-ahead range [start, stop) for an object of
// size bytes, or an empty range when none is due, and grows the window up to
// maxWindow. It never computes a value past size or maxWindow, so it cannot
// overflow int64 near the maximum object size.
func (s *stream) advance(size, maxWindow int64) (start, stop int64) {
	if s.raEnd >= size || s.raEnd-s.next > s.window/2 {
		return 0, 0
	}

	start = s.raEnd
	stop = start + min(s.window, size-start)
	s.raEnd = stop

	if s.window > maxWindow/2 {
		s.window = maxWindow
	} else {
		s.window *= 2
	}

	return start, stop
}

// prefetch starts background fetches for the missing chunks in [start, stop).
// Cached and pending chunks are skipped, and it stops at the first chunk it
// cannot get a buffer for without overcommitting.
func (b *Backend) prefetch(start, stop int64) {
	var jobs [][]*chunk

	b.mu.Lock()

	if b.closed {
		b.mu.Unlock()
		return
	}

	var run []*chunk

	for idx := start / b.chunkSize; idx*b.chunkSize < stop; idx++ {
		if b.chunks[idx] != nil {
			if len(run) > 0 {
				jobs, run = append(jobs, run), nil
			}

			continue
		}

		c := b.newChunkLocked(idx, true)
		if c == nil {
			break
		}

		run = append(run, c)
	}

	if len(run) > 0 {
		jobs = append(jobs, run)
	}

	b.wg.Add(len(jobs))
	b.mu.Unlock()

	for _, job := range jobs {
		b.stats.readahead.Add(1)

		go b.fetch(job, true)
	}
}
