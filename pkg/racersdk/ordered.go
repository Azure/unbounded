// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"io"
	"sync"
)

// orderedRead owns at most two page-sized buffers, including the current lease,
// queued leases and an in-flight receive. Negotiated credits can reduce this to
// one. No buffer crosses request boundaries. Only Release makes storage reusable.
// The connection admission slot stays held until consumption ends or cancellation
// has joined the receiver and dropped its buffers, not merely until wire EOF.
type orderedRead struct {
	mu     sync.Mutex
	stream *PageStream
	ready  chan *PageLease
	slots  chan struct{}
	done   chan struct{}
	err    error // Published by closing ready; read only after ready is drained.
	lease  *PageLease
	offset int
}

func (v *Value) startOrdered(s *PageStream) {
	limit := min(2, s.pageCredits, int(s.byteCredits/uint64(PageSize)))
	s.buffers = make(chan []byte, limit)
	r := &orderedRead{stream: s, ready: make(chan *PageLease, limit), slots: make(chan struct{}, limit), done: make(chan struct{})}

	v.mu.Lock()
	defer v.mu.Unlock()

	if v.terminal != nil {
		return
	}

	v.ordered = r
	go r.receive()
}

func (r *orderedRead) receive() {
	defer close(r.done)
	defer close(r.ready)

	for {
		select {
		case r.slots <- struct{}{}:
		case <-r.stream.owner.ctx.Done():
			r.err = ioFailure("subscription", r.stream.owner.ctx.Err())
			return
		}

		p, err := r.stream.next()
		if err != nil {
			<-r.slots
			r.err = err

			return
		}
		// ready has room for every occupied slot, even if cancellation wins.
		r.ready <- p

		r.stream.mu.Lock()
		complete := r.stream.complete
		r.stream.mu.Unlock()

		if complete {
			r.err = io.EOF
			return
		}
	}
}

// shutdown runs after cancel and socket close. It never waits on caller-owned
// Write, which only sees the separately bounded WriteTo scratch buffer.
func (r *orderedRead) shutdown() {
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.lease != nil {
		_ = r.lease.Release() //nolint:errcheck // Terminal cleanup drops local ownership; the closed socket needs no credit return.
		r.lease = nil
		<-r.slots
	}

	for p := range r.ready {
		_ = p.Release() //nolint:errcheck // Preserve the already published terminal error during cleanup.

		<-r.slots
	}

	for len(r.stream.buffers) > 0 {
		<-r.stream.buffers
	}
}

func (r *orderedRead) read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.stream.owner.err(); err != nil {
		return 0, err
	}

	if r.lease == nil {
		select {
		case lease, ok := <-r.ready:
			if !ok {
				return 0, r.err
			}

			r.lease, r.offset = lease, 0
		case <-r.stream.owner.ctx.Done():
			return 0, ioFailure("read", r.stream.owner.ctx.Err())
		}
	}

	n := copy(p, r.lease.Data[r.offset:])

	r.offset += n
	if r.offset == len(r.lease.Data) {
		err := r.lease.Release()
		r.lease = nil
		<-r.slots

		return n, err
	}

	return n, nil
}
