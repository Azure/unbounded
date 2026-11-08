// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

import "crypto/sha256"

// DefaultInternerSize is the default number of entries per Interner table.
const DefaultInternerSize = 4096

type applyKey struct {
	base  Digest
	delta Digest
}

// fifo is a bounded map evicting the oldest insertion.
type fifo[K comparable, V any] struct {
	m     map[K]V
	order []K
	next  int
}

func newFIFO[K comparable, V any](n int) fifo[K, V] {
	return fifo[K, V]{m: make(map[K]V, n), order: make([]K, 0, n)}
}

func (f *fifo[K, V]) get(k K) (V, bool) {
	v, ok := f.m[k]

	return v, ok
}

func (f *fifo[K, V]) put(k K, v V) {
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

// Interner shares identical views and decoded deltas, for example across
// thousands of simulated members that apply the same deltas. It is only a
// cache: results equal those of Apply and DecodeDelta. Each table is bounded
// and evicts in insertion order. It is not safe for concurrent use.
// Production code does not use it.
type Interner struct {
	byID    fifo[Digest, *View]
	deltas  fifo[Digest, *Delta]
	applied fifo[applyKey, *View]

	// Hits and Misses count Apply and Delta cache lookups.
	Hits, Misses uint64
}

// NewInterner returns an interner keeping up to size entries per table
// (DefaultInternerSize if size <= 0).
func NewInterner(size int) *Interner {
	if size <= 0 {
		size = DefaultInternerSize
	}

	return &Interner{
		byID:    newFIFO[Digest, *View](size),
		deltas:  newFIFO[Digest, *Delta](size),
		applied: newFIFO[applyKey, *View](size),
	}
}

// View returns the interned view with v's digest, interning v if there is
// none. Views with equal digests are assumed equal.
func (in *Interner) View(v *View) *View {
	if w, ok := in.byID.get(v.Digest()); ok && w.Version() == v.Version() {
		return w
	}

	in.byID.put(v.Digest(), v)

	return v
}

// Apply is Apply(base, d) memoized on (base digest, delta digest). The
// result is interned.
func (in *Interner) Apply(base *View, d *Delta) (*View, error) {
	k := applyKey{base: base.Digest(), delta: d.Digest}
	if v, ok := in.applied.get(k); ok && base.Version() == d.Base && v.Version() == d.Version {
		in.Hits++

		return v, nil
	}

	in.Misses++

	v, err := Apply(base, d)
	if err != nil {
		return nil, err
	}

	v = in.View(v)
	in.applied.put(k, v)

	return v, nil
}

// Delta is DecodeDelta(encoded) memoized on the content of encoded. The
// returned delta is shared and must not be modified.
func (in *Interner) Delta(encoded []byte) (*Delta, error) {
	k := Digest(sha256.Sum256(encoded))
	if d, ok := in.deltas.get(k); ok {
		in.Hits++

		return d, nil
	}

	in.Misses++

	d, err := DecodeDelta(encoded)
	if err != nil {
		return nil, err
	}

	in.deltas.put(k, d)

	return d, nil
}
