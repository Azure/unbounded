// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/Azure/unbounded/internal/cachemesh"
)

// readStep bounds one read into a fill buffer, so readers of an in-progress
// chunk see progress at least every readStep bytes (cut-through).
const readStep = 1 << 20

var (
	errBusy   = errors.New("meshfetch: chunk budget exhausted")
	errClosed = errors.New("meshfetch: node closed")
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

// fill is one chunk in memory, complete or being filled. Bytes buf[:n] are
// immutable once published under mu, so readers copy them out without the
// lock. One goroutine (the filler) writes buf[n:].
type fill struct {
	key   chunkKey
	store *store

	// refs counts holders: the store map, the filler, and every reader.
	// The buffer returns to the pool when it drops to zero.
	refs atomic.Int32

	mu        sync.Mutex
	wake      chan struct{}
	buf       []byte
	n         int
	total     int64
	ctype     string
	metaKnown bool
	done      bool
	err       error
	// upstream is the slot the fill reads from, for loop detection.
	upstream cachemesh.Slot
	// fromOrigin is set while the fill reads from the origin.
	fromOrigin bool

	// Guarded by store.mu.
	prev, next *fill
	inMap      bool
	listed     bool
	charge     int64
	// owned marks a chunk this member filled as its owner. Owned chunks
	// sit on their own LRU list and are evicted after the others, so an
	// owner keeps serving them while transit copies churn.
	owned bool
}

func (f *fill) release() {
	if f.refs.Add(-1) == 0 {
		f.store.recycle(f)
	}
}

// broadcast wakes waiting readers. Caller holds f.mu.
func (f *fill) broadcast() {
	close(f.wake)
	f.wake = make(chan struct{})
}

// setMeta records the blob size and content type and allocates the buffer.
// It returns errMetaMismatch when an earlier attempt saw different values.
func (f *fill) setMeta(total int64, ctype string, chunkLen int) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.metaKnown {
		if f.total != total || len(f.buf) != chunkLen {
			return errMetaMismatch
		}

		return nil
	}

	f.buf = f.store.alloc(chunkLen)
	f.total = total
	f.ctype = ctype
	f.metaKnown = true
	f.broadcast()

	return nil
}

var errMetaMismatch = errors.New("meshfetch: chunk size differs between sources")

// progress returns the bytes filled so far and whether metadata is known.
func (f *fill) progress() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.n, f.metaKnown
}

// readFrom reads r into the buffer until the chunk is full. onRead runs
// after every successful read (stall detection).
func (f *fill) readFrom(r io.Reader, onRead func()) error {
	f.mu.Lock()
	n, buf := f.n, f.buf
	f.mu.Unlock()

	for n < len(buf) {
		end := min(n+readStep, len(buf))

		k, err := r.Read(buf[n:end])
		if k > 0 {
			n += k

			f.mu.Lock()
			f.n = n
			f.broadcast()
			f.mu.Unlock()

			if onRead != nil {
				onRead()
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				if n < len(buf) {
					return io.ErrUnexpectedEOF
				}

				return nil
			}

			return err
		}
	}

	return nil
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
			total, ctype, chunkLen = f.total, f.ctype, len(f.buf)
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

// streamTo writes buf[off:] to w as it becomes available. It returns the
// number of bytes written and the first error: the fill's, the writer's, or
// ctx's.
func (f *fill) streamTo(ctx context.Context, w io.Writer, off int) (int64, error) {
	var written int64

	for {
		f.mu.Lock()

		if f.metaKnown && off > len(f.buf) {
			f.mu.Unlock()
			return written, io.ErrShortBuffer
		}

		if f.n > off {
			b := f.buf[off:f.n]
			f.mu.Unlock()

			k, err := w.Write(b)
			written += int64(k)
			off += k

			if err != nil {
				return written, err
			}

			continue
		}

		if f.metaKnown && off == len(f.buf) {
			f.mu.Unlock()
			return written, nil
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

// store is the in-memory chunk cache: a map of fills plus LRU lists of
// complete ones, under a byte budget that also covers fills in progress.
// Chunks this member owns are kept on a separate list and evicted only
// when no other complete chunk is left, or when they hold more than half
// of the budget.
type store struct {
	chunkSize int64
	budget    int64
	maxFills  int

	mu        sync.Mutex
	m         map[chunkKey]*fill
	head      fill // LRU sentinel of other chunks: head.next is most recent
	ownedHead fill // LRU sentinel of owned chunks
	ownedUsed int64
	used      int64
	inflight  int
	space     chan struct{}
	closed    bool

	// poolMu guards pool. It is taken under store.mu and fill.mu, never
	// the other way around.
	poolMu  sync.Mutex
	pool    [][]byte
	onEvict func(bytes int64)
}

func newStore(chunkSize, budget int64, maxFills int, onEvict func(int64)) *store {
	s := &store{
		chunkSize: chunkSize,
		budget:    budget,
		maxFills:  maxFills,
		m:         make(map[chunkKey]*fill),
		space:     make(chan struct{}),
		onEvict:   onEvict,
	}
	s.head.next = &s.head
	s.head.prev = &s.head
	s.ownedHead.next = &s.ownedHead
	s.ownedHead.prev = &s.ownedHead

	return s
}

// get returns a referenced fill for k, or nil.
func (s *store) get(k chunkKey) *fill {
	s.mu.Lock()
	defer s.mu.Unlock()

	f := s.m[k]
	if f == nil {
		return nil
	}

	f.refs.Add(1)

	if f.listed {
		s.unlink(f)
		s.pushFront(f)
	}

	return f
}

// acquire returns the fill for k: an existing one (created == false) or a
// new one charged to the budget (created == true). A new fill holds three
// references: the map, the filler, and the caller. With wait false it
// returns errBusy when the budget or fill cap is exhausted; with wait true
// it blocks until space frees or ctx ends.
func (s *store) acquire(ctx context.Context, k chunkKey, wait bool) (f *fill, created bool, err error) {
	for {
		s.mu.Lock()

		if s.closed {
			s.mu.Unlock()
			return nil, false, errClosed
		}

		if f = s.m[k]; f != nil {
			f.refs.Add(1)

			if f.listed {
				s.unlink(f)
				s.pushFront(f)
			}

			s.mu.Unlock()

			return f, false, nil
		}

		if s.inflight < s.maxFills && s.makeRoom(s.chunkSize) {
			f = &fill{key: k, store: s, wake: make(chan struct{}), total: -1, charge: s.chunkSize, upstream: noSlot}
			f.refs.Store(3)
			f.inMap = true
			s.m[k] = f
			s.used += f.charge
			s.inflight++
			s.mu.Unlock()

			return f, true, nil
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

// makeRoom evicts least recently used complete chunks until n more bytes
// fit: other chunks first, owned chunks once no other one is left or they
// exceed half of the budget. Caller holds s.mu.
func (s *store) makeRoom(n int64) bool {
	for s.used+n > s.budget {
		victim := s.head.prev
		if victim == &s.head || 2*s.ownedUsed > s.budget {
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
func (s *store) markOwned(f *fill) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f.owned = true
}

func (s *store) evictLocked(f *fill) {
	if f.listed {
		s.unlink(f)
	}

	if f.inMap {
		delete(s.m, f.key)
		f.inMap = false
		s.used -= f.charge

		if f.owned {
			s.ownedUsed -= f.charge
		}

		if s.onEvict != nil {
			s.onEvict(f.charge)
		}

		f.release()
	}

	s.signalSpace()
}

func (s *store) signalSpace() {
	close(s.space)
	s.space = make(chan struct{})
}

// finished moves a complete fill to the LRU list or drops a failed one.
func (s *store) finished(f *fill, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.inflight--

	if f.inMap {
		// Charge the real size: short final chunks and small manifests
		// do not hold a full chunk.
		if c := int64(cap(f.buf)); err == nil && c < f.charge {
			s.used -= f.charge - c
			f.charge = c
		}

		if err != nil {
			delete(s.m, f.key)
			f.inMap = false
			s.used -= f.charge
			f.release()
		} else {
			if f.owned {
				s.ownedUsed += f.charge
			}

			s.pushFront(f)
		}
	}

	s.makeRoom(0)
	s.signalSpace()
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
	if f.owned {
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

// alloc returns a buffer of length n, reusing a pooled full-size chunk
// buffer when n is the chunk size.
func (s *store) alloc(n int) []byte {
	if int64(n) != s.chunkSize {
		return make([]byte, n)
	}

	s.poolMu.Lock()
	if k := len(s.pool); k > 0 {
		b := s.pool[k-1]
		s.pool[k-1] = nil
		s.pool = s.pool[:k-1]
		s.poolMu.Unlock()

		return b[:n]
	}
	s.poolMu.Unlock()

	return make([]byte, n)
}

// recycle returns f's buffer to the pool once nothing references it.
func (s *store) recycle(f *fill) {
	f.mu.Lock()
	b := f.buf
	f.buf = nil
	f.mu.Unlock()

	if int64(cap(b)) != s.chunkSize {
		return
	}

	s.poolMu.Lock()
	defer s.poolMu.Unlock()

	// Keep at most the budget's worth of idle buffers.
	if int64(len(s.pool)+1)*s.chunkSize <= s.budget {
		s.pool = append(s.pool, b[:cap(b)])
	}
}

// close fails waiters and drops idle buffers.
func (s *store) close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	s.signalSpace()

	s.poolMu.Lock()
	s.pool = nil
	s.poolMu.Unlock()
}

// Stats is a point-in-time view of the chunk cache.
type Stats struct {
	// Bytes charged to the budget: cached chunks plus fills in progress.
	Bytes int64
	// Chunks in the cache, complete or in progress.
	Chunks int
	// Inflight is the number of fills in progress.
	Inflight int
	// PooledBuffers is the number of idle chunk buffers kept for reuse.
	PooledBuffers int
}

func (s *store) stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.poolMu.Lock()
	pooled := len(s.pool)
	s.poolMu.Unlock()

	return Stats{Bytes: s.used, Chunks: len(s.m), Inflight: s.inflight, PooledBuffers: pooled}
}
