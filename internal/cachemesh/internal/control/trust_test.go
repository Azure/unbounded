// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package control

import (
	"slices"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// forgeTerm returns a well-formed view one NewTerm delta above v, led by
// slot at term v.Term()+bump. Its digest matches its content: only the
// Lease can tell it from a real one.
func forgeTerm(t *testing.T, v *view.View, slot view.Slot, bump uint64) *view.View {
	t.Helper()

	d := view.NewDelta(v, view.Op{Kind: view.KindNewTerm, Slot: slot, Term: v.Term() + bump})

	nv, err := view.Apply(v, d)
	if err != nil {
		t.Fatalf("forge: %v", err)
	}

	return nv
}

// forgedResponse is a catch-up response carrying snapshot nv that claims
// to come from nv's leader.
func forgedResponse(t *testing.T, nv *view.View) []byte {
	t.Helper()

	m, ok := nv.Member(nv.Leader())
	if !ok {
		t.Fatal("forged view has no leader member")
	}

	var r Message

	r.Kind = KindCatchUpResponse
	r.From = MemberRef{ID: m.ID, Incarnation: m.Incarnation, Slot: nv.Leader()}
	r.FromAddr = m.Addr
	r.LeaderSlot = nv.Leader()
	r.Version = nv.Version()
	r.Snapshot = view.EncodeSnapshot(nv)

	return Append(nil, &r)
}

// forgedPing is a ping that claims term term from evil:1.
func forgedPing(term uint64) []byte {
	var p Message

	p.Kind = KindPing
	p.From = MemberRef{ID: "evil", Incarnation: 1, Slot: view.NoSlot}
	p.FromAddr = "evil:1"
	p.LeaderSlot = view.NoSlot
	p.Version = view.Version{Term: term, Seq: 1}

	return Append(nil, &p)
}

// otherMember returns a member slot of v other than its leader and the
// node at index skip.
func otherMember(c *cluster, skip int) view.Slot {
	v := c.nodes[c.leader()].View()

	for i, n := range c.nodes {
		if s := n.Self(); i != skip && s != view.NoSlot && s != v.Leader() {
			return s
		}
	}

	c.t.Fatal("no other member")

	return view.NoSlot
}

// verify makes node i read the Lease and waits for the result, so its view
// is backed by the Lease.
func (c *cluster) verify(i int) {
	c.t.Helper()

	n := c.nodes[i]
	n.verifiedAt = 0
	n.verifyLease(c.now)
	c.flush(i)
	c.run(100 * time.Millisecond)

	if !n.viewBacked() {
		li, ok := n.el.Leader()
		c.t.Fatalf("node %d view %v not backed after a read: %+v %v", i, n.View().Version(), li, ok)
	}
}

func follower(c *cluster) int {
	for i, n := range c.nodes {
		if ok, _ := n.Leading(); !ok && n.Elector().State() == StateFollower && !c.down[i] {
			return i
		}
	}

	c.t.Fatal("no follower")

	return -1
}

// TestHigherTermDoesNotStepDown: a higher term in a message is a claim,
// not evidence. The leader keeps leading and keeps the Lease; the forged
// term only raises the term of the next election.
func TestHigherTermDoesNotStepDown(t *testing.T) {
	c := grow(t, 5)
	c.run(2 * time.Minute)

	l := c.leader()
	_, term := c.nodes[l].Leading()
	high := term + 1<<32

	// A data-plane note carries a truncated term: never a step-down.
	c.nodes[l].NoteVersion(c.now, 0, view.Version{Term: uint64(uint32(high)) + 1, Seq: 1})

	if ok, _ := c.nodes[l].Leading(); !ok {
		t.Fatal("stepped down on a data-plane note")
	}

	evs := eventLog(&c.nodes[l].cfg)
	c.nodes[l].Handle(c.now, "nowhere:1", forgedPing(high))

	if ok, _ := c.nodes[l].Leading(); !ok {
		t.Fatal("stepped down on an unverified higher term")
	}

	if c.nodes[l].Elector().State() != StateLeader {
		t.Fatal("elector left the leader state")
	}

	for _, op := range c.nodes[l].LeaseOps() {
		if op.Kind == OpUpdate && op.Record.Holder == "" {
			t.Fatal("released the Lease on an unverified higher term")
		}
	}

	for _, call := range c.nodes[l].Outbox() {
		if call.To == "nowhere:1" {
			t.Fatal("leader contacted the sender of a forged term")
		}
	}

	// A forged broadcast of a later term led by another member is not
	// applied either.
	forged := forgeTerm(t, c.nodes[l].View(), otherMember(c, l), 3)
	d := view.NewDelta(c.nodes[l].View(), view.Op{Kind: view.KindNewTerm, Slot: forged.Leader(), Term: forged.Term()})

	var b Message

	b.Kind = KindBroadcast
	b.From = MemberRef{ID: "evil", Incarnation: 1, Slot: view.NoSlot}
	b.FromAddr = "evil:1"
	b.LeaderSlot = view.NoSlot
	b.Version = d.Version
	b.Level = LevelLocal
	b.Delta = view.EncodeDelta(d)
	c.nodes[l].Handle(c.now, "evil:1", Append(nil, &b))

	if c.nodes[l].View().Term() != term {
		t.Fatalf("leader applied a forged term: %v", c.nodes[l].View().Version())
	}

	if !slices.Contains(*evs, "term-unverified") {
		t.Fatalf("no term-unverified event: %v", *evs)
	}

	c.flush(l)
	c.run(time.Minute)

	if nl := c.leader(); nl != l {
		t.Fatalf("leader moved from %d to %d", l, nl)
	}

	if _, nt := c.nodes[l].Leading(); nt != term {
		t.Fatalf("term moved from %d to %d", term, nt)
	}

	if c.lease.rec.Holder != c.nodes[l].cfg.ID || c.lease.rec.Term != term {
		t.Fatalf("Lease %+v", c.lease.rec)
	}

	c.waitConverged(time.Minute)

	// The next election still starts above every term seen.
	if got := c.nodes[l].Elector().MaxTerm(); got != high {
		t.Fatalf("max term %d, want %d", got, high)
	}
}

// TestTermInflationBounded: a term more than MaxTermLead above the Lease
// is ignored, so messages cannot push terms toward MaxSaneTerm.
func TestTermInflationBounded(t *testing.T) {
	c := grow(t, 3)
	l := c.leader()
	_, term := c.nodes[l].Leading()

	evs := eventLog(&c.nodes[l].cfg)
	c.nodes[l].Handle(c.now, "evil:1", forgedPing(term+MaxTermLead+1))

	if got := c.nodes[l].Elector().MaxTerm(); got != term {
		t.Fatalf("max term %d after an absurd term, want %d", got, term)
	}

	if !slices.Contains(*evs, "absurd-term") {
		t.Fatalf("no absurd-term event: %v", *evs)
	}
}

// TestForgedSnapshotBackedView: a member whose view the Lease backs does
// not catch up from the sender of a higher term, and refuses a snapshot of
// a later term led by a member the Lease does not name.
func TestForgedSnapshotBackedView(t *testing.T) {
	c := grow(t, 6)
	c.run(30 * time.Second)

	f := follower(c)
	c.verify(f)

	n := c.nodes[f]
	before := n.View()
	evs := eventLog(&n.cfg)

	n.Handle(c.now, "evil:1", forgedPing(before.Term()+5))

	for _, call := range n.Outbox() {
		if call.To == "evil:1" {
			t.Fatal("backed member caught up from the sender of a forged term")
		}
	}

	forged := forgeTerm(t, before, otherMember(c, f), 5)

	var m Message
	if err := Decode(forgedResponse(t, forged), &m); err != nil {
		t.Fatal(err)
	}

	n.onCatchUp(c.now, &m, "evil:1")

	if n.View() != before {
		t.Fatalf("backed member adopted a forged term: %v", n.View().Version())
	}

	if !slices.Contains(*evs, "term-unverified") {
		t.Fatalf("no term-unverified event: %v", *evs)
	}

	c.flush(f)
	c.waitConverged(time.Minute)
}

// TestForgedSnapshotUnbackedRollsBack: a member the Lease does not back
// (it never read it) adopts a forged later-term snapshot only as
// unconfirmed. The real leader's next delta, of a lower term, makes it read
// the Lease, which names another leader; it re-fetches from the Lease
// holder and rolls back.
func TestForgedSnapshotUnbackedRollsBack(t *testing.T) {
	c := grow(t, 6)
	c.run(30 * time.Second)

	f := follower(c)
	n := c.nodes[f]

	// Forget any earlier Lease read, as a follower that never read it.
	n.el.noteHolder(Record{})

	l := c.leader()
	_, term := c.nodes[l].Leading()
	forged := forgeTerm(t, n.View(), otherMember(c, f), 5)

	var m Message
	if err := Decode(forgedResponse(t, forged), &m); err != nil {
		t.Fatal(err)
	}

	evs := eventLog(&n.cfg)
	n.onCatchUp(c.now, &m, "evil:1")

	if n.View().Version() != forged.Version() {
		t.Fatalf("unbacked member at %v, want the forged %v", n.View().Version(), forged.Version())
	}

	if !n.unconfirmed || n.snapFrom != "evil:1" {
		t.Fatalf("forged snapshot adopted as confirmed (unconfirmed %v, from %q)", n.unconfirmed, n.snapFrom)
	}

	var r Message
	if err := Decode(n.serveCatchUp(&Message{Kind: KindCatchUpRequest}), &r); err != nil {
		t.Fatal(err)
	}

	if len(r.Snapshot) > 0 || len(r.Deltas) > 0 {
		t.Fatal("unconfirmed forged view served to others")
	}

	c.flush(f)

	// Make the leader publish so a lower-term delta reaches the member.
	c.nodes[l].Invalidate(c.now, 7)
	c.waitConverged(time.Minute)

	if nl := c.leader(); nl != l {
		t.Fatalf("leader moved from %d to %d", l, nl)
	}

	if _, nt := c.nodes[l].Leading(); nt != term {
		t.Fatalf("term moved from %d to %d", term, nt)
	}

	for _, want := range []string{"lease-verify", "lease-contradicts-view", "lease-rollback"} {
		if !slices.Contains(*evs, want) {
			t.Fatalf("no %s event: %v", want, *evs)
		}
	}
}

// TestImpersonatedLeaderSnapshotUnconfirmed: a response that claims to come
// from the Lease holder (its identity and slot) but arrives from another
// address is not vouched for, so it is adopted only as unconfirmed.
func TestImpersonatedLeaderSnapshotUnconfirmed(t *testing.T) {
	c := grow(t, 4)
	l := c.leader()
	cfg := c.cfg(4, []string{addr(l)})
	j := c.add(cfg)
	n := c.nodes[j]

	li, _ := c.nodes[l].Elector().Leader()
	n.el.noteHolder(Record{Holder: li.ID, Incarnation: li.Incarnation, Term: li.Term, Addr: li.Addr})

	var m Message
	if err := Decode(c.nodes[l].serveCatchUp(&Message{Kind: KindCatchUpRequest}), &m); err != nil {
		t.Fatal(err)
	}

	n.onCatchUp(c.now, &m, "evil:1")

	if !n.unconfirmed || n.snapFrom != "evil:1" {
		t.Fatalf("impersonated leader snapshot adopted as confirmed (unconfirmed %v, from %q)", n.unconfirmed, n.snapFrom)
	}
}

// TestVouchedSnapshotConfirmed: a snapshot served by the Lease holder of its
// term is adopted as confirmed; the same snapshot without Lease knowledge
// is adopted unconfirmed.
func TestVouchedSnapshotConfirmed(t *testing.T) {
	for _, backed := range []bool{true, false} {
		c := grow(t, 4)
		l := c.leader()
		cfg := c.cfg(4, []string{addr(l)})
		j := c.add(cfg)
		n := c.nodes[j]

		if backed {
			c.verify(l) // no-op for the leader; keeps the clock moving

			li, _ := c.nodes[l].Elector().Leader()
			n.el.noteHolder(Record{Holder: li.ID, Incarnation: li.Incarnation, Term: li.Term, Addr: li.Addr})
		}

		var m Message
		if err := Decode(c.nodes[l].serveCatchUp(&Message{Kind: KindCatchUpRequest}), &m); err != nil {
			t.Fatal(err)
		}

		n.onCatchUp(c.now, &m, addr(l))

		if n.View().Version() != c.nodes[l].View().Version() {
			t.Fatalf("backed=%v: joiner at %v", backed, n.View().Version())
		}

		if n.unconfirmed == backed {
			t.Fatalf("backed=%v: unconfirmed=%v", backed, n.unconfirmed)
		}
	}
}

// TestCatchUpTargetsLeaseLeader: a member whose view the Lease backs, told
// of a later term, reads the Lease and catches up from the holder the
// Lease names, not from the sender.
func TestCatchUpTargetsLeaseLeader(t *testing.T) {
	c := grow(t, 5)
	c.run(30 * time.Second)

	f := follower(c)
	c.verify(f)

	n := c.nodes[f]
	n.verifiedAt = 0
	n.Handle(c.now, "evil:1", forgedPing(n.View().Term()+1))

	ops := n.LeaseOps()
	if len(ops) != 1 || ops[0].Kind != OpGet {
		t.Fatalf("lease ops %+v, want one Get", ops)
	}

	for _, call := range n.Outbox() {
		if call.To == "evil:1" {
			t.Fatal("caught up from the sender")
		}
	}

	// The Lease moved to another member at a later term.
	k := (f + 1) % len(c.nodes)
	if k == c.leader() {
		k = (k + 1) % len(c.nodes)
	}

	rec := c.lease.rec.Clone()
	rec.Holder, rec.Incarnation, rec.Addr = c.nodes[k].cfg.ID, c.nodes[k].cfg.Incarnation, addr(k)
	rec.Term = n.View().Term() + 1
	rec.RenewTime = c.wallOf(f)

	n.OnLease(c.now, c.wallOf(f), Result{Seq: ops[0].Seq, Kind: OpGet, Record: rec})

	var to []string

	for _, call := range n.Outbox() {
		var m Message
		if Decode(call.Msg, &m) == nil && m.Kind == KindCatchUpRequest {
			to = append(to, call.To)
		}
	}

	if !slices.Equal(to, []string{addr(k)}) {
		t.Fatalf("catch-up to %v, want [%s]", to, addr(k))
	}
}

// TestFailoverWithBackedViews: when every member's view is backed by the
// Lease, a real failover (the Lease moves to a new holder at a later term)
// still converges.
func TestFailoverWithBackedViews(t *testing.T) {
	c := grow(t, 8)
	c.run(2 * time.Minute)

	old := c.leader()
	_, oldTerm := c.nodes[old].Leading()

	for i := range c.nodes {
		if i != old {
			c.verify(i)
		}
	}

	c.down[old] = true

	var err error

	for end := c.now + int64(3*time.Minute); c.now < end; {
		c.run(time.Second)

		if l := c.leader(); l >= 0 && l != old {
			if err = c.converged(); err == nil && c.nodes[l].View().Len() == 7 {
				break
			}
		}
	}

	if err != nil {
		t.Fatal(err)
	}

	l := c.leader()
	if _, term := c.nodes[l].Leading(); l == old || term <= oldTerm {
		t.Fatalf("leader %d term %d after failover from %d term %d", l, term, old, oldTerm)
	}
}
