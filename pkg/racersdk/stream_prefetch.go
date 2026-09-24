// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"sync"
)

// A pending page has a private Stream solely for socket/permit ownership and
// header validation. It never consumes a body or starts another prefetch. Its
// mutex serializes preparation, adoption, and disposal independently of the
// foreground stream, so cancellation never needs the foreground mutex.
type streamPrefetch struct {
	mu    sync.Mutex
	page  *Stream
	stop  func() bool
	taken bool
	ready chan struct{}
}

func (s *Stream) startPrefetch() {
	pool := s.object.client.streamPool
	if !pool.prefetch || s.next != nil || s.pageEnd >= s.end {
		return
	}

	permit, ok := s.object.client.admission.tryAcquire(s.ctx)
	if !ok {
		return
	}

	ctx, cancel := context.WithCancel(s.ctx)
	page := &Stream{object: s.object, ctx: ctx, cancel: cancel, permit: permit, offset: s.pageEnd, end: s.end}
	next := &streamPrefetch{page: page, ready: make(chan struct{})}
	// Lock before publication: cleanup can cancel immediately, but must wait for
	// preparation to finish before releasing or transferring its resources.
	page.mu.Lock()
	pool.mu.Lock()
	if pool.pending == nil {
		pool.pending = make(map[*streamPrefetch]struct{})
	}

	pool.pending[next] = struct{}{}
	pool.mu.Unlock()

	s.next = next
	next.stop = context.AfterFunc(ctx, next.discard)

	go func() {
		defer close(next.ready)
		defer page.mu.Unlock()

		if err := page.preparePage(); err != nil {
			_ = page.fail(err) //nolint:errcheck // The pending page retains the deferred error.
		}
	}()
}

func (p *streamPrefetch) forget() {
	pool := p.page.object.client.streamPool
	pool.mu.Lock()
	delete(pool.pending, p)
	pool.mu.Unlock()
}

func (p *streamPrefetch) discard() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.taken {
		return
	}

	p.forget()
	// Cancel before locking to interrupt a dial or blocked header read. Close
	// releases even a permit acquired before any socket was obtained.
	_ = p.page.Close() //nolint:errcheck // Discarding a pending page has no transfer result.
}

func (s *Stream) discardPrefetch() {
	if s.next != nil {
		s.next.discard()
		s.next = nil
	}
}

func (p *streamPrefetch) take(s *Stream) (bool, error) {
	// Do not hold the ownership lock while waiting for headers: idle cleanup
	// must still be able to cancel an in-flight speculative request.
	<-p.ready
	p.mu.Lock()
	defer p.mu.Unlock()

	p.forget()
	page := p.page
	page.mu.Lock()
	defer page.mu.Unlock()

	if page.closed {
		return false, nil // Idle cleanup discarded it; fetch in the foreground.
	}

	if page.err != nil {
		p.stop()
		page.cancel()

		return true, page.err
	}
	// Stop pending cleanup before adoption. If cancellation already dispatched
	// cleanup, do not hand off a socket it might close. The parent context check
	// in foreground preparation preserves cancellation/deadline error ordering.
	if !p.stop() || page.ctx.Err() != nil {
		page.release(false)
		page.cancel()

		return false, nil
	}

	s.conn, page.conn = page.conn, nil
	s.permit, page.permit = page.permit, nil
	s.stop, s.stopped = page.stop, page.stopped
	s.pageEnd, s.responseClose = page.pageEnd, page.responseClose
	s.pageCancel = page.cancel
	page.closed = true
	p.taken = true

	return true, nil
}
