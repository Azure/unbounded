// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
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
	Stats Counters

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
	leaseQPS  int // peak steady-state lease QPS observed
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
	// seeds are the configured seed addresses (the data plane routes
	// through them as gateways while the member is not in its view).
	seeds []string
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

func parseAddr(a string) (uint32, bool) {
	if len(a) < 2 || a[0] != 'm' {
		return 0, false
	}

	v, err := strconv.ParseUint(a[1:], 10, 32)

	return uint32(v), err == nil
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

	m.seeds = cfg.Seeds

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
		to, ok := parseAddr(call.To)
		if !ok || int(to) >= len(c.members) {
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
	c.Stats.Add("deltas", 1)
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

	// Lease calls: per member and per second, within the budgets in
	// control.go; the steady-state budget applies in quiet periods (no
	// fault within the last 90s, after warmup).
	now := c.Sim.Now()
	quiet := now > max(c.sc.Warmup, startupQuiet) && !c.faultActive(now-30*sec, now)

	if quiet {
		series, _ := c.Lease.QPS().Series(10)
		for _, q := range series {
			c.leaseQPS = max(c.leaseQPS, int(q))
		}
	}

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
