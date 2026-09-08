// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package coldstart_test

import (
	"context"
	"sync"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// stubDisco implements coldstart.Discovery. providers is the canned
// FindProviders response, advanced one entry per call so tests can arrange
// "DHT empty, then non-empty"; the last entry repeats once exhausted.
type stubDisco struct {
	mu        sync.Mutex
	providers [][]ifaces.Provider
	idx       int
	health    float64
}

func (s *stubDisco) FindProviders(_ context.Context, _ digest.Digest) ([]ifaces.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.idx >= len(s.providers) {
		if len(s.providers) == 0 {
			return nil, nil
		}

		return s.providers[len(s.providers)-1], nil
	}

	out := s.providers[s.idx]
	s.idx++

	return out, nil
}

func (s *stubDisco) Health() float64 {
	if s.health == 0 {
		return 1.0
	}

	return s.health
}
