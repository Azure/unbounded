// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh"
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
	// pageSize is the unit of in-flight buffers, local content reads, and
	// read coalescing. It divides chunkSize.
	pageSize int
	// window is the number of in-flight pages a fill holds.
	window int
	// maxFills caps fills in progress.
	maxFills int
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
// borrowed from the in-flight budget, and pages being read from the local
// content store. Bytes behind a
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

// defaultLingerTTL bounds how long an unused chunk stays in memory.
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
		pages: newPageCache(pool),
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

// get returns a peer reader of the fill for k at byte off, or nil when
// there is none that can serve from off. It never starts a fill.
func (s *store) get(k chunkKey, off int) *reader {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.joinLocked(k, off, true)
}

// joinLocked returns a reader at byte off of the fill for k when there is
// one that can serve from off, else nil. Caller holds s.mu.
func (s *store) joinLocked(k chunkKey, off int, shared bool) *reader {
	f := s.m[k]
	if f == nil {
		return nil
	}

	f.mu.Lock()

	var r *reader
	if f.joinable(off) {
		r = f.newReaderLocked(off, shared)
	}
	f.mu.Unlock()

	if r != nil {
		f.refs.Add(1)
		s.touch(f)
	}

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

		if r = s.joinLocked(k, off, shared); r != nil {
			s.mu.Unlock()
			return r, false, nil
		}

		if f := s.m[k]; f != nil {
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
	// pages being sent, and lingering chunks.
	PageBytes int64
	// IdlePageBytes is memory in idle pooled page buffers.
	IdlePageBytes int64
	// LocalReads counts page reads from the local content store.
	LocalReads int64
	// LocalErrors counts failed local content reads.
	LocalErrors int64
	// LocalFills counts fills read from the local content store.
	LocalFills int64
	// PageReadsShared counts page reads served by a concurrent reader's
	// read.
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
	ps := int64(s.cfg.pageSize)
	st.PageBytes = int64(live) * ps
	st.IdlePageBytes = int64(idle) * ps
	st.LocalReads = s.pages.reads.Load()
	st.LocalErrors = s.localErrors.Load()
	st.LocalFills = s.localFills.Load()
	st.PageReadsShared = s.pages.hits.Load()
	st.ExtraPageBytes = s.extraPages.Load() * int64(s.cfg.pageSize)

	return st
}

// readStep bounds one read into a page, so readers of an in-progress chunk
// see progress at least every readStep bytes (cut-through).
const readStep = 256 << 10

var errMetaMismatch = errors.New("meshfetch: chunk size differs between sources")

// fill is one chunk, complete or being filled.
//
// The filler reads the chunk into a small window of in-flight pages.
// Readers that keep up copy bytes out of those pages as they arrive
// (cut-through, and the "read once" path of a hot fan-out). A page leaves
// the window once every reader has passed it, or once the local containerd
// content store holds it: containerd writes the blob while it pulls
// through this agent, so a reader that falls behind reads those bytes back
// from the content store through the store's page cache, which coalesces
// concurrent reads of the same page. When a reader lags behind a full
// window of a fill that peers read and the content store does not hold the
// page, the fill borrows extra pages from the store's in-flight budget so
// the peers are not slowed down. Otherwise (no peers, or no budget) the
// slowest reader paces the fill, and a reader stuck for longer than the
// stall timeout is dropped and gets errLost.
type fill struct {
	key   chunkKey
	store *store

	// refs counts holders: the store map, the filler, and every reader.
	refs atomic.Int32
	// owned marks a chunk this member fills as its owner. An owned chunk
	// lingers in memory like one peers read (peers are expected), and a
	// lingering owned chunk is reclaimed after the others.
	owned atomic.Bool

	mu        sync.Mutex
	wake      chan struct{}
	total     int64
	ctype     string
	chunkLen  int
	metaKnown bool
	n         int
	done      bool
	err       error
	// upstream is the slot the fill reads from, for loop detection.
	upstream cachemesh.Slot
	// fromOrigin is set while the fill reads from the origin.
	fromOrigin bool

	// win holds pages winStart.. of the chunk in memory.
	win      []*page
	winStart int
	// readers are the open readers and their positions.
	readers map[*reader]struct{}
	peers   int
	// hadPeers is set once a peer has read the fill.
	hadPeers      bool
	fillerWaiting bool
	// extra counts window pages beyond the base window, charged to the
	// store's in-flight budget.
	extra int
	// localEnd is how many leading bytes of the chunk the local content
	// store held when last asked, at localAt.
	localEnd int
	localAt  time.Time
	detached bool

	// Guarded by store.mu.
	prev, next *fill
	inMap      bool
	listed     bool
	// inMemory marks a complete chunk lingering in memory, on the store's
	// memory list.
	inMemory bool
	lastUse  time.Time
}

// reader is one consumer of a fill. Its position holds back the window
// unless the local content store holds the bytes at it.
type reader struct {
	f    *fill
	peer bool
	// Guarded by f.mu.
	pos    int
	kicked bool
}

func (f *fill) release() {
	if f.refs.Add(-1) == 0 {
		f.recycle()
	}
}

// recycle frees the window once nothing references f.
func (f *fill) recycle() {
	f.mu.Lock()
	win := f.win
	f.win = nil
	extra := f.extra
	f.extra = 0
	f.mu.Unlock()

	f.store.releaseExtra(extra)

	for _, p := range win {
		p.release()
	}
}

// broadcast wakes waiters. Caller holds f.mu.
func (f *fill) broadcast() {
	close(f.wake)
	f.wake = make(chan struct{})
}

// joinable reports whether f can serve a reader starting at off. Caller
// holds f.mu.
func (f *fill) joinable(off int) bool {
	return !f.detached && max(off, f.localFloorLocked()) >= f.winStart*f.store.cfg.pageSize
}

// localPageLocked reports whether the local content store held the whole
// page holding byte off when last asked. Caller holds f.mu.
func (f *fill) localPageLocked(off int) bool {
	if !f.metaKnown || off >= f.chunkLen {
		return false
	}

	ps := f.store.cfg.pageSize

	return f.localEnd >= min(f.chunkLen, (off/ps+1)*ps)
}

// chunkStart is the blob offset of the chunk.
func (f *fill) chunkStart() int64 { return f.key.index * f.store.cfg.chunkSize }

// refreshLocal asks the content store how much of the chunk it holds, at
// most every localRefresh unless force is set. It reports whether the
// answer grew. Caller must not hold f.mu.
func (f *fill) refreshLocal(force bool) bool {
	src := f.store.cfg.content
	if !f.store.localOK(f.key.digest) {
		return false
	}

	f.mu.Lock()
	known, chunkLen, end := f.metaKnown, f.chunkLen, f.localEnd

	if !known || end >= chunkLen || (!force && time.Since(f.localAt) < localRefresh) {
		f.mu.Unlock()
		return false
	}

	f.localAt = time.Now()
	f.mu.Unlock()

	avail, _ := src.Available(f.key.digest)
	got := int(min(max(avail-f.chunkStart(), 0), int64(chunkLen)))

	f.mu.Lock()
	defer f.mu.Unlock()

	if got <= f.localEnd {
		return false
	}

	f.localEnd = got

	if f.fillerWaiting {
		f.broadcast()
	}

	return true
}

// newReaderLocked registers a reader at off; peer marks a reader serving
// another member. Caller holds f.mu and a reference for the reader.
func (f *fill) newReaderLocked(off int, peer bool) *reader {
	r := &reader{f: f, pos: off, peer: peer}
	if f.readers == nil {
		f.readers = make(map[*reader]struct{})
	}

	f.readers[r] = struct{}{}

	if peer {
		f.peers++
		f.hadPeers = true
	}

	return r
}

// newReader registers a reader at off for a reference the caller holds.
func (f *fill) newReader(off int, peer bool) *reader {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.newReaderLocked(off, peer)
}

// dropReaderLocked unregisters r. Caller holds f.mu.
func (f *fill) dropReaderLocked(r *reader) {
	if _, ok := f.readers[r]; !ok {
		return
	}

	delete(f.readers, r)

	if r.peer {
		f.peers--
	}
}

// close unregisters r and drops its reference.
func (r *reader) close() {
	f := r.f

	f.mu.Lock()
	f.dropReaderLocked(r)

	if f.fillerWaiting {
		f.broadcast()
	}
	f.mu.Unlock()

	f.release()
}

// setMeta records the blob size and content type. It returns
// errMetaMismatch when an earlier attempt saw different values.
func (f *fill) setMeta(total int64, ctype string, chunkLen int) error {
	f.mu.Lock()

	if f.metaKnown {
		ok := f.total == total && f.chunkLen == chunkLen
		f.mu.Unlock()

		if !ok {
			return errMetaMismatch
		}

		return nil
	}

	f.chunkLen = chunkLen
	f.total = total
	f.ctype = ctype
	f.metaKnown = true
	f.broadcast()
	f.mu.Unlock()

	return nil
}

// progress returns the bytes filled so far and whether metadata is known.
func (f *fill) progress() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.n, f.metaKnown
}

// slot returns the window page holding byte n, waiting for room in the
// window: a page leaves it once every reader that needs it from memory
// has passed it (readers at bytes the local content store holds read from
// there). A fill that peers read borrows pages from the in-flight budget
// rather than wait. touch is called while waiting so a slow reader does
// not count as a stalled upstream.
func (f *fill) slot(ctx context.Context, n int, touch func()) (*page, error) {
	s := f.store
	ps := s.cfg.pageSize
	pi := n / ps

	var (
		kick  <-chan time.Time
		timer *time.Timer
		poll  *time.Ticker
		pollC <-chan time.Time
	)

	defer func() {
		if timer != nil {
			timer.Stop()
		}

		if poll != nil {
			poll.Stop()
		}
	}()

	f.mu.Lock()
	defer f.mu.Unlock()

	refreshed := false

	for {
		if pi < f.winStart+len(f.win) {
			return f.win[pi-f.winStart], nil
		}

		if len(f.win) < s.cfg.window {
			p := s.pool.get()
			f.win = append(f.win, p)

			return p, nil
		}

		oldEnd := (f.winStart + 1) * ps
		if f.minReaderLocked() >= oldEnd {
			// A fill that peers read keeps its whole chunk in borrowed
			// pages while the budget allows, so later peers can still
			// join it (see store.finished), unless the local content
			// store already holds it.
			if f.keepWholeLocked() {
				if p := f.borrowLocked(); p != nil {
					return p, nil
				}
			}

			f.dropOldestLocked()

			continue
		}

		if !refreshed && s.cfg.content != nil {
			// A lagging reader may be at bytes containerd has written
			// since the content store was last asked.
			refreshed = true

			f.mu.Unlock()
			f.refreshLocal(false)
			f.mu.Lock()

			continue
		}

		if f.peers > 0 {
			if p := f.borrowLocked(); p != nil {
				return p, nil
			}
		}

		if timer == nil {
			timer = time.NewTimer(s.cfg.stallTimeout)
			kick = timer.C

			if s.cfg.content != nil {
				poll = time.NewTicker(4 * localRefresh)
				pollC = poll.C
			}
		}

		f.fillerWaiting = true
		ch := f.wake
		f.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			f.mu.Lock()
			f.fillerWaiting = false

			return nil, ctx.Err()
		case <-pollC:
			f.refreshLocal(false)
		case <-kick:
			f.mu.Lock()
			f.kickLocked(oldEnd)
			f.mu.Unlock()

			timer.Reset(s.cfg.stallTimeout)
		}

		if touch != nil {
			touch()
		}

		f.mu.Lock()
		f.fillerWaiting = false
	}
}

// keepWholeLocked reports whether f should keep every page in memory: a
// fill that peers read or that this member owns, that still holds its
// first page, and whose chunk the local content store does not hold.
// Caller holds f.mu.
func (f *fill) keepWholeLocked() bool {
	return (f.hadPeers || f.owned.Load()) && f.winStart == 0 && f.store.cfg.extraBudget > 0 && !f.localPageLocked(0)
}

// borrowLocked appends a page borrowed from the in-flight budget to the
// window, reclaiming a lingering chunk if the budget is exhausted. It
// returns nil when there is nothing to borrow. Caller holds f.mu.
func (f *fill) borrowLocked() *page {
	if !f.store.borrowExtra() && !f.reclaimLocked() {
		return nil
	}

	f.extra++
	p := f.store.pool.get()
	f.win = append(f.win, p)

	return p
}

// reclaimLocked evicts one lingering in-memory chunk and borrows a page
// from the budget it returns. It drops f.mu while it takes the store lock.
// Caller holds f.mu.
func (f *fill) reclaimLocked() bool {
	s := f.store

	f.mu.Unlock()

	ok := false

	for !ok && s.evictLingering() {
		ok = s.borrowExtra()
	}

	f.mu.Lock()

	return ok
}

// dropOldestLocked frees the oldest window page and returns a borrowed
// page to the in-flight budget. Caller holds f.mu.
func (f *fill) dropOldestLocked() {
	f.win[0].release()
	copy(f.win, f.win[1:])
	f.win[len(f.win)-1] = nil
	f.win = f.win[:len(f.win)-1]
	f.winStart++

	if f.extra > 0 {
		f.extra--
		f.store.releaseExtra(1)
	}
}

// localFloorLocked returns the end of the whole pages the local content
// store held when last asked. Caller holds f.mu.
func (f *fill) localFloorLocked() int {
	if f.localEnd >= f.chunkLen {
		return f.chunkLen
	}

	ps := f.store.cfg.pageSize

	return f.localEnd / ps * ps
}

// needLocked returns the first byte reader r still needs from memory:
// bytes before the local floor it reads from the content store. Caller
// holds f.mu.
func (f *fill) needLocked(r *reader) int {
	return max(r.pos, f.localFloorLocked())
}

// minReaderLocked returns the lowest byte a reader still needs from
// memory. Caller holds f.mu.
func (f *fill) minReaderLocked() int {
	low := int(^uint(0) >> 1)

	for r := range f.readers {
		low = min(low, f.needLocked(r))
	}

	return low
}

// kickLocked drops readers holding back the page that ends at end. They
// get errLost and start over with a new fill. Caller holds f.mu.
func (f *fill) kickLocked(end int) {
	for r := range f.readers {
		if f.needLocked(r) < end {
			r.kicked = true
			f.dropReaderLocked(r)
		}
	}

	f.broadcast()
}

// readFrom reads r into the window until the chunk is full. touch runs
// after every successful read (stall detection).
func (f *fill) readFrom(ctx context.Context, r io.Reader, touch func()) error {
	ps := f.store.cfg.pageSize

	for {
		f.mu.Lock()
		n, chunkLen := f.n, f.chunkLen
		f.mu.Unlock()

		if n >= chunkLen {
			return nil
		}

		p, err := f.slot(ctx, n, touch)
		if err != nil {
			return err
		}

		pi := n / ps
		po := n - pi*ps
		end := min(ps, chunkLen-pi*ps)

		k, err := r.Read(p.buf[po:min(end, po+readStep)])
		if k > 0 {
			f.mu.Lock()
			f.n += k
			f.broadcast()
			f.mu.Unlock()

			if touch != nil {
				touch()
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				if n+k < chunkLen {
					return io.ErrUnexpectedEOF
				}

				return nil
			}

			return err
		}
	}
}

// finish marks the fill complete (err == nil) or failed.
func (f *fill) finish(err error) {
	f.mu.Lock()
	f.done = true
	f.err = err

	f.broadcast()
	f.mu.Unlock()

	f.store.finished(f, err)
}

// meta waits until the size is known or the fill fails.
func (f *fill) meta(ctx context.Context) (total int64, ctype string, chunkLen int, err error) {
	for {
		f.mu.Lock()
		if f.metaKnown {
			total, ctype, chunkLen = f.total, f.ctype, f.chunkLen
			f.mu.Unlock()

			return total, ctype, chunkLen, nil
		}

		if f.done {
			err = f.err
			f.mu.Unlock()

			return 0, "", 0, err
		}

		ch := f.wake
		f.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			return 0, "", 0, ctx.Err()
		}
	}
}

// streamTo writes the chunk from the reader's position to w as bytes
// become available: from the fill's window, or from the local content
// store through the page cache. It returns the bytes written and the first error: the fill's,
// the writer's, ctx's, or errLost.
func (r *reader) streamTo(ctx context.Context, w io.Writer) (int64, error) {
	f := r.f
	s := f.store
	ps := s.cfg.pageSize

	var (
		written   int64
		refreshed bool
	)

	for {
		f.mu.Lock()

		if r.kicked {
			f.mu.Unlock()
			return written, errLost
		}

		off := r.pos

		if f.metaKnown && off > f.chunkLen {
			f.mu.Unlock()
			return written, io.ErrShortBuffer
		}

		if f.metaKnown && off == f.chunkLen {
			f.mu.Unlock()
			return written, nil
		}

		if off < f.n {
			pi := off / ps

			var (
				b    []byte
				done func()
			)

			switch {
			case pi >= f.winStart:
				p := f.win[pi-f.winStart]
				p.ref()

				end := min(f.n, (pi+1)*ps)
				f.mu.Unlock()

				b, done = p.buf[off-pi*ps:end-pi*ps], p.release
			case f.localPageLocked(off):
				n := min(ps, f.chunkLen-pi*ps)
				f.mu.Unlock()

				idx := f.chunkStart()/int64(ps) + int64(pi)

				e, err := s.pages.get(s.cfg.content, f.key.digest, idx, n)
				if err != nil {
					s.pages.put(e)
					s.localErrors.Add(1)

					f.mu.Lock()
					f.localEnd = 0
					f.mu.Unlock()

					return written, errors.Join(errLost, err)
				}

				b, done = e.p.buf[off-pi*ps:n], func() { s.pages.put(e) }
			default:
				f.mu.Unlock()

				if !refreshed && f.refreshLocal(true) {
					refreshed = true
					continue
				}

				return written, errLost
			}

			k, err := w.Write(b)

			done()

			written += int64(k)

			f.mu.Lock()
			r.pos += k

			if f.fillerWaiting {
				f.broadcast()
			}
			f.mu.Unlock()

			if err != nil {
				return written, err
			}

			continue
		}

		if f.done {
			err := f.err
			f.mu.Unlock()

			if err == nil {
				err = io.ErrUnexpectedEOF
			}

			return written, err
		}

		ch := f.wake
		f.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			return written, ctx.Err()
		}
	}
}

// page is a reference-counted page buffer. On Linux page buffers are anonymous mappings
// outside the Go heap: their memory is exactly the live pages, it does not
// inflate the garbage collector's heap goal, and it returns to the system
// as soon as a buffer is unmapped.
type page struct {
	buf  []byte
	refs atomic.Int32
	pool *pagePool
}

func (p *page) ref() { p.refs.Add(1) }

func (p *page) release() {
	if p.refs.Add(-1) == 0 {
		p.pool.put(p)
	}
}

// pagePool hands out page buffers and keeps a few idle ones for reuse.
type pagePool struct {
	size    int
	maxIdle int

	mu   sync.Mutex
	idle []*page
	// live counts allocated pages that are not idle.
	live atomic.Int64
}

func newPagePool(size, maxIdle int) *pagePool {
	return &pagePool{size: size, maxIdle: maxIdle}
}

// get returns a page with one reference.
func (pp *pagePool) get() *page {
	pp.live.Add(1)

	pp.mu.Lock()
	if k := len(pp.idle); k > 0 {
		p := pp.idle[k-1]
		pp.idle[k-1] = nil
		pp.idle = pp.idle[:k-1]
		pp.mu.Unlock()
		p.refs.Store(1)

		return p
	}
	pp.mu.Unlock()

	p := &page{buf: allocPage(pp.size), pool: pp}
	p.refs.Store(1)

	return p
}

func (pp *pagePool) put(p *page) {
	pp.live.Add(-1)

	pp.mu.Lock()
	if len(pp.idle) < pp.maxIdle {
		pp.idle = append(pp.idle, p)
		pp.mu.Unlock()

		return
	}
	pp.mu.Unlock()

	freePage(p.buf)
	p.buf = nil
}

func (pp *pagePool) stats() (live, idle int) {
	pp.mu.Lock()
	defer pp.mu.Unlock()

	return int(pp.live.Load()), len(pp.idle)
}

func (pp *pagePool) drain() {
	pp.mu.Lock()
	idle := pp.idle
	pp.idle = nil
	pp.mu.Unlock()

	for _, p := range idle {
		freePage(p.buf)
		p.buf = nil
	}
}

// pageKey names one page of a blob by its absolute page index. Every copy
// of a blob holds the same bytes, so readers of different fills of the
// same chunk share pages.
type pageKey struct {
	digest string
	idx    int64
}

// diskPage is a page read from the local content store, shared by every
// concurrent reader of the same page (read coalescing). It is freed when
// its last reader is done.
type diskPage struct {
	key   pageKey
	p     *page
	ready chan struct{}
	err   error

	// readers is guarded by pageCache.mu.
	readers int
}

// pageCache coalesces concurrent page reads from the local content store:
// one read per page however many readers want it at once.
type pageCache struct {
	pool *pagePool

	mu    sync.Mutex
	m     map[pageKey]*diskPage
	reads atomic.Int64
	hits  atomic.Int64
}

func newPageCache(pool *pagePool) *pageCache {
	return &pageCache{pool: pool, m: make(map[pageKey]*diskPage)}
}

// get returns page idx (absolute, in units of the page size) of blob d
// with n valid bytes, reading it from src unless another reader is reading
// or holding it. The caller must call put.
func (c *pageCache) get(src ContentSource, d string, idx int64, n int) (*diskPage, error) {
	k := pageKey{digest: d, idx: idx}

	c.mu.Lock()
	if e := c.m[k]; e != nil {
		e.readers++
		c.mu.Unlock()
		c.hits.Add(1)

		<-e.ready

		return e, e.err
	}

	e := &diskPage{key: k, ready: make(chan struct{}), readers: 1}
	c.m[k] = e
	c.mu.Unlock()

	c.reads.Add(1)

	e.p = c.pool.get()

	_, err := src.ReadAt(d, e.p.buf[:n], idx*int64(c.pool.size))
	if err != nil {
		e.err = err

		c.mu.Lock()
		if c.m[k] == e {
			delete(c.m, k)
		}
		c.mu.Unlock()
	}

	close(e.ready)

	return e, err
}

// put drops a reader of e and frees the page after the last one.
func (c *pageCache) put(e *diskPage) {
	c.mu.Lock()
	e.readers--
	last := e.readers == 0

	if last && c.m[e.key] == e {
		delete(c.m, e.key)
	}
	c.mu.Unlock()

	if last {
		e.p.release()
	}
}

// purge forgets every page of blob d, so later readers read it afresh.
// Pages with readers are freed when the last one is done.
func (c *pageCache) purge(d string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for k := range c.m {
		if k.digest == d {
			delete(c.m, k)
		}
	}
}
