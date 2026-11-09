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
// (cut-through, and the "read once" path of a hot fan-out). A fill that is
// written to disk queues every completed page to its writer and drops a
// page from the window once it is on disk; readers that fall behind the
// window read the page back from disk through the store's page cache,
// which coalesces concurrent reads of the same page. A fill that is not
// written to disk (stream-through) drops a page only once every reader has
// passed it, so its slowest reader paces it; a reader stuck for longer
// than the stall timeout is dropped and gets errLost.
type fill struct {
	key   chunkKey
	store *store

	// refs counts holders: the store map, the filler, and every reader.
	refs atomic.Int32
	// wantDisk is set once disk space is reserved for the fill.
	wantDisk atomic.Bool

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
	readers       map[*reader]struct{}
	fillerWaiting bool

	file       *chunkFile
	diskFailed bool
	diskSize   int64
	writes     chan int
	queued     int
	// durable bytes of the chunk are on disk.
	durable  int
	detached bool

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

// reader is one consumer of a fill. Its position holds back the window of
// a fill that is not written to disk.
type reader struct {
	f *fill
	// Guarded by f.mu.
	pos    int
	kicked bool
}

func (f *fill) release() {
	if f.refs.Add(-1) == 0 {
		f.recycle()
	}
}

// recycle frees the window and closes the file once nothing references f.
func (f *fill) recycle() {
	f.mu.Lock()
	win := f.win
	f.win = nil
	cf := f.file
	f.mu.Unlock()

	for _, p := range win {
		p.release()
	}

	f.store.pages.purge(f)

	if cf != nil {
		cf.remove()
		cf.close()
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
	return !f.detached && (off >= f.winStart*f.store.cfg.pageSize || off < f.durable)
}

// newReaderLocked registers a reader at off. Caller holds f.mu and a
// reference for the reader.
func (f *fill) newReaderLocked(off int) *reader {
	r := &reader{f: f, pos: off}
	if f.readers == nil {
		f.readers = make(map[*reader]struct{})
	}

	f.readers[r] = struct{}{}

	return r
}

// newReader registers a reader at off for a reference the caller holds.
func (f *fill) newReader(off int) *reader {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.newReaderLocked(off)
}

// close unregisters r and drops its reference.
func (r *reader) close() {
	f := r.f

	f.mu.Lock()
	delete(f.readers, r)

	if f.fillerWaiting {
		f.broadcast()
	}
	f.mu.Unlock()

	f.release()
}

// share is called under store.mu when a peer joins f: under
// PersistShared, it reserves disk so the chunk is written for later
// readers. The caller then calls startDisk without store.mu.
func (f *fill) share() {
	if f.store.cfg.persist == PersistShared && !f.wantDisk.Load() {
		f.store.reserveLocked(f)
	}
}

// startDisk creates the chunk file and starts the writer once disk is
// reserved and the size is known. A fill whose first page is gone is not
// written: its reservation is returned.
func (f *fill) startDisk() {
	if !f.wantDisk.Load() {
		return
	}

	s := f.store

	f.mu.Lock()
	if f.file != nil || f.diskFailed || !f.metaKnown || f.detached || (f.done && f.err != nil) {
		f.mu.Unlock()
		return
	}

	if f.winStart > 0 {
		f.diskFailed = true
		f.mu.Unlock()
		s.unreserve(f)

		return
	}

	var (
		cf  *chunkFile
		err error
	)

	if s.faults != nil && s.faults.create != nil {
		err = s.faults.create()
	}

	if err == nil {
		cf, err = s.disk.create(int64(f.chunkLen), s.direct())
	}

	if err != nil {
		f.diskFailed = true
		f.mu.Unlock()
		s.diskFailed(err)
		s.unreserve(f)

		return
	}

	f.file = cf
	f.diskSize = alignUp(int64(f.chunkLen))

	f.writes = make(chan int, s.cfg.window+1)
	go f.writeLoop(cf, f.writes)

	f.queueLocked()
	f.mu.Unlock()
}

// unreserve returns f's disk reservation.
func (s *store) unreserve(f *fill) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if f.inMap && !f.listed {
		s.used -= f.charge
		f.charge = 0
	}

	s.signalSpace()
}

// persisting reports whether completed pages go to disk. Caller holds f.mu.
func (f *fill) persisting() bool { return f.file != nil && !f.diskFailed }

// queueLocked hands completed pages to the writer. Caller holds f.mu.
func (f *fill) queueLocked() {
	ps := f.store.cfg.pageSize
	for f.writes != nil && f.queued < f.n {
		end := min(f.queued+ps, f.chunkLen)
		if end > f.n {
			return
		}

		f.writes <- f.queued / ps

		f.queued = end
	}
}

// writeLoop writes queued pages to cf in order. When the fill is complete
// and on disk, it frees the window so readers read from disk.
func (f *fill) writeLoop(cf *chunkFile, ch <-chan int) {
	s := f.store
	ps := s.cfg.pageSize

	for idx := range ch {
		f.mu.Lock()
		if f.diskFailed || idx < f.winStart || idx >= f.winStart+len(f.win) {
			f.mu.Unlock()
			continue
		}

		p := f.win[idx-f.winStart]
		p.ref()

		end := min(f.chunkLen, (idx+1)*ps)
		f.mu.Unlock()

		var err error
		if s.faults != nil && s.faults.write != nil {
			err = s.faults.write(idx)
		}

		if err == nil {
			err = cf.writeAt(p.buf[:end-idx*ps], int64(idx)*int64(ps))
		}

		p.release()

		f.mu.Lock()
		if err != nil {
			f.diskFailed = true
			f.broadcast()
			done := f.done
			f.mu.Unlock()

			s.diskFailed(err)

			if done {
				s.detach(f)
			}

			continue
		}

		f.durable = end
		f.broadcast()
		f.mu.Unlock()
		s.diskWrites.Add(1)
	}

	f.mu.Lock()

	var win []*page

	if !f.diskFailed && f.done && f.err == nil && f.durable == f.chunkLen {
		win = f.win
		f.winStart += len(f.win)
		f.win = nil
		f.broadcast()
	}
	f.mu.Unlock()

	for _, p := range win {
		p.release()
	}
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

	f.startDisk()

	return nil
}

// progress returns the bytes filled so far and whether metadata is known.
func (f *fill) progress() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.n, f.metaKnown
}

// slot returns the window page holding byte n, waiting for room in the
// window: a page leaves it once it is on disk, or for a fill not written
// to disk, once every reader has passed it. touch is called while waiting
// so a slow reader does not count as a stalled upstream.
func (f *fill) slot(ctx context.Context, n int, touch func()) (*page, error) {
	s := f.store
	ps := s.cfg.pageSize
	pi := n / ps

	var (
		kick  <-chan time.Time
		timer *time.Timer
	)

	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	f.mu.Lock()
	defer f.mu.Unlock()

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
		if f.persisting() && f.durable >= oldEnd || !f.persisting() && f.minReaderLocked() >= oldEnd {
			f.win[0].release()
			copy(f.win, f.win[1:])
			f.win[len(f.win)-1] = nil
			f.win = f.win[:len(f.win)-1]
			f.winStart++

			continue
		}

		if timer == nil {
			timer = time.NewTimer(s.cfg.stallTimeout)
			kick = timer.C
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

// minReaderLocked returns the lowest position of a reader that still
// needs the window. Readers before durable read from disk. Caller holds
// f.mu.
func (f *fill) minReaderLocked() int {
	low := int(^uint(0) >> 1)

	for r := range f.readers {
		if r.pos >= f.durable && r.pos < low {
			low = r.pos
		}
	}

	return low
}

// kickLocked drops readers holding back the page that ends at end. They
// get errLost and start over with a new fill. Caller holds f.mu.
func (f *fill) kickLocked(end int) {
	for r := range f.readers {
		if r.pos >= f.durable && r.pos < end {
			r.kicked = true
			delete(f.readers, r)
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
			f.queueLocked()
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

	if f.writes != nil {
		close(f.writes)
		f.writes = nil
	}

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
// become available: from the fill's window, or from disk through the page
// cache. It returns the bytes written and the first error: the fill's,
// the writer's, ctx's, or errLost.
func (r *reader) streamTo(ctx context.Context, w io.Writer) (int64, error) {
	f := r.f
	s := f.store
	ps := s.cfg.pageSize

	var written int64

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
			case off < f.durable:
				cf, n := f.file, min(ps, f.chunkLen-pi*ps)
				f.mu.Unlock()

				e, err := s.pages.get(f, cf, pi, n)
				if err != nil {
					s.pages.put(e)
					s.diskFailed(err)
					s.detach(f)

					return written, errors.Join(errLost, err)
				}

				b, done = e.p.buf[off-pi*ps:n], func() { s.pages.put(e) }
			default:
				f.mu.Unlock()
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
