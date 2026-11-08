// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"fmt"
	"slices"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/elect"
	"github.com/Azure/unbounded/internal/cachemesh/internal/leader"
	"github.com/Azure/unbounded/internal/cachemesh/internal/node"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// Control-plane invariants beyond the per-event safety checks in
// cluster.go: leader overlap, Lease call budgets per member and per second,
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
	// lossPeakPerSecond bounds elector Lease calls in any one second. During
	// leader loss each lottery round admits Binomial(N, Budget/N) members
	// (mean LotteryBudgetQPS), each making one read and, if the Lease is
	// expired, one acquiring write in the next round. Twice the mean covers
	// the binomial tail (P(X > 2*mean) is below 1e-4 for a mean of 20), so
	// reads plus writes stay below 3*LotteryBudgetQPS; candidates and the
	// leader add theirs.
	lossPeakPerSecond = 3*lotteryBudget + candidates + 2
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
	overlaps   int // overlaps excused by clocks outside the safety bounds

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

// Terms returns the simulated times at which new terms were published.
func (c *Cluster) Terms() []int64 { return slices.Clone(c.ctl.termStarts) }

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
		// The node's own seed read (see node.ownLeaseBit).
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

	if c.ctl.perSec[s] > lossPeakPerSecond {
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
		case s.Neighbors > 4*c.sc.MaxNeighbors:
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
// few per fault (a lost leader, a fenced lineage, a released Lease).
func termBudget(sc Scenario) int {
	return 2 + 4*len(sc.Faults)
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
