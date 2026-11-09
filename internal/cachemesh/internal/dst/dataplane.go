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

	"github.com/Azure/unbounded/internal/cachemesh/internal/epoch"
	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
	"github.com/Azure/unbounded/internal/cachemesh/internal/health"
	"github.com/Azure/unbounded/internal/cachemesh/internal/node"
	"github.com/Azure/unbounded/internal/cachemesh/internal/route"
	"github.com/Azure/unbounded/internal/cachemesh/internal/topology"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// KindData is the Net message kind of data-plane requests and replies. It
// is above every wire.Kind, so control traffic statistics stay separate.
const KindData uint16 = 60

// MetaTTL is the TTL of the metadata keyspaces.
const MetaTTL = 5 * time.Minute

// Data-plane keyspace seeds. Metadata uses four keyspaces "meta/0" to
// "meta/3", the ones the KeyspaceInvalidation fault bumps.
var (
	seedBlob  = hashing.Seed("blob")
	seedLayer = hashing.Seed("layer")
	seedMeta  = [4]uint64{hashing.Seed("meta/0"), hashing.Seed("meta/1"), hashing.Seed("meta/2"), hashing.Seed("meta/3")}
)

// HealthVariant selects the neighbor health configuration of a run.
type HealthVariant uint8

// Health variants.
const (
	// HealthDefault uses the library defaults.
	HealthDefault HealthVariant = iota
	// HealthPanic allows 60% of peers to be ejected and panics above 20%,
	// which makes the panic state reachable (with the defaults at most 10%
	// can be ejected and panic needs more than 50%).
	HealthPanic
	// HealthHedge raises the hedge budget to 50% of routed requests, so
	// that hedging is exercised at simulation request rates (the default
	// 5% needs about 20 routed requests per shard and interval before the
	// first hedge).
	HealthHedge
)

// String returns the variant name.
func (h HealthVariant) String() string {
	switch h {
	case HealthPanic:
		return "panic"
	case HealthHedge:
		return "hedge"
	default:
		return "default"
	}
}

// DataPlaneConfig configures a DataPlane. Zero values select defaults.
type DataPlaneConfig struct {
	// QPS is the cluster-wide request rate. Default: the member count
	// clamped to [20, 120].
	QPS int
	// HopTimeout bounds one hop (send, queue, service, ack). Default 250ms.
	HopTimeout time.Duration
	// Deadline is the origin's end-to-end deadline. Default 5s.
	Deadline time.Duration
	// Service is the mean service time per hop. Default 300us; slow
	// members serve slower by their slow factor.
	Service time.Duration
	// QueueCap bounds a member's in-flight data-plane requests; arrivals
	// beyond it are rejected with Overload. Default 16.
	QueueCap int
	// BurstEvery and BurstSize schedule herds: BurstSize requests for the
	// hottest blob from random members within 20ms. Defaults 30s and 160.
	BurstEvery time.Duration
	BurstSize  int
	// NoHedge disables hedging.
	NoHedge bool
	// TruncatePPM is the fraction of requests (parts per million) sent
	// with a TTL of one hop, to exercise TTL exhaustion. Default 2000.
	TruncatePPM uint32
	// Health is the health variant.
	Health HealthVariant
	// RouteSamples is the number of partitions per member walked by the
	// final route check (invariant 11). Default 8; at least 1.
	RouteSamples int
}

func (c *DataPlaneConfig) defaults(members int) {
	if c.QPS <= 0 {
		c.QPS = min(120, max(20, members))
	}

	if c.HopTimeout <= 0 {
		c.HopTimeout = 250 * time.Millisecond
	}

	if c.Deadline <= 0 {
		c.Deadline = 5 * time.Second
	}

	if c.Service <= 0 {
		c.Service = 300 * time.Microsecond
	}

	if c.QueueCap <= 0 {
		c.QueueCap = 16
	}

	if c.BurstEvery <= 0 {
		c.BurstEvery = 30 * time.Second
	}

	if c.BurstSize <= 0 {
		c.BurstSize = 160
	}

	if c.TruncatePPM == 0 {
		c.TruncatePPM = 2000
	}

	if c.RouteSamples <= 0 {
		c.RouteSamples = 8
	}
}

// HealthConfig returns the health configuration of variant v.
func HealthConfig(v HealthVariant) health.Config {
	switch v {
	case HealthPanic:
		return health.Config{MaxEjectionPercent: 60, PanicThreshold: 20}
	case HealthHedge:
		return health.Config{HedgeRatio: 0.5}
	default:
		return health.Config{}
	}
}

// Data-plane invariant parameters (see designs/cachemesh.md).
const (
	// detectWindow is how long after a slow-member fault starts health
	// detection is given before tail-latency protection is judged.
	detectWindow = 30 * sec
	// slowJudged is the smallest slow factor (permille) judged: below
	// about 10x a slow member's latency stays under DegradedFloor.
	slowJudged = 10_000
	// stampSample is how many recently issued stamps are tracked.
	stampSample = 64
	// stampMembers is how many members check each stamp per pass.
	stampMembers = 8
	// checkEvery is the period of the data-plane checks.
	checkEvery = 5 * sec
	// ejectWindow covers the longest default ejection (300s plus jitter).
	ejectWindow = 335 * sec
)

// Request results.
type dpResult uint8

const (
	resOpen dpResult = iota
	resServed
	resFailed
	resTTL
	resNoRoute
	resDeadline
)

var resNames = [...]string{"open", "served", "failed", "ttl-exhausted", "no-route", "deadline"}

// Ack codes carried in reply Data.
const (
	ackSuccess uint8 = iota + 1
	ackOverload
	ackFailure
)

type dpReq struct {
	id       uint64
	origin   uint32
	start    int64
	seed     uint64
	ttl      time.Duration
	live     int
	end      int64 // time the last branch resolved (0 while open)
	res      dpResult
	done     bool
	fault    bool    // started in a fault window
	trunc    bool    // sent with a truncated TTL
	hedged   bool    // a hedge was sent
	slowWin  bool    // started in a judged slow-member window
	slowExp  float64 // expected slow touches without protection, per hop
	slowSeen bool    // a hop was sent to a slow member
	slowHop1 bool    // the origin's first hop went to a slow member
}

// dpCopy is one branch of a request: the members that routed it so far,
// with their view versions.
type dpCopy struct {
	req       *dpReq
	path      []uint32
	vers      []view.Version
	members   bool // every router so far was in its view
	failovers int
	hedge     bool
	// trail records each router's decision (kind, owner slot, next slot),
	// for failure messages.
	trail []string
}

func (cp *dpCopy) note(d *route.Decision, flags uint8) {
	cp.trail = append(cp.trail, fmt.Sprintf("%s(owner=%d next=%d alt=%d in=%#x)", d.Kind, d.Owner.Slot, d.Next.Slot, d.Alternate.Slot, flags))
}

func (cp *dpCopy) extend(m uint32, v view.Version, member bool) *dpCopy {
	n := &dpCopy{req: cp.req, failovers: cp.failovers, hedge: cp.hedge, members: cp.members && member}
	n.path = append(make([]uint32, 0, len(cp.path)+1), cp.path...)
	n.path = append(n.path, m)
	n.vers = append(make([]view.Version, 0, len(cp.vers)+1), cp.vers...)
	n.vers = append(n.vers, v)
	n.trail = append([]string(nil), cp.trail...)

	return n
}

// dpHop is one attempt to hand a branch from one member to the next. It is
// the Data of the request message, the target of the service-completion
// event, and the completer of the call.
type dpHop struct {
	w        *DataPlane
	cp       *dpCopy
	from     uint32
	fromN    *node.Node
	to       uint32
	toSlot   view.Slot
	toGen    uint64
	alt      route.PeerRef
	hdr      route.Header
	altHdr   route.Header
	buf      [route.HeaderSize]byte
	failover bool
	sent     int64
	msg      Message // request as received, for the reply
	received bool
	resolved bool // the sender saw the outcome
	abandon  bool // the sender gave up (timeout); the receiver drops it
}

type dpMember struct {
	n         *node.Node
	gen       uint64
	env       *route.Env
	st        *route.State
	inflight  int
	busyUntil int64
	lastEp    uint64
	decisions uint64 // Route and Forward calls, for the hedge budget
	hedges    uint64
	seeds     []string // gateway seeds env was built with
	seedBuf   []string
}

// trackRing keeps the largest tracked-peer count of each check period over
// the longest ejection (MaxEjection plus 10% jitter).
type trackRing struct {
	max [ejectWindow / checkEvery]uint16
	at  [ejectWindow / checkEvery]int64 // period index of each bucket
}

func (r *trackRing) note(now int64, n int) {
	p := now / checkEvery
	k := p % int64(len(r.max))

	if r.at[k] != p {
		r.at[k], r.max[k] = p, 0
	}

	r.max[k] = max(r.max[k], uint16(min(n, 1<<16-1)))
}

func (r *trackRing) peak(now int64) int {
	p := now / checkEvery
	m := 0

	for k := range r.max {
		if r.at[k] > p-int64(len(r.max)) {
			m = max(m, int(r.max[k]))
		}
	}

	return m
}

type dpStamp struct {
	issuer uint32
	n      *node.Node
	at     int64
	seed   uint64
	st     epoch.Stamp
}

// DataPlane is the data-plane Workload: it routes requests hop by hop over
// the cluster's Net with each member's own route.State, feeds outcomes to
// the members' health trackers, hedges, and checks data-plane invariants.
type DataPlane struct {
	cfg  DataPlaneConfig
	c    *Cluster
	rng  *rand.Rand
	keys *KeyGen
	mem  []dpMember
	trk  []trackRing
	gen  uint64
	hcfg health.Config

	clients  []uint32 // hot members that issue half of the requests
	open     []*dpReq
	nextID   uint64
	settled  bool  // membership settled at the last check
	lastView int64 // time of the last view change on any member
	stamps   []dpStamp
	period   time.Duration
	driftPPB int64 // see stampSlack

	// Results.
	Latency, Steady, Faulty, SlowWin Histogram
	// ClientSteady is the steady-state latency of client requests, the
	// baseline of the tail-latency check.
	ClientSteady Histogram
	Counts       Counters

	slowN, slowTouched, slowHop1 int
	slowExpect                   float64
	hopsSum, hopsN               uint64
	maxStampAge                  int64 // largest age at which a stamp was Fresh
	maxEject                     string

	cov struct {
		kinds                        [7]CoverageHandle
		failover, ttl, handoff, note CoverageHandle
		hedgeSent, hedgeWon, denied  CoverageHandle
		expired, invalidated, over   CoverageHandle
		health                       [5]CoverageHandle
	}
}

// NewDataPlane returns a data-plane workload.
func NewDataPlane(cfg DataPlaneConfig) *DataPlane {
	return &DataPlane{cfg: cfg, hcfg: HealthConfig(cfg.Health)}
}

// DataPlaneCoverage lists the coverage states the data plane registers.
var DataPlaneCoverage = []string{
	"dp-kind-Self", "dp-kind-Relay", "dp-kind-Owner", "dp-kind-Backup", "dp-kind-Gateway", "dp-kind-Direct",
	"dp-kind-NoRoute",
	"dp-health-healthy", "dp-health-degraded", "dp-health-busy", "dp-health-ejected", "dp-health-probing",
	"dp-uneject", "dp-panic", "dp-hedge-sent", "dp-hedge-won", "dp-hedge-denied",
	"dp-failover", "dp-ttl-exhausted", "dp-handoff", "dp-stamp-expired", "dp-stamp-invalidated",
	"dp-note-version", "dp-overload", "dp-route-check", "dp-tail-judged",
}

// ConfigureNode installs the variant's health configuration and coverage
// hooks. Use it as ClusterConfig.Node.
func (w *DataPlane) ConfigureNode(member uint32, cfg *node.Config) {
	hc := w.hcfg
	cov := func(name string) func() {
		return func() {
			if w.c != nil {
				w.c.Cov.Hit(name)
			}
		}
	}
	panicHit, unejectHit := cov("dp-panic"), cov("dp-uneject")
	hc.Hooks.OnPanic = func(p bool) {
		if p {
			panicHit()
		}
	}
	hc.Hooks.OnUneject = func(uint32) { unejectHit() }
	cfg.Health = hc

	// The tracked set changes exactly when neighbors are added or removed;
	// record its size for the ejection-limit check.
	cfg.Hooks.OnNeighbors = func(_, _ []view.Slot) {
		if w.c != nil && w.c.members[member] != nil {
			w.noteTracked(member, w.c.members[member].n.Health().Tracked())
		}
	}

	// Record view changes for the steady-state definition.
	onView := cfg.Hooks.OnView
	cfg.Hooks.OnView = func(v *view.View) {
		if onView != nil {
			onView(v)
		}

		if w.c != nil {
			w.lastView = w.c.Sim.Now()
		}
	}
}

// Attach implements Workload.
func (w *DataPlane) Attach(c *Cluster) {
	w.c = c
	w.cfg.defaults(c.sc.Members)
	w.rng = c.Sim.Stream("dataplane")
	w.keys = NewKeyGen(c.Sim.Stream("dataplane/keys"), WorkloadConfig{})
	w.mem = make([]dpMember, len(c.members))
	w.trk = make([]trackRing, len(c.members))
	w.period = epoch.DefaultPeriod
	w.driftPPB = scenarioDriftPPB(c.sc)

	for k := range w.cov.kinds {
		w.cov.kinds[k] = c.Cov.Handle("dp-kind-" + route.Kind(k).String())
	}

	h := func(n string) CoverageHandle { return c.Cov.Handle("dp-" + n) }
	w.cov.failover, w.cov.ttl, w.cov.handoff, w.cov.note = h("failover"), h("ttl-exhausted"), h("handoff"), h("note-version")
	w.cov.hedgeSent, w.cov.hedgeWon, w.cov.denied = h("hedge-sent"), h("hedge-won"), h("hedge-denied")
	w.cov.expired, w.cov.invalidated, w.cov.over = h("stamp-expired"), h("stamp-invalidated"), h("overload")

	for s := range w.cov.health {
		w.cov.health[s] = h("health-" + health.State(s).String())
	}
}

// Start implements Workload.
func (w *DataPlane) Start(c *Cluster) {
	step := 100 * int64(time.Millisecond)

	// Hot clients: a few members (heavy pullers) issue half of the
	// requests, so that they route enough to each neighbor for health
	// protection to be measurable at simulation request rates.
	live := w.live()
	w.rng.Shuffle(len(live), func(a, b int) { live[a], live[b] = live[b], live[a] })
	w.clients = live[:min(len(live), max(1, min(4, len(live)/8)))]

	var gen func()

	gen = func() {
		w.generate(step)
		c.Sim.After(step, gen)
	}
	gen()

	var burst func()

	burst = func() {
		w.burst()
		c.Sim.After(int64(w.cfg.BurstEvery), burst)
	}
	c.Sim.After(int64(w.cfg.BurstEvery)/2, burst)

	var check func()

	check = func() {
		w.periodic()
		c.Sim.After(checkEvery, check)
	}
	c.Sim.After(checkEvery, check)
}

// --- Member state ---

// state returns member i's routing state, rebuilding it when the process,
// its view, or its slot changed. NewState is O(1).
func (w *DataPlane) state(i uint32) *route.State {
	m := w.c.members[i]
	if m == nil || m.dead {
		return nil
	}

	dm := &w.mem[i]
	if dm.n != m.n {
		w.gen++

		*dm = dpMember{n: m.n, gen: w.gen}
		dm.seeds = m.n.Seeds(nil)
		dm.env = w.newEnv(i, m, dm.seeds)
	}

	// While not a member, route through every seed the node knows,
	// including those learned from the Lease and from views, as the
	// public Mesh does.
	rebuilt := false

	if m.n.Self() == view.NoSlot {
		dm.seedBuf = m.n.Seeds(dm.seedBuf[:0])
		if !slices.Equal(dm.seedBuf, dm.seeds) {
			dm.seeds = slices.Clone(dm.seedBuf)
			dm.env = w.newEnv(i, m, dm.seeds)
			rebuilt = true
		}
	}

	if rebuilt || dm.st == nil || dm.st.View() != m.n.View() || dm.st.Self() != m.n.Self() {
		dm.st = route.NewState(m.n.View(), m.n.Self(), dm.env)
	}

	return dm.st
}

// newEnv returns member i's routing Env with seeds other than its own
// address as gateways.
func (w *DataPlane) newEnv(i uint32, m *member, seeds []string) *route.Env {
	self := memberAddr(i)
	gws := make([]route.PeerRef, 0, len(seeds))

	for _, s := range seeds {
		if s != self {
			gws = append(gws, route.PeerRef{Slot: view.NoSlot, ID: s, Addr: s})
		}
	}

	return route.NewEnv(route.Config{
		Health: m.n.Health(), Epoch: m.n.Epoch(), Notes: &route.Notes{}, Gateways: gws,
		SelfID: memberID(i), SelfIncarnation: m.inc, PartitionBits: w.c.sc.PartitionBits,
	})
}

// noteTracked records member i's tracked-peer count.
func (w *DataPlane) noteTracked(i uint32, n int) {
	if int(i) < len(w.trk) {
		w.trk[i].note(w.c.Sim.Now(), n)
	}
}

// running reports whether member i is up and not paused.
func (w *DataPlane) running(i uint32) bool {
	m := w.c.members[i]

	return m != nil && !m.dead && !m.paused
}

func (w *DataPlane) mono(i uint32) int64 { return w.c.members[i].clock.Mono(w.c.Sim.Now()) }

// slow returns member i's slow factor in permille (1000 is normal).
func (w *DataPlane) slow(i uint32) int64 {
	if s := w.c.Net.slow[i]; s > 1000 {
		return int64(s)
	}

	return 1000
}

// faultNear reports whether a fault is active, or ended less than pad ago,
// at any time in [from, to].
func (w *DataPlane) faultNear(from, to, pad int64) bool {
	for _, f := range w.c.sc.Faults {
		if f.Start <= to && f.End()+pad >= from {
			return true
		}
	}

	return false
}

// --- Generation ---

func (w *DataPlane) live() []uint32 {
	return w.c.Procs.InState(nil, Up)
}

func (w *DataPlane) generate(step int64) {
	n := int64(w.cfg.QPS) * step / sec

	live := w.live()
	if len(live) == 0 {
		return
	}

	// While members are slow, clients pull harder (n extra requests), so
	// that the requests a client sends to each neighbor between health
	// re-probes (a Degraded peer is retried once its EWMA decays, about
	// every 20s) outnumber the re-probes and protection is measurable.
	extra := int64(0)
	if w.slowActive() {
		extra = n
	}

	for k := range n + extra {
		origin := live[w.rng.IntN(len(live))]
		if len(w.clients) > 0 && (k >= n || w.rng.IntN(2) == 0) {
			origin = w.clients[w.rng.IntN(len(w.clients))]
		}

		at := w.rng.Int64N(step)
		key, kind, rank := w.keys.Next()
		k := append([]byte(nil), key...)

		seed, ttl := seedLayer, time.Duration(0)

		switch kind {
		case KeyBlob:
			seed = seedBlob
		case KeyMeta:
			seed, ttl = seedMeta[rank%4], MetaTTL
		}

		w.c.Sim.After(at, func() { w.issue(origin, seed, ttl, k, false) })
	}
}

// slowActive reports whether a slow-member fault is active.
func (w *DataPlane) slowActive() bool {
	now := w.c.Sim.Now()

	for _, f := range w.c.sc.Faults {
		if f.Kind == SlowMembers && f.Start <= now && now < f.End() {
			return true
		}
	}

	return false
}

// burst sends a herd of requests for the hottest blob.
func (w *DataPlane) burst() {
	live := w.live()
	if len(live) == 0 {
		return
	}

	key := append([]byte(nil), w.keys.Blob(0)...)

	for range w.cfg.BurstSize {
		origin := live[w.rng.IntN(len(live))]
		w.c.Sim.After(w.rng.Int64N(20_000_000), func() { w.issue(origin, seedBlob, 0, key, true) })
	}
}

// issue starts a request at origin.
func (w *DataPlane) issue(origin uint32, seed uint64, ttl time.Duration, key []byte, burst bool) {
	if !w.running(origin) {
		return
	}

	st := w.state(origin)
	now := w.c.Sim.Now()
	w.nextID++

	r := &dpReq{id: w.nextID, origin: origin, start: now, seed: seed, ttl: ttl, live: 1}
	r.fault = !w.settled || w.faultNear(now, now, 30*sec)
	w.open = append(w.open, r)
	*w.Counts.Counter("requests")++

	if burst {
		*w.Counts.Counter("burst-requests")++
	}

	w.slowWindow(r, now)

	d := st.Route(seed, key)
	w.decided(origin, &d)

	cp := &dpCopy{req: r, path: []uint32{origin}, vers: []view.Version{st.View().Version()}, members: st.Member()}
	cp.note(&d, 0)

	if d.Kind == route.KindRelay && !burst && w.rng.Uint32N(1_000_000) < w.cfg.TruncatePPM {
		d.Header.TTL = d.Header.Hops + 1
		r.trunc = true
	}

	w.dispatch(origin, cp, st, &d, true)

	w.c.Sim.After(int64(w.cfg.Deadline), func() {
		if !r.done {
			w.finish(r, resDeadline, false)
		}
	})
}

// slowWindow marks a request that starts in a judged slow-member window.
func (w *DataPlane) slowWindow(r *dpReq, now int64) {
	judged := false

	for _, f := range w.c.sc.Faults {
		if f.Kind == SlowMembers && f.Value >= slowJudged && f.Start+detectWindow <= now && now < f.End() {
			judged = true
		}
	}

	if judged && (w.otherFaultNear(now) || !w.settled) {
		judged = false
	}

	if !judged || w.slow(r.origin) > 1000 || !slices.Contains(w.clients, r.origin) || !w.state(r.origin).Member() {
		return
	}

	slow, live := 0, 0

	for i := range w.c.members {
		if w.running(uint32(i)) {
			live++

			if w.slow(uint32(i)) >= slowJudged {
				slow++
			}
		}
	}

	if slow == 0 || live == 0 {
		return
	}

	r.slowWin = true
	r.slowExp = float64(slow) / float64(live)
}

// otherFaultNear reports whether a fault other than judged slow members
// (at least slowJudged), clock skew or drift, and control-plane-only faults
// (API server outage, lease conflicts, keyspace invalidation) is active at
// now or ended less than two minutes ago. Tail-latency protection is judged
// only while judged slow members are the only data-plane fault: outlier
// detection is relative to the median of a member's peers, so while most
// peers time out (partition, loss) a slow member is not an outlier and the
// inflated median takes a while to recover; crashed or paused peers take
// the few ejections MaxEjectionPercent allows; and members slowed below the
// judged factor shift the median without counting as slow.
func (w *DataPlane) otherFaultNear(now int64) bool {
	for _, f := range w.c.sc.Faults {
		switch {
		case f.Kind == SlowMembers && f.Value >= slowJudged:
			continue
		case f.Kind == ClockSkew, f.Kind == ClockDrift, f.Kind == APIServerOutage,
			f.Kind == LeaseConflicts, f.Kind == KeyspaceInvalidation:
			continue
		}

		if f.Start <= now && f.End()+otherFaultPad >= now {
			return true
		}
	}

	return false
}

// otherFaultPad is how long after another fault ends tail latency is not
// judged.
const otherFaultPad = 120 * sec

// decided records coverage for a routing decision made at member i.
func (w *DataPlane) decided(i uint32, d *route.Decision) {
	w.mem[i].decisions++
	if int(d.Kind) < len(w.cov.kinds) {
		w.cov.kinds[d.Kind].Hit()
	}

	if !d.PrevOwner.IsZero() {
		w.cov.handoff.Hit()
	}
}

// dispatch acts on decision d made at member at for branch cp. origin
// reports whether at is the request's origin (which may hedge).
func (w *DataPlane) dispatch(at uint32, cp *dpCopy, st *route.State, d *route.Decision, origin bool) {
	r := cp.req

	switch d.Kind {
	case route.KindSelf:
		w.served(at, cp, st, d)
	case route.KindNoRoute:
		w.branchEnd(r, resNoRoute)
	default:
		to, ok := w.c.member(d.Next.Addr)
		if !ok {
			w.branchEnd(r, resFailed)

			return
		}

		h := w.send(at, cp, to, d.Next.Slot, d.Header, d.Alternate, false)
		h.altHdr = d.AlternateHeader()

		if origin && r.slowWin && w.slow(to) >= slowJudged {
			r.slowHop1 = true
		}

		if origin && !w.cfg.NoHedge && !d.Alternate.IsZero() && d.HedgeAfter > 0 {
			hdr, alt := h.altHdr, d.Alternate
			w.c.Sim.After(int64(d.HedgeAfter), func() { w.hedge(at, h, hdr, alt) })
		}
	}
}

func (w *DataPlane) hedge(at uint32, first *dpHop, hdr route.Header, alt route.PeerRef) {
	r := first.cp.req
	if r.done || first.resolved || !w.running(at) || w.c.members[at].n != first.fromN {
		return
	}

	to, ok := w.c.member(alt.Addr)
	if !ok {
		return
	}

	st := w.state(at)
	if !st.TryHedge(hdr) {
		w.cov.denied.Hit()
		*w.Counts.Counter("hedge-denied")++

		return
	}

	w.cov.hedgeSent.Hit()
	w.mem[at].hedges++
	*w.Counts.Counter("hedges")++
	r.hedged = true
	r.live++

	hdr.Flags |= route.FlagHedge
	cp := &dpCopy{req: r, path: first.cp.path, vers: first.cp.vers, members: first.cp.members, hedge: true}
	w.send(at, cp, to, alt.Slot, hdr, route.PeerRef{}, true)
}

// send hands branch cp from member from to member to.
func (w *DataPlane) send(from uint32, cp *dpCopy, to uint32, slot view.Slot, hdr route.Header, alt route.PeerRef, failover bool) *dpHop {
	h := &dpHop{
		w: w, cp: cp, from: from, fromN: w.c.members[from].n, to: to, toSlot: slot,
		alt: alt, hdr: hdr, failover: failover, sent: w.c.Sim.Now(),
	}
	h.hdr.Put(&h.buf)

	*w.Counts.Counter("hops")++

	if w.slow(to) >= slowJudged {
		cp.req.slowSeen = true
	}

	w.c.Net.Call(Message{From: from, To: to, Kind: KindData, Payload: h.buf[:], Data: h}, int64(w.cfg.HopTimeout), h)

	return h
}

// DeliverData implements DataHandler: a hop arrives at its receiver.
func (w *DataPlane) DeliverData(msg *Message) {
	h, ok := msg.Data.(*dpHop)
	if !ok || h.received || h.abandon || !w.running(msg.To) {
		return
	}

	h.received = true
	h.msg = *msg

	w.state(msg.To)

	dm := &w.mem[msg.To]
	if dm.inflight >= w.cfg.QueueCap {
		w.cov.over.Hit()
		*w.Counts.Counter("overload")++
		w.c.Net.Reply(&h.msg, Message{Kind: KindData, Data: ackOverload})

		return
	}

	dm.inflight++
	h.toGen = dm.gen

	now := w.c.Sim.Now()
	svc := (int64(w.cfg.Service)/2 + w.rng.Int64N(int64(w.cfg.Service)+1)) * w.slow(msg.To) / 1000
	done := max(now, dm.busyUntil) + svc
	dm.busyUntil = done
	w.c.Sim.AtTarget(done, h, 0, 0)
}

// Fire runs when the receiver has served the hop: it routes the branch on.
func (h *dpHop) Fire(_, _ uint64) {
	w := h.w
	dm := &w.mem[h.to]

	if dm.gen == h.toGen {
		dm.inflight--
	}

	if h.abandon || !w.running(h.to) || dm.gen != h.toGen {
		return // the sender times out
	}

	st := w.state(h.to)

	hdr, err := route.ParseHeader(h.msg.Payload)
	if err != nil {
		w.c.Net.Reply(&h.msg, Message{Kind: KindData, Data: ackFailure})

		return
	}

	d, err := st.Forward(hdr)

	w.notes(h.to)

	if errors.Is(err, route.ErrBadHeader) {
		w.c.Net.Reply(&h.msg, Message{Kind: KindData, Data: ackFailure})

		return
	}

	w.c.Net.Reply(&h.msg, Message{Kind: KindData, Data: ackSuccess})

	r := h.cp.req
	r.live++

	cp := h.cp.extend(h.to, st.View().Version(), st.Member())
	cp.note(&d, hdr.Flags)
	w.decided(h.to, &d)

	if errors.Is(err, route.ErrTTLExhausted) {
		w.cov.ttl.Hit()
		*w.Counts.Counter("ttl-exhausted")++

		// Steady state: membership settled and no view change on any
		// member since a second before the request started.
		steady := !r.fault && w.lastView < r.start-sec
		if steady && !r.trunc {
			*w.Counts.Counter("ttl-exhausted-steady")++
		}

		if !r.trunc && (cp.consistent() || steady) {
			w.c.Fail(fmt.Errorf("dataplane: request %d exhausted its TTL (steady=%v) on path %v at versions %v: %v",
				r.id, steady, cp.path, cp.vers, cp.trail))
		}

		w.branchEnd(r, resTTL)

		return
	}

	w.dispatch(h.to, cp, st, &d, false)
}

// notes passes a newer version seen in a header to member i's control
// plane, as the public Mesh does.
func (w *DataPlane) notes(i uint32) {
	note, ok := w.mem[i].env.Notes().Take()
	if !ok {
		return
	}

	w.cov.note.Hit()

	m := w.c.members[i]
	m.n.NoteVersion(w.mono(i), note.Sender, view.Version{Term: uint64(note.Term), Seq: note.Seq})
	m.after()
}

// Complete implements Completer for the sender side of a hop.
func (h *dpHop) Complete(_ uint64, resp *Message, err error) {
	w := h.w
	r := h.cp.req
	h.resolved = true

	code := ackFailure

	switch {
	case err != nil:
		h.abandon = true
		code = 0
	case resp != nil:
		if c, ok := resp.Data.(uint8); ok {
			code = c
		}
	}

	m := w.c.members[h.from]
	if m == nil || m.dead || m.n != h.fromN || m.paused {
		w.branchEnd(r, resFailed) // the holder is gone

		return
	}

	lat := time.Duration(w.c.Sim.Now() - h.sent)
	hl := m.n.Health()

	switch code {
	case ackSuccess:
		hl.Observe(h.toSlot, health.Success, lat)
		w.branchEnd(r, resOpen)

		return
	case ackOverload:
		hl.Observe(h.toSlot, health.Overload, lat)
	case 0:
		hl.Observe(h.toSlot, health.Timeout, lat)
	default:
		hl.Observe(h.toSlot, health.Failure, lat)
	}

	if s, _, _ := hl.PeerState(h.toSlot); s != health.StateHealthy {
		w.cov.health[s].Hit()
	}

	alt, ok := w.c.member(h.alt.Addr)
	if h.failover || h.alt.IsZero() || !ok || alt == h.to || r.done {
		w.branchEnd(r, resFailed)

		return
	}

	w.cov.failover.Hit()
	*w.Counts.Counter("failovers")++

	cp := *h.cp
	cp.failovers++
	cp.trail = append(cp.trail[:len(cp.trail):len(cp.trail)], "failover")
	w.send(h.from, &cp, alt, h.alt.Slot, h.altHdr, route.PeerRef{}, true)
}

// consistent reports whether every router of the branch was a member at
// the same view version.
func (cp *dpCopy) consistent() bool {
	if !cp.members {
		return false
	}

	for _, v := range cp.vers[1:] {
		if v != cp.vers[0] {
			return false
		}
	}

	return true
}

// served completes a branch served at member at.
func (w *DataPlane) served(at uint32, cp *dpCopy, st *route.State, d *route.Decision) {
	r := cp.req

	if cp.consistent() {
		w.checkPath(cp, st, d)
	}

	if r.ttl > 0 && len(w.stamps) < 4*stampSample && w.rng.IntN(4) == 0 {
		w.stamps = append(w.stamps, dpStamp{
			issuer: at, n: w.c.members[at].n, at: w.c.Sim.Now(), seed: r.seed, st: st.Stamp(r.seed, r.ttl),
		})
	}

	if !r.done && cp.hedge {
		w.cov.hedgeWon.Hit()
		*w.Counts.Counter("hedge-won")++
	}

	if !r.done {
		w.hopsSum += uint64(len(cp.path) - 1)
		w.hopsN++
	}

	w.finish(r, resServed, true)
}

// checkPath checks invariant 5 on a branch whose routers all shared one
// view version.
func (w *DataPlane) checkPath(cp *dpCopy, st *route.State, d *route.Decision) {
	r := cp.req
	v := st.View()
	dims := int(v.Shape().Dims)

	fail := func(format string, args ...any) {
		w.c.Fail(fmt.Errorf("dataplane: request %d path %v at %v: %s: %v", r.id, cp.path, v.Version(), fmt.Sprintf(format, args...), cp.trail))
	}

	if hops := len(cp.path) - 1; hops > dims {
		fail("%d hops above D=%d", hops, dims)
	}

	for i, a := range cp.path {
		for _, b := range cp.path[i+1:] {
			if a == b {
				fail("member %d visited twice", a)
			}
		}
	}

	self := st.Self()
	if d.Owner.Slot != self && v.Cell(self) != v.Cell(d.Owner.Slot) {
		fail("served at slot %d (cell %d), owner %d in cell %d", self, v.Cell(self), d.Owner.Slot, v.Cell(d.Owner.Slot))
	}
}

// branchEnd resolves one branch of r with result res (resOpen when the
// branch was handed on).
func (w *DataPlane) branchEnd(r *dpReq, res dpResult) {
	r.live--
	if r.live < 0 {
		w.c.Fail(fmt.Errorf("dataplane: request %d branch count below zero", r.id))
	}

	if r.live == 0 {
		w.resolved(r)
	}

	if res != resOpen && r.res == resOpen {
		r.res = res // first failure reason, kept unless a branch serves
	}

	if r.live == 0 && !r.done {
		if res == resOpen {
			res = r.res
		}

		w.finish(r, res, false)
	}
}

// resolved records that the last branch of r ended and checks that it did
// so within the hang bound.
func (w *DataPlane) resolved(r *dpReq) {
	r.end = w.c.Sim.Now()
	if d := r.end - r.start; d > w.hangBound() {
		w.c.Fail(fmt.Errorf("dataplane: request %d from member %d took %v to resolve, bound %v",
			r.id, r.origin, time.Duration(d), time.Duration(w.hangBound())))
	}
}

// finish records the terminal result of r. branch ends a branch too (a
// branch served the request).
func (w *DataPlane) finish(r *dpReq, res dpResult, branch bool) {
	if branch {
		r.live--
		if r.live == 0 {
			w.resolved(r)
		}
	}

	if r.done {
		return
	}

	if res == resOpen {
		res = resFailed
	}

	r.done = true
	r.res = res
	now := w.c.Sim.Now()
	lat := now - r.start

	*w.Counts.Counter("result-" + resNames[res])++
	w.c.Trace.Add(40, r.id, uint64(res), uint64(lat))

	if res == resDeadline && !r.fault && w.settled && !w.faultNear(r.start, now, 30*sec) {
		w.c.Fail(fmt.Errorf("dataplane: request %d from member %d missed its %v deadline in steady state",
			r.id, r.origin, w.cfg.Deadline))
	}

	// Slow-window requests count whatever their result: failures are
	// what slow members cause.
	if r.slowWin {
		w.SlowWin.Record(lat)
		w.slowN++
		w.slowExpect += r.slowExp

		if r.slowSeen {
			w.slowTouched++
		}

		if r.slowHop1 {
			w.slowHop1++
		}
	}

	if res != resServed {
		return
	}

	w.Latency.Record(lat)

	if r.fault {
		w.Faulty.Record(lat)
	} else {
		w.Steady.Record(lat)

		if slices.Contains(w.clients, r.origin) {
			w.ClientSteady.Record(lat)
		}
	}
}

// --- Periodic checks ---

func (w *DataPlane) periodic() {
	w.settled = w.membershipSettled()
	w.compact()
	w.checkMembers()
	w.checkStamps()
}

// membershipSettled reports whether there is a leader, every running
// member is at the leader's version, and every member of that view is
// running. It is O(members).
func (w *DataPlane) membershipSettled() bool {
	l, ok := w.c.Leader()
	if !ok {
		return false
	}

	lv := w.c.members[l].n.View()
	n := 0

	for i, m := range w.c.members {
		if m == nil || m.dead {
			continue
		}

		if m.paused || m.n.View() != lv || m.n.Self() == view.NoSlot {
			return false
		}

		n++

		if !w.running(uint32(i)) {
			return false
		}
	}

	return n == lv.Len()
}

// hangBound is the longest a request may take until its last branch
// resolves: every hop resolves within HopTimeout (the ack follows the
// queue and service time), each hop fails over at most once, the TTL
// bounds the hops (MaxDims+3 with a gateway hop), and a hedge starts
// within HedgeMax.
func (w *DataPlane) hangBound() int64 {
	hedgeMax := w.hcfg.HedgeMax
	if hedgeMax <= 0 {
		hedgeMax = time.Second
	}

	return int64(topology.MaxDims+3)*2*int64(w.cfg.HopTimeout) + int64(hedgeMax)
}

// compact drops resolved requests and checks that none hangs.
func (w *DataPlane) compact() {
	now := w.c.Sim.Now()
	keep := w.open[:0]

	for _, r := range w.open {
		if r.done && r.live == 0 {
			continue
		}

		if now-r.start > w.hangBound() {
			w.c.Fail(fmt.Errorf("dataplane: request %d from member %d unresolved after %v (done=%v, %d live branches)",
				r.id, r.origin, time.Duration(now-r.start), r.done, r.live))

			r.done, r.live = true, 0

			continue
		}

		keep = append(keep, r)
	}

	clear(w.open[len(keep):])
	w.open = keep
}

// checkMembers checks epochs (6), neighbor counts (4), and ejection limits
// on every running member.
func (w *DataPlane) checkMembers() {
	sc := &w.c.sc
	bounds := map[*view.View][2]int{}
	buf := make([]view.Slot, 0, 256)

	for i := range w.c.members {
		idx := uint32(i)
		if !w.running(idx) {
			continue
		}

		w.state(idx)
		dm := &w.mem[i]
		n := dm.n

		if ep := n.Epoch().Current(); ep < dm.lastEp {
			w.c.Fail(fmt.Errorf("dataplane: member %d epoch regressed %d -> %d", i, dm.lastEp, ep))
		} else {
			dm.lastEp = ep
		}

		hl := n.Health()
		cts := hl.Counts()

		w.healthCov(cts)

		// The limit applies when a peer is ejected; peers ejected while
		// more were tracked stay ejected (at most MaxEjection plus
		// jitter) when neighbors are removed, so judge against the peak
		// tracked count over that window (recorded on every neighbor
		// change).
		w.noteTracked(idx, cts.Tracked)
		peak := w.trk[i].peak(w.c.Sim.Now())

		limit := max(1, peak*w.maxEjectPct()/100)
		if ej := cts.Ejected + cts.Probing; ej > limit {
			w.maxEject = fmt.Sprintf("member %d: %d of %d tracked peers ejected (peak %d), limit %d", i, ej, cts.Tracked, peak, limit)
			*w.Counts.Counter("ejection-limit-exceeded")++
		}

		if n.Self() == view.NoSlot {
			continue
		}

		v := n.View()

		b, ok := bounds[v]
		if !ok {
			b = neighborBound(v, sc.CellSize, sc.MaxNeighbors)
			bounds[v] = b
		}

		buf = n.Neighbors(buf[:0])
		if len(buf) > b[0] {
			w.c.Fail(fmt.Errorf("dataplane: member %d has %d neighbors, bound %d (balanced=%v) at %v",
				i, len(buf), b[0], b[1] == 1, v.Version()))
		}
	}
}

// maxEjectPct is the configured MaxEjectionPercent (default 10).
func (w *DataPlane) maxEjectPct() int {
	if w.hcfg.MaxEjectionPercent > 0 {
		return w.hcfg.MaxEjectionPercent
	}

	return 10
}

// healthCov records the health states present in cts.
func (w *DataPlane) healthCov(cts health.Counts) {
	if cts.Healthy > 0 {
		w.cov.health[health.StateHealthy].Hit()
	}

	if cts.Degraded > 0 {
		w.cov.health[health.StateDegraded].Hit()
	}

	if cts.Busy > 0 {
		w.cov.health[health.StateBusy].Hit()
	}

	if cts.Ejected > 0 {
		w.cov.health[health.StateEjected].Hit()
	}

	if cts.Probing > 0 {
		w.cov.health[health.StateProbing].Hit()
	}
}

// neighborBound returns invariant 4's neighbor bound for members of v and
// whether v's cells are balanced (1) or not (0). Balanced cells (occupancy
// differing by at most one) must keep every member within MaxNeighbors,
// or within the estimate of the best shape when no shape can (topology
// falls back to MaxDims). Unbalanced cells, during a reshape or cell moves,
// may reach twice that.
func neighborBound(v *view.View, cellSize, maxNeighbors int) [2]int {
	n := v.Len()
	limit := maxNeighbors

	if best := topology.Choose(n, cellSize, maxNeighbors); best.Cells() > 0 {
		occ := (n + int(best.Cells()) - 1) / int(best.Cells())
		limit = max(limit, best.EstimatedNeighbors(occ))
	}

	sh := v.Shape()
	lo, hi := n, 0

	for c := range sh.Cells() {
		o := v.Occupancy(c)
		lo, hi = min(lo, o), max(hi, o)
	}

	if hi-lo <= 1 {
		return [2]int{limit, 1}
	}

	return [2]int{2 * limit, 0}
}

// stampSlack bounds how long after its expiry, in simulated time, another
// member may still find a stamp Fresh. A member's epoch phase lags the
// issuer's by at most one period (it anchors the epoch when a heartbeat or
// snapshot arrives, which can be up to a period after the epoch began),
// plus the leader's tick and the member's own tick (Advance runs once per
// TickInterval), plus message latency and clock drift. Drift is bounded by
// the scenario's largest ClockDrift (and a 0.05% floor for the
// oscillator error of undrifted clocks), counted twice: the issuer's and
// the checker's clocks may drift in opposite directions.
func (w *DataPlane) stampSlack(age int64) int64 {
	drift := age * w.driftPPB / 1_000_000_000

	return int64(w.period) + 2*int64(node.DefaultTickInterval) + sec + 2*drift
}

// MinDriftPPB is the drift stampSlack allows for even when a scenario
// injects no ClockDrift (0.05%).
const MinDriftPPB = 500_000

// scenarioDriftPPB returns the largest drift magnitude sc applies, at least
// MinDriftPPB.
func scenarioDriftPPB(sc Scenario) int64 {
	d := int64(MinDriftPPB)

	for _, f := range sc.Faults {
		if f.Kind == ClockDrift {
			d = max(d, f.Value, -f.Value)
		}
	}

	return min(d, MaxDriftPPB)
}

// checkStamps checks invariant 6 on the sampled stamps.
func (w *DataPlane) checkStamps() {
	now := w.c.Sim.Now()
	ttl := int64(MetaTTL)
	period := int64(w.period)
	keep := w.stamps[:0]

	var live []uint32

	for _, s := range w.stamps {
		// A whole-cluster restart crashed every process that could hold
		// the value, and the epoch restarts with the new lineage: the
		// stamp no longer exists anywhere.
		if w.c.ctl.coldRestartAt > 0 && s.at <= w.c.ctl.coldRestartAt {
			continue
		}

		age := now - s.at
		limit := ttl + period + w.stampSlack(age)

		if live == nil {
			live = w.live()
		}

		// The issuer must still find the stamp Fresh for T - period when
		// epochs advance normally (no fault nearby).
		if age <= ttl-period && w.running(s.issuer) && w.c.members[s.issuer].n == s.n &&
			!w.faultNear(s.at-90*sec, now, 90*sec) {
			st := w.state(s.issuer)
			if st.View().Gen(s.seed) == s.st.Gen && !st.Fresh(s.seed, s.st) {
				w.c.Fail(fmt.Errorf("dataplane: stamp %+v issued by member %d at %.1fs stale after %v (epoch %d)",
					s.st, s.issuer, float64(s.at)/1e9, time.Duration(age), st.Env().Epoch().Current()))
			}
		}

		for k := 0; k < stampMembers && len(live) > 0; k++ {
			i := live[w.rng.IntN(len(live))]
			if !w.running(i) {
				continue
			}

			st := w.state(i)
			fresh := st.Fresh(s.seed, s.st)

			if st.View().Gen(s.seed) != s.st.Gen {
				if fresh {
					w.c.Fail(fmt.Errorf("dataplane: member %d finds stamp %+v of an invalidated generation fresh", i, s.st))
				}

				w.cov.invalidated.Hit()

				continue
			}

			if !fresh {
				w.cov.expired.Hit()

				continue
			}

			// The bound holds on members of their view, which hear the
			// leader's heartbeats. A non-member may have anchored its
			// epoch from a stale lineage's snapshot (for example a
			// restarted process that caught up from a partitioned peer)
			// and lags until it hears the leader.
			if !st.Member() {
				continue
			}

			w.maxStampAge = max(w.maxStampAge, age)

			if age > limit {
				w.c.Fail(fmt.Errorf("dataplane: member %d finds stamp %+v issued by member %d fresh %v after issue (limit %v, epoch %d, view %v epoch %d member %v)",
					i, s.st, s.issuer, time.Duration(age), time.Duration(limit), st.Env().Epoch().Current(),
					st.View().Version(), st.View().Epoch(), st.Member()))
			}
		}

		if age <= limit+30*sec {
			keep = append(keep, s)
		}
	}

	clear(w.stamps[len(keep):])
	w.stamps = keep
}

// --- Final checks ---

// Check implements Workload. It runs after the cluster converged.
func (w *DataPlane) Check(c *Cluster) error {
	if err := w.checkRoutes(); err != nil {
		return err
	}

	if w.maxEject != "" {
		return fmt.Errorf("dataplane: ejection limit exceeded (%d times), last: %s",
			w.Counts.Get("ejection-limit-exceeded"), w.maxEject)
	}

	if err := w.checkHedges(); err != nil {
		return err
	}

	if err := w.checkTail(); err != nil {
		return err
	}

	_ = c

	return nil
}

// checkRoutes is invariant 11: from every member, sampled partitions reach
// the owner (or a backup in its cell when the owner is unusable to the
// deciding member) in at most D hops with no Direct decision.
func (w *DataPlane) checkRoutes() error {
	for i := range w.c.members {
		idx := uint32(i)
		if !w.running(idx) || w.c.members[i].leaving {
			continue
		}

		st := w.state(idx)
		if !st.Member() {
			return fmt.Errorf("route check: live member %d is not in its view", i)
		}

		v := st.View()
		bits := st.PartitionBits()
		dims := int(v.Shape().Dims)

		for range w.cfg.RouteSamples {
			p := w.rng.Uint32N(uint32(1) << bits)
			if err := w.walk(idx, p, dims); err != nil {
				return fmt.Errorf("route check from member %d, partition %d: %w", i, p, err)
			}
		}
	}

	w.c.Cov.Hit("dp-route-check")

	return nil
}

func (w *DataPlane) walk(from, p uint32, dims int) error {
	st := w.state(from)
	d := st.RoutePartition(p)
	at := from

	for hops := 0; ; hops++ {
		switch d.Kind {
		case route.KindSelf:
			v := st.View()
			if d.Owner.Slot != st.Self() && v.Cell(st.Self()) != v.Cell(d.Owner.Slot) {
				return fmt.Errorf("served at member %d outside the owner's cell", at)
			}

			return nil
		case route.KindRelay, route.KindOwner, route.KindBackup:
		default:
			return fmt.Errorf("%s decision at member %d after %d hops", d.Kind, at, hops)
		}

		if hops >= dims {
			return fmt.Errorf("more than D=%d hops", dims)
		}

		next, ok := w.c.member(d.Next.Addr)
		if !ok || !w.running(next) {
			// Liveness accepts a leaving member that is still in the view
			// after its process stopped (it is removed by failure
			// detection); the walk cannot continue through it.
			if mm, in := st.View().Member(d.Next.Slot); in && w.c.leavingID(mm.ID, mm.Incarnation) {
				*w.Counts.Counter("route-check-leaving")++

				return nil
			}

			return fmt.Errorf("next hop %q is not running", d.Next.Addr)
		}

		if d.Kind == route.KindOwner || d.Kind == route.KindBackup {
			if d.Kind == route.KindBackup && st.Env().Health().Usable(d.Owner.Slot) == health.Healthy {
				return fmt.Errorf("backup chosen at member %d with a healthy owner", at)
			}

			return nil
		}

		at, st = next, w.state(next)

		var err error

		d, err = st.Forward(d.Header)
		if err != nil {
			return fmt.Errorf("forward at member %d: %w", at, err)
		}
	}
}

// checkHedges checks that each member's hedges stay within the budget of
// its routed decisions.
func (w *DataPlane) checkHedges() error {
	ratio := w.hcfg.HedgeRatio
	if ratio <= 0 {
		ratio = 0.05
	}

	for i := range w.mem {
		dm := &w.mem[i]
		if float64(dm.hedges) > ratio*float64(dm.decisions)+1 {
			return fmt.Errorf("dataplane: member %d sent %d hedges for %d routed requests, budget %.0f%%",
				i, dm.hedges, dm.decisions, ratio*100)
		}
	}

	return nil
}

// Tail-latency thresholds (see checkTail).
const (
	// tailMinExpected is the least expected number of slow touches (without
	// protection) for the touch reduction to be judged.
	tailMinExpected = 20
	// tailTouchRatio bounds touched/expected. Over 64 judged runs in a
	// 500-run sweep the ratio was at most 0.30 (mean 0.12); without health
	// protection it is about 1.
	tailTouchRatio = 0.5
	// tailMinSamples is the least number of requests in the baseline and
	// in the judged windows for latency quantiles to be compared.
	tailMinSamples = 200
)

// checkTail checks tail-latency protection in judged slow-member windows
// (client requests that start at least detectWindow after a slow-member
// fault of at least 10x began, with no partition, message loss, or rolling
// restart within the last minute):
//
//   - Touch reduction: the requests that send a hop to a slow member are at
//     most tailTouchRatio of the expected count without protection (the
//     slow share of running members times the mean path length).
//   - p99 is at most HopTimeout plus three times the steady-state p99 of
//     client requests: at the 99th percentile a request pays at most one
//     hop timeout before failing over, and nothing piles up behind slow
//     members.
//   - p50 is at most twice the steady-state p50 plus 1ms.
func (w *DataPlane) checkTail() error {
	if w.slowN == 0 {
		return nil
	}

	w.c.Cov.Hit("dp-tail-judged")

	if exp := w.slowExpect * w.meanHops(); exp >= tailMinExpected && float64(w.slowTouched) > tailTouchRatio*exp {
		return fmt.Errorf("dataplane: %d of %d requests in slow-member windows touched a slow member, expected %.1f without protection (limit %.0f%%)",
			w.slowTouched, w.slowN, exp, tailTouchRatio*100)
	}

	base := &w.ClientSteady
	if base.Count() < tailMinSamples || w.SlowWin.Count() < tailMinSamples {
		return nil
	}

	if p99, limit := w.SlowWin.Quantile(0.99), int64(w.cfg.HopTimeout)+3*base.Quantile(0.99); p99 > limit {
		return fmt.Errorf("dataplane: p99 %v in slow-member windows above %v (baseline p99 %v)",
			time.Duration(p99), time.Duration(limit), time.Duration(base.Quantile(0.99)))
	}

	if p50, limit := w.SlowWin.Quantile(0.5), 2*base.Quantile(0.5)+int64(time.Millisecond); p50 > limit {
		return fmt.Errorf("dataplane: p50 %v in slow-member windows above %v (baseline p50 %v)",
			time.Duration(p50), time.Duration(limit), time.Duration(base.Quantile(0.5)))
	}

	return nil
}

func (w *DataPlane) meanHops() float64 {
	if w.hopsN == 0 {
		return 1
	}

	return float64(w.hopsSum) / float64(w.hopsN)
}

// Summary returns a one-line summary of the run.
func (w *DataPlane) Summary() string {
	meanHops := 0.0
	if w.hopsN > 0 {
		meanHops = float64(w.hopsSum) / float64(w.hopsN)
	}

	return fmt.Sprintf("dp: req=%d served=%d failed=%d deadline=%d ttl=%d noroute=%d hops=%.2f hedges=%d/%d won=%d failovers=%d overload=%d slow=%d/%d(exp %.1f) hop1=%d(exp %.1f) maxStampAge=%v steady[%s] faulty[%s] clientSteady[%s] slowwin[%s]",
		w.Counts.Get("requests"), w.Counts.Get("result-served"), w.Counts.Get("result-failed"),
		w.Counts.Get("result-deadline"), w.Counts.Get("result-ttl-exhausted"), w.Counts.Get("result-no-route"),
		meanHops, w.Counts.Get("hedges"), w.Counts.Get("hedge-denied"), w.Counts.Get("hedge-won"),
		w.Counts.Get("failovers"), w.Counts.Get("overload"), w.slowTouched, w.slowN, w.slowExpect*meanHops, w.slowHop1, w.slowExpect,
		time.Duration(w.maxStampAge), histMS(&w.Steady), histMS(&w.Faulty), histMS(&w.ClientSteady), histMS(&w.SlowWin))
}

func histMS(h *Histogram) string {
	ms := func(v int64) string { return strconv.FormatFloat(float64(v)/1e6, 'f', 1, 64) }

	return fmt.Sprintf("n=%d p50=%s p99=%s max=%s", h.Count(), ms(h.Quantile(0.5)), ms(h.Quantile(0.99)), ms(h.Max()))
}
