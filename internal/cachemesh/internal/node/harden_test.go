// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/elect"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/wire"
)

// forgeSnapshot encodes v as a snapshot whose seq is raised by bump and
// whose digest is corrupted, so the content no longer matches the digest.
func forgeSnapshot(t *testing.T, v *view.View, bump uint64) ([]byte, view.Version) {
	t.Helper()

	b := view.EncodeSnapshot(v)
	r := b[1:]
	_, n1 := binary.Uvarint(r)
	_, n2 := binary.Uvarint(r[n1:])
	rest := r[n1+n2:]

	ver := view.Version{Term: v.Term(), Seq: v.Version().Seq + bump}
	out := []byte{b[0]}
	out = binary.AppendUvarint(out, ver.Term)
	out = binary.AppendUvarint(out, ver.Seq)
	d := bytes.Clone(rest[:32])
	d[0] ^= 0xff
	out = append(out, d...)
	out = append(out, rest[32:]...)

	fv, err := view.DecodeSnapshot(out)
	if err != nil {
		t.Fatalf("forged snapshot does not decode: %v", err)
	}

	if fv.Version() != ver || fv.Digest() == v.Digest() {
		t.Fatalf("forged snapshot at %v", fv.Version())
	}

	return out, ver
}

// evilSeed serves a forged snapshot of the leader's current view to every
// catch-up request and acknowledges everything else.
func evilSeed(t *testing.T, c *cluster, bump uint64, served *int) func(string, []byte) []byte {
	return func(_ string, msg []byte) []byte {
		var in, out wire.Message
		if wire.Decode(msg, &in) != nil {
			return nil
		}

		out.From = wire.MemberRef{ID: "evil", Incarnation: 1, Slot: view.NoSlot}
		out.LeaderSlot = view.NoSlot

		if in.Kind != wire.KindCatchUpRequest {
			out.Kind = wire.KindAck

			return wire.Append(nil, &out)
		}

		*served++
		snap, ver := forgeSnapshot(t, c.nodes[c.leader()].View(), bump)
		out.Kind = wire.KindCatchUpResponse
		out.Version = ver
		out.Snapshot = snap

		return wire.Append(nil, &out)
	}
}

func eventLog(cfg *Config) *[]string {
	var evs []string

	cfg.Hooks.OnEvent = func(e string) { evs = append(evs, e) }

	return &evs
}

// TestCorruptSnapshotSeed: a joiner adopts a snapshot whose digest does
// not match its content from a corrupt seed. The next delta on that base
// fails its digest check, and the member repairs from a different source
// (the delta's sender), not the corrupt seed.
func TestCorruptSnapshotSeed(t *testing.T) {
	c := grow(t, 5)
	c.run(30 * time.Second)

	l := c.leader()
	served := 0
	c.fake["evil:1"] = evilSeed(t, c, 0, &served)

	raws := map[view.Version][]byte{}
	prev := c.nodes[l].cfg.Hooks.OnPublish
	c.nodes[l].cfg.Hooks.OnPublish = func(d *view.Delta) {
		prev(d)

		raws[d.Base] = view.EncodeDelta(d)
	}

	cfg := c.cfg(5, []string{"evil:1"})
	evs := eventLog(&cfg)
	j := c.add(cfg)

	for i := 0; c.nodes[j].View().Version().IsZero(); i++ {
		if i > 100 {
			t.Fatal("joiner never adopted the corrupt snapshot")
		}

		c.run(time.Millisecond)
	}

	jv, lv := c.nodes[j].View(), c.nodes[l].View()
	if jv.Digest() == lv.Digest() {
		t.Fatalf("joiner at %v, leader at %v", jv.Version(), lv.Version())
	}

	c.nodes[l].Invalidate(c.now, 99)

	for i := 0; raws[jv.Version()] == nil; i++ {
		if i > 2000 {
			t.Fatal("leader published nothing")
		}

		c.run(time.Millisecond)
	}

	raw := raws[jv.Version()]

	if c.nodes[j].View() != jv {
		t.Fatalf("joiner moved on to %v before the delta", c.nodes[j].View().Version())
	}

	before := served

	var b wire.Message

	b.Kind = wire.KindBroadcast
	b.From = wire.MemberRef{ID: c.nodes[l].cfg.ID, Incarnation: c.nodes[l].cfg.Incarnation, Slot: lv.Leader()}
	b.Version = lv.Version()
	b.Level = wire.LevelLocal
	b.Delta = raw
	c.nodes[j].Handle(c.now, addr(l), wire.Append(nil, &b))
	c.flush(j)

	if !slices.Contains(*evs, "bad-base") {
		t.Fatalf("corrupt base not detected: %v", *evs)
	}

	c.run(100 * time.Millisecond)

	if c.nodes[j].View().Digest() == jv.Digest() {
		t.Fatal("still on the corrupt base")
	}

	if served != before {
		t.Fatal("repair asked the corrupt seed")
	}

	c.waitConverged(2 * time.Minute)
}

// TestForgedFutureSnapshot: a snapshot that claims a version the leader
// never published is never built on; the member re-fetches from the leader
// of its term and rolls back to the leader's view.
func TestForgedFutureSnapshot(t *testing.T) {
	c := grow(t, 6)
	c.run(30 * time.Second)

	served := 0
	c.fake["evil:1"] = evilSeed(t, c, 1000, &served)

	cfg := c.cfg(6, []string{"evil:1"})
	evs := eventLog(&cfg)
	c.add(cfg)

	c.waitConverged(3 * time.Minute)

	if !slices.Contains(*evs, "snapshot-rollback") {
		t.Fatalf("no rollback: %v", *evs)
	}
}

// TestSuspectViewNotServed: a member does not serve a snapshot it has not
// confirmed.
func TestSuspectViewNotServed(t *testing.T) {
	c := grow(t, 3)
	n := c.nodes[1]
	n.unconfirmed = true

	var q, m wire.Message

	q.Kind = wire.KindCatchUpRequest
	q.From = wire.MemberRef{ID: "x", Slot: view.NoSlot}

	if err := wire.Decode(n.Handle(c.now, "x", wire.Append(nil, &q)), &m); err != nil || len(m.Snapshot) != 0 || len(m.Deltas) != 0 {
		t.Fatalf("served an unconfirmed view: %v snapshot %d deltas %d", err, len(m.Snapshot), len(m.Deltas))
	}
}

// soloStart starts a single member without seeds and drives its Lease
// operations against l until it has issued an acquire, which it returns
// unexecuted.
func soloStart(t *testing.T, l *memLease) (*Node, elect.Op, int64) {
	t.Helper()

	now := 1000 * sec
	wall := func() time.Time { return time.Unix(0, now) }
	n := New(Config{ID: "solo", Incarnation: 7, Addr: "solo:1", PartitionBits: 8}, rand.New(rand.NewPCG(1, 2)))
	n.Start(now, wall())

	for range 50 {
		for _, op := range n.LeaseOps() {
			if op.Kind == elect.OpCreate || op.Kind == elect.OpUpdate {
				return n, op, now
			}

			n.OnLease(now, wall(), l.do(op, wall()))
		}

		now += sec
		n.Tick(now, wall())
	}

	t.Fatal("no acquire issued")

	return nil, elect.Op{}, 0
}

// TestLeaveDuringAcquire: Leave while a Lease acquire is in flight; when the
// acquire succeeds the member must not lead, and it releases the Lease.
func TestLeaveDuringAcquire(t *testing.T) {
	var l memLease

	n, op, now := soloStart(t, &l)
	n.Leave(now)

	res := l.do(op, time.Unix(0, now))
	if res.Err != nil {
		t.Fatal(res.Err)
	}

	n.OnLease(now+sec/100, time.Unix(0, now), res)

	if ok, _ := n.Leading(); ok {
		t.Fatal("a leaving member started a term")
	}

	if n.Elector().State() == elect.Leader {
		t.Fatal("elector still leader")
	}

	released := false

	for _, op := range n.LeaseOps() {
		if op.Kind == elect.OpUpdate && op.Record.Holder == "" {
			released = true

			l.do(op, time.Unix(0, now))
		}
	}

	if !released || l.rec.Holder != "" {
		t.Fatalf("Lease not released: %+v", l.rec)
	}
}

// TestLeaveDuringAcquireCluster: after the leader dies, the first member to
// send an acquiring write is told to leave before the result arrives. It
// must not lead, another member must take over, and it must finish leaving.
func TestLeaveDuringAcquireCluster(t *testing.T) {
	c := grow(t, 6)
	c.run(2 * time.Minute)

	old := c.leader()
	leaver := -1

	c.onLeaseOp = func(i int, op elect.Op) {
		if leaver < 0 && i != old && op.Record.Holder != "" && (op.Kind == elect.OpUpdate || op.Kind == elect.OpCreate) {
			leaver = i

			c.nodes[i].Leave(c.now)
		}
	}
	c.down[old] = true

	for end := c.now + int64(4*time.Minute); c.now < end; {
		c.run(time.Second)

		if leaver >= 0 && c.nodes[leaver].Left() {
			if l := c.leader(); l >= 0 && l != leaver && c.converged() == nil {
				break
			}
		}
	}

	if leaver < 0 {
		t.Fatal("no acquire observed")
	}

	if !c.nodes[leaver].Left() {
		t.Fatal("leaver did not finish leaving")
	}

	if l := c.leader(); l < 0 || l == leaver || l == old {
		t.Fatalf("leader %d (leaver %d, old %d)", l, leaver, old)
	}
}

// TestHigherTermStepsDown: a leader that sees a control message with a
// higher term steps down and releases at once, even when the terms differ
// only above 32 bits; the cluster then elects a leader above both.
func TestHigherTermStepsDown(t *testing.T) {
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

	var p wire.Message

	p.Kind = wire.KindPing
	p.From = wire.MemberRef{ID: "n1", Incarnation: 1, Slot: view.NoSlot}
	p.Version = view.Version{Term: high, Seq: 1}
	p.LeaderSlot = view.NoSlot
	c.nodes[l].Handle(c.now, "nowhere:1", wire.Append(nil, &p))

	if ok, _ := c.nodes[l].Leading(); ok {
		t.Fatal("still leading after a higher term")
	}

	if c.nodes[l].Elector().State() == elect.Leader {
		t.Fatal("elector still leader")
	}

	c.flush(l)
	c.waitConverged(3 * time.Minute)

	if _, nt := c.nodes[c.leader()].Leading(); nt <= high {
		t.Fatalf("new term %d not above observed %d", nt, high)
	}
}

// TestAbsurdTermIgnored: a term at or above elect.MaxSaneTerm is garbage.
func TestAbsurdTermIgnored(t *testing.T) {
	c := grow(t, 3)
	l := c.leader()

	var p wire.Message

	p.Kind = wire.KindPing
	p.From = wire.MemberRef{ID: "x", Slot: view.NoSlot}
	p.Version = view.Version{Term: ^uint64(0), Seq: 1}
	p.LeaderSlot = view.NoSlot
	c.nodes[l].Handle(c.now, "x", wire.Append(nil, &p))

	if ok, _ := c.nodes[l].Leading(); !ok {
		t.Fatal("stepped down on an absurd term")
	}
}

// restartAll crashes every node and starts fresh processes (new
// incarnations) at the same addresses, with the given seeds.
func restartAll(c *cluster, keep []int, seeds func(i int) []string) {
	n := len(c.nodes)
	for i := range n {
		if !slices.Contains(keep, i) {
			c.down[i] = true
		}
	}

	c.run(time.Second)

	for i := range n {
		if !slices.Contains(keep, i) {
			c.add(c.cfg(i, seeds(i)))
		}
	}
}

func TestColdRestartNoSeeds(t *testing.T) {
	c := grow(t, 5)
	c.run(2 * time.Minute)

	if c.lease.rec.Holder == "" || len(c.lease.rec.Seeds) == 0 {
		t.Fatalf("Lease has no holder or seeds: %+v", c.lease.rec)
	}

	restartAll(c, nil, func(int) []string { return nil })
	c.waitConverged(5 * time.Minute)

	if c.nodes[c.leader()].View().Len() != 5 {
		t.Fatal("not everyone joined")
	}
}

func TestColdRestartWithSeeds(t *testing.T) {
	c := grow(t, 5)
	c.run(2 * time.Minute)

	restartAll(c, nil, func(i int) []string { return []string{addr((i + 1) % 5), addr((i + 2) % 5)} })
	c.waitConverged(5 * time.Minute)
}

func TestPartialRestartLeaderDead(t *testing.T) {
	c := grow(t, 6)
	c.run(2 * time.Minute)

	l := c.leader()

	var keep []int

	for i := range 6 {
		if i != l && len(keep) < 2 {
			keep = append(keep, i)
		}
	}

	restartAll(c, keep, func(int) []string { return nil })
	c.waitConverged(5 * time.Minute)
}

// TestSkewedPartitionedJoinerNoColdStart: a joiner with a wall clock a
// minute ahead that cannot reach anyone must not take a live leader's Lease,
// however long it waits: the Lease keeps changing under it.
func TestSkewedPartitionedJoinerNoColdStart(t *testing.T) {
	c := grow(t, 4)
	c.run(time.Minute)

	_, term := c.nodes[c.leader()].Leading()

	c.skew[4] = time.Minute
	for i := range 4 {
		c.cut[[2]int{4, i}] = true
	}

	cfg := c.cfg(4, nil)
	evs := eventLog(&cfg)
	c.add(cfg)
	c.run(5 * time.Minute)

	if slices.Contains(*evs, "cold-start") {
		t.Fatal("a partitioned joiner cold-started next to a live leader")
	}

	if _, nt := c.nodes[c.leader()].Leading(); nt != term {
		t.Fatalf("term changed %d -> %d", term, nt)
	}

	clear(c.cut)
	c.waitConverged(2 * time.Minute)
}

// TestPendingCallsBounded floods a member with routed messages while its
// leader is unreachable; the relays it sends stay bounded.
func TestPendingCallsBounded(t *testing.T) {
	c := grow(t, 4)
	n := c.nodes[1]

	for i := range 4 {
		c.cut[[2]int{1, i}] = true
	}

	var j, r wire.Message

	j.Kind = wire.KindJoin
	j.Member = wire.MemberRef{ID: "x", Incarnation: 1, Slot: view.NoSlot}
	r.Kind = wire.KindRouted
	r.ToLeader = true
	r.TTL = 4
	r.Inner = wire.Append(nil, &j)
	msg := wire.Append(nil, &r)

	for range 3 * MaxPendingCalls {
		n.Handle(c.now, "x", msg)
	}

	if p := n.PendingCalls(); p > MaxPendingCalls {
		t.Fatalf("%d pending calls", p)
	}

	clear(c.cut)
	c.run(30 * time.Second)

	if p := n.PendingCalls(); p > 100 {
		t.Fatalf("%d pending calls after the flood", p)
	}
}

// TestSeedsBounded: a joiner that hears broadcasts from many peers keeps a
// bounded seed list.
func TestSeedsBounded(t *testing.T) {
	n := New(Config{ID: "j", Incarnation: 1, Addr: "j:1", Seeds: []string{"s:1"}, PartitionBits: 8}, rand.New(rand.NewPCG(1, 1)))
	n.Start(sec, time.Unix(1, 0))

	var b wire.Message

	b.Kind = wire.KindBroadcast
	b.From = wire.MemberRef{ID: "p", Slot: view.NoSlot}
	b.Delta = []byte{1}

	for i := range 1000 {
		n.Handle(sec, "p"+strconv.Itoa(i), wire.Append(nil, &b))
	}

	if s := n.Stats().Seeds; s > maxSeeds {
		t.Fatalf("%d seeds", s)
	}
}

// TestJoinerSingleLeaseRead: a starting member without seeds issues its own
// cached Lease read and nothing else until that read is answered.
func TestJoinerSingleLeaseRead(t *testing.T) {
	n := New(Config{ID: "j", Incarnation: 1, Addr: "j:1", PartitionBits: 8}, rand.New(rand.NewPCG(1, 1)))
	n.Start(sec, time.Unix(1, 0))

	ops := len(n.LeaseOps())
	for i := range 20 {
		n.Tick(int64(i+2)*sec, time.Unix(int64(i+2), 0))
		ops += len(n.LeaseOps())
	}

	if ops != 1 {
		t.Fatalf("%d Lease ops before the first read returned", ops)
	}
}
