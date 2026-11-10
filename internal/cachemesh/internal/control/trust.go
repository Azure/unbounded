// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package control

import (
	"errors"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// Lease corroboration.
//
// Control messages, deltas, and snapshots are not signed: any peer that can
// send a message can claim any term, leader, or version. The Lease is the
// only source of truth for who leads at which term, so a claim of a later
// term (or of leadership) is believed only when the Lease, as last read by
// this member's elector, backs it:
//
//   - A view is backed when this member leads its term, or the last Lease
//     read names the leader of the view (ID and incarnation) at the view's
//     term. A member with a backed view refuses views of a later term that
//     the Lease does not show, and reads the Lease (Verify) instead; if the
//     read shows a newer holder it catches up from that holder's address.
//   - A snapshot is trusted (adopted as confirmed) only when the Lease names
//     its leader at its term and that leader served it. Any other snapshot
//     is unconfirmed until a delta applies on top of it.
//   - A member whose view is not backed reads the Lease when a delta of a
//     lower term arrives (two terms are live, so one of them is forged or
//     stale). If the Lease contradicts the view, it re-fetches from the
//     Lease holder and adopts the holder's snapshot even at a lower version.
//   - A higher term in a message never makes a leader step down; only its
//     own Lease renewals do.
//   - Terms more than MaxTermLead above the highest term the Lease has
//     shown are ignored, so messages cannot push terms toward MaxSaneTerm.

// MaxTermLead bounds how far a term seen in a message or view may lead the
// highest term this member has seen in the Lease. A real cluster gains one
// term per election, so this is never reached; it keeps a peer that only
// sends messages from inflating terms toward MaxSaneTerm, since every
// legitimate acquisition can raise the Lease term by at most MaxTermLead.
const MaxTermLead = uint64(1) << 32

// errTermRefused reports a delta of a later term that admitTerm refused.
var errTermRefused = errors.New("control: later term not backed by the Lease")

// termCeiling returns the highest term this member believes in a message,
// delta, or snapshot.
func (n *Node) termCeiling() uint64 {
	return min(n.el.LeaseTerm()+MaxTermLead, MaxSaneTerm-1)
}

// saneTerm reports whether t is at most termCeiling, recording an event
// otherwise.
func (n *Node) saneTerm(t uint64) bool {
	if t > n.termCeiling() {
		n.event("absurd-term")

		return false
	}

	return true
}

// leaseNames reports whether li names the leader of v at term t.
func leaseNames(li LeaderInfo, v *view.View, t uint64) bool {
	if li.Term != t || li.ID == "" {
		return false
	}

	m, ok := v.Member(v.Leader())

	return ok && m.ID == li.ID && m.Incarnation == li.Incarnation
}

// viewBacked reports whether the Lease backs the current view: this member
// leads its term, or the last Lease read names its leader at its term.
func (n *Node) viewBacked() bool {
	t := n.v.Term()
	if t == 0 {
		return false
	}

	if n.leading {
		return t == n.term
	}

	li, ok := n.el.Leader()

	return ok && leaseNames(li, n.v, t)
}

// admitTerm reports whether a view of term t may replace the current view.
// Terms at or below the current one are left to the usual version rules.
// A later term is refused when it is absurd, or when the current view is
// backed by the Lease and the Lease does not show term t; in the latter
// case the member reads the Lease to learn whether it changed.
func (n *Node) admitTerm(now int64, t uint64) bool {
	if t <= n.v.Term() {
		return true
	}

	if !n.saneTerm(t) {
		return false
	}

	if !n.viewBacked() {
		return true
	}

	if li, ok := n.el.Leader(); ok && li.Term >= t {
		return true
	}

	n.event("term-unverified")
	n.verifyLease(now)

	return false
}

// vouched reports whether snapshot nv, served in response m by the peer at
// from, comes from the Lease holder of its term: the last Lease read names
// nv's leader at nv's term, that leader is the responder, and the response
// came from the address the Lease records. The responder's identity in m is
// only a claim, so the address is what ties the response to the holder.
func (n *Node) vouched(nv *view.View, m *Message, from string) bool {
	if nv.Leader() != m.From.Slot || nv.Version() != m.Version {
		return false
	}

	li, ok := n.el.Leader()
	if !ok || li.ID != m.From.ID || li.Addr == "" || li.Addr != from || !leaseNames(li, nv, nv.Term()) {
		return false
	}

	return true
}

// verifyLease reads the Lease, at most once per LeaseDuration, unless this
// member leads (its renewals already check the Lease) or another Lease
// operation is outstanding.
func (n *Node) verifyLease(now int64) {
	if n.leading || (n.verifiedAt != 0 && now-n.verifiedAt < int64(n.cfg.Election.LeaseDuration)) {
		return
	}

	ops := n.el.Verify(n.electInput(now, n.wall))
	if len(ops) == 0 {
		return
	}

	n.verifiedAt = now
	n.verifySeq = ops[0].Seq
	n.event("lease-verify")
	n.leaseOps = append(n.leaseOps, ops...)
}

// afterVerify acts on the result of a Verify read. A Lease that names a
// later term than the view is handled by tickElector. A Lease that
// contradicts a view it does not back (it names another leader at the
// view's term, or a lower term) means the view came from a forged or stale
// source: re-fetch from the Lease holder, whose snapshot onCatchUp adopts
// even at a lower version.
func (n *Node) afterVerify(now int64) {
	li, ok := n.el.Leader()
	if !ok || n.leading || li.Addr == "" || li.Addr == n.cfg.Addr {
		return
	}

	switch {
	case li.Term > n.v.Term():
		n.catchUp(now, li.Addr, false)
	case !n.viewBacked():
		n.event("lease-contradicts-view")
		n.catchUp(now, li.Addr, true)
	}
}
