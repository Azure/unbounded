// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Health tracking for the local DHT.
//
// Design : each agent computes a single
// `p2p_dht_health_score` in [0, 1] as the geometric mean of three
// signals:
//
// 1. **Routing-table coverage**: `min(1, rt.Size / rt_target)`. The
// target is the membership view size minus one (i.e. every other
// agent should ideally appear in our routing table), with a small
// floor so a fresh cluster doesn't read as "fully covered" while
// we have one peer.
//
// 2. **Lookup latency**: p95 of FindProviders latencies over a 5-min
// rolling window. <200ms -> 1.0; >5s -> 0.0; linear in between.
//
// 3. **Self-test success rate**: success/(success+failure) of the
// last N (default 10) periodic Provide(self_id) -> FindProviders
// self-test cycles. A failed self-test means the local routing
// layer cannot get our provider records published and retrieved
// via the routing layer - a strong signal that direct-origin-fallback fallback is
// warranted.
//
// State labels for human consumption (the design doc):
// - Healthy ≥ 0.7
// - Degraded ≥ 0.3
// - Unhealthy < 0.3
//
// Used by:
// - the cold-start orchestrator's rule-6 degraded-expand (the design doc),
// - direct-origin-fallback direct-origin fallback gating (the design doc),
// - bootstrap-window suppression (the design doc).
//
// The monitor is created when the libp2p host is built and is safe
// to query from any goroutine. Latency samples are recorded by
// Host.FindProviders; self-tests are driven by a background goroutine
// launched from discovery.New.

package discovery

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"
)

// MonitorOptions configures a Monitor.
type MonitorOptions struct {
	// Now is the time source; defaults to time.Now.
	Now func() time.Time

	// RoutingTableSize returns the current size of the local DHT
	// routing table. Required.
	RoutingTableSize func() int

	// RoutingTableTarget returns the expected steady-state
	// routing-table size, computed per the design doc as
	// `min(informer_node_count, kademlia_max_routing_table_size)`.
	// Nil or a return value <= 0 disables the routing-table
	// component (it contributes 1.0).
	RoutingTableTarget func() int

	// LatencyWindow is the rolling window for p95 lookup latency.
	// Defaults to 5min (the design doc).
	LatencyWindow time.Duration

	// LatencyFloor and LatencyCeiling bound the linear-interpolation
	// region for the latency component. p95 ≤ floor -> 1.0; ≥ ceiling
	// -> 0.0. Defaults: 200ms / 5s (the design doc).
	LatencyFloor   time.Duration
	LatencyCeiling time.Duration

	// SelfTestWindow is the number of most-recent self-test outcomes
	// to retain. Defaults to 10.
	SelfTestWindow int
}

// Monitor implements the design doc DHT health scoring. It is safe for
// concurrent use.
type Monitor struct {
	opts    MonitorOptions
	started time.Time

	mu        sync.Mutex
	latencies []latencySample
	selftests []selfTestResult
}

type latencySample struct {
	at time.Time
	d  time.Duration
}

type selfTestResult struct {
	at time.Time
	ok bool
}

// NewMonitor builds a Monitor. RoutingTableSize must be non-nil; all
// other fields receive defaults.
func NewMonitor(opts MonitorOptions) *Monitor {
	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.LatencyWindow <= 0 {
		opts.LatencyWindow = 5 * time.Minute
	}

	if opts.LatencyFloor <= 0 {
		opts.LatencyFloor = 200 * time.Millisecond
	}

	if opts.LatencyCeiling <= 0 || opts.LatencyCeiling <= opts.LatencyFloor {
		opts.LatencyCeiling = 5 * time.Second
	}

	if opts.SelfTestWindow <= 0 {
		opts.SelfTestWindow = 10
	}

	return &Monitor{
		opts:    opts,
		started: opts.Now(),
	}
}

// ObserveLatency records the duration of a successful DHT lookup.
// Failed lookups should be recorded via the self-test path instead;
// per-call failures are not penalized here so a single slow node
// can't tank the score.
func (m *Monitor) ObserveLatency(d time.Duration) {
	if d < 0 {
		return
	}

	now := m.opts.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	m.latencies = append(m.latencies, latencySample{at: now, d: d})
	m.evictOldLatenciesLocked(now)
}

// SetRoutingTableTarget swaps the routing-table-target closure
// atomically. Used by main to wire the membership-derived target
// (the design doc's `min(informer_node_count, kademlia_max_routing_table_size)`)
// after the informer has come online, since Monitor is constructed
// before memberView in discovery.New.
func (m *Monitor) SetRoutingTableTarget(fn func() int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.opts.RoutingTableTarget = fn
}

// RecordSelfTest stores the outcome of one self-test cycle.
func (m *Monitor) RecordSelfTest(ok bool) {
	now := m.opts.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	m.selftests = append(m.selftests, selfTestResult{at: now, ok: ok})
	if len(m.selftests) > m.opts.SelfTestWindow {
		m.selftests = m.selftests[len(m.selftests)-m.opts.SelfTestWindow:]
	}
}

// Score returns the geometric mean of routing-table coverage, latency
// score, and self-test success rate. All three are in [0, 1]. With
// no data yet, the corresponding component reads 1.0 so a newly
// started agent isn't punished into Unhealthy before its window has
// filled.
func (m *Monitor) Score() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	rt := m.routingCoverageLocked()
	lat := m.latencyScoreLocked()

	st := m.selfTestScoreLocked()
	if rt <= 0 || lat <= 0 || st <= 0 {
		return 0
	}

	return math.Cbrt(rt * lat * st)
}

// State returns the human-readable health label per the design doc thresholds.
func (m *Monitor) State() string {
	s := m.Score()
	switch {
	case s >= 0.7:
		return "healthy"
	case s >= 0.3:
		return "degraded"
	default:
		return "unhealthy"
	}
}

// InBootstrapWindow reports whether the agent is still in the
// suppression window. direct-origin-fallback origin fallback must not fire while this
// is true: an empty DHT result during early bootstrap is a false
// signal, not evidence of a cold cluster.
//
// The suppression applies when either:
// - now-started < bootstrapWindow, OR
// - routing-table size < (routingTableTarget × bootstrapRoutingTablePct / 100).
//
// When RoutingTableTarget is nil or returns 0 (no known cluster
// size), only the time component is consulted.
func (m *Monitor) InBootstrapWindow(bootstrapWindow time.Duration, bootstrapRoutingTablePct int) bool {
	now := m.opts.Now()
	if bootstrapWindow > 0 && now.Sub(m.started) < bootstrapWindow {
		return true
	}

	m.mu.Lock()
	target := m.routingTableTargetLocked()
	m.mu.Unlock()

	if target > 0 && m.opts.RoutingTableSize != nil && bootstrapRoutingTablePct > 0 {
		threshold := (target * bootstrapRoutingTablePct) / 100
		if threshold < 1 {
			threshold = 1
		}

		if m.opts.RoutingTableSize() < threshold {
			return true
		}
	}

	return false
}

// routingTableTargetLocked reads the current target value from the
// closure (if set). Returns 0 when no source is wired, which
// disables the routing-table component. Caller must hold m.mu.
func (m *Monitor) routingTableTargetLocked() int {
	if m.opts.RoutingTableTarget == nil {
		return 0
	}

	return m.opts.RoutingTableTarget()
}

// routingCoverageLocked computes the routing-table component. With
// no callback or zero target, returns 1.0 (no signal).
func (m *Monitor) routingCoverageLocked() float64 {
	target := m.routingTableTargetLocked()
	if m.opts.RoutingTableSize == nil || target <= 0 {
		return 1.0
	}

	size := m.opts.RoutingTableSize()
	if size <= 0 {
		return 0.0
	}

	ratio := float64(size) / float64(target)
	if ratio > 1.0 {
		ratio = 1.0
	}

	return ratio
}

// latencyScoreLocked computes the p95-latency component over the
// rolling window. Empty window -> 1.0. Evicts stale samples first
// so that a quiet period (no DHT lookups -> no ObserveLatency calls)
// doesn't pin the agent permanently unhealthy on the last few bad
// samples.
func (m *Monitor) latencyScoreLocked() float64 {
	m.evictOldLatenciesLocked(m.opts.Now())

	if len(m.latencies) == 0 {
		return 1.0
	}

	durs := make([]time.Duration, len(m.latencies))
	for i, s := range m.latencies {
		durs[i] = s.d
	}

	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	// p95 index, clamped within bounds.
	idx := int(float64(len(durs))*0.95) - 1
	if idx < 0 {
		idx = 0
	}

	if idx >= len(durs) {
		idx = len(durs) - 1
	}

	p95 := durs[idx]
	switch {
	case p95 <= m.opts.LatencyFloor:
		return 1.0
	case p95 >= m.opts.LatencyCeiling:
		return 0.0
	default:
		span := float64(m.opts.LatencyCeiling - m.opts.LatencyFloor)
		return 1.0 - float64(p95-m.opts.LatencyFloor)/span
	}
}

// selfTestScoreLocked is the success ratio of the last N self-tests.
// Empty window -> 1.0.
func (m *Monitor) selfTestScoreLocked() float64 {
	if len(m.selftests) == 0 {
		return 1.0
	}

	ok := 0
	for _, r := range m.selftests {
		if r.ok {
			ok++
		}
	}

	return float64(ok) / float64(len(m.selftests))
}

// evictOldLatenciesLocked drops samples older than the rolling window.
// Called under m.mu.
func (m *Monitor) evictOldLatenciesLocked(now time.Time) {
	cutoff := now.Add(-m.opts.LatencyWindow)

	i := 0
	for i < len(m.latencies) && m.latencies[i].at.Before(cutoff) {
		i++
	}

	if i > 0 {
		m.latencies = m.latencies[i:]
	}
}

// RunSelfTestLoop drives the periodic self-test cycle. It blocks
// until ctx is canceled. `selfTest` should perform one Provide ->
// FindProviders round-trip and return whether it succeeded.
// Failures are debounced: a failed cycle only contributes to the
// score, the loop itself always continues.
func (m *Monitor) RunSelfTestLoop(ctx context.Context, period time.Duration, selfTest func(context.Context) bool) {
	if period <= 0 {
		period = 60 * time.Second
	}

	t := time.NewTicker(period)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			stCtx, cancel := context.WithTimeout(ctx, period/2)
			ok := selfTest(stCtx)

			cancel()
			m.RecordSelfTest(ok)
		}
	}
}
