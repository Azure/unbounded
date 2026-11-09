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

// Interner shares identical views and decoded deltas, for example across
// thousands of simulated members that apply the same deltas. It is only a
// cache: results equal those of Apply and DecodeDelta. Each table is bounded
// and evicts in insertion order. It is not safe for concurrent use.
// Production code does not use it.
type Interner struct {
	byID    FIFO[Digest, *View]
	deltas  FIFO[Digest, *Delta]
	applied FIFO[applyKey, *View]

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
		byID:    NewFIFO[Digest, *View](size),
		deltas:  NewFIFO[Digest, *Delta](size),
		applied: NewFIFO[applyKey, *View](size),
	}
}

// View returns the interned view with v's digest, interning v if there is
// none. Views with equal digests are assumed equal.
func (in *Interner) View(v *View) *View {
	if w, ok := in.byID.Get(v.Digest()); ok && w.Version() == v.Version() {
		return w
	}

	in.byID.Put(v.Digest(), v)

	return v
}

// Apply is Apply(base, d) memoized on (base digest, delta digest). The
// result is interned.
func (in *Interner) Apply(base *View, d *Delta) (*View, error) {
	k := applyKey{base: base.Digest(), delta: d.Digest}
	if v, ok := in.applied.Get(k); ok && base.Version() == d.Base && v.Version() == d.Version {
		in.Hits++

		return v, nil
	}

	in.Misses++

	v, err := Apply(base, d)
	if err != nil {
		return nil, err
	}

	v = in.View(v)
	in.applied.Put(k, v)

	return v, nil
}

// Delta is DecodeDelta(encoded) memoized on the content of encoded. The
// returned delta is shared and must not be modified.
func (in *Interner) Delta(encoded []byte) (*Delta, error) {
	k := Digest(sha256.Sum256(encoded))
	if d, ok := in.deltas.Get(k); ok {
		in.Hits++

		return d, nil
	}

	in.Misses++

	d, err := DecodeDelta(encoded)
	if err != nil {
		return nil, err
	}

	in.deltas.Put(k, d)

	return d, nil
}
