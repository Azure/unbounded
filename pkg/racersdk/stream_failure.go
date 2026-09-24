// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

// StreamFailure describes the first failed stream operation. Offsets are object
// offsets, not downstream byte counts. Err retains the original error before
// context cancellation can replace it; it may contain a target or socket path
// and must not be logged verbatim. ContextErr is captured before cleanup.
// No request headers or origin data are stored.
type StreamFailure struct {
	Operation  string
	PageOffset int64
	Offset     int64
	StatusCode int
	Err        error
	ContextErr error
}

// Failure returns a copy of the first failure, even after Close. Nil means no
// stream operation has failed. This is diagnostic state, not a retry boundary:
// some bytes may already have reached the destination.
func (s *Stream) Failure() *StreamFailure {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failure == nil {
		return nil
	}

	failure := *s.failure

	return &failure
}
