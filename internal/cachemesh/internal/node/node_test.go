// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/elect"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/wire"
)

const sec = int64(time.Second)

var errDown = errors.New("down")

// memLease is a minimal in-memory Lease store.
type memLease struct {
	rec    elect.Record
	exists bool
	rv     int
}

func (l *memLease) do(op elect.Op, wall time.Time) elect.Result {
	res := elect.Result{Seq: op.Seq, Kind: op.Kind}

	switch op.Kind {
	case elect.OpGet, elect.OpGetCached:
		if !l.exists {
			res.Err = elect.ErrNotFound
		} else {
			res.Record = l.rec.Clone()
		}
	case elect.OpCreate:
		if l.exists {
			res.Err = elect.ErrConflict

			return res
		}

		l.exists = true
		l.rv++
		l.rec = op.Record.Clone()
		l.rec.ResourceVersion = strconv.Itoa(l.rv)
		res.Record = l.rec.Clone()
	case elect.OpUpdate:
		switch {
		case !l.exists:
			res.Err = elect.ErrNotFound
		case op.Record.ResourceVersion != l.rec.ResourceVersion:
			res.Err = elect.ErrConflict
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
	onLeaseOp func(i int, op elect.Op)
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
		Elect:        elect.Config{ExpectedClusterSize: 10, MinCandidateAge: 20 * time.Second},
		DeltaLogSize: 32,
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

	var m wire.Message
	if err := wire.Decode(n.Handle(c.now, "x", []byte{1, 2, 3}), &m); err != nil || m.Kind != wire.KindNack {
		t.Fatalf("garbage: %v %v", err, m.Kind)
	}

	// A routed message whose inner bytes are garbage is rejected.
	var r wire.Message

	r.Kind = wire.KindRouted
	r.ToLeader = true
	r.TTL = 3
	r.Inner = []byte{9, 9}

	if err := wire.Decode(n.Handle(c.now, "x", wire.Append(nil, &r)), &m); err != nil || m.Kind != wire.KindNack {
		t.Fatalf("bad inner: %v %v", err, m.Kind)
	}

	// A join with the wrong partition bits is ignored.
	var j wire.Message

	j.Kind = wire.KindJoin
	j.Member = wire.MemberRef{ID: "bad", Incarnation: 1, Slot: view.NoSlot}
	j.Addr = "bad:1"
	j.PartitionBits = 12

	n.Handle(c.now, "x", wire.Append(nil, &j))
	c.run(3 * time.Second)

	if _, ok := n.View().Lookup("bad"); ok {
		t.Fatal("admitted a member with the wrong partition bits")
	}

	// A catch-up from a member at our version is empty.
	var q wire.Message

	q.Kind = wire.KindCatchUpRequest
	q.Have, q.HaveDigest = n.View().Version(), n.View().Digest()

	if err := wire.Decode(n.Handle(c.now, "x", wire.Append(nil, &q)), &m); err != nil ||
		m.Kind != wire.KindCatchUpResponse || len(m.Deltas) != 0 || len(m.Snapshot) != 0 {
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
