// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh"
)

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
				if s.borrowExtra() || f.reclaimLocked() {
					f.extra++
					p := s.pool.get()
					f.win = append(f.win, p)

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
			if s.borrowExtra() || f.reclaimLocked() {
				f.extra++
				p := s.pool.get()
				f.win = append(f.win, p)

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
