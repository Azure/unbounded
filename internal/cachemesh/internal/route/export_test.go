// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package route

import "github.com/Azure/unbounded/internal/cachemesh/internal/view"

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
