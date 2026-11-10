// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"fmt"
	"math/rand/v2"

	"github.com/Azure/unbounded/internal/cachemesh/internal/control"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// adversaryAddr is the control address of the forging peer. It is not a
// member address, so it never joins a view; members reach it only when a
// forged message makes them catch up from it.
const adversaryAddr = "x0"

// adversary is a peer that can send any control message but cannot write
// the Lease. It forges views of later terms, led by members the Lease does
// not name, and offers them as pings, NewTerm broadcasts, and catch-up
// snapshots.
type adversary struct {
	c   *Cluster
	r   *rand.Rand
	end int64
	// every is the time between strikes; each strike hits count members.
	every int64
	count int
	// forged holds the digest of every view the adversary built, so the
	// view invariants can tell forged views from published ones.
	forged map[view.Digest]bool
}

// startForgery runs a Forgery fault: from f.Start to f.End() it strikes
// f.Count members every f.Value nanoseconds. When no other fault overlaps,
// it also checks at the end that the leader and its term did not change:
// a peer that only sends messages must not move leadership.
func (c *Cluster) startForgery(f Fault) {
	if c.adv == nil {
		c.adv = &adversary{c: c, forged: map[view.Digest]bool{}}
	}

	a := c.adv
	a.r = NewRand(f.Seed, "forgery")
	a.end = f.End()
	a.every = max(f.Value, 10_000_000)
	a.count = max(f.Count, 1)

	l, hasLeader := c.Leader()

	var term uint64
	if hasLeader {
		_, term = c.members[l].n.Leading()
	}

	terms := len(c.ctl.termStarts)

	a.strike()

	if !hasLeader || c.otherFaultOverlaps(f) {
		return
	}

	c.Sim.At(f.End(), func() {
		nl, ok := c.Leader()
		if !ok {
			c.Fail(fmt.Errorf("forgery: no leader at the end (was %d)", l))

			return
		}

		if _, nt := c.members[nl].n.Leading(); nl != l || nt != term {
			c.Fail(fmt.Errorf("forgery: leader moved from %d term %d to %d term %d", l, term, nl, nt))

			return
		}

		if n := len(c.ctl.termStarts); n != terms {
			c.Fail(fmt.Errorf("forgery: %d new terms", n-terms))

			return
		}

		c.Cov.Hit("forgery-leader-kept")
	})
}

func (c *Cluster) otherFaultOverlaps(f Fault) bool {
	for _, g := range c.sc.Faults {
		if g.Kind != Checkpoint && g.Kind != Forgery && g.Start <= f.End() && g.End()+minSettle >= f.Start {
			return true
		}
	}

	return false
}

// strike forges messages to a.count random live members and schedules the
// next strike.
func (a *adversary) strike() {
	c := a.c
	now := c.Sim.Now()

	if now >= a.end || c.err != nil {
		return
	}

	live := c.Procs.InState(nil, Up)
	for range a.count {
		if len(live) == 0 {
			break
		}

		m := c.members[live[a.r.IntN(len(live))]]
		if m == nil || m.dead || m.paused {
			continue
		}

		a.hit(m)
	}

	c.Sim.After(a.every, a.strike)
}

// term returns a forged term above base: usually a little above it, at
// times far above it (beyond control.MaxTermLead) or at MaxSaneTerm.
func (a *adversary) term(base uint64) uint64 {
	switch a.r.IntN(8) {
	case 0:
		return base + control.MaxTermLead + 1 + a.r.Uint64N(1<<20)
	case 1:
		return control.MaxSaneTerm
	default:
		return base + 1 + a.r.Uint64N(1000)
	}
}

// forge returns a view one NewTerm delta above v, led by a random member
// of v at a forged term, with the delta. It returns nil if v has no
// members or the term cannot be applied.
func (a *adversary) forge(v *view.View) (*view.View, *view.Delta) {
	c := v.SlotCap()
	if c == 0 {
		return nil, nil
	}

	slot := view.Slot(a.r.Uint32N(c))
	if _, ok := v.Member(slot); !ok {
		return nil, nil
	}

	d := view.NewDelta(v, view.Op{Kind: view.KindNewTerm, Slot: slot, Term: a.term(v.Term())})

	nv, err := view.Apply(v, d)
	if err != nil {
		return nil, nil
	}

	a.forged[nv.Digest()] = true

	return nv, d
}

func (a *adversary) header(m *control.Message, kind control.Kind) {
	m.Kind = kind
	m.From = control.MemberRef{ID: "adversary", Incarnation: 1, Slot: view.NoSlot}
	m.FromAddr = adversaryAddr
	m.LeaderSlot = view.NoSlot
}

// hit sends one forged message to m: a ping claiming a later term (which
// baits a catch-up from the adversary), or a NewTerm broadcast on m's view.
func (a *adversary) hit(m *member) {
	c := a.c
	now := c.Sim.Now()

	var msg control.Message

	switch a.r.IntN(2) {
	case 0:
		a.header(&msg, control.KindPing)
		msg.Version = view.Version{Term: a.term(m.n.View().Term()), Seq: 1}

		c.Cov.Hit("forgery-ping")
	default:
		_, d := a.forge(m.n.View())
		if d == nil {
			return
		}

		a.header(&msg, control.KindBroadcast)
		msg.Version = d.Version
		msg.Level = control.LevelLocal
		msg.Delta = view.EncodeDelta(d)

		c.Cov.Hit("forgery-broadcast")
	}

	m.n.Handle(m.clock.Mono(now), adversaryAddr, control.Append(nil, &msg))
	m.after()
}

// serve answers a call from m to the adversary: a catch-up gets a forged
// snapshot of the leader's view (claiming to come from the forged leader),
// anything else an ack.
func (a *adversary) serve(m *member, id uint64, req []byte) {
	c := a.c

	var in, out control.Message
	if control.Decode(req, &in) != nil {
		return
	}

	a.header(&out, control.KindAck)

	if in.Kind == control.KindCatchUpRequest {
		base := m.n.View()
		if l, ok := c.Leader(); ok {
			base = c.members[l].n.View()
		}

		if nv, _ := a.forge(base); nv != nil {
			lm, _ := nv.Member(nv.Leader())
			out.Kind = control.KindCatchUpResponse
			out.From = control.MemberRef{ID: lm.ID, Incarnation: lm.Incarnation, Slot: nv.Leader()}
			out.FromAddr = lm.Addr
			out.LeaderSlot = nv.Leader()
			out.Version = nv.Version()
			out.Snapshot = view.EncodeSnapshot(nv)

			c.Cov.Hit("forgery-snapshot")
		}
	}

	resp := control.Append(nil, &out)

	c.Sim.After(4_000_000, func() { m.complete(id, resp, nil) })
}

// forgedView reports whether v was built by the adversary.
func (c *Cluster) forgedView(v *view.View) bool {
	return c.adv != nil && c.adv.forged[v.Digest()]
}
