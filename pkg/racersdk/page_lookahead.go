// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

// The worker exclusively owns page until joined closes. It uses the same page
// validation/retry machinery as the foreground, but never consumes a body or
// starts another worker. Only atomic attempt counters can be read before joining.
type pageFuture struct {
	page   *preparedPage
	take   chan struct{}
	joined chan struct{}
	err    error
}

func (s *Stream) startFuture() {
	budget := s.object.client.streamPool.speculative
	if budget == nil || s.future != nil || s.page.pageEnd >= s.end || s.ctx.Err() != nil {
		return
	}

	select {
	case budget <- struct{}{}:
	default:
		return
	}

	offset, end := s.page.pageEnd, s.end
	f := &pageFuture{
		page: newPreparedPage(s.ctx, s.object, offset),
		take: make(chan struct{}), joined: make(chan struct{}),
	}
	s.future = f

	go func() {
		defer close(f.joined)
		defer func() { <-budget }()

		f.err = f.page.preparePageWithRetry(offset, end)
		if f.err != nil {
			f.page.release(false)
		}

		// Keep the permit while a prepared response is parked. Cancellation also
		// disposes of Prepare-only abandoned work without needing a consumer to
		// reach this page. Close/failure join this worker before returning.
		select {
		case <-f.take:
		case <-f.page.ctx.Done():
			f.page.close(false)

			if f.err == nil {
				f.err = f.page.ctx.Err()
			}
		}
	}()
}

func (s *Stream) mergePageAttempts(p *preparedPage) {
	s.stats.PageRequests += p.requests.Load()
	s.stats.PageRetries += p.retries.Load()
}

func (s *Stream) takeFuture() error {
	s.page.close(!s.page.responseClose)

	f := s.future
	close(f.take)
	<-f.joined
	s.mergePageAttempts(s.page)
	s.page, s.future = f.page, nil
	f.page = nil

	return f.err
}

func (s *Stream) discardFuture() {
	if f := s.future; f != nil {
		f.page.cancel()
		<-f.joined
		s.mergePageAttempts(f.page)
		s.future = nil
	}
}
