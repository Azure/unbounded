// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import "context"

// The worker exclusively owns page until joined closes. It uses the same page
// validation/retry machinery as the foreground, but never consumes a body or
// starts another worker. Only atomic attempt counters can be read before joining.
type pageFuture struct {
	page   *Stream
	take   chan struct{}
	joined chan struct{}
	err    error
}

func (s *Stream) startFuture() {
	budget := s.object.client.streamPool.speculative
	if budget == nil || s.future != nil || s.pageEnd >= s.end || s.ctx.Err() != nil {
		return
	}

	select {
	case budget <- struct{}{}:
	default:
		return
	}

	ctx, cancel := context.WithCancel(s.ctx)
	f := &pageFuture{
		page: &Stream{object: s.object, ctx: ctx, cancel: cancel, offset: s.pageEnd, end: s.end, pageOffset: s.pageEnd},
		take: make(chan struct{}), joined: make(chan struct{}),
	}
	s.future = f

	go func() {
		defer close(f.joined)
		defer func() { <-budget }()

		f.err = f.page.preparePageWithRetry()
		if f.err != nil {
			f.page.release(false)
		}

		// Keep the permit while a prepared response is parked. Cancellation also
		// disposes of Prepare-only abandoned work without needing a consumer to
		// reach this page. Close/failure join this worker before returning.
		select {
		case <-f.take:
		case <-ctx.Done():
			f.page.release(false)

			if f.err == nil {
				f.err = ctx.Err()
			}
		}
	}()
}

func (s *Stream) mergeFuture(f *pageFuture) {
	s.requests.Add(f.page.requests.Load())
	s.retries.Add(f.page.retries.Load())
	s.future = nil
}

func (s *Stream) takeFuture() error {
	f := s.future
	close(f.take)
	<-f.joined
	s.mergeFuture(f)

	p := f.page
	s.operation, s.pageOffset, s.statusCode = p.operation, p.pageOffset, p.statusCode
	s.conn, s.stop, s.stopped = p.conn, p.stop, p.stopped
	s.pageEnd, s.responseClose = p.pageEnd, p.responseClose

	// The socket cancellation callback belongs to the child's context until
	// release. Stopping that callback and joining it before canceling the child
	// lets the adopted socket retain the original stream deadline safely.
	if s.conn != nil {
		oldStop, oldDone, cancel := s.stop, s.stopped, p.cancel
		s.stop = func() bool {
			stopped := oldStop()
			if !stopped {
				<-oldDone
			}

			cancel()

			return stopped
		}
	} else {
		p.cancel()
	}

	return f.err
}

func (s *Stream) discardFuture() {
	if f := s.future; f != nil {
		f.page.cancel()
		<-f.joined
		s.mergeFuture(f)
	}
}
