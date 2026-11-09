// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"sync"
	"sync/atomic"
)

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
