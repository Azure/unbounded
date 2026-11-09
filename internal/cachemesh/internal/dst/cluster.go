// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/elect"
	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
	"github.com/Azure/unbounded/internal/cachemesh/internal/leader"
	"github.com/Azure/unbounded/internal/cachemesh/internal/node"
	"github.com/Azure/unbounded/internal/cachemesh/internal/placement"
	"github.com/Azure/unbounded/internal/cachemesh/internal/topology"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/wire"
)

// Workload is the data-plane hook of a Cluster. Attach is called once when
// the cluster is built, Start after the warmup, and Check at the end of the
// run after the control-plane liveness checks passed. A workload drives
// requests through the members' nodes (Cluster.Node) on the cluster's
// simulator and reports violations with Cluster.Fail.
type Workload interface {
	Attach(c *Cluster)
	Start(c *Cluster)
	Check(c *Cluster) error
}

// ClusterConfig configures a control-plane simulation.
type ClusterConfig struct {
	Scenario Scenario
	// Prebuilt starts the initial members from a view built by running the
	// leader logic offline, instead of joining them one by one through a
	// bootstrap member. Use it for large clusters.
	Prebuilt bool
	// Node, if set, adjusts each member's node configuration.
	Node func(member uint32, cfg *node.Config)
	// Workload is the optional data-plane workload.
	Workload Workload
	// Coverage collects reached states; nil allocates one.
	Coverage *Coverage
}

// Cluster runs simulated cachemesh members, each a node.Node with its own
// clock, on a Sim, Net, LeaseServer, and Processes.
type Cluster struct {
	Sim   *Sim
	Net   *Net
	Lease *LeaseServer
	Procs *Processes
	Cov   *Coverage
	Trace *Trace
	// Deltas counts view deltas applied by members.
	Deltas uint64

	cfg     ClusterConfig
	sc      Scenario
	shared  *node.Shared
	members []*member
	rng     *rand.Rand
	initial *view.View

	// Invariant state.
	err       error
	terms     map[uint64]string // term -> publisher identity
	versions  map[view.Version]view.Digest
	validated map[*view.View]bool
	faultEnd  int64
	ctl       controlState

	cov covHandles
}

type covHandles struct {
	viewDeltas, viewSnap, viewEmpty CoverageHandle
	msg                             [32]CoverageHandle
	named                           map[string]CoverageHandle
}

// nodeEvent records a node event without allocating on repeat hits.
func (c *Cluster) nodeEvent(name string) {
	if h, ok := c.cov.named[name]; ok {
		h.Hit()

		return
	}

	h := c.Cov.Handle("node-" + name)
	c.cov.named[name] = h
	h.Hit()
}

type member struct {
	c       *Cluster
	idx     uint32
	inc     uint64
	addr    string
	n       *node.Node
	clock   *Clock
	tick    Handle
	tickAt  int64
	dead    bool
	paused  bool
	last    view.Version
	lastEp  uint64
	leaving bool
}

// Fire runs a scheduled tick.
func (m *member) Fire(_, _ uint64) {
	m.tick = Handle{}

	if m.dead || m.paused {
		return
	}

	now := m.c.Sim.Now()
	m.n.Tick(m.clock.Mono(now), m.clock.Wall(now))
	m.after()
}

func memberAddr(i uint32) string { return "m" + strconv.FormatUint(uint64(i), 10) }

func memberID(i uint32) string { return "member-" + strconv.FormatUint(uint64(i), 10) }

// member returns the index of the member at address a, or false if a is not
// the address of a member of c.
func (c *Cluster) member(a string) (uint32, bool) {
	if len(a) < 2 || a[0] != 'm' {
		return 0, false
	}

	v, err := strconv.Atoi(a[1:])
	if err != nil || v < 0 || v >= len(c.members) {
		return 0, false
	}

	return uint32(v), true
}

// NewCluster builds a cluster for cfg.Scenario on a fresh simulator.
func NewCluster(cfg ClusterConfig) *Cluster {
	sc := cfg.Scenario
	sim := New(sc.Seed)

	c := &Cluster{
		Sim: sim, cfg: cfg, sc: sc,
		Trace:     NewTrace(),
		shared:    node.NewShared(4096),
		rng:       sim.Stream("cluster"),
		terms:     map[uint64]string{},
		versions:  map[view.Version]view.Digest{},
		validated: map[*view.View]bool{},
	}

	c.Cov = cfg.Coverage
	if c.Cov == nil {
		c.Cov = NewCoverage()
	}

	c.cov = covHandles{
		viewDeltas: c.Cov.Handle("catchup-deltas"),
		viewSnap:   c.Cov.Handle("catchup-snapshot"),
		viewEmpty:  c.Cov.Handle("catchup-empty"),
		named:      map[string]CoverageHandle{},
	}

	for k := range c.cov.msg {
		c.cov.msg[k] = c.Cov.Handle("msg-" + wire.Kind(k).String())
	}

	c.Net = NewNet(sim, sc.Capacity, c, NetConfig{Latency: 2_000_000})
	c.Lease = NewLeaseServer(sim, LeaseConfig{})
	c.Procs = NewProcesses(sim, sc.Capacity, c)
	c.members = make([]*member, sc.Capacity)
	c.ctl.init(sc.Capacity)

	if cfg.Prebuilt {
		c.initial = c.prebuild(sc.Members)
	}

	for i := range uint32(sc.Members) {
		// Stagger starts over the first seconds so the cluster does not
		// start in lockstep.
		sim.At(int64(i)*int64(time.Millisecond)%(5*sec), func() { c.Procs.Start(i) })
	}

	for _, f := range sc.Faults {
		sim.At(f.Start, func() { c.apply(f) })

		if f.Kind != Checkpoint {
			c.faultEnd = max(c.faultEnd, f.End())
		}
	}

	if cfg.Workload != nil {
		cfg.Workload.Attach(c)
		sim.At(sc.Warmup, func() { cfg.Workload.Start(c) })
	}

	return c
}

// Err returns the first invariant violation, if any.
func (c *Cluster) Err() error { return c.err }

// Fail records an invariant violation; the first one wins.
func (c *Cluster) Fail(err error) {
	if c.err == nil {
		c.err = fmt.Errorf("t=%.3fs: %w", float64(c.Sim.Now())/1e9, err)
	}
}

// Node returns member i's node, or nil if it is not running.
func (c *Cluster) Node(i uint32) *node.Node {
	if m := c.members[i]; m != nil && !m.dead {
		return m.n
	}

	return nil
}

// Clock returns member i's clock, or nil if it never started.
func (c *Cluster) Clock(i uint32) *Clock {
	if m := c.members[i]; m != nil {
		return m.clock
	}

	return nil
}

// Members returns the capacity (number of member indices).
func (c *Cluster) Members() int { return len(c.members) }

// Run runs the scenario, checking safety invariants continuously, and then
// checks liveness: from the end of the last fault plus a minimum settle
// period it polls every 10 simulated seconds until the cluster has
// converged, and fails if it has not by the scenario's end plus the time
// the rebalancer needs to move every partition once (2^PartitionBits /
// MaxMovesPerTick ticks). It returns the first violation.
func (c *Cluster) Run() error {
	start := max(c.faultEnd, c.sc.Warmup) + minSettle
	deadline := max(c.sc.Duration, start) + c.rebalanceBound()

	for c.err == nil {
		now := c.Sim.Now()
		if now >= start {
			err := c.liveness()
			if err == nil {
				break
			}

			if now >= deadline {
				c.Fail(err)

				break
			}
		}

		c.Sim.RunUntil(now + 10*sec)
		c.checkPeriodic()
	}

	if c.err == nil {
		c.Cov.Hit("converged")
		c.checkSane()

		// Terms must not churn once faults healed (skipped for large
		// clusters to bound the suite's runtime).
		if c.sc.Members <= quietRunMembers {
			c.quietRun(quietRunFor)
		}
	}

	if c.err == nil && c.cfg.Workload != nil {
		if err := c.cfg.Workload.Check(c); err != nil {
			c.Fail(err)
		}
	}

	return c.err
}

// quietRunMembers and quietRunFor configure the post-convergence check.
const (
	quietRunMembers = 512
	quietRunFor     = 60 * sec
)

// minSettle is the least time after the last fault before liveness is
// checked: enough for failure detection, removal, and a leader election.
const minSettle = 60 * sec

func (c *Cluster) rebalanceBound() int64 {
	return int64(1)<<c.sc.PartitionBits/int64(leader.DefaultMaxMoves)*sec + 60*sec
}

// --- Prebuilt views ---

// prebuild returns a settled view of members 0..n-1 (incarnations as
// assigned by Processes.Start are unknown here, so prebuilt members use
// incarnation 1 on their first start).
func (c *Cluster) prebuild(n int) *view.View {
	self := leader.Member{ID: memberID(0), Incarnation: 1, Addr: memberAddr(0)}
	l := leader.New(c.leaderConfig(self, n), nil)
	v := view.Genesis()
	now := int64(0)
	apply := func(ops []view.Op) {
		if ops == nil {
			return
		}

		nv, err := view.Apply(v, view.NewDelta(v, ops...))
		if err != nil {
			panic("dst: prebuild: " + err.Error())
		}

		v = nv
	}

	apply(l.Tick(now, 0, v, 1))

	for i := uint32(1); i < uint32(n); i++ {
		l.Join(now, leader.Member{ID: memberID(i), Incarnation: 1, Addr: memberAddr(i), Zone: "z"})
	}

	period := int64(leader.DefaultEpochPeriod)

	for step := 0; step < 600; step++ {
		now += 5 * sec
		ops := l.Tick(now, uint64(now/period), v, 0)
		apply(ops)

		if ops == nil && step > 40 {
			break
		}
	}

	return v
}

func (c *Cluster) leaderConfig(self leader.Member, n int) leader.Config {
	return leader.Config{
		Self: self, PartitionBits: c.sc.PartitionBits, CellSize: c.sc.CellSize,
		MaxNeighbors: c.sc.MaxNeighbors, MaxJoinsPerTick: max(leader.DefaultMaxJoinsPerTick, n),
		MaxPending: max(leader.DefaultMaxPending, n),
	}
}

// --- Processes ---

// OnStart implements Hooks.
func (c *Cluster) OnStart(i uint32, inc uint64) {
	now := c.Sim.Now()

	clock := NewClock(int64(1+c.rng.IntN(1000)) * sec)
	if old := c.members[i]; old != nil {
		clock = old.clock
	}

	m := &member{c: c, idx: i, inc: inc, addr: memberAddr(i), clock: clock}

	if c.initial != nil && c.Procs.Get(i).Starts == 1 && int(i) < c.sc.Members {
		m.inc = 1
	}

	cfg := node.Config{
		ID: memberID(i), Incarnation: m.inc, Addr: m.addr, Zone: "z" + strconv.Itoa(int(i%3)),
		PartitionBits: c.sc.PartitionBits,
		Elect:         elect.Config{ExpectedClusterSize: c.sc.Members},
		Leader:        c.leaderConfig(leader.Member{}, 0),
		Shared:        c.shared,
		Hooks:         c.hooks(m),
	}
	cfg.Leader.MaxJoinsPerTick, cfg.Leader.MaxPending = 0, 0
	cfg.DeltaLogSize = c.sc.DeltaLogSize

	if c.initial != nil && m.inc == 1 {
		cfg.Initial = c.initial
	}

	// Seeds: member 0 bootstraps from the Lease; most others get two random
	// lower-indexed members, some rely on the Lease.
	if i > 0 && (c.initial == nil || m.inc != 1) {
		if c.rng.IntN(8) != 0 {
			for range 2 {
				cfg.Seeds = append(cfg.Seeds, memberAddr(c.rng.Uint32N(max(1, min(i, uint32(c.sc.Members))))))
			}
		}
	}

	c.onStartControl(i, &cfg)

	if c.cfg.Node != nil {
		c.cfg.Node(i, &cfg)
	}

	m.n = node.New(cfg, c.Sim.Stream("node/"+strconv.FormatUint(uint64(i), 10)+"/"+strconv.FormatUint(inc, 10)))
	c.members[i] = m
	c.Net.Resume(i)
	c.Trace.Add(1, uint64(i), inc, 0)

	m.n.Start(clock.Mono(now), clock.Wall(now))
	c.Procs.MarkUp(i)
	m.after()
}

// OnCrash implements Hooks.
func (c *Cluster) OnCrash(i uint32) {
	if m := c.members[i]; m != nil {
		m.dead = true
		c.Sim.Cancel(m.tick)
		c.Net.DropHeld(i)
		c.Trace.Add(2, uint64(i), m.inc, 0)
	}
}

// OnLeave implements Hooks.
func (c *Cluster) OnLeave(i uint32) {
	if m := c.members[i]; m != nil && !m.dead {
		m.leaving = true
		m.n.Leave(m.clock.Mono(c.Sim.Now()))
		m.after()
	}
}

// OnPause implements Hooks.
func (c *Cluster) OnPause(i uint32) {
	if m := c.members[i]; m != nil {
		m.paused = true

		c.Net.Pause(i)
	}
}

// OnResume implements Hooks.
func (c *Cluster) OnResume(i uint32) {
	if m := c.members[i]; m != nil && !m.dead {
		m.paused = false

		c.Net.Resume(i)
		m.after()
	}
}

// --- Messaging ---

// after flushes a node's outputs and reschedules its tick.
func (m *member) after() {
	if m.dead {
		return
	}

	c := m.c
	now := c.Sim.Now()

	for _, call := range m.n.Outbox() {
		to, ok := c.member(call.To)
		if !ok {
			m.complete(call.ID, nil, errors.New("bad address"))

			continue
		}

		id := call.ID
		c.Net.Call(Message{From: m.idx, To: to, Kind: peekKind(call.Msg), Payload: call.Msg}, int64(call.Timeout),
			CompleterFunc(func(_ uint64, resp *Message, err error) {
				if resp != nil {
					m.complete(id, resp.Payload, err)
				} else {
					m.complete(id, nil, err)
				}
			}))
	}

	for _, op := range m.n.LeaseOps() {
		c.leaseOp(m, op)
		c.Lease.Submit(m.idx, op, func(r elect.Result) { m.leaseResult(r) })
	}

	if m.paused {
		return
	}

	at := m.clock.SimTime(m.n.NextDeadline())
	at = max(at, now)

	if m.tick.Valid() && m.tickAt == at {
		return
	}

	c.Sim.Cancel(m.tick)
	m.tick = c.Sim.AtTarget(at, m, 0, 0)
	m.tickAt = at
}

func (m *member) complete(id uint64, resp []byte, err error) {
	if m.dead {
		return
	}

	if m.paused {
		// The process is frozen: deliver when it resumes.
		m.c.Sim.After(100_000_000, func() { m.complete(id, resp, err) })

		return
	}

	now := m.c.Sim.Now()
	m.n.OnReply(m.clock.Mono(now), id, resp, err)
	m.after()
}

func (m *member) leaseResult(r elect.Result) {
	if m.dead {
		return
	}

	if m.paused {
		m.c.Sim.After(100_000_000, func() { m.leaseResult(r) })

		return
	}

	now := m.c.Sim.Now()
	m.n.OnLease(m.clock.Mono(now), m.clock.Wall(now), r)
	m.after()
}

// peekKind returns the wire kind of an encoded message for traffic stats,
// 0 if it is malformed.
func peekKind(b []byte) uint16 {
	k, err := wire.PeekKind(b)
	if err != nil {
		return 0
	}

	return uint16(k)
}

// DataHandler is implemented by a Workload that receives its own messages
// (Kind KindData) over the cluster's Net.
type DataHandler interface {
	DeliverData(msg *Message)
}

// Deliver implements Handler.
func (c *Cluster) Deliver(msg *Message) {
	if msg.Kind == KindData {
		if dh, ok := c.cfg.Workload.(DataHandler); ok {
			dh.DeliverData(msg)
		}

		return
	}

	m := c.members[msg.To]
	if m == nil || m.dead {
		return // the caller times out
	}

	now := c.Sim.Now()
	resp := m.n.Handle(m.clock.Mono(now), memberAddr(msg.From), msg.Payload)
	c.Net.Reply(msg, Message{Kind: peekKind(resp), Payload: resp})
	m.after()
}

// --- Hooks and safety invariants ---

func (c *Cluster) hooks(m *member) node.Hooks {
	cov := c.Cov

	return node.Hooks{
		OnView: func(v *view.View) {
			c.onView(m, v)
		},
		OnPublish: func(d *view.Delta) {
			c.onPublish(m, d)
		},
		OnCatchUp: func(k node.CatchUpKind) {
			switch k {
			case node.CatchUpDeltas:
				c.cov.viewDeltas.Hit()
			case node.CatchUpSnapshot:
				c.cov.viewSnap.Hit()
			case node.CatchUpEmpty:
				c.cov.viewEmpty.Hit()
			}
		},
		OnMessage: func(k wire.Kind) { c.cov.msg[k%32].Hit() },
		OnEvent:   func(name string) { c.nodeEvent(name) },
		Leader: leader.Hooks{
			OnAdmit:     func(view.Slot, string) { cov.Hit("leader-admit") },
			OnRemove:    func(_ view.Slot, r leader.RemoveReason) { cov.Hit("leader-remove-" + strconv.Itoa(int(r))) },
			OnPromote:   func(_ view.Slot, s view.MemberState, _ uint32) { cov.Hit("leader-promote-" + s.String()) },
			OnDamped:    func(int, int) { cov.Hit("leader-damped") },
			OnReshape:   func(_, _ topology.Shape) { cov.Hit("leader-reshape") },
			OnCellMove:  func(int) { cov.Hit("leader-cell-move") },
			OnPlan:      func(mand, opt int) { c.planHit(mand, opt) },
			OnBootstrap: func() { cov.Hit("leader-bootstrap") },
			OnDrop:      func(q string) { cov.Hit("leader-drop-" + q) },
		},
		Elect: elect.Hooks{
			OnState: func(_, to elect.State) { cov.Hit("elect-" + to.String()) },
			OnOp:    func(k elect.OpKind) { cov.Hit("lease-op-" + k.String()) },
		},
	}
}

func (c *Cluster) planHit(mand, opt int) {
	if mand > 0 {
		c.Cov.Hit("leader-plan-mandatory")
	}

	if opt > 0 {
		c.Cov.Hit("leader-plan-optional")
	}
}

func (c *Cluster) identity(m *member) string {
	return memberID(m.idx) + "/" + strconv.FormatUint(m.inc, 10)
}

func (c *Cluster) onPublish(m *member, d *view.Delta) {
	id := c.identity(m)

	if p, ok := c.terms[d.Version.Term]; ok && p != id {
		c.Fail(fmt.Errorf("term %d published by %s and %s", d.Version.Term, p, id))
	}

	c.terms[d.Version.Term] = id
	c.notePublish(d)
	c.checkOverlap(m, d)

	if len(d.Ops) > 0 && d.Ops[0].Kind == view.KindNewTerm {
		c.Cov.Hit("new-term")
	}

	c.Trace.Add(3, d.Version.Term, d.Version.Seq, uint64(d.Digest[0])|uint64(d.Digest[1])<<8)
	c.Deltas++
}

func (c *Cluster) onView(m *member, v *view.View) {
	ver := v.Version()

	if old, ok := c.versions[ver]; ok && old != v.Digest() {
		c.Fail(fmt.Errorf("version %v has two digests %v and %v", ver, old, v.Digest()))
	} else if !ok {
		c.versions[ver] = v.Digest()
	}

	if ver.Less(m.last) {
		c.Fail(fmt.Errorf("member %d: version regressed %v -> %v", m.idx, m.last, ver))
	}

	m.last = ver

	if !c.validated[v] {
		if err := view.Validate(v); err != nil {
			c.Fail(fmt.Errorf("member %d: invalid view %v: %w", m.idx, ver, err))
		}

		if len(c.validated) > 1<<14 {
			clear(c.validated)
		}

		c.validated[v] = true
	}

	ep := m.n.Epoch().Current()
	if ep < m.lastEp {
		c.Fail(fmt.Errorf("member %d: epoch regressed %d -> %d", m.idx, m.lastEp, ep))
	}

	m.lastEp = ep
	c.Trace.Add(4, uint64(m.idx), ver.Term, ver.Seq)
}

// checkPeriodic runs every 10 simulated seconds.
func (c *Cluster) checkPeriodic() {
	if err := CheckAudit(c.Lease.Audit()); err != nil {
		c.Fail(err)
	}

	for _, m := range c.members {
		if m == nil || m.dead {
			continue
		}

		if m.leaving && m.n.Left() {
			c.Procs.Exited(m.idx)
			m.dead = true
			c.Sim.Cancel(m.tick)
			c.Cov.Hit("member-left")
		}

		if p := m.n.PendingCalls(); p > 4096 {
			c.Fail(fmt.Errorf("member %d: %d pending calls", m.idx, p))
		}

		if el := m.n.Elector().State(); el == elect.Leader {
			c.Cov.Hit("elect-leader-held")
		}
	}

	// Lease calls: per member and per second, within the Lease budgets
	// below; the steady-state budget applies in quiet periods (no
	// fault within the last 90s, after warmup).
	now := c.Sim.Now()
	quiet := now > max(c.sc.Warmup, startupQuiet) && !c.faultActive(now-30*sec, now)

	c.checkLease(now, quiet)
	c.checkBounded()
}

// startupQuiet is when the initial joins (one cached Lease read per member
// without seeds) are over.
const startupQuiet = 120 * sec

func (c *Cluster) faultActive(from, to int64) bool {
	for _, f := range c.sc.Faults {
		// Membership faults cause joins and elections for a while.
		if f.Kind != Checkpoint && f.Start <= to && f.End()+90*sec >= from {
			return true
		}
	}

	return false
}

// --- Liveness ---

// Leader returns the member index of the unique publishing leader.
func (c *Cluster) Leader() (uint32, bool) {
	var (
		best  uint32
		found bool
		term  uint64
	)

	for _, m := range c.members {
		if m == nil || m.dead {
			continue
		}

		if ok, t := m.n.Leading(); ok && t >= term {
			best, found, term = m.idx, true, t
		}
	}

	return best, found
}

// liveness returns nil once the cluster has converged.
func (c *Cluster) liveness() error {
	l, ok := c.Leader()
	if !ok {
		return errors.New("liveness: no leader")
	}

	lv := c.members[l].n.View()

	leaders := 0
	live := map[string]uint64{}

	for _, m := range c.members {
		if m == nil || m.dead || m.leaving {
			continue
		}

		if ok, _ := m.n.Leading(); ok {
			leaders++
		}

		live[memberID(m.idx)] = m.inc

		if m.n.View().Digest() != lv.Digest() {
			return fmt.Errorf("liveness: member %d at %v, leader %d at %v", m.idx, m.n.View().Version(), l, lv.Version())
		}
	}

	if leaders != 1 {
		return fmt.Errorf("liveness: %d leaders", leaders)
	}

	var (
		inView int
		err    error
	)

	lv.ForEachMember(func(_ view.Slot, mm *view.Member) bool {
		inc, ok := live[mm.ID]
		if !ok || inc != mm.Incarnation {
			// Leaving members may remain while draining.
			if !c.leavingID(mm.ID, mm.Incarnation) {
				err = fmt.Errorf("liveness: %s/%d in view but not live", mm.ID, mm.Incarnation)

				return false
			}
		} else {
			inView++
		}

		return true
	})

	if err != nil {
		return err
	}

	if inView != len(live) {
		return fmt.Errorf("liveness: %d of %d live members in view", inView, len(live))
	}

	pm := make([]placement.Member, 0, lv.Len())
	lv.ForEachMember(func(s view.Slot, mm *view.Member) bool {
		pm = append(pm, placement.Member{Slot: s, Key: mm.Key, Weight: mm.Weight, Cell: mm.Cell})

		return true
	})

	if ok, err := placement.NewPlanner(placement.Config{}).Settled(lv.Table(), pm); err != nil || !ok {
		return fmt.Errorf("liveness: placement not settled (%v)", err)
	}

	lo, hi := lv.Len(), 0
	for cell := range lv.Shape().Cells() {
		o := lv.Occupancy(cell)
		lo, hi = min(lo, o), max(hi, o)
	}

	if hi-lo > 1 {
		return fmt.Errorf("liveness: cell occupancy %d..%d", lo, hi)
	}

	// The epoch restarts at zero with a new cluster (after a cold restart).
	if want := uint64((c.Sim.Now()-c.ctl.genesisAt)/int64(leader.DefaultEpochPeriod)) / 2; lv.Epoch() < want {
		return fmt.Errorf("liveness: epoch %d below %d", lv.Epoch(), want)
	}

	return nil
}

func (c *Cluster) leavingID(id string, inc uint64) bool {
	for _, m := range c.members {
		if m != nil && m.leaving && memberID(m.idx) == id && m.inc == inc {
			return true
		}
	}

	return false
}

// --- Faults ---

// pick returns up to k distinct live members chosen with the fault seed.
func (c *Cluster) pick(f Fault, k int, notLeader bool) []uint32 {
	r := NewRand(f.Seed, "pick")
	live := c.Procs.InState(nil, Up)
	l, hasLeader := c.Leader()

	live = slices.DeleteFunc(live, func(i uint32) bool {
		return c.members[i] == nil || c.members[i].leaving || (notLeader && hasLeader && i == l)
	})

	r.Shuffle(len(live), func(a, b int) { live[a], live[b] = live[b], live[a] })

	// Never take down more than a third of the members at once.
	k = min(k, len(live)/3)

	return live[:max(0, k)]
}

func (c *Cluster) apply(f Fault) {
	c.Cov.Hit("fault-" + f.Kind.String())
	c.Trace.Add(5, uint64(f.Kind), uint64(f.Start), 0)

	sim := c.Sim

	switch f.Kind {
	case Crash:
		for _, i := range c.pick(f, f.Count, true) {
			c.Procs.Crash(i)
		}
	case Restart:
		for _, i := range c.pick(f, f.Count, false) {
			c.Procs.Crash(i)
			sim.After(f.Duration, func() { c.Procs.Start(i) })
		}
	case MassJoin:
		started := 0

		for i := range uint32(len(c.members)) {
			if started < f.Count && c.Procs.State(i) == Down && c.members[i] == nil {
				c.Procs.Start(i)

				started++
			}
		}
	case ScaleDown, GracefulDrain:
		for _, i := range c.pick(f, f.Count, false) {
			c.Procs.Leave(i)
		}
	case RollingRestart:
		// Restarts are Value apart and all happen within Duration.
		n := f.Count
		if f.Value > 0 {
			n = min(n, int(f.Duration/f.Value)+1)
		}

		for k, i := range c.pick(f, n, false) {
			sim.After(int64(k)*f.Value, func() {
				if c.Procs.Crash(i) {
					sim.After(sec, func() { c.Procs.Start(i) })
				}
			})
		}
	case NetworkPartition:
		r := NewRand(f.Seed, "partition")
		for i := range uint32(len(c.members)) {
			if r.Uint32N(1_000_000) < f.PPM {
				c.Net.SetGroup(i, 1+r.Uint32N(uint32(max(1, f.Groups-1))))
			}
		}

		sim.After(f.Duration, c.Net.Heal)
	case LeaderCrash:
		if l, ok := c.Leader(); ok {
			c.Procs.Crash(l)
			sim.After(f.Duration, func() { c.Procs.Start(l) })
		}
	case APIServerOutage:
		c.Lease.AddOutage(f.Start, f.End())
	case SlowMembers:
		for _, i := range c.pick(f, f.Count, false) {
			c.Net.SetSlow(i, uint32(f.Value))
			sim.After(f.Duration, func() { c.Net.SetSlow(i, 1000) })
		}
	case ClockSkew:
		for _, i := range c.pick(f, f.Count, false) {
			if cl := c.Clock(i); cl != nil {
				cl.SetOffset(sim.Now(), time.Duration(f.Value))
			}
		}
	case ClockDrift:
		for _, i := range c.pick(f, f.Count, false) {
			if cl := c.Clock(i); cl != nil {
				cl.SetDrift(sim.Now(), f.Value)
				sim.After(f.Duration, func() { cl.SetDrift(sim.Now(), 0) })
			}
		}
	case MessageLoss:
		c.Net.SetLoss(f.PPM)
		sim.After(f.Duration, func() { c.Net.SetLoss(0) })
	case MessageDuplication:
		c.Net.SetDup(f.PPM)
		sim.After(f.Duration, func() { c.Net.SetDup(0) })
	case KeyspaceInvalidation:
		if ps := c.pick(f, 1, false); len(ps) > 0 {
			m := c.members[ps[0]]
			m.n.Invalidate(m.clock.Mono(sim.Now()), hashing.Seed("meta/"+strconv.FormatUint(f.Seed%4, 10)))
			m.after()
		}
	case LeaseConflicts:
		c.Lease.SetConflictPPM(f.PPM)
		sim.After(f.Duration, func() { c.Lease.SetConflictPPM(0) })
	case Pause:
		for _, i := range c.pick(f, f.Count, false) {
			if c.Procs.Pause(i) {
				sim.After(f.Duration, func() { c.Procs.Resume(i) })
			}
		}
	default:
		c.applyControl(f)
	}
}

// Control-plane invariants beyond the per-event safety checks above:
// leader overlap, Lease call budgets per member and per second,
// term churn, and bounded node state.

// Lease budgets, from the elect defaults the simulated members use.
const (
	candidates    = elect.DefaultCandidates
	retryPeriod   = int64(elect.DefaultRetryPeriod)
	lotteryBudget = elect.DefaultLotteryBudgetQPS

	// quietPeakPerSecond bounds elector Lease calls in any one second of a
	// quiet period: each candidate and the leader call at most once per
	// RetryPeriod (2s, so at most once in any second), plus one for a
	// candidate set that changes within the window.
	quietPeakPerSecond = candidates + 1 + 1
	// quietPerWindow bounds elector calls in a 10s check window of a quiet
	// period: (Candidates+1) callers, each at most once per RetryPeriod,
	// plus one call each for the window edges.
	quietPerWindow = int(10*sec/retryPeriod)*(candidates+1) + candidates + 1
	// lossPeakPerSecond bounds elector Lease calls in any one second once a
	// cluster exists. During leader loss each lottery round admits about
	// Binomial(N, Budget/N) members (mean LotteryBudgetQPS, N at least
	// ExpectedClusterSize), each making one read and, if the Lease is
	// expired, one acquiring write at its next tick, so one second can hold
	// a round's reads and the previous round's writes: mean
	// 2*LotteryBudgetQPS, standard deviation about sqrt(2*Budget) (6.3).
	// 4*LotteryBudgetQPS is more than six standard deviations above the
	// mean; candidates and the leader add theirs. Before the first term
	// and for formation after it (a cluster forming) members that
	// find no Lease bootstrap as candidates, which this bound does not
	// cover.
	lossPeakPerSecond = 4*lotteryBudget + candidates + 2
	// formation is how long after the first term the members that found no
	// Lease at start (bootstrap candidates) take to discover the leader.
	formation = 30 * sec
	// ownReadGap is the least simulated time between two of a joiner's own
	// seed reads: one per 16 join attempts (32s) on a clock that may run
	// up to 10% fast, with margin.
	ownReadGap = 25 * sec
)

// controlState is the control-plane invariant state of a Cluster.
type controlState struct {
	noSeeds    []bool
	ownReadAt  []int64
	electOps   []uint32 // elector ops per member in the current window
	electState []elect.State
	perSec     []uint32 // elector ops per simulated second
	peak       uint32
	quietPeak  uint32

	termStarts []int64
	genesisAt  int64
	// coldRestartAt is when the last ColdRestart crashed every member.
	coldRestartAt int64
	overlaps      int // overlaps excused by clocks outside the safety bounds

	maxStats node.Stats
}

func (s *controlState) init(capacity int) {
	s.noSeeds = make([]bool, capacity)
	s.ownReadAt = make([]int64, capacity)
	s.electOps = make([]uint32, capacity)
	s.electState = make([]elect.State, capacity)

	for i := range s.ownReadAt {
		s.ownReadAt[i] = -1
	}
}

// ControlStats summarizes the control-plane measurements of a run.
func (c *Cluster) ControlStats() string {
	return fmt.Sprintf("terms=%d leasePeak=%d/s quietPeak=%d/s overlapsExcused=%d maxState=%+v",
		len(c.ctl.termStarts), c.ctl.peak, c.ctl.quietPeak, c.ctl.overlaps, c.ctl.maxStats)
}

// onStartControl adjusts a starting member's configuration.
func (c *Cluster) onStartControl(i uint32, cfg *node.Config) {
	if c.ctl.noSeeds[i] {
		c.ctl.noSeeds[i] = false
		cfg.Seeds = nil
	}

	c.ctl.ownReadAt[i] = -1
}

// leaseOp accounts one Lease operation as member m emits it.
func (c *Cluster) leaseOp(m *member, op elect.Op) {
	now := c.Sim.Now()

	if op.Seq>>63 == 1 {
		// The node's own seed read. This mirrors node.ownLeaseBit (the top
		// bit of the op sequence number), which is unexported; keep the two
		// in sync.
		if last := c.ctl.ownReadAt[m.idx]; last >= 0 && now-last < ownReadGap {
			c.Fail(fmt.Errorf("member %d: own Lease reads %.1fs apart", m.idx, float64(now-last)/1e9))
		}

		c.ctl.ownReadAt[m.idx] = now

		return
	}

	c.ctl.electOps[m.idx]++

	s := int(now / sec)
	for len(c.ctl.perSec) <= s {
		c.ctl.perSec = append(c.ctl.perSec, 0)
	}

	c.ctl.perSec[s]++
	c.ctl.peak = max(c.ctl.peak, c.ctl.perSec[s])

	if c.ctl.perSec[s] > lossPeakPerSecond && len(c.ctl.termStarts) > 0 && now > c.ctl.termStarts[0]+formation {
		c.Fail(fmt.Errorf("%d elector Lease calls in second %d, budget %d", c.ctl.perSec[s], s, lossPeakPerSecond))
	}
}

// checkLease runs every 10 simulated seconds over the window that ended.
func (c *Cluster) checkLease(now int64, quiet bool) {
	defer func() {
		clear(c.ctl.electOps)

		for i, m := range c.members {
			if m != nil && !m.dead {
				c.ctl.electState[i] = m.n.Elector().State()
			} else {
				c.ctl.electState[i] = elect.Follower
			}
		}
	}()

	if !quiet {
		return
	}

	total := 0

	for s := max(0, int((now-10*sec)/sec)); s < min(len(c.ctl.perSec), int(now/sec)); s++ {
		total += int(c.ctl.perSec[s])
		c.ctl.quietPeak = max(c.ctl.quietPeak, c.ctl.perSec[s])

		if c.ctl.perSec[s] > quietPeakPerSecond {
			c.Fail(fmt.Errorf("steady state: %d elector Lease calls in second %d, budget %d", c.ctl.perSec[s], s, quietPeakPerSecond))
		}
	}

	if total > quietPerWindow {
		c.Fail(fmt.Errorf("steady state: %d elector Lease calls in 10s, budget %d", total, quietPerWindow))
	}

	callers := 0

	for i, m := range c.members {
		ops := c.ctl.electOps[i]
		if ops == 0 {
			continue
		}

		callers++

		if m == nil || m.dead {
			continue
		}

		// Followers make no calls: a caller must have been a candidate or
		// the leader at one end of the window.
		was, is := c.ctl.electState[i], m.n.Elector().State()
		active := func(s elect.State) bool { return s == elect.Candidate || s == elect.Leader }

		if !active(was) && !active(is) {
			c.Fail(fmt.Errorf("steady state: member %d (elector %v -> %v) made %d Lease calls", i, was, is, ops))
		}
	}

	if callers > candidates+2 {
		c.Fail(fmt.Errorf("steady state: %d members made Lease calls, budget %d", callers, candidates+2))
	}
}

// clocksInBounds reports whether the live members' clocks are within what
// election safety assumes: wall clocks within ClockSkewAllowance of each
// other (the lottery compares wall time), and monotonic rates with
// rmax/rmin below LeaseDuration/RenewDeadline (candidates and leaders
// measure local durations).
func (c *Cluster) clocksInBounds() bool {
	now := c.Sim.Now()
	truth := Epoch.Add(time.Duration(now))

	var (
		lo, hi     int64
		dlo, dhi   int64
		first      = true
		skewBudget = int64(elect.DefaultClockSkewAllowance)
	)

	for _, m := range c.members {
		if m == nil || m.dead {
			continue
		}

		off := int64(m.clock.Wall(now).Sub(truth))
		d := m.clock.DriftPPB()

		if first {
			lo, hi, dlo, dhi, first = off, off, d, d, false

			continue
		}

		lo, hi, dlo, dhi = min(lo, off), max(hi, off), min(dlo, d), max(dhi, d)
	}

	if hi-lo > skewBudget {
		return false
	}

	// (1+dhi)/(1+dlo) < LeaseDuration/RenewDeadline.
	ld, rd := int64(elect.DefaultLeaseDuration/1e6), int64(elect.DefaultRenewDeadline/1e6)

	return (nsPerSec+dhi)*rd < (nsPerSec+dlo)*ld
}

// checkOverlap runs when m publishes d: no other member may lead another
// term with publishing allowed at this instant, unless clocks are outside
// the bounds the election's safety argument assumes.
func (c *Cluster) checkOverlap(m *member, d *view.Delta) {
	now := c.Sim.Now()

	for _, o := range c.members {
		if o == nil || o == m || o.dead || o.paused {
			continue
		}

		ok, t := o.n.Leading()
		if !ok || t == d.Version.Term || !o.n.Elector().CanPublish(o.clock.Mono(now)) {
			continue
		}

		if c.clocksInBounds() {
			c.Fail(fmt.Errorf("leaders overlap: member %d publishes term %d while member %d may publish term %d",
				m.idx, d.Version.Term, o.idx, t))

			return
		}

		c.ctl.overlaps++
		c.Cov.Hit("leader-overlap-excused")
	}
}

// notePublish records term starts and bootstraps.
func (c *Cluster) notePublish(d *view.Delta) {
	if len(d.Ops) == 0 || d.Ops[0].Kind != view.KindNewTerm {
		return
	}

	c.ctl.termStarts = append(c.ctl.termStarts, c.Sim.Now())

	for i := range d.Ops {
		if d.Ops[i].Kind == view.KindConfig {
			c.ctl.genesisAt = c.Sim.Now()
		}
	}
}

// checkBounded fails if any member's internal state exceeds its bound.
func (c *Cluster) checkBounded() {
	mx := &c.ctl.maxStats

	for _, m := range c.members {
		if m == nil || m.dead {
			continue
		}

		s := m.n.Stats()
		mx.Calls, mx.Outbox, mx.LeaseOps = max(mx.Calls, s.Calls), max(mx.Outbox, s.Outbox), max(mx.LeaseOps, s.LeaseOps)
		mx.Ahead, mx.Seeds, mx.LeaseSeeds, mx.Log = max(mx.Ahead, s.Ahead), max(mx.Seeds, s.Seeds), max(mx.LeaseSeeds, s.LeaseSeeds), max(mx.Log, s.Log)
		mx.Subjects, mx.Neighbors = max(mx.Subjects, s.Subjects), max(mx.Neighbors, s.Neighbors)
		mx.Joins, mx.Leaves, mx.Suspects, mx.Invals = max(mx.Joins, s.Joins), max(mx.Leaves, s.Leaves), max(mx.Suspects, s.Suspects), max(mx.Invals, s.Invals)

		var bad string

		switch {
		case s.Calls > node.MaxPendingCalls:
			bad = "pending calls"
		case s.Ahead > node.DefaultMaxAhead:
			bad = "queued deltas"
		case s.Seeds > 4*elect.MaxSeeds:
			bad = "seeds"
		case s.LeaseSeeds > elect.MaxSeeds:
			bad = "Lease seeds"
		case s.Log > view.DefaultDeltaLogSize:
			bad = "delta log"
		case s.LingerOverdue, s.Neighbors-s.Lingering > c.sc.Capacity:
			bad = "neighbors"
		case s.Joins > leader.DefaultMaxPending, s.Leaves > leader.DefaultMaxPending,
			s.Suspects > leader.DefaultMaxPending, s.Invals > leader.DefaultMaxPending:
			bad = "leader queues"
		}

		if bad != "" {
			c.Fail(fmt.Errorf("member %d: unbounded %s: %+v", m.idx, bad, s))
		}
	}
}

// applyControl applies the control-plane fault kinds.
func (c *Cluster) applyControl(f Fault) {
	sim := c.Sim

	switch f.Kind {
	case ColdRestart:
		c.ctl.coldRestartAt = sim.Now()

		for _, i := range c.Procs.Live(nil) {
			if c.Procs.Crash(i) {
				c.ctl.noSeeds[i] = f.Value == 0

				sim.After(f.Duration, func() { c.Procs.Start(i) })
			}
		}
	case PartialRestart:
		l, hasLeader := c.Leader()
		live := c.Procs.Live(nil)
		r := NewRand(f.Seed, "partial")
		r.Shuffle(len(live), func(a, b int) { live[a], live[b] = live[b], live[a] })

		kept := 0

		for _, i := range live {
			if kept < f.Count && (!hasLeader || i != l) && c.Procs.State(i) == Up {
				kept++

				continue
			}

			if c.Procs.Crash(i) {
				c.ctl.noSeeds[i] = true

				sim.After(f.Duration, func() { c.Procs.Start(i) })
			}
		}
	case Checkpoint:
		c.checkpoint(f.Start + f.Duration)
	default:
	}
}

// checkpoint requires convergence by deadline, polling every 10s.
func (c *Cluster) checkpoint(deadline int64) {
	err := c.liveness()
	if err == nil {
		c.Cov.Hit("checkpoint-converged")
		c.checkSane()

		return
	}

	if c.Sim.Now() >= deadline {
		c.Fail(fmt.Errorf("checkpoint: %w", err))

		return
	}

	c.Sim.After(10*sec, func() { c.checkpoint(deadline) })
}

// checkSane checks that a converged cluster's view version and term count
// are plausible for the time simulated: at most one delta per LeaderTick
// since the term started, and at most a few terms per fault.
func (c *Cluster) checkSane() {
	l, ok := c.Leader()
	if !ok {
		return
	}

	v := c.members[l].n.View()
	if maxSeq := uint64(c.Sim.Now()/int64(node.DefaultLeaderTick)) + 1; v.Version().Seq > maxSeq {
		c.Fail(fmt.Errorf("view seq %d above %d", v.Version().Seq, maxSeq))
	}

	if terms := len(c.ctl.termStarts); terms > termBudget(c.sc) {
		c.Fail(fmt.Errorf("%d terms, budget %d", terms, termBudget(c.sc)))
	}
}

// termBudget bounds the number of terms of a scenario: the first, and a
// few per fault (a lost leader, a fenced lineage, a released Lease). Each
// injected Lease conflict on a renewal ends a term, so LeaseConflicts adds
// one per RetryPeriod it lasts.
func termBudget(sc Scenario) int {
	n := 2

	for _, f := range sc.Faults {
		n += 4

		if f.Kind == LeaseConflicts {
			n += int(f.Duration / retryPeriod)
		}
	}

	return n
}

// quietRun runs for d after convergence and fails if a new term starts or
// the cluster stops being converged: terms must not churn once faults
// healed.
func (c *Cluster) quietRun(d int64) {
	terms := len(c.ctl.termStarts)
	l, _ := c.Leader()

	for end := c.Sim.Now() + d; c.Sim.Now() < end && c.err == nil; {
		c.Sim.RunUntil(c.Sim.Now() + 10*sec)
		c.checkPeriodic()
	}

	if c.err != nil {
		return
	}

	if n := len(c.ctl.termStarts); n != terms {
		c.Fail(fmt.Errorf("term churn after heal: %d new terms", n-terms))

		return
	}

	if l2, ok := c.Leader(); !ok || l2 != l {
		c.Fail(fmt.Errorf("leader changed after heal: %d -> %d", l, l2))

		return
	}

	if err := c.liveness(); err != nil {
		c.Fail(fmt.Errorf("after heal: %w", err))
	}
}

// CoverageHandle counts hits of one coverage point. Hit is a single
// increment.
type CoverageHandle struct {
	n *uint64
}

// Hit records that the state was reached.
func (h CoverageHandle) Hit() { *h.n++ }

// Coverage records which named states a simulation reached. Registered
// states that are never hit are reported by Missing.
type Coverage struct {
	hits       map[string]*uint64
	registered map[string]bool
}

// NewCoverage returns an empty registry.
func NewCoverage() *Coverage {
	return &Coverage{hits: make(map[string]*uint64), registered: make(map[string]bool)}
}

func (c *Coverage) counter(name string) *uint64 {
	p, ok := c.hits[name]
	if !ok {
		p = new(uint64)
		c.hits[name] = p
	}

	return p
}

// Register declares states that must be reached.
func (c *Coverage) Register(names ...string) {
	for _, n := range names {
		c.registered[n] = true
		c.counter(n)
	}
}

// Handle returns a cheap handle for the named state. Getting a handle does
// not register the state as required.
func (c *Coverage) Handle(name string) CoverageHandle {
	return CoverageHandle{n: c.counter(name)}
}

// Hit records one hit of the named state.
func (c *Coverage) Hit(name string) { *c.counter(name)++ }

// Merge adds o's hits and registrations into c.
func (c *Coverage) Merge(o *Coverage) {
	for k, v := range o.hits {
		*c.counter(k) += *v
	}

	for k := range o.registered {
		c.registered[k] = true
	}
}

// Missing returns the registered states with no hits, sorted.
func (c *Coverage) Missing() []string {
	var out []string

	for k := range c.registered {
		if *c.hits[k] == 0 {
			out = append(out, k)
		}
	}

	slices.Sort(out)

	return out
}

// Report lists every known state with its hit count in name order, marking
// registered states that were missed.
func (c *Coverage) Report() string {
	names := make([]string, 0, len(c.hits))
	for k := range c.hits {
		names = append(names, k)
	}

	slices.Sort(names)

	var b strings.Builder

	for _, k := range names {
		mark := ""
		if c.registered[k] && *c.hits[k] == 0 {
			mark = "  MISSING"
		}

		fmt.Fprintf(&b, "%-40s %10d%s\n", k, *c.hits[k], mark)
	}

	return b.String()
}
