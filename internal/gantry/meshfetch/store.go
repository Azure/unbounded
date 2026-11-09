// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

var (
	errBusy   = errors.New("meshfetch: chunk fill limit reached")
	errClosed = errors.New("meshfetch: node closed")
	// errLost reports that a reader needs bytes the fill no longer holds:
	// they are neither in memory nor in the local content store.
	errLost = errors.New("meshfetch: chunk bytes no longer available")
)

// chunkKey names one chunk of one blob.
type chunkKey struct {
	digest string // canonical "sha256:<hex>"
	kind   uint8  // kindBlob or kindManifest
	index  int64
}

const (
	kindBlob     uint8 = 0
	kindManifest uint8 = 1
)

// routeKey encodes k into buf for cachemesh routing: kind, digest, index.
func (k chunkKey) routeKey(buf []byte) []byte {
	buf = append(buf[:0], k.kind)
	buf = append(buf, k.digest...)

	var idx [8]byte

	for i := range 8 {
		idx[i] = byte(uint64(k.index) >> (56 - 8*i)) //nolint:gosec // index is never negative
	}

	return append(buf, idx[:]...)
}

// storeConfig configures a store.
type storeConfig struct {
	chunkSize int64
	// pageSize is the unit of in-flight buffers, local content reads,
	// read coalescing, and the hot tier. It divides chunkSize.
	pageSize int
	// window is the number of in-flight pages a fill holds.
	window int
	// maxFills caps fills in progress.
	maxFills int
	// hotBudget bounds the hot page tier in memory.
	hotBudget int64
	// extraBudget bounds window pages beyond the base windows of all
	// fills, borrowed by fills whose readers lag.
	extraBudget int64
	// content is the local containerd content store; nil when there is
	// none. It is the only disk tier: meshfetch never writes blob bytes.
	content ContentSource
	// stallTimeout is how long a fill waits for a lagging reader that
	// the local content store does not cover before dropping it.
	stallTimeout time.Duration
	// lingerTTL drops a chunk held only in memory once nobody has used it
	// for this long, so an idle agent returns the memory.
	lingerTTL time.Duration
	logger    *slog.Logger
}

// store is the chunk store: a map of fills (in progress, or complete and
// lingering in memory for later peers) over a page tier. Memory holds only
// in-flight pages of fills, pages being sent, lingering chunks in pages
// borrowed from the in-flight budget, and the hot page tier. Bytes behind a
// fill's window are read back from the local containerd content store,
// which containerd writes while it pulls through this agent.
type store struct {
	cfg   storeConfig
	pool  *pagePool
	pages *pageCache

	mu sync.Mutex
	m  map[chunkKey]*fill
	// memHead lists complete chunks held only in memory, in pages borrowed
	// from the in-flight budget (lingering chunks).
	memHead  fill
	memN     int
	inflight int
	space    chan struct{}
	closed   bool
	stop     chan struct{}

	extraPages  atomic.Int64
	localFills  atomic.Int64
	localErrors atomic.Int64

	// distrusted maps digests that failed verification to the time until
	// which their local content is not read: a failed blob may still sit
	// in an ingest.
	distrusted sync.Map
}

// distrustTTL is how long a digest that failed verification is not read
// from the content store.
const distrustTTL = 10 * time.Minute

// localOK reports whether the local content of digest d may be read.
func (s *store) localOK(d string) bool {
	if s.cfg.content == nil {
		return false
	}

	v, ok := s.distrusted.Load(d)
	if !ok {
		return true
	}

	if until, _ := v.(time.Time); time.Now().Before(until) { //nolint:errcheck // only time.Time is stored
		return false
	}

	s.distrusted.Delete(d)

	return true
}

// defaultLingerTTL bounds how long an unused chunk or hot page stays in
// memory.
const defaultLingerTTL = 30 * time.Second

// localRefresh bounds how often a fill asks the content store how much of
// its chunk is local.
const localRefresh = 10 * time.Millisecond

func newStore(cfg storeConfig) *store {
	if cfg.pageSize <= 0 || int64(cfg.pageSize) > cfg.chunkSize {
		cfg.pageSize = int(min(cfg.chunkSize, 1<<20))
	}

	if cfg.window < 2 {
		cfg.window = 4
	}

	if cfg.stallTimeout <= 0 {
		cfg.stallTimeout = 30 * time.Second
	}

	if cfg.logger == nil {
		cfg.logger = slog.Default()
	}

	if cfg.lingerTTL <= 0 {
		cfg.lingerTTL = defaultLingerTTL
	}

	// Keep up to 64MiB of idle page buffers so steady churn does not
	// allocate.
	pool := newPagePool(cfg.pageSize, max(2*cfg.window, (64<<20)/cfg.pageSize))

	s := &store{
		cfg:   cfg,
		pool:  pool,
		pages: newPageCache(pool, cfg.hotBudget),
		m:     make(map[chunkKey]*fill),
		space: make(chan struct{}),
		stop:  make(chan struct{}),
	}
	s.memHead.next = &s.memHead
	s.memHead.prev = &s.memHead

	go s.expireLoop()

	return s
}

// borrowExtra charges one page to the in-flight budget if it has room.
func (s *store) borrowExtra() bool {
	limit := s.cfg.extraBudget / int64(s.cfg.pageSize)

	for {
		n := s.extraPages.Load()
		if n >= limit {
			return false
		}

		if s.extraPages.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

func (s *store) releaseExtra(pages int) {
	if pages > 0 {
		s.extraPages.Add(-int64(pages))
	}
}

// get returns a reader of the fill for k at byte off, or nil when there is
// none that can serve from off.
func (s *store) get(k chunkKey, off int) *reader {
	s.mu.Lock()
	defer s.mu.Unlock()

	f := s.m[k]
	if f == nil {
		return nil
	}

	f.mu.Lock()
	ok := f.joinable(off)

	var r *reader
	if ok {
		r = f.newReaderLocked(off, true)
	}
	f.mu.Unlock()

	if !ok {
		return nil
	}

	f.refs.Add(1)
	s.touch(f)

	return r
}

func (s *store) touch(f *fill) {
	f.lastUse = time.Now()

	if f.listed {
		s.unlink(f)
		s.pushFront(f)
	}
}

// acquire returns a reader at byte off of the fill for k: an existing one
// that can serve from off (created == false) or a new one (created ==
// true). A new fill holds three references: the map, the filler, and the
// returned reader. An existing fill that has dropped bytes before off is
// detached from the map and replaced. shared marks a request from a peer.
// With wait false
// acquire returns errBusy when the fill cap is reached; with wait true it
// blocks until a fill ends or ctx ends.
func (s *store) acquire(ctx context.Context, k chunkKey, off int, wait, shared bool) (r *reader, created bool, err error) {
	for {
		s.mu.Lock()

		if s.closed {
			s.mu.Unlock()
			return nil, false, errClosed
		}

		if f := s.m[k]; f != nil {
			f.mu.Lock()
			if f.joinable(off) {
				r = f.newReaderLocked(off, shared)
			}
			f.mu.Unlock()

			if r != nil {
				f.refs.Add(1)

				s.touch(f)
				s.mu.Unlock()

				return r, false, nil
			}

			s.detachLocked(f)
		}

		if s.inflight < s.cfg.maxFills {
			f := s.newFillLocked(k)
			r = f.newReader(off, shared)
			s.mu.Unlock()

			return r, true, nil
		}

		ch := s.space
		s.mu.Unlock()

		if !wait {
			return nil, false, errBusy
		}

		select {
		case <-ch:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
}

func (s *store) newFillLocked(k chunkKey) *fill {
	f := &fill{key: k, store: s, wake: make(chan struct{}), total: -1, upstream: noSlot}
	f.refs.Store(3)
	f.inMap = true
	s.m[k] = f
	s.inflight++

	return f
}

// newLocalFill returns a fill of k outside the map, with one reference
// for the filler and one for the returned reader at offset off. It still
// counts as a fill in progress.
func (s *store) newLocalFill(k chunkKey, off int) (*fill, *reader) {
	f := &fill{key: k, store: s, wake: make(chan struct{}), total: -1, upstream: noSlot, detached: true}
	f.refs.Store(2)

	s.mu.Lock()
	s.inflight++
	s.mu.Unlock()

	return f, f.newReader(off, true)
}

// markOwned records that f is filled by this member as the chunk's owner.
// A lingering owned chunk is reclaimed after the others.
func (s *store) markOwned(f *fill) {
	f.owned.Store(true)
}

// detachLocked removes f from the map and the memory list. Readers
// holding f keep reading what it has. Caller holds s.mu.
func (s *store) detachLocked(f *fill) {
	if !f.inMap {
		return
	}

	if f.listed {
		s.unlink(f)
		s.memN--
	}

	delete(s.m, f.key)
	f.inMap = false

	f.mu.Lock()
	f.detached = true
	f.mu.Unlock()

	s.signalSpace()
	f.release()
}

func (s *store) signalSpace() {
	close(s.space)
	s.space = make(chan struct{})
}

// finished keeps a complete fill that peers read or that this member owns
// and that still holds every page in memory on the memory list, so later peers can join it
// until the in-flight budget needs its pages or it expires. Every other
// fill leaves the map: later readers get a new fill, which reads the local
// content store when it covers the chunk.
func (s *store) finished(f *fill, err error) {
	f.mu.Lock()
	inMemory := (f.hadPeers || f.owned.Load()) && f.winStart == 0 && f.metaKnown && f.n == f.chunkLen
	f.mu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.inflight--

	if f.inMap {
		if err == nil && inMemory {
			f.inMemory = true
			f.lastUse = time.Now()
			s.memN++
			s.pushFront(f)
		} else {
			s.detachLocked(f)
		}
	}

	s.signalSpace()
}

// expireLoop drops chunks held only in memory that nobody used for
// lingerTTL.
func (s *store) expireLoop() {
	t := time.NewTicker(max(s.cfg.lingerTTL/4, 10*time.Millisecond))
	defer t.Stop()

	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			s.expire(now)
			s.pages.expire(now.Add(-s.cfg.lingerTTL))
		}
	}
}

// expire drops in-memory chunks unused since now - lingerTTL.
func (s *store) expire(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for v := s.memHead.prev; v != &s.memHead; {
		prev := v.prev
		if now.Sub(v.lastUse) < s.cfg.lingerTTL {
			// The list is in use order: the rest is newer.
			return
		}

		s.detachLocked(v)
		v = prev
	}
}

// evictLingering drops a lingering chunk, so its pages return to the
// in-flight budget once its readers are done: the least recently used one
// this member does not own, else the least recently used one. It reports
// whether there was one.
func (s *store) evictLingering() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	victim := s.memHead.prev
	if victim == &s.memHead {
		return false
	}

	for v := victim; v != &s.memHead; v = v.prev {
		if !v.owned.Load() {
			victim = v
			break
		}
	}

	s.detachLocked(victim)

	return true
}

// evictDigest drops every cached chunk and page of digest d, and stops
// reading its local content for a while.
func (s *store) evictDigest(d string) {
	s.distrusted.Store(d, time.Now().Add(distrustTTL))

	s.mu.Lock()

	for k, f := range s.m {
		if k.digest == d && f.listed {
			s.detachLocked(f)
		}
	}
	s.mu.Unlock()

	s.pages.purge(d)
}

// pushFront makes f the most recent lingering chunk.
func (s *store) pushFront(f *fill) {
	h := &s.memHead

	f.prev = h
	f.next = h.next
	h.next.prev = f
	h.next = f
	f.listed = true
}

func (s *store) unlink(f *fill) {
	f.prev.next = f.next
	f.next.prev = f.prev
	f.prev, f.next = nil, nil
	f.listed = false
}

// close fails waiters and drops idle buffers.
func (s *store) close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.closed {
		close(s.stop)
	}

	s.closed = true
	s.signalSpace()
	s.pool.drain()
}

// Stats is a point-in-time view of the chunk store.
type Stats struct {
	// Chunks in the store, in progress or lingering.
	Chunks int
	// Inflight is the number of fills in progress.
	Inflight int
	// PageBytes is memory in page buffers in use: in-flight fill pages,
	// pages being sent, lingering chunks, and the hot tier.
	PageBytes int64
	// HotBytes is memory in the hot page tier.
	HotBytes int64
	// IdlePageBytes is memory in idle pooled page buffers.
	IdlePageBytes int64
	// LocalReads counts page reads from the local content store.
	LocalReads int64
	// LocalErrors counts failed local content reads.
	LocalErrors int64
	// LocalFills counts fills read from the local content store.
	LocalFills int64
	// PageReadsShared counts page reads served by another reader's read
	// or by the hot tier.
	PageReadsShared int64
	// ExtraPageBytes is memory borrowed from the in-flight budget by
	// fills whose readers lag and by lingering chunks.
	ExtraPageBytes int64
	// MemoryChunks is the number of complete chunks lingering in memory.
	MemoryChunks int
	// HolderPeeks counts owner reads from a recent requester's local
	// content store, by outcome ("ok" or "miss").
	HolderPeeks, HolderMisses int64
}

func (s *store) stats() Stats {
	s.mu.Lock()
	st := Stats{Chunks: len(s.m), Inflight: s.inflight, MemoryChunks: s.memN}
	s.mu.Unlock()

	live, idle := s.pool.stats()
	ps := alignUp(int64(s.cfg.pageSize))
	st.PageBytes = int64(live) * ps
	st.IdlePageBytes = int64(idle) * ps
	st.HotBytes = s.pages.hotBytes()
	st.LocalReads = s.pages.reads.Load()
	st.LocalErrors = s.localErrors.Load()
	st.LocalFills = s.localFills.Load()
	st.PageReadsShared = s.pages.hits.Load()
	st.ExtraPageBytes = s.extraPages.Load() * int64(s.cfg.pageSize)

	return st
}
