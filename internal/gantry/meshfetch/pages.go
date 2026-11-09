// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"sync"
	"sync/atomic"
	"unsafe"
)

// page is a reference-counted page buffer. Its capacity is the page size
// rounded up to dioAlign, and its start is dioAlign aligned, so it can be
// used for O_DIRECT I/O.
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

	p := &page{buf: alignedBuf(int(alignUp(int64(pp.size)))), pool: pp}
	p.refs.Store(1)

	return p
}

func (pp *pagePool) put(p *page) {
	pp.live.Add(-1)

	pp.mu.Lock()
	defer pp.mu.Unlock()

	if len(pp.idle) < pp.maxIdle {
		pp.idle = append(pp.idle, p)
	}
}

func (pp *pagePool) stats() (live, idle int) {
	pp.mu.Lock()
	defer pp.mu.Unlock()

	return int(pp.live.Load()), len(pp.idle)
}

func (pp *pagePool) drain() {
	pp.mu.Lock()
	defer pp.mu.Unlock()

	pp.idle = nil
}

// alignedBuf returns n bytes starting at a dioAlign boundary.
func alignedBuf(n int) []byte {
	b := make([]byte, n+dioAlign)
	off := 0

	if r := int(uintptr(unsafe.Pointer(&b[0])) & (dioAlign - 1)); r != 0 { //nolint:gosec // address arithmetic for alignment only
		off = dioAlign - r
	}

	return b[off : off+n : off+n]
}

// pageKey names one page of one fill. The fill pointer, not the chunk key,
// identifies the data: a refilled chunk is a different fill.
type pageKey struct {
	f   *fill
	idx int
}

// diskPage is a page read from a chunk file, shared by every concurrent
// reader of the same page (read coalescing). Pages that had two or more
// readers may stay in the hot tier after the last reader is done.
type diskPage struct {
	key   pageKey
	p     *page
	n     int
	ready chan struct{}
	err   error

	// Guarded by pageCache.mu.
	readers    int
	shared     bool
	hot        bool
	dead       bool
	prev, next *diskPage
}

// pageCache coalesces disk page reads and keeps a small hot tier of pages
// with concurrent demand.
type pageCache struct {
	pool   *pagePool
	budget int64

	mu    sync.Mutex
	m     map[pageKey]*diskPage
	head  diskPage // hot LRU sentinel: head.next is most recent
	used  int64
	reads atomic.Int64
	hits  atomic.Int64
}

func newPageCache(pool *pagePool, budget int64) *pageCache {
	c := &pageCache{pool: pool, budget: budget, m: make(map[pageKey]*diskPage)}
	c.head.next = &c.head
	c.head.prev = &c.head

	return c
}

// get returns page idx of f with n valid bytes, reading it from f's file
// unless another reader is reading it or it is in the hot tier. The caller
// must call put.
func (c *pageCache) get(f *fill, file *chunkFile, idx, n int) (*diskPage, error) {
	k := pageKey{f: f, idx: idx}

	c.mu.Lock()
	if e := c.m[k]; e != nil {
		e.readers++
		e.shared = true

		if e.hot {
			c.unlink(e)
			c.pushFront(e)
		}
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
	e.n = n

	err := f.store.readFile(file, e.p.buf, int64(idx)*int64(c.pool.size), n)
	if err != nil {
		e.err = err

		c.mu.Lock()
		if c.m[k] == e {
			delete(c.m, k)
		}

		e.dead = true
		c.mu.Unlock()
	}

	close(e.ready)

	return e, err
}

// put drops a reader of e. A page that had concurrent readers stays in
// the hot tier, which evicts least recently used pages over its budget.
func (c *pageCache) put(e *diskPage) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e.readers--
	if e.readers > 0 || e.hot {
		return
	}

	if !e.dead && e.err == nil && e.shared && int64(len(e.p.buf)) <= c.budget {
		e.hot = true
		c.used += int64(len(e.p.buf))
		c.pushFront(e)
		c.trim()

		return
	}

	c.drop(e)
}

// trim evicts idle hot pages until the tier fits its budget.
func (c *pageCache) trim() {
	for v := c.head.prev; c.used > c.budget && v != &c.head; {
		prev := v.prev
		if v.readers == 0 {
			c.drop(v)
		}

		v = prev
	}
}

// drop removes e from the map and the hot list and frees its page. Caller
// holds c.mu and e has no readers.
func (c *pageCache) drop(e *diskPage) {
	if c.m[e.key] == e {
		delete(c.m, e.key)
	}

	if e.hot {
		c.unlink(e)
		c.used -= int64(len(e.p.buf))
		e.hot = false
	}

	e.dead = true

	if e.p != nil {
		e.p.release()
		e.p = nil
	}
}

// purge drops every page of f. Pages with readers are dropped when the
// last one is done.
func (c *pageCache) purge(f *fill) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for k, e := range c.m {
		if k.f != f {
			continue
		}

		delete(c.m, k)

		e.dead = true
		if e.readers == 0 {
			c.drop(e)
		} else if e.hot {
			c.unlink(e)
			c.used -= int64(len(e.p.buf))
			e.hot = false
		}
	}
}

func (c *pageCache) pushFront(e *diskPage) {
	e.prev = &c.head
	e.next = c.head.next
	c.head.next.prev = e
	c.head.next = e
}

func (c *pageCache) unlink(e *diskPage) {
	e.prev.next = e.next
	e.next.prev = e.prev
	e.prev, e.next = nil, nil
}

func (c *pageCache) hotBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.used
}
