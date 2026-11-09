// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

// FIFO is a bounded map that evicts the oldest insertion. It backs the
// simulation-wide caches (Interner and the node package's Shared). It is not
// safe for concurrent use.
type FIFO[K comparable, V any] struct {
	m     map[K]V
	order []K
	next  int
}

// NewFIFO returns a FIFO holding at most n entries.
func NewFIFO[K comparable, V any](n int) FIFO[K, V] {
	return FIFO[K, V]{m: make(map[K]V, n), order: make([]K, 0, n)}
}

// Get returns the value stored for k.
func (f *FIFO[K, V]) Get(k K) (V, bool) {
	v, ok := f.m[k]

	return v, ok
}

// Put stores v for k. Replacing an existing key keeps its eviction order;
// inserting a new key into a full FIFO evicts the oldest key.
func (f *FIFO[K, V]) Put(k K, v V) {
	if _, ok := f.m[k]; ok {
		f.m[k] = v

		return
	}

	if len(f.order) < cap(f.order) {
		f.order = append(f.order, k)
	} else {
		delete(f.m, f.order[f.next])
		f.order[f.next] = k
		f.next = (f.next + 1) % len(f.order)
	}

	f.m[k] = v
}
