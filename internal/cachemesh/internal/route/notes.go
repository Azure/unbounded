// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package route

import (
	"sync/atomic"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// Note records a sender whose view version was ahead of the receiver's.
type Note struct {
	// Term is the sender's view term, truncated to 32 bits.
	Term uint32
	// Seq is the sender's view seq.
	Seq uint64
	// Sender is the slot of the sender in the receiver's view.
	Sender view.Slot
	// Epoch is the sender's epoch.
	Epoch uint64
}

func (n *Note) less(term uint32, seq uint64) bool {
	return n.Term < term || (n.Term == term && n.Seq < seq)
}

// Notes collects the highest view version seen in incoming headers so that
// the control plane can catch up from that sender. Forward calls note on
// the hot path: one atomic load and a comparison unless the header is ahead
// of both this member's view and the recorded note, in which case it
// allocates one Note. The zero value is ready to use and safe for
// concurrent use.
type Notes struct {
	p atomic.Pointer[Note]
}

// note records h if its version is ahead of own (term truncated to 32 bits)
// and of the recorded note.
func (n *Notes) note(h *Header, ownTerm uint32, ownSeq uint64) {
	if h.Sender == view.NoSlot {
		return
	}

	if own := (Note{Term: ownTerm, Seq: ownSeq}); !own.less(h.Term, h.Seq) {
		return
	}

	var nn *Note

	for {
		cur := n.p.Load()
		if cur != nil && !cur.less(h.Term, h.Seq) {
			return
		}

		if nn == nil {
			nn = &Note{Term: h.Term, Seq: h.Seq, Sender: h.Sender, Epoch: h.Epoch}
		}

		if n.p.CompareAndSwap(cur, nn) {
			return
		}
	}
}

// NoteHeader records h as if it had been received by a member whose view
// is at version own.
func (n *Notes) NoteHeader(h Header, own view.Version) {
	n.note(&h, uint32(own.Term), own.Seq)
}

// Peek returns the recorded note without clearing it.
func (n *Notes) Peek() (Note, bool) {
	if p := n.p.Load(); p != nil {
		return *p, true
	}

	return Note{}, false
}

// Take returns and clears the recorded note. Control path.
func (n *Notes) Take() (Note, bool) {
	p := n.p.Swap(nil)
	if p == nil {
		return Note{}, false
	}

	return *p, true
}
