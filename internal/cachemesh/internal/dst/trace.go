// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"encoding/binary"

	"github.com/zeebo/xxh3"
)

// Trace is a running hash of recorded events. Two runs of the same seed must
// produce the same Sum; a mismatch means something nondeterministic leaked
// into the simulation.
type Trace struct {
	h   *xxh3.Hasher
	buf [26]byte
	n   uint64
}

// NewTrace returns an empty trace.
func NewTrace() *Trace { return &Trace{h: xxh3.New()} }

// Add records an event of the given kind with three arguments. It does not
// allocate.
func (t *Trace) Add(kind uint16, a, b, c uint64) {
	binary.LittleEndian.PutUint16(t.buf[0:], kind)
	binary.LittleEndian.PutUint64(t.buf[2:], a)
	binary.LittleEndian.PutUint64(t.buf[10:], b)
	binary.LittleEndian.PutUint64(t.buf[18:], c)
	t.write(t.buf[:])
	t.n++
}

// AddBytes records an event carrying opaque bytes. The length is included
// so adjacent records cannot alias.
func (t *Trace) AddBytes(kind uint16, p []byte) {
	t.Add(kind, uint64(len(p)), 0, 0)
	t.write(p)
}

// AddString records an event carrying a string.
func (t *Trace) AddString(kind uint16, s string) {
	t.Add(kind, uint64(len(s)), 0, 0)

	if _, err := t.h.WriteString(s); err != nil {
		panic(err)
	}
}

// write feeds p to the hash. xxh3.Hasher.Write never fails.
func (t *Trace) write(p []byte) {
	if _, err := t.h.Write(p); err != nil {
		panic(err)
	}
}

// Len returns the number of records added.
func (t *Trace) Len() uint64 { return t.n }

// Sum returns the 64-bit hash of everything recorded so far.
func (t *Trace) Sum() uint64 { return t.h.Sum64() }
