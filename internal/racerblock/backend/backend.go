// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package backend serves random-access reads of one pinned version of a
// Racer object, for the racer-block ublk device.
//
// Racer opens a new connection for every ranged Get, so [Backend] never maps
// block requests one to one onto Gets. It caches fixed-size chunks, fetches
// each run of contiguous missing chunks of a request with one Get, lets
// concurrent requests share in-flight fetches, and prefetches ahead of
// sequential streams.
package backend

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// ErrClosed is returned by ReadAt after Close.
var ErrClosed = errors.New("racerblock backend: closed")

// Stats are cumulative counters, plus current and peak memory use.
type Stats struct {
	Hits             uint64 // chunk lookups served from completed chunks
	Misses           uint64 // chunk lookups that started a demand fetch
	CoalescedWaits   uint64 // chunk lookups that waited on another fetch
	Fetches          uint64 // Source.Open calls, including retries
	FetchedBytes     uint64 // bytes copied into chunks
	ReadaheadFetches uint64 // read-ahead fetch jobs started
	Retries          uint64 // retried fetch attempts
	Errors           uint64 // fetch jobs that failed
	Evictions        uint64 // completed chunks evicted for space
	Overcommits      uint64 // demand buffers allocated beyond CacheBytes

	ResidentBytes int64 // buffers held by cached and pending chunks
	PeakBytes     int64 // high-water mark of ResidentBytes
}

// Backend reads one pinned object version through a chunk cache.
//
// Memory: buffers of resident and pending chunks, plus the free list, are
// counted against CacheBytes. A fetch first reuses a free buffer, then
// allocates if under budget, then evicts the least recently used completed
// chunk that no reader is copying from. When nothing can be evicted because
// every buffer is pending or pinned, read-ahead is skipped, and demand fetches
// overcommit so they always make progress without waiting on each other.
// Overcommit is bounded by the chunks spanned by concurrent ReadAt calls, and
// the excess is evicted as soon as later fetches complete.
type Backend struct {
	src       Source
	cfg       Config
	log       *slog.Logger
	size      int64
	etag      string
	chunkSize int64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	closed bool
	chunks map[int64]*chunk
	lru    list.List // completed chunks, most recent at front
	used   int64     // bytes of buffers owned by chunks
	free   [][]byte

	ra readahead

	limiter logLimiter
	stats   counters
}

type counters struct {
	hits, misses, coalesced, fetches, fetchedBytes, readahead, retries, errors, evictions, overcommits atomic.Uint64
	peak                                                                                               atomic.Int64
}

type chunk struct {
	idx       int64
	buf       []byte
	done      chan struct{} // closed once ready or err is set
	ready     bool          // guarded by Backend.mu
	err       error         // set before done is closed
	readahead bool
	refs      atomic.Int32 // readers copying or about to copy from buf
	elem      *list.Element
}

// Open pins the object version and size and returns a Backend.
//
// With WarmTTL > 0, Open warms the object with cfg.ETag, which also verifies
// that a pinned version exists. Otherwise it calls Stat; if cfg.ETag is set
// and is not the current version, Open verifies it with a one-byte pinned
// read and takes the size from that read's metadata, so a missing version
// fails fast with racersdk.ErrVersionMismatch.
func Open(ctx context.Context, src Source, cfg Config) (*Backend, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, fmt.Errorf("racerblock backend: config: %w", err)
	}

	md, retainUntil, err := pin(ctx, src, cfg)
	if err != nil {
		return nil, fmt.Errorf("racerblock backend: open: %w", err)
	}

	if md.ETag == "" {
		return nil, errors.New("racerblock backend: open: object has no ETag")
	}

	if cfg.ETag != "" && md.ETag != cfg.ETag {
		return nil, fmt.Errorf("racerblock backend: open: %w: got ETag %q, want %q", racersdk.ErrVersionMismatch, md.ETag, cfg.ETag)
	}

	if md.Size <= 0 {
		return nil, fmt.Errorf("racerblock backend: open: object size %d is not positive", md.Size)
	}

	bctx, cancel := context.WithCancel(context.Background())
	b := &Backend{
		src:       src,
		cfg:       cfg,
		log:       cfg.Logger,
		size:      md.Size,
		etag:      md.ETag,
		chunkSize: cfg.ChunkSize,
		ctx:       bctx,
		cancel:    cancel,
		chunks:    make(map[int64]*chunk),
	}
	b.limiter.last = make(map[string]int64)

	if cfg.WarmTTL > 0 {
		b.wg.Go(func() { b.refreshWarm(retainUntil) })
	}

	return b, nil
}

// pin returns the pinned metadata and, with WarmTTL > 0, the retention
// deadline Racer granted at Open. A zero deadline means Racer refused to
// retain the object.
func pin(ctx context.Context, src Source, cfg Config) (racersdk.Metadata, time.Time, error) {
	if cfg.WarmTTL > 0 {
		res, err := src.Warm(ctx, cfg.WarmTTL, cfg.ETag)
		if err != nil {
			return racersdk.Metadata{}, time.Time{}, fmt.Errorf("warm: %w", err)
		}

		return res.Metadata, res.RetainUntil, nil
	}

	md, err := src.Stat(ctx)
	if err != nil {
		return racersdk.Metadata{}, time.Time{}, fmt.Errorf("stat: %w", err)
	}

	if cfg.ETag == "" || md.ETag == cfg.ETag {
		return md, time.Time{}, nil
	}

	r, err := src.Open(ctx, 0, 1, cfg.ETag)
	if err != nil {
		return racersdk.Metadata{}, time.Time{}, fmt.Errorf("verify pinned ETag %q: %w", cfg.ETag, err)
	}
	defer r.Close() //nolint:errcheck // Only the metadata matters.

	mr, ok := r.(MetadataReader)
	if !ok {
		return racersdk.Metadata{}, time.Time{}, fmt.Errorf("verify pinned ETag %q: source does not report metadata", cfg.ETag)
	}

	return mr.Metadata(), time.Time{}, nil
}

// Size returns the object length.
func (b *Backend) Size() int64 { return b.size }

// ETag returns the pinned version.
func (b *Backend) ETag() string { return b.etag }

// Stats returns a snapshot of the counters.
func (b *Backend) Stats() Stats {
	b.mu.Lock()
	used := b.used
	b.mu.Unlock()

	s := &b.stats

	return Stats{
		Hits: s.hits.Load(), Misses: s.misses.Load(), CoalescedWaits: s.coalesced.Load(),
		Fetches: s.fetches.Load(), FetchedBytes: s.fetchedBytes.Load(), ReadaheadFetches: s.readahead.Load(),
		Retries: s.retries.Load(), Errors: s.errors.Load(), Evictions: s.evictions.Load(),
		Overcommits: s.overcommits.Load(), ResidentBytes: used, PeakBytes: s.peak.Load(),
	}
}

// Close stops warming and cancels in-flight fetches, waiting for them to end.
// ReadAt fails with ErrClosed afterward. Close is idempotent.
func (b *Backend) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()

	b.cancel()
	b.wg.Wait()

	return nil
}

// ReadAt fills p with the object bytes at off. Bytes at or past Size are
// zero. It is safe for concurrent use. A canceled ctx stops the wait but not
// fetches that other readers may share.
func (b *Backend) ReadAt(ctx context.Context, p []byte, off int64) error {
	if off < 0 {
		return fmt.Errorf("racerblock backend: negative offset %d", off)
	}

	if int64(len(p)) > math.MaxInt64-off {
		return fmt.Errorf("racerblock backend: range at %d of length %d overflows", off, len(p))
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()

	if closed {
		return ErrClosed
	}

	end := off + int64(len(p))
	if end > b.size {
		clear(p[max(b.size-off, 0):])
		end = b.size
	}

	if off >= end {
		return nil
	}

	b.noteAccess(off, end)

	for i, last := off/b.chunkSize, (end-1)/b.chunkSize; i <= last; {
		entries, err := b.plan(i, last)
		if err != nil {
			return err
		}

		i, err = b.consume(ctx, entries, p, off, end)
		if err != nil {
			return err
		}
	}

	return nil
}

type entry struct {
	c     *chunk
	owned bool // this call started the fetch
}

// plan pins every chunk in [first, last], starting demand fetches for runs of
// missing chunks.
func (b *Backend) plan(first, last int64) ([]entry, error) {
	entries := make([]entry, 0, last-first+1)

	var jobs [][]*chunk

	b.mu.Lock()

	if b.closed {
		b.mu.Unlock()
		return nil, ErrClosed
	}

	var run []*chunk

	for idx := first; idx <= last; idx++ {
		c := b.chunks[idx]
		if c != nil {
			if len(run) > 0 {
				jobs, run = append(jobs, run), nil
			}

			c.refs.Add(1)

			if c.ready {
				b.lru.MoveToFront(c.elem)
				b.stats.hits.Add(1)
			} else {
				b.stats.coalesced.Add(1)
			}

			entries = append(entries, entry{c: c})

			continue
		}

		c = b.newChunkLocked(idx, false)
		c.refs.Add(1)
		run = append(run, c)
		entries = append(entries, entry{c: c, owned: true})

		b.stats.misses.Add(1)
	}

	if len(run) > 0 {
		jobs = append(jobs, run)
	}

	b.wg.Add(len(jobs))
	b.mu.Unlock()

	for _, job := range jobs {
		go b.fetch(job, false)
	}

	return entries, nil
}

// consume copies the planned chunks into p and unpins them. It returns the
// next chunk index to plan, which is past the last entry unless a read-ahead
// fetch failed and the chunk must be fetched again on demand.
func (b *Backend) consume(ctx context.Context, entries []entry, p []byte, off, end int64) (int64, error) {
	release := func(from int) {
		for _, e := range entries[from:] {
			e.c.refs.Add(-1)
		}
	}

	for k, e := range entries {
		c := e.c

		select {
		case <-c.done:
		default:
			select {
			case <-c.done:
			case <-ctx.Done():
				release(k)
				return 0, ctx.Err()
			}
		}

		if c.err != nil {
			release(k)

			if c.readahead && !e.owned && b.ctx.Err() == nil {
				return c.idx, nil
			}

			if errors.Is(c.err, context.Canceled) && b.ctx.Err() != nil {
				return 0, ErrClosed
			}

			return 0, fmt.Errorf("racerblock backend: read chunk %d: %w", c.idx, c.err)
		}

		start := c.idx * b.chunkSize
		from, to := max(off, start), min(end, start+int64(len(c.buf)))
		copy(p[from-off:to-off], c.buf[from-start:to-start])
		c.refs.Add(-1)
	}

	return entries[len(entries)-1].c.idx + 1, nil
}

func (b *Backend) chunkLen(idx int64) int64 {
	return min(b.chunkSize, b.size-idx*b.chunkSize)
}

// newChunkLocked registers a pending chunk. Demand chunks always get a buffer;
// read-ahead chunks return nil if the budget is exhausted.
func (b *Backend) newChunkLocked(idx int64, readahead bool) *chunk {
	buf := b.allocLocked(!readahead)
	if buf == nil {
		return nil
	}

	c := &chunk{idx: idx, buf: buf[:b.chunkLen(idx)], done: make(chan struct{}), readahead: readahead}
	b.chunks[idx] = c

	return c
}

func (b *Backend) allocLocked(demand bool) []byte {
	cs := b.chunkSize

	var buf []byte

	switch {
	case len(b.free) > 0:
		buf = b.free[len(b.free)-1]
		b.free[len(b.free)-1] = nil
		b.free = b.free[:len(b.free)-1]
	case b.used+cs <= b.cfg.CacheBytes:
		buf = make([]byte, cs)
	default:
		buf = b.evictLocked()
		if buf == nil {
			if !demand {
				return nil
			}

			b.stats.overcommits.Add(1)

			buf = make([]byte, cs)
		}
	}

	b.used += cs
	if b.used > b.stats.peak.Load() {
		b.stats.peak.Store(b.used)
	}

	return buf[:cs]
}

// evictLocked removes the least recently used unpinned completed chunk and
// returns its buffer, still counted as used by the caller's accounting.
func (b *Backend) evictLocked() []byte {
	for el := b.lru.Back(); el != nil; el = el.Prev() {
		c := el.Value.(*chunk) //nolint:forcetypeassert,errcheck // The list only holds chunks.
		if c.refs.Load() != 0 {
			continue
		}

		b.lru.Remove(el)
		delete(b.chunks, c.idx)

		buf := c.buf[:cap(c.buf)]
		c.buf, c.elem = nil, nil
		b.used -= b.chunkSize
		b.stats.evictions.Add(1)

		return buf
	}

	return nil
}

// trimLocked evicts unpinned completed chunks while demand overcommit has
// pushed usage over the budget, dropping their buffers.
func (b *Backend) trimLocked() {
	for b.used > b.cfg.CacheBytes {
		if b.evictLocked() == nil {
			return
		}
	}
}

// releaseLocked returns a buffer to the free list, or drops it if keeping it
// would exceed the budget.
func (b *Backend) releaseLocked(buf []byte) {
	b.used -= b.chunkSize
	if b.used+int64(len(b.free)+1)*b.chunkSize <= b.cfg.CacheBytes {
		b.free = append(b.free, buf[:cap(buf)])
	}
}

func (b *Backend) publish(c *chunk) {
	b.mu.Lock()
	c.ready = true
	c.elem = b.lru.PushFront(c)
	b.trimLocked()
	b.mu.Unlock()
	close(c.done)
}

func (b *Backend) fail(cs []*chunk, err error) {
	b.mu.Lock()

	for _, c := range cs {
		if b.chunks[c.idx] == c {
			delete(b.chunks, c.idx)
		}

		b.releaseLocked(c.buf)
		c.buf = nil
		c.err = err
	}

	b.mu.Unlock()

	for _, c := range cs {
		close(c.done)
	}
}

// fetch fills a run of contiguous pending chunks with as few Opens as
// possible, publishing each chunk as soon as it is complete.
func (b *Backend) fetch(cs []*chunk, readahead bool) {
	defer b.wg.Done()

	attempt := 1

	for {
		n, err := b.fetchOnce(cs)
		cs = cs[n:]

		if err == nil {
			return
		}

		if n > 0 {
			attempt = 1
		}

		if !retryable(err) || b.ctx.Err() != nil || attempt >= b.cfg.MaxAttempts {
			b.stats.errors.Add(1)

			if b.ctx.Err() == nil {
				b.logFailure(cs, readahead, err)
			}

			b.fail(cs, err)

			return
		}

		b.stats.retries.Add(1)

		if !sleep(b.ctx, backoff(attempt)) {
			b.fail(cs, b.ctx.Err())
			return
		}

		attempt++
	}
}

func (b *Backend) fetchOnce(cs []*chunk) (int, error) {
	start := cs[0].idx * b.chunkSize

	var length int64
	for _, c := range cs {
		length += int64(len(c.buf))
	}

	b.stats.fetches.Add(1)

	r, err := b.src.Open(b.ctx, start, length, b.etag)
	if err != nil {
		return 0, err
	}
	defer r.Close() //nolint:errcheck // Every byte was read or the read already failed.

	for n, c := range cs {
		got, err := io.ReadFull(r, c.buf)
		b.stats.fetchedBytes.Add(uint64(got)) //nolint:gosec // got is never negative.

		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}

			return n, fmt.Errorf("read at %d: %w", c.idx*b.chunkSize, err)
		}

		b.publish(c)
	}

	return len(cs), nil
}

func (b *Backend) logFailure(cs []*chunk, readahead bool, err error) {
	if len(cs) == 0 || !b.limiter.allow(err.Error()) {
		return
	}

	level := slog.LevelError
	if retryable(err) || readahead {
		level = slog.LevelWarn
	}

	b.log.Log(b.ctx, level, "racer fetch failed",
		"etag", b.etag, "offset", cs[0].idx*b.chunkSize, "chunks", len(cs), "readahead", readahead, "error", err)
}
