// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import "sync"

// identityAdmission retains identities through response writes and flushes.
// Each endpoint owns an independent set and fixes its capacity at construction.
type identityAdmission[T comparable] struct {
	mu    sync.Mutex
	limit int
	held  map[T]struct{}
}

func newIdentityAdmission[T comparable](limit int) *identityAdmission[T] {
	return &identityAdmission[T]{limit: limit, held: make(map[T]struct{})}
}

func (a *identityAdmission[T]) acquire(id T) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, exists := a.held[id]; exists || len(a.held) >= a.limit {
		return false
	}

	a.held[id] = struct{}{}

	return true
}

func (a *identityAdmission[T]) release(id T) {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.held, id)
}

func (a *identityAdmission[T]) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return len(a.held)
}
