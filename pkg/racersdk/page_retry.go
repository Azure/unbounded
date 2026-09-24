// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

const (
	pageRetries         = 4
	pageRetryWaitBudget = 5 * time.Second
)

// Only rejected page headers are retryable. preparePage has not consumed or
// forwarded any page body, and the immutable Object and offset remain pinned.
// Body reads, splice failures, and stale sockets never enter this retry loop.
func (s *Stream) preparePageWithRetry() error {
	var waited time.Duration

	for retry := 0; ; retry++ {
		err := s.preparePage()
		if err == nil {
			return nil
		}

		var status *HTTPError
		if s.ctx.Err() != nil || retry == pageRetries || !errors.As(err, &status) ||
			(status.StatusCode != 429 && status.StatusCode != 503 && status.StatusCode != 504) {
			return err
		}

		delay, ok := pageRetryDelay(status.RetryAfter, retry, time.Now())
		if !ok || delay > pageRetryWaitBudget-waited {
			return err
		}

		if end, ok := s.ctx.Deadline(); ok && time.Until(end) <= delay {
			return err
		}

		// Never pool a rejected response or drain an untrusted error body. This
		// also discards header read-ahead before another attempt is dispatched.
		s.release(false)
		s.operation = "page_retry_wait"

		timer := time.NewTimer(delay)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return err // fail preserves the HTTP evidence and returns ctx.Err().
		case <-timer.C:
		}

		waited += delay
	}
}

// Retry-After is a minimum, never truncated to retry earlier than requested.
// Excessive/invalid hints fail closed. Positive jitter also applies to hints.
func pageRetryDelay(hint string, retry int, now time.Time) (time.Duration, bool) {
	base := (100 * time.Millisecond) << retry
	minimum := time.Duration(0)

	if hint != "" {
		seconds, err := strconv.ParseUint(hint, 10, 64)
		if err == nil {
			if seconds > uint64(pageRetryWaitBudget/time.Second) {
				return 0, false
			}

			minimum = time.Duration(seconds) * time.Second
		} else {
			date, err := http.ParseTime(hint)
			if err != nil {
				return 0, false
			}

			minimum = max(time.Duration(0), date.Sub(now))
		}
	}

	if minimum > pageRetryWaitBudget {
		return 0, false
	}

	return max(base, minimum) + time.Duration(rand.Int64N(int64(base))), true
}
