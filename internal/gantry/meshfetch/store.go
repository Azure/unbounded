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
	// they were neither written to disk nor kept in memory.
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

// PersistPolicy selects which chunks are written to the chunk directory.
type PersistPolicy string

const (
	// PersistAll writes every chunk to disk.
	PersistAll PersistPolicy = "all"
	// PersistShared writes only chunks this agent owns or serves to a
	// peer. A chunk only this agent consumes streams through memory, with
	// the fill paced by its slowest reader, and is never written.
	PersistShared PersistPolicy = "shared"
	// PersistOwned writes only chunks this agent owns. Every other chunk
	// streams through memory; a peer that asks for one after its first
	// page is gone gets a new fill, which refetches it from upstream.
	PersistOwned PersistPolicy = "owned"
)

// storeConfig configures a store.
type storeConfig struct {
	chunkSize int64
	// pageSize is the unit of in-flight buffers, disk I/O, read
	// coalescing, and the hot tier. It divides chunkSize.
	pageSize int
	// window is the number of in-flight pages a fill holds.
	window int
	// maxFills caps fills in progress.
	maxFills int
	// dir is the chunk directory; empty disables the disk tier.
	dir string
	// diskBudget bounds bytes in dir.
	diskBudget int64
	// hotBudget bounds the hot page tier in memory.
	hotBudget int64
	// extraBudget bounds window pages beyond the base windows of all
	// fills, borrowed by fills whose readers lag.
	extraBudget int64
	pageCache   PageCacheMode
	persist     PersistPolicy
	// stallTimeout is how long a fill that is not written to disk waits
	// for a reader before dropping it.
	stallTimeout time.Duration
	// lingerTTL drops a chunk held only in memory once nobody has used it
	// for this long, so an idle agent returns the memory.
	lingerTTL time.Duration
	onEvict   func(bytes int64)
	logger    *slog.Logger
}

// diskFaults injects disk errors in tests.
type diskFaults struct {
	create func() error
	write  func(idx int) error
	read   func(idx int) error
}

// store is the chunk store: a map of fills plus LRU lists of complete
// chunks on disk, under a disk byte budget that also covers fills being
// written. Chunks this member owns are kept on a separate list and
// evicted only when no other complete chunk is left, or when they hold
// more than half of the budget. Memory holds only in-flight pages of
// fills, pages being sent, and the hot page tier.
type store struct {
	cfg   storeConfig
	disk  *diskDir
	pool  *pagePool
	pages *pageCache

	mu        sync.Mutex
	m         map[chunkKey]*fill
	head      fill // LRU sentinel of other chunks: head.next is most recent
	ownedHead fill // LRU sentinel of owned chunks
	// memHead lists complete chunks held only in memory, in pages borrowed
	// from the in-flight budget (lingering relay chunks).
	memHead   fill
	memN      int
	ownedUsed int64
	used      int64
	inflight  int
	space     chan struct{}
	closed    bool
	stop      chan struct{}
	// diskFullUntil pauses disk writes after the file system ran out of
	// space.
	diskFullUntil time.Time

	extraPages atomic.Int64
	spills     atomic.Int64

	diskWrites  atomic.Int64
	diskReads   atomic.Int64
	diskErrors  atomic.Int64
	streamFills atomic.Int64

	faults *diskFaults
}

// diskFullBackoff is how long new fills skip the disk after ENOSPC.
const diskFullBackoff = 10 * time.Second

// defaultLingerTTL bounds how long an unused chunk or hot page stays in
// memory.
const defaultLingerTTL = 30 * time.Second

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

	if cfg.persist == "" {
		cfg.persist = PersistAll
	}

	if cfg.pageCache == "" {
		cfg.pageCache = PageCacheBypass
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
	s.head.next = &s.head
	s.head.prev = &s.head
	s.ownedHead.next = &s.ownedHead
	s.ownedHead.prev = &s.ownedHead
	s.memHead.next = &s.memHead
	s.memHead.prev = &s.memHead

	go s.expireLoop()

	if cfg.dir != "" && cfg.diskBudget > 0 {
		d, err := openDiskDir(cfg.dir, cfg.pageCache)
		if err != nil {
			cfg.logger.Warn("cachemesh chunk dir unusable; chunks stream through memory only",
				slog.String("dir", cfg.dir), slog.Any("err", err))
		} else {
			s.disk = d
		}
	}

	return s
}

// direct reports whether O_DIRECT is possible: pages must be aligned.
func (s *store) direct() bool {
	return s.cfg.pageCache == PageCacheBypass && s.cfg.pageSize%dioAlign == 0
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

// reserveSpill reserves disk for a fill that spills while it is in the
// map. Detached fills do not spill: nothing would return the charge.
func (s *store) reserveSpill(f *fill) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return f.inMap && !s.closed && s.reserveLocked(f)
}

func (s *store) readFile(cf *chunkFile, buf []byte, off int64, n int) error {
	s.diskReads.Add(1)

	if s.faults != nil && s.faults.read != nil {
		if err := s.faults.read(int(off / int64(s.cfg.pageSize))); err != nil {
			return err
		}
	}

	return cf.readAt(buf, off, n)
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
// detached from the map and replaced. shared marks a request from a peer:
// under PersistShared the chunk is then written to disk. With wait false
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

				if shared {
					f.share()
				}

				s.touch(f)
				s.mu.Unlock()

				if shared {
					f.startDisk(false)
				}

				return r, false, nil
			}

			s.detachLocked(f)
		}

		if s.inflight < s.cfg.maxFills {
			f := s.newFillLocked(k, shared)
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

func (s *store) newFillLocked(k chunkKey, shared bool) *fill {
	f := &fill{key: k, store: s, wake: make(chan struct{}), total: -1, upstream: noSlot}
	f.refs.Store(3)
	f.inMap = true
	s.m[k] = f
	s.inflight++

	if s.cfg.persist == PersistAll || (shared && s.cfg.persist == PersistShared) {
		s.reserveLocked(f)
	}

	if !f.wantDisk.Load() {
		s.streamFills.Add(1)
	}

	return f
}

// reserveLocked charges a full chunk of disk to f when the disk tier has
// room, so f is written to disk once its size is known. Caller holds s.mu.
func (s *store) reserveLocked(f *fill) bool {
	if f.wantDisk.Load() {
		return true
	}

	if s.disk == nil || time.Now().Before(s.diskFullUntil) || !s.makeRoom(s.cfg.chunkSize) {
		return false
	}

	f.wantDisk.Store(true)
	f.charge = s.cfg.chunkSize
	s.used += f.charge

	return true
}

// makeRoom evicts least recently used complete chunks until n more bytes
// fit: other chunks first, owned chunks once no other one is left or they
// exceed half of the budget. Caller holds s.mu.
func (s *store) makeRoom(n int64) bool {
	for s.used+n > s.cfg.diskBudget {
		victim := s.head.prev
		if victim == &s.head || 2*s.ownedUsed > s.cfg.diskBudget {
			if o := s.ownedHead.prev; o != &s.ownedHead {
				victim = o
			}
		}

		if victim == &s.head {
			return false
		}

		s.evictLocked(victim)
	}

	return true
}

// markOwned records that f is filled by this member as the chunk's owner.
// Owned chunks are always written to disk when the disk tier has room.
func (s *store) markOwned(f *fill) {
	s.mu.Lock()
	f.owned = true
	reserved := s.reserveLocked(f)
	s.mu.Unlock()

	if reserved {
		f.startDisk(false)
	}
}

// evictLocked drops a complete chunk. Caller holds s.mu.
func (s *store) evictLocked(f *fill) {
	charge := f.charge
	s.detachLocked(f)

	if s.cfg.onEvict != nil && charge > 0 {
		s.cfg.onEvict(charge)
	}
}

// detachLocked removes f from the map and the LRU lists, returns its disk
// charge, and unlinks its file. Readers holding f keep reading what it has.
// Caller holds s.mu.
func (s *store) detachLocked(f *fill) {
	if !f.inMap {
		return
	}

	if f.listed {
		s.unlink(f)

		switch {
		case f.inMemory:
			s.memN--
		case f.owned:
			s.ownedUsed -= f.charge
		}
	}

	delete(s.m, f.key)
	f.inMap = false
	s.used -= f.charge
	f.charge = 0

	f.mu.Lock()
	f.detached = true
	cf := f.file
	f.mu.Unlock()

	if cf != nil {
		cf.remove()
	}

	s.pages.purge(f)
	s.signalSpace()
	f.release()
}

// detach drops f from the map if it is still there.
func (s *store) detach(f *fill) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.detachLocked(f)
}

func (s *store) signalSpace() {
	close(s.space)
	s.space = make(chan struct{})
}

// finished lists a complete fill that is on disk as a cached chunk, and
// drops a failed fill or one that only streamed through memory.
func (s *store) finished(f *fill, err error) {
	f.mu.Lock()
	onDisk := f.file != nil && !f.diskFailed && f.diskStart == 0
	size := f.diskSize
	// A fill peers read that still holds every page in memory lingers
	// for later peers until the in-flight budget needs its pages.
	inMemory := !onDisk && f.hadPeers && f.winStart == 0 && f.metaKnown && f.n == f.chunkLen
	f.mu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.inflight--

	if f.inMap {
		switch {
		case err == nil && inMemory:
			f.inMemory = true
			f.lastUse = time.Now()
			s.memN++
			s.pushFront(f)
		case err != nil || !onDisk:
			s.detachLocked(f)
		default:
			// Charge the real size: short final chunks and small
			// manifests do not hold a full chunk.
			if size < f.charge {
				s.used -= f.charge - size
				f.charge = size
			}

			if f.owned {
				s.ownedUsed += f.charge
			}

			s.pushFront(f)
			s.makeRoom(0)
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

// evictLingering drops the least recently used chunk held only in memory,
// so its pages return to the in-flight budget once its readers are done.
// It reports whether there was one.
func (s *store) evictLingering() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	victim := s.memHead.prev
	if victim == &s.memHead {
		return false
	}

	s.detachLocked(victim)

	return true
}

// diskFailed records a failed disk operation. A full disk pauses disk
// writes for new fills for a while.
func (s *store) diskFailed(err error) {
	s.diskErrors.Add(1)

	full := errors.Is(err, errDiskFull)

	s.mu.Lock()
	if full {
		s.diskFullUntil = time.Now().Add(diskFullBackoff)
	}
	s.mu.Unlock()

	s.cfg.logger.Warn("cachemesh chunk disk error; streaming through memory",
		slog.Bool("disk_full", full), slog.Any("err", err))
}

// evictDigest drops every cached chunk of digest d.
func (s *store) evictDigest(d string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for k, f := range s.m {
		if k.digest == d && f.listed {
			s.evictLocked(f)
		}
	}
}

// pushFront makes f the most recent chunk of its list.
func (s *store) pushFront(f *fill) {
	h := &s.head

	switch {
	case f.inMemory:
		h = &s.memHead
	case f.owned:
		h = &s.ownedHead
	}

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
	// DiskBytes is charged to the disk budget: chunks on disk plus fills
	// being written.
	DiskBytes int64
	// Chunks in the store, complete or in progress.
	Chunks int
	// Inflight is the number of fills in progress.
	Inflight int
	// PageBytes is memory in page buffers in use: in-flight fill pages,
	// pages being sent, and the hot tier.
	PageBytes int64
	// HotBytes is memory in the hot page tier.
	HotBytes int64
	// IdlePageBytes is memory in idle pooled page buffers.
	IdlePageBytes int64
	// DiskReads and DiskWrites count page reads and writes.
	DiskReads  int64
	DiskWrites int64
	// DiskErrors counts failed disk operations.
	DiskErrors int64
	// PageReadsShared counts page reads served by another reader's disk
	// read or by the hot tier.
	PageReadsShared int64
	// StreamFills counts fills not written to disk from their start.
	StreamFills int64
	// ExtraPageBytes is memory borrowed from the in-flight budget by
	// fills whose readers lag.
	ExtraPageBytes int64
	// Spills counts fills that spilled to disk when the in-flight budget
	// was exhausted.
	Spills int64
	// MemoryChunks is the number of complete chunks held only in memory.
	MemoryChunks int
}

func (s *store) stats() Stats {
	s.mu.Lock()
	st := Stats{DiskBytes: s.used, Chunks: len(s.m), Inflight: s.inflight, MemoryChunks: s.memN}
	s.mu.Unlock()

	live, idle := s.pool.stats()
	ps := alignUp(int64(s.cfg.pageSize))
	st.PageBytes = int64(live) * ps
	st.IdlePageBytes = int64(idle) * ps
	st.HotBytes = s.pages.hotBytes()
	st.DiskReads = s.diskReads.Load()
	st.DiskWrites = s.diskWrites.Load()
	st.DiskErrors = s.diskErrors.Load()
	st.PageReadsShared = s.pages.hits.Load()
	st.StreamFills = s.streamFills.Load()
	st.ExtraPageBytes = s.extraPages.Load() * int64(s.cfg.pageSize)
	st.Spills = s.spills.Load()

	return st
}
