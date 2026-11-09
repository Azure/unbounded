// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package control

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/route"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

const sec = int64(time.Second)

var errDown = errors.New("down")

// memLease is a minimal in-memory Lease store.
type memLease struct {
	rec    Record
	exists bool
	rv     int
}

func (l *memLease) do(op Op, wall time.Time) Result {
	res := Result{Seq: op.Seq, Kind: op.Kind}

	switch op.Kind {
	case OpGet, OpGetCached:
		if !l.exists {
			res.Err = ErrNotFound
		} else {
			res.Record = l.rec.Clone()
		}
	case OpCreate:
		if l.exists {
			res.Err = ErrConflict

			return res
		}

		l.exists = true
		l.rv++
		l.rec = op.Record.Clone()
		l.rec.ResourceVersion = strconv.Itoa(l.rv)
		res.Record = l.rec.Clone()
	case OpUpdate:
		switch {
		case !l.exists:
			res.Err = ErrNotFound
		case op.Record.ResourceVersion != l.rec.ResourceVersion:
			res.Err = ErrConflict
		default:
			l.rv++
			l.rec = op.Record.Clone()
			l.rec.ResourceVersion = strconv.Itoa(l.rv)
			res.Record = l.rec.Clone()
		}
	}

	_ = wall

	return res
}

type event struct {
	at  int64
	seq uint64
	fn  func()
}

// cluster is a tiny deterministic driver: one global clock, fixed message
// latency, no loss unless a member is down or partitioned.
type cluster struct {
	t      *testing.T
	now    int64
	nodes  []*Node
	down   []bool
	byAddr map[string]int
	lease  memLease
	events []event
	seq    uint64
	lat    int64
	cut    map[[2]int]bool
	shared *Shared
	pubs   map[view.Version]view.Digest
	valid  map[*view.View]bool
	// fake serves calls to addresses that are not nodes (hostile peers).
	fake map[string]func(from string, msg []byte) []byte
	// skew offsets the wall clock of a node index.
	skew map[int]time.Duration
	// onLeaseOp, if set, sees every Lease op as a node emits it.
	onLeaseOp func(i int, op Op)
}

func (c *cluster) wallOf(i int) time.Time { return c.wall().Add(c.skew[i]) }

func addr(i int) string { return "n" + strconv.Itoa(i) + ":1" }

func newCluster(t *testing.T) *cluster {
	return &cluster{
		t: t, now: 1000 * sec, byAddr: map[string]int{}, lat: int64(time.Millisecond),
		cut: map[[2]int]bool{}, shared: NewShared(0), pubs: map[view.Version]view.Digest{}, valid: map[*view.View]bool{},
		fake: map[string]func(string, []byte) []byte{}, skew: map[int]time.Duration{},
	}
}

func (c *cluster) wall() time.Time { return time.Unix(0, c.now) }

func (c *cluster) cfg(i int, seeds []string) Config {
	return Config{
		ID: "n" + strconv.Itoa(i), Incarnation: uint64(1000 + len(c.nodes)), Addr: addr(i), Zone: "z",
		Seeds: seeds, PartitionBits: 10, Shared: c.shared,
		ExpectedSize: 10,
		Tuning:       Tuning{Election: ElectionConfig{MinCandidateAge: 20 * time.Second}, DeltaLogSize: 32},
	}
}

func (c *cluster) add(cfg Config) int {
	i := len(c.nodes)
	if j, ok := c.byAddr[cfg.Addr]; ok {
		i = j
	}

	pubs := c.pubs
	cfg.Hooks.OnPublish = func(d *view.Delta) {
		if old, ok := pubs[d.Version]; ok && old != d.Digest {
			c.t.Fatalf("version %v published twice with different digests", d.Version)
		}

		pubs[d.Version] = d.Digest
	}

	n := New(cfg, rand.New(rand.NewPCG(uint64(i), cfg.Incarnation)))
	if i == len(c.nodes) {
		c.nodes = append(c.nodes, n)
		c.down = append(c.down, false)
	} else {
		c.nodes[i] = n
		c.down[i] = false
	}

	c.byAddr[cfg.Addr] = i
	n.Start(c.now, c.wallOf(i))
	c.flush(i)

	return i
}

func (c *cluster) at(t int64, fn func()) {
	c.seq++
	c.events = append(c.events, event{at: t, seq: c.seq, fn: fn})
}

func (c *cluster) flush(i int) {
	n := c.nodes[i]
	inc := n.cfg.Incarnation

	for _, call := range n.Outbox() {
		c.deliver(i, inc, call)
	}

	for _, op := range n.LeaseOps() {
		if c.onLeaseOp != nil {
			c.onLeaseOp(i, op)
		}

		c.at(c.now+c.lat, func() {
			if c.down[i] || c.nodes[i].cfg.Incarnation != inc {
				return
			}

			res := c.lease.do(op, c.wall())
			c.at(c.now+c.lat, func() {
				if c.down[i] || c.nodes[i].cfg.Incarnation != inc {
					return
				}

				c.nodes[i].OnLease(c.now, c.wallOf(i), res)
				c.flush(i)
			})
		})
	}
}

func (c *cluster) deliver(from int, inc uint64, call Call) {
	to, ok := c.byAddr[call.To]
	if call.Timeout <= 0 {
		c.t.Fatalf("call with timeout %v to %s", call.Timeout, call.To)
	}

	if h, fake := c.fake[call.To]; fake {
		fromAddr := c.nodes[from].cfg.Addr

		c.at(c.now+c.lat, func() {
			resp := h(fromAddr, call.Msg)

			c.at(c.now+c.lat, func() {
				if c.down[from] || c.nodes[from].cfg.Incarnation != inc {
					return
				}

				c.nodes[from].OnReply(c.now, call.ID, resp, nil)
				c.flush(from)
			})
		})

		return
	}

	reply := func(resp []byte, err error) {
		if c.down[from] || c.nodes[from].cfg.Incarnation != inc {
			return
		}

		c.nodes[from].OnReply(c.now, call.ID, resp, err)
		c.flush(from)
	}

	if !ok || c.down[to] || c.cut[[2]int{from, to}] || c.cut[[2]int{to, from}] {
		c.at(c.now+int64(call.Timeout), func() { reply(nil, errDown) })

		return
	}

	c.at(c.now+c.lat, func() {
		if c.down[to] {
			c.at(c.now+int64(call.Timeout), func() { reply(nil, errDown) })

			return
		}

		resp := c.nodes[to].Handle(c.now, c.nodes[from].cfg.Addr, call.Msg)
		c.flush(to)
		c.at(c.now+c.lat, func() { reply(resp, nil) })
	})
}

// run advances time by d, ticking nodes at their deadlines.
func (c *cluster) run(d time.Duration) {
	end := c.now + int64(d)
	iters := 0

	for {
		iters++
		if iters > 1_000_000 {
			for i, n := range c.nodes {
				c.t.Logf("node %d down %v deadline %d now %d", i, c.down[i], n.NextDeadline(), c.now)
			}

			c.t.Fatalf("run loop stuck: %d events", len(c.events))
		}

		next := end

		for i, n := range c.nodes {
			if !c.down[i] {
				next = min(next, max(n.NextDeadline(), c.now))
			}
		}

		for _, e := range c.events {
			next = min(next, e.at)
		}

		if next >= end {
			c.now = end

			return
		}

		c.now = next

		// Events first, in order.
		slices.SortFunc(c.events, func(a, b event) int {
			if a.at != b.at {
				return int(a.at - b.at)
			}

			return int(a.seq - b.seq)
		})

		var due []event

		for len(c.events) > 0 && c.events[0].at <= c.now {
			due = append(due, c.events[0])
			c.events = c.events[1:]
		}

		for _, e := range due {
			e.fn()
		}

		for i, n := range c.nodes {
			if !c.down[i] && n.NextDeadline() <= c.now {
				n.Tick(c.now, c.wallOf(i))
				c.flush(i)
			}
		}

		c.check()
	}
}

func (c *cluster) check() {
	for i, n := range c.nodes {
		if c.down[i] {
			continue
		}

		if c.valid[n.View()] {
			continue
		}

		if err := view.Validate(n.View()); err != nil {
			c.t.Fatalf("node %d: %v", i, err)
		}

		c.valid[n.View()] = true
	}
}

func (c *cluster) leader() int {
	l := -1

	for i, n := range c.nodes {
		if ok, _ := n.Leading(); ok && !c.down[i] {
			if l >= 0 {
				_, t1 := c.nodes[l].Leading()
				_, t2 := n.Leading()
				c.t.Logf("two leading: %d (term %d) and %d (term %d)", l, t1, i, t2)
			}

			l = i
		}
	}

	return l
}

// converged reports whether every live node has the leader's view and the
// view's members are exactly the live nodes.
func (c *cluster) converged() error {
	l := c.leader()
	if l < 0 {
		return errors.New("no leader")
	}

	lv := c.nodes[l].View()
	live := 0

	for i, n := range c.nodes {
		if c.down[i] || n.Left() {
			continue
		}

		live++

		if n.View().Digest() != lv.Digest() {
			return fmt.Errorf("node %d at %v, leader %d at %v", i, n.View().Version(), l, lv.Version())
		}

		if n.Self() == view.NoSlot {
			return fmt.Errorf("node %d not a member", i)
		}
	}

	if lv.Len() != live {
		return fmt.Errorf("view has %d members, %d live", lv.Len(), live)
	}

	return nil
}

func (c *cluster) waitConverged(d time.Duration) {
	c.t.Helper()

	var err error

	for end := c.now + int64(d); c.now < end; {
		c.run(time.Second)

		if err = c.converged(); err == nil {
			return
		}
	}

	c.t.Fatalf("not converged: %v", err)
}

func grow(t *testing.T, n int) *cluster {
	t.Helper()

	c := newCluster(t)
	c.add(c.cfg(0, nil))
	c.run(5 * time.Second)

	if c.leader() != 0 {
		t.Fatalf("bootstrap: leader %d", c.leader())
	}

	for i := 1; i < n; i++ {
		c.add(c.cfg(i, []string{addr(i / 2)}))
	}

	c.waitConverged(2 * time.Minute)

	return c
}

func TestBootstrapAndJoin(t *testing.T) {
	c := grow(t, 12)
	v := c.nodes[0].View()

	if v.Len() != 12 || v.Term() != 1 {
		t.Fatalf("len %d term %d", v.Len(), v.Term())
	}

	c.run(2 * time.Minute)

	if err := c.converged(); err != nil {
		t.Fatal(err)
	}

	v = c.nodes[0].View()
	v.ForEachMember(func(_ view.Slot, m *view.Member) bool {
		if m.State != view.Active || m.Weight != 1000 {
			t.Errorf("%s %v %d", m.ID, m.State, m.Weight)
		}

		return true
	})

	// Every member tracks exactly its view neighbors.
	for _, n := range c.nodes {
		want := view.Neighbors(n.View(), n.Self(), nil)
		if got := n.Neighbors(nil); !slices.Equal(got, want) {
			t.Fatalf("neighbors %v want %v", got, want)
		}
	}
}

func TestJoinViaLeaseSeeds(t *testing.T) {
	c := grow(t, 4)
	c.run(time.Minute) // the leader publishes seeds in the Lease

	if len(c.lease.rec.Seeds) == 0 {
		t.Fatal("no seeds in the Lease")
	}

	c.add(c.cfg(4, nil))
	c.waitConverged(time.Minute)
}

func TestLeaderCrashFailover(t *testing.T) {
	c := grow(t, 10)
	c.run(2 * time.Minute) // past MinCandidateAge

	old := c.leader()
	_, oldTerm := c.nodes[old].Leading()
	c.down[old] = true

	var err error

	for end := c.now + int64(3*time.Minute); c.now < end; {
		c.run(time.Second)

		l := c.leader()
		if l >= 0 && l != old {
			if err = c.converged(); err == nil && c.nodes[l].View().Len() == 9 {
				break
			}
		}
	}

	l := c.leader()
	if l < 0 || l == old {
		t.Fatalf("no new leader")
	}

	if _, term := c.nodes[l].Leading(); term <= oldTerm {
		t.Fatalf("term %d not above %d", term, oldTerm)
	}

	if err := c.converged(); err != nil {
		t.Fatal(err)
	}

	if c.nodes[l].View().Len() != 9 {
		t.Fatalf("crashed leader not removed: %d members", c.nodes[l].View().Len())
	}
}

func TestMemberCrashRemoved(t *testing.T) {
	c := grow(t, 16)
	c.run(30 * time.Second)

	victim := 7
	c.down[victim] = true

	var err error

	for end := c.now + int64(2*time.Minute); c.now < end; {
		c.run(time.Second)

		if err = c.converged(); err == nil {
			break
		}
	}

	if err != nil {
		t.Fatal(err)
	}

	if _, ok := c.nodes[c.leader()].View().Lookup("n7"); ok {
		t.Fatal("victim still a member")
	}

	// Restart with a new incarnation: rejoins.
	c.add(c.cfg(victim, []string{addr(0)}))
	c.waitConverged(time.Minute)
}

func TestLeave(t *testing.T) {
	c := grow(t, 8)
	c.run(30 * time.Second)
	c.nodes[5].Leave(c.now)
	c.run(time.Minute)

	if !c.nodes[5].Left() {
		t.Fatal("member did not observe its removal")
	}

	if err := c.converged(); err != nil {
		t.Fatal(err)
	}

	if c.nodes[c.leader()].View().Len() != 7 {
		t.Fatal("not removed")
	}
}

func TestLeaderLeaves(t *testing.T) {
	c := grow(t, 6)
	c.run(2 * time.Minute)

	old := c.leader()
	c.nodes[old].Leave(c.now)

	for end := c.now + int64(3*time.Minute); c.now < end; {
		c.run(time.Second)

		if l := c.leader(); l >= 0 && l != old && c.nodes[old].Left() {
			break
		}
	}

	if l := c.leader(); l < 0 || l == old {
		t.Fatalf("leader %d", l)
	}

	if !c.nodes[old].Left() {
		t.Fatal("old leader not removed")
	}
}

func TestSnapshotCatchUp(t *testing.T) {
	c := grow(t, 6)
	c.run(30 * time.Second)

	// Partition a member away for longer than the delta log covers.
	victim := 3
	for i := range c.nodes {
		c.cut[[2]int{victim, i}] = true
	}

	snaps := 0
	c.nodes[victim].cfg.Hooks.OnCatchUp = func(k CatchUpKind) {
		if k == CatchUpSnapshot {
			snaps++
		}
	}

	lead := c.leader()
	for i := range 40 {
		c.nodes[lead].Invalidate(c.now, uint64(i))
		c.run(1100 * time.Millisecond)
	}

	clear(c.cut)
	c.run(30 * time.Second)

	if err := c.converged(); err != nil && !c.nodes[victim].Left() {
		t.Logf("after heal: %v", err)
	}

	if snaps == 0 && c.nodes[victim].Self() != view.NoSlot {
		t.Fatal("no snapshot catch-up")
	}

	c.waitConverged(2 * time.Minute)
}

func TestHandleBadInput(t *testing.T) {
	c := newCluster(t)
	c.add(c.cfg(0, nil))
	c.run(5 * time.Second)

	n := c.nodes[0]

	var m Message
	if err := Decode(n.Handle(c.now, "x", []byte{1, 2, 3}), &m); err != nil || m.Kind != KindNack {
		t.Fatalf("garbage: %v %v", err, m.Kind)
	}

	// A routed message whose inner bytes are garbage is rejected.
	var r Message

	r.Kind = KindRouted
	r.ToLeader = true
	r.TTL = 3
	r.Inner = []byte{9, 9}

	if err := Decode(n.Handle(c.now, "x", Append(nil, &r)), &m); err != nil || m.Kind != KindNack {
		t.Fatalf("bad inner: %v %v", err, m.Kind)
	}

	// A join with the wrong partition bits is ignored.
	var j Message

	j.Kind = KindJoin
	j.Member = MemberRef{ID: "bad", Incarnation: 1, Slot: view.NoSlot}
	j.Addr = "bad:1"
	j.PartitionBits = 12

	n.Handle(c.now, "x", Append(nil, &j))
	c.run(3 * time.Second)

	if _, ok := n.View().Lookup("bad"); ok {
		t.Fatal("admitted a member with the wrong partition bits")
	}

	// A catch-up from a member at our version is empty.
	var q Message

	q.Kind = KindCatchUpRequest
	q.Have, q.HaveDigest = n.View().Version(), n.View().Digest()

	if err := Decode(n.Handle(c.now, "x", Append(nil, &q)), &m); err != nil ||
		m.Kind != KindCatchUpResponse || len(m.Deltas) != 0 || len(m.Snapshot) != 0 {
		t.Fatalf("catch-up at head: %v", err)
	}
}

func TestDeterminism(t *testing.T) {
	digest := func() view.Digest {
		c := grow(t, 10)
		c.run(time.Minute)

		return c.nodes[0].View().Digest()
	}

	if a, b := digest(), digest(); a != b {
		t.Fatalf("runs differ: %v %v", a, b)
	}
}

func TestLargerCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("larger cluster")
	}

	c := grow(t, 50)
	c.run(time.Minute)

	if err := c.converged(); err != nil {
		t.Fatal(err)
	}

	if d := c.nodes[0].View().Shape().Dims; c.nodes[0].View().Shape().Cells() < 4 {
		t.Fatalf("shape dims %d cells %d", d, c.nodes[0].View().Shape().Cells())
	}

	// Bounded state everywhere.
	for i, n := range c.nodes {
		if n.PendingCalls() > 200 {
			t.Fatalf("node %d has %d pending calls", i, n.PendingCalls())
		}
	}
}

// TestInitialView starts a cluster from a view taken from another cluster:
// members already in it need no join, and a leader is elected over it.
func TestInitialView(t *testing.T) {
	src := grow(t, 6)
	src.run(2 * time.Minute)
	v := src.nodes[0].View()

	c := newCluster(t)

	for i := range 6 {
		cfg := c.cfg(i, nil)
		// Reuse the identities of the source cluster.
		cfg.Incarnation = src.nodes[i].cfg.Incarnation
		cfg.Initial = v
		c.add(cfg)

		if c.nodes[i].Self() == view.NoSlot {
			t.Fatalf("node %d not a member of its initial view", i)
		}
	}

	c.waitConverged(2 * time.Minute)

	if c.nodes[c.leader()].View().Term() <= v.Term() {
		t.Fatal("no new term over the initial view")
	}
}

// TestEpochCountsFromStart is a regression test: before the first
// heartbeat the epoch counted from mono 0, so a member whose monotonic
// clock read days of uptime started days of epochs ahead of the cluster
// and, since epochs never decrease, stamped values that stayed fresh far
// past their TTL elsewhere (found by the DST data-plane workload).
func TestEpochCountsFromStart(t *testing.T) {
	c := newCluster(t)
	cfg := c.cfg(0, []string{addr(9)})
	n := New(cfg, rand.New(rand.NewPCG(1, 2)))
	start := int64(10 * 24 * time.Hour)

	n.Start(start, c.wall())

	if got := n.Epoch().Current(); got != 0 {
		t.Fatalf("epoch at start %d, want 0", got)
	}

	n.Tick(start+31*sec, c.wall())

	if got := n.Epoch().Current(); got != 2 {
		t.Fatalf("epoch after 31s %d, want 2", got)
	}
}

// TestExpiredCallsDeterministic is a regression test: calls that expire
// together used to be failed in map iteration order, and failing a call
// sends (suspicions, broadcast retries, catch-ups), so the outbox order
// varied between identical runs (found by the DST run-twice check).
func TestExpiredCallsDeterministic(t *testing.T) {
	c := grow(t, 12)
	v := c.nodes[0].View()
	cfg := c.cfg(3, nil)
	cfg.ID, cfg.Incarnation = c.nodes[3].cfg.ID, c.nodes[3].cfg.Incarnation
	cfg.Initial = v

	var first []string

	for run := range 20 {
		n := New(cfg, rand.New(rand.NewPCG(7, 7)))
		start := c.now

		n.Start(start, c.wall())

		// Never answer: every ping and catch-up stays pending.
		var out []string

		for step := int64(1); step <= 60; step++ {
			n.Tick(start+step*sec, c.wall())

			for _, call := range n.Outbox() {
				out = append(out, call.To+"/"+strconv.FormatUint(call.ID, 10)+"/"+string(call.Msg))
			}
		}

		if run == 0 {
			first = out

			continue
		}

		if !slices.Equal(out, first) {
			t.Fatalf("run %d outbox differs:\n%v\n%v", run, out, first)
		}
	}
}

// TestWholeMembershipReplaced is a regression test: when every member of
// the view restarted with a new incarnation before a leader could admit
// any of them, every process was a joiner with seeds, and joiners with
// seeds never entered the lottery, so no leader was ever elected (found by
// the DST data-plane workload once a fresh member could no longer
// bootstrap a second cluster next to the old one).
func TestWholeMembershipReplaced(t *testing.T) {
	c := grow(t, 4)
	old := c.nodes[0].View()

	// Restart every member at once, each holding the old view (as if it
	// had caught up from a peer that had not restarted yet).
	for i := range 4 {
		cfg := c.cfg(i, []string{addr((i + 1) % 4)})
		cfg.Initial = old
		c.add(cfg)
	}

	c.waitConverged(5 * time.Minute)

	v := c.nodes[c.leader()].View()
	if v.Term() <= old.Term() || v.Epoch() < old.Epoch() {
		t.Fatalf("new view %v epoch %d, old %v epoch %d", v.Version(), v.Epoch(), old.Version(), old.Epoch())
	}

	// The lineage continues: no second genesis.
	if _, ok := v.Config(); !ok || v.Version().Seq == 1 && v.Len() == 1 {
		t.Fatalf("lineage restarted: %v", v.Version())
	}
}

// TestPingEpochAntiEntropy is a regression test: pings carry the sender's
// epoch, but it was ignored, so a leader whose epoch lagged (a lottery
// winner that anchored from a stale snapshot) never caught up and every
// member that anchored to its heartbeats lagged with it, stretching stamp
// lifetimes (found by the DST data-plane workload).
func TestPingEpochAntiEntropy(t *testing.T) {
	c := newCluster(t)
	a := New(c.cfg(0, nil), rand.New(rand.NewPCG(1, 1)))
	b := New(c.cfg(1, nil), rand.New(rand.NewPCG(2, 2)))
	a.Start(c.now, c.wall())
	b.Start(c.now, c.wall())

	b.ep.Anchor(10, 15*time.Second, c.now)

	var m Message

	b.header(&m, KindPing)
	a.Handle(c.now+sec, b.cfg.Addr, Append(nil, &m))

	if got := a.Epoch().Current(); got != 10 {
		t.Fatalf("epoch after ping from a peer at 10: %d", got)
	}

	// A peer behind does not pull it back or move its phase.
	a.header(&m, KindPing)
	m.Epoch = 3
	a.Handle(c.now+2*sec, b.cfg.Addr, Append(nil, &m))

	if got := a.Epoch().Advance(c.now + 16*sec); got != 11 {
		t.Fatalf("epoch 15s after the anchor: %d, want 11", got)
	}
}

// TestLeavingNonMemberDoesNotJoin is a regression test: a member removed
// from the view (for example during a partition) that was then asked to
// leave kept sending joins, and a join that reached the leader after the
// process had stopped re-admitted it as a ghost Active member (found by the
// DST data-plane workload's route check).
func TestLeavingNonMemberDoesNotJoin(t *testing.T) {
	c := newCluster(t)
	n := New(c.cfg(1, []string{addr(0)}), rand.New(rand.NewPCG(3, 3)))
	n.Start(c.now, c.wall())

	if !hasJoin(t, n.Outbox()) {
		t.Fatal("a joiner sends a join at start")
	}

	n.joined = true // it was a member of an earlier view, then removed
	n.Leave(c.now + sec)

	for step := int64(2); step < 30; step++ {
		n.Tick(c.now+step*sec, c.wall())

		if hasJoin(t, n.Outbox()) {
			t.Fatalf("join sent %ds after Leave", step-1)
		}
	}
}

// TestLeaveAfterRemovalCompletes is a regression test: a member removed
// from its view and then asked to leave no longer joins, so no later view
// could show its removal, and Left stayed false forever (the process never
// finished leaving and fell into the Lease lottery; found by the DST
// sweep). It has already left. A never-joined process does not.
func TestLeaveAfterRemovalCompletes(t *testing.T) {
	c := newCluster(t)
	n := New(c.cfg(1, []string{addr(0)}), rand.New(rand.NewPCG(3, 3)))
	n.Start(c.now, c.wall())

	n.joined = true // it was a member of an earlier view, then removed
	n.Leave(c.now + sec)

	if !n.Left() {
		t.Fatal("a removed member asked to leave has not left")
	}

	fresh := New(c.cfg(2, []string{addr(0)}), rand.New(rand.NewPCG(4, 4)))
	fresh.Start(c.now, c.wall())
	fresh.Leave(c.now + sec)

	if fresh.Left() {
		t.Fatal("a never-joined process left without joining")
	}
}

// TestLeavingNeverJoinedStillJoins checks that a process that is asked to
// leave before it ever joined keeps joining: its admission replaces a stale
// record of its earlier incarnation, which nothing else removes, and the
// leave then drains it.
func TestLeavingNeverJoinedStillJoins(t *testing.T) {
	c := newCluster(t)
	n := New(c.cfg(1, []string{addr(0)}), rand.New(rand.NewPCG(3, 3)))
	n.Start(c.now, c.wall())
	n.Outbox()
	n.Leave(c.now + sec)

	joined := false

	for step := int64(2); step < 30 && !joined; step++ {
		n.Tick(c.now+step*sec, c.wall())
		joined = hasJoin(t, n.Outbox())
	}

	if !joined {
		t.Fatal("a never-joined leaving process stopped joining")
	}
}

func hasJoin(t *testing.T, calls []Call) bool {
	t.Helper()

	for _, call := range calls {
		var m Message
		if err := Decode(call.Msg, &m); err != nil {
			t.Fatal(err)
		}

		if m.Kind == KindJoin {
			return true
		}

		if m.Kind == KindRouted && len(m.Inner) > 0 {
			if k, err := PeekKind(m.Inner); err == nil && k == KindJoin {
				return true
			}
		}
	}

	return false
}

func TestSeedsAccessor(t *testing.T) {
	n := New(Config{ID: "a", Incarnation: 1, Addr: "a:1", Seeds: []string{"s:1"}}, rand.New(rand.NewPCG(1, 2)))
	if got := n.Seeds(nil); !slices.Equal(got, []string{"s:1"}) {
		t.Fatalf("configured seeds = %v", got)
	}

	// A member without configured seeds learns them from its Lease read.
	n = New(Config{ID: "a", Incarnation: 1, Addr: "a:1"}, rand.New(rand.NewPCG(1, 2)))
	n.Start(1, time.Now())

	ops := n.LeaseOps()
	if len(ops) == 0 {
		t.Fatal("no lease read")
	}

	n.OnLease(2, time.Now(), Result{Seq: ops[0].Seq, Kind: ops[0].Kind, Record: Record{
		Holder: "l", Addr: "l:1", Seeds: []string{"x:1", "a:1"},
	}})

	if got := n.Seeds([]string{"keep"}); !slices.Equal(got, []string{"keep", "l:1", "x:1"}) {
		t.Fatalf("lease seeds = %v", got)
	}
}

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
		var in, out Message
		if Decode(msg, &in) != nil {
			return nil
		}

		out.From = MemberRef{ID: "evil", Incarnation: 1, Slot: view.NoSlot}
		out.LeaderSlot = view.NoSlot

		if in.Kind != KindCatchUpRequest {
			out.Kind = KindAck

			return Append(nil, &out)
		}

		*served++
		snap, ver := forgeSnapshot(t, c.nodes[c.leader()].View(), bump)
		out.Kind = KindCatchUpResponse
		out.Version = ver
		out.Snapshot = snap

		return Append(nil, &out)
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

	var b Message

	b.Kind = KindBroadcast
	b.From = MemberRef{ID: c.nodes[l].cfg.ID, Incarnation: c.nodes[l].cfg.Incarnation, Slot: lv.Leader()}
	b.Version = lv.Version()
	b.Level = LevelLocal
	b.Delta = raw
	c.nodes[j].Handle(c.now, addr(l), Append(nil, &b))
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

	var q, m Message

	q.Kind = KindCatchUpRequest
	q.From = MemberRef{ID: "x", Slot: view.NoSlot}

	if err := Decode(n.Handle(c.now, "x", Append(nil, &q)), &m); err != nil || len(m.Snapshot) != 0 || len(m.Deltas) != 0 {
		t.Fatalf("served an unconfirmed view: %v snapshot %d deltas %d", err, len(m.Snapshot), len(m.Deltas))
	}
}

// soloStart starts a single member without seeds and drives its Lease
// operations against l until it has issued an acquire, which it returns
// unexecuted.
func soloStart(t *testing.T, l *memLease) (*Node, Op, int64) {
	t.Helper()

	now := 1000 * sec
	wall := func() time.Time { return time.Unix(0, now) }
	n := New(Config{ID: "solo", Incarnation: 7, Addr: "solo:1", PartitionBits: 8}, rand.New(rand.NewPCG(1, 2)))
	n.Start(now, wall())

	for range 50 {
		for _, op := range n.LeaseOps() {
			if op.Kind == OpCreate || op.Kind == OpUpdate {
				return n, op, now
			}

			n.OnLease(now, wall(), l.do(op, wall()))
		}

		now += sec
		n.Tick(now, wall())
	}

	t.Fatal("no acquire issued")

	return nil, Op{}, 0
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

	if n.Elector().State() == StateLeader {
		t.Fatal("elector still leader")
	}

	released := false

	for _, op := range n.LeaseOps() {
		if op.Kind == OpUpdate && op.Record.Holder == "" {
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

	c.onLeaseOp = func(i int, op Op) {
		if leaver < 0 && i != old && op.Record.Holder != "" && (op.Kind == OpUpdate || op.Kind == OpCreate) {
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

	var p Message

	p.Kind = KindPing
	p.From = MemberRef{ID: "n1", Incarnation: 1, Slot: view.NoSlot}
	p.Version = view.Version{Term: high, Seq: 1}
	p.LeaderSlot = view.NoSlot
	c.nodes[l].Handle(c.now, "nowhere:1", Append(nil, &p))

	if ok, _ := c.nodes[l].Leading(); ok {
		t.Fatal("still leading after a higher term")
	}

	if c.nodes[l].Elector().State() == StateLeader {
		t.Fatal("elector still leader")
	}

	c.flush(l)
	c.waitConverged(3 * time.Minute)

	if _, nt := c.nodes[c.leader()].Leading(); nt <= high {
		t.Fatalf("new term %d not above observed %d", nt, high)
	}
}

// TestAbsurdTermIgnored: a term at or above MaxSaneTerm is garbage.
func TestAbsurdTermIgnored(t *testing.T) {
	c := grow(t, 3)
	l := c.leader()

	var p Message

	p.Kind = KindPing
	p.From = MemberRef{ID: "x", Slot: view.NoSlot}
	p.Version = view.Version{Term: ^uint64(0), Seq: 1}
	p.LeaderSlot = view.NoSlot
	c.nodes[l].Handle(c.now, "x", Append(nil, &p))

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

	var j, r Message

	j.Kind = KindJoin
	j.Member = MemberRef{ID: "x", Incarnation: 1, Slot: view.NoSlot}
	r.Kind = KindRouted
	r.ToLeader = true
	r.TTL = 4
	r.Inner = Append(nil, &j)
	msg := Append(nil, &r)

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

	var b Message

	b.Kind = KindBroadcast
	b.From = MemberRef{ID: "p", Slot: view.NoSlot}
	b.Delta = []byte{1}

	for i := range 1000 {
		n.Handle(sec, "p"+strconv.Itoa(i), Append(nil, &b))
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

// TestColdRestartStaleView: every member died and fresh processes start
// from the last view (for example a persisted one), which none of them is
// in, so none is a candidate. They must still elect a leader and rejoin.
func TestColdRestartStaleView(t *testing.T) {
	c := grow(t, 5)
	c.run(2 * time.Minute)

	v := c.nodes[c.leader()].View()

	for i := range 5 {
		c.down[i] = true
	}

	c.run(time.Second)

	for i := range 5 {
		cfg := c.cfg(i, []string{addr((i + 1) % 5)})
		cfg.Initial = v
		c.add(cfg)
	}

	c.waitConverged(5 * time.Minute)
}

func TestExpandVersion(t *testing.T) {
	own := view.Version{Term: 5<<32 | 10, Seq: 3}
	for _, tc := range []struct {
		term uint32
		want uint64
	}{
		{10, 5<<32 | 10},
		{11, 5<<32 | 11},
		{9, 6<<32 | 9},
	} {
		if got := ExpandVersion(own, route.Note{Term: tc.term, Seq: 1}); got.Term != tc.want || got.Seq != 1 {
			t.Errorf("term %d: got %+v", tc.term, got)
		}
	}
}

// TestRouterRefresh checks that a Router rebuilds its State only when the
// view or the gateway seeds change, and that gateways exclude this member.
func TestRouterRefresh(t *testing.T) {
	n := New(Config{ID: "a", Addr: "a:1", Seeds: []string{"a:1", "b:1"}}, rand.New(rand.NewPCG(1, 1)))
	r := NewRouter(n)

	st := r.State()
	if st == nil || r.Refresh() || r.State() != st {
		t.Fatal("Refresh rebuilt an unchanged state")
	}

	if d := st.RouteHash(1); d.Kind != route.KindGateway || d.Next.Addr != "b:1" {
		t.Fatalf("decision %+v, want gateway b:1", d)
	}

	n.addSeed("c:1")

	if !r.Refresh() || r.State() == st {
		t.Fatal("Refresh kept the state after the seeds changed")
	}

	if r.TakeNote(0) {
		t.Fatal("TakeNote reported a note that was never recorded")
	}
}

// TestCatchUpTargetsAdvertisedAddr is a regression test: an HTTP transport
// passes RemoteAddr, which carries the sender's ephemeral source port, as
// the transport-level source. Catch-up after an ahead ping, a missed
// broadcast, or a higher term must dial the sender's advertised address
// carried in the message, never the transport-level source.
func TestCatchUpTargetsAdvertisedAddr(t *testing.T) {
	const ephemeral = "10.0.0.9:54321"

	for _, tc := range []struct {
		name string
		kind Kind
		ver  view.Version
	}{
		{"ahead-ping", KindPing, view.Version{Term: 0, Seq: 7}},
		{"higher-term-ping", KindPing, view.Version{Term: 5, Seq: 1}},
		{"higher-term-ack", KindAck, view.Version{Term: 5, Seq: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t)
			a := New(c.cfg(0, nil), rand.New(rand.NewPCG(1, 1)))
			a.Start(c.now, c.wall())
			a.Outbox()

			var m Message

			m.Kind = tc.kind
			m.From = MemberRef{ID: "n1", Incarnation: 1, Slot: view.NoSlot}
			m.FromAddr = addr(1)
			m.Version = tc.ver
			m.LeaderSlot = view.NoSlot
			a.Handle(c.now+sec, ephemeral, Append(nil, &m))

			var found bool

			for _, call := range a.Outbox() {
				if call.To == ephemeral {
					t.Fatalf("dialed transport source %q", call.To)
				}

				if k, err := PeekKind(call.Msg); err == nil && k == KindCatchUpRequest {
					if call.To != addr(1) {
						t.Fatalf("catch-up to %q, want %q", call.To, addr(1))
					}

					found = true
				}
			}

			if !found {
				t.Fatal("no catch-up request sent")
			}
		})
	}
}

// TestFromAddrRoundTrip checks the envelope carries the sender's
// advertised address and that header fills it from Config.Addr.
func TestFromAddrRoundTrip(t *testing.T) {
	c := newCluster(t)
	a := New(c.cfg(0, nil), rand.New(rand.NewPCG(1, 1)))

	var m, got Message

	a.header(&m, KindPing)

	if m.FromAddr != addr(0) {
		t.Fatalf("header FromAddr %q, want %q", m.FromAddr, addr(0))
	}

	if err := Decode(Append(nil, &m), &got); err != nil || got.FromAddr != addr(0) {
		t.Fatalf("round trip: %q, %v", got.FromAddr, err)
	}

	m.FromAddr = strings.Repeat("x", MaxAddrLen+1)
	if err := m.Validate(); err == nil {
		t.Fatal("oversized FromAddr accepted")
	}
}
