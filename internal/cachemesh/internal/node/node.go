// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package node is the per-member cachemesh control-plane state machine.
//
// # Design
//
// A Node owns everything one member knows about the cluster: its current
// membership view and a view.DeltaLog for serving catch-up, an
// elect.Elector, a leader.Leader that plans deltas while this member leads,
// an epoch.Tracker, and a health.Tracker for its data-plane neighbors.
//
//   - Views. Deltas arrive in Broadcast messages and catch-up responses.
//     A delta whose base is the current version is applied; one that is
//     ahead is queued (bounded) and the member catches up from the sender;
//     one that is behind or from a lower term is dropped. When a peer's log
//     no longer covers the requested version it answers with a snapshot.
//     Versions never regress: a snapshot is adopted only when it is newer,
//     or at the same version with a different digest when it comes from
//     the leader (fork repair).
//   - Dissemination. The leader applies its delta and sends Broadcast at
//     wire.LevelLeader. A member receiving level l forwards, using the
//     post-apply view, to one member of every adjacent cell in each
//     dimension j > l (level j), the member with the highest
//     hashing.Score(member.Key, delta.Seq) that the health tracker deems
//     usable, and, unless l is wire.LevelLocal, to its cell-mates at
//     LevelLocal. A failed forward is retried once with the next-ranked
//     member of that cell.
//   - Anti-entropy. Pings carry versions; a member that sees a newer
//     version (Ping, Pong, Broadcast, NoteVersion) catches up from that
//     peer. At most one catch-up is in flight.
//   - Monitoring. Every PingInterval the member pings its view.Subjects.
//     After SuspectAfter consecutive failures it reports Suspect to the
//     leader, repeated every SuspectRefresh while the failure persists.
//   - Joining. A member that is not in a view sends Join and a catch-up
//     request to a random seed every JoinRetry. Seeds come from
//     Config.Seeds or from a single cached Lease read (the leader address
//     and the seeds it publishes). A member with no seeds whose read finds
//     no live leader bootstraps: it becomes a candidate and, once elected,
//     publishes the first view.
//   - Routing to the leader. Join, Leave, Suspect, and Invalidate are
//     wrapped in wire Routed{ToLeader}. They are sent straight to the
//     leader's address when known; a receiver that is not the leader
//     acknowledges and relays to the leader it knows (TTL bounded).
//   - Leadership. On elect.Acquired the node resets its leader.Leader and
//     publishes a delta starting the new term. Every LeaderTick it asks the
//     leader for a delta and publishes it, only while elect.CanPublish
//     holds and its view is still at its own term.
//   - Leaving. Leave sends Leave to the leader until the member observes
//     itself Draining; the leader removes it after DrainFor. A leader that
//     is leaving releases the Lease once its partitions have moved away.
//
// # Contract
//
// A Node is single-threaded and sans-I/O. It reads no clock (time is passed
// in as node-local monotonic nanoseconds and wall time for Lease
// comparisons), uses only the injected random source, starts no goroutines,
// and no output depends on map iteration order. The driver:
//
//   - calls Start once, then Tick whenever NextDeadline has passed;
//   - passes every inbound control request to Handle and sends back the
//     returned response;
//   - drains Outbox, sends each Call, and reports the reply or error (for
//     example a timeout after Call.Timeout) exactly once with OnReply;
//   - drains LeaseOps, executes them against an elect.Store, and reports
//     results with OnLease.
//
// Slices returned by Handle, Outbox, and LeaseOps are owned by the caller.
// Work per call and the size of every queue are bounded.
package node

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/elect"
	"github.com/Azure/unbounded/internal/cachemesh/internal/epoch"
	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
	"github.com/Azure/unbounded/internal/cachemesh/internal/health"
	"github.com/Azure/unbounded/internal/cachemesh/internal/leader"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/wire"
)

// Defaults.
const (
	DefaultPingInterval     = 2 * time.Second
	DefaultPingTimeout      = time.Second
	DefaultSuspectAfter     = 3
	DefaultSuspectRefresh   = 5 * time.Second
	DefaultJoinRetry        = 2 * time.Second
	DefaultCatchUpMaxDeltas = 256
	DefaultCallTimeout      = 2 * time.Second
	DefaultNeighborLinger   = 30 * time.Second
	DefaultLeaderTick       = time.Second
	DefaultTickInterval     = time.Second
	DefaultMaxAhead         = 64
	DefaultRoutedTTL        = 4
	DefaultSeedRefresh      = time.Minute
)

// LeaseSeed is the hashing seed for candidate selection
// (elect.TopCandidates).
var LeaseSeed = hashing.Seed("cachemesh/lease")

// Config configures a Node. Zero values select defaults.
type Config struct {
	ID          string
	Incarnation uint64
	Addr, Zone  string
	// Seeds are control addresses of existing members to join through.
	Seeds []string
	// PartitionBits must match the cluster. Default 20.
	PartitionBits uint8

	Elect  elect.Config
	Leader leader.Config
	Health health.Config

	EpochPeriod      time.Duration
	PingInterval     time.Duration
	PingTimeout      time.Duration
	SuspectAfter     int
	SuspectRefresh   time.Duration
	JoinRetry        time.Duration
	CatchUpMaxDeltas int
	CallTimeout      time.Duration
	DeltaLogSize     int
	NeighborLinger   time.Duration
	LeaderTick       time.Duration
	// TickInterval is the cadence of elector and health ticks.
	TickInterval time.Duration
	// MaxAhead bounds queued deltas that are ahead of the view.
	MaxAhead int

	// Initial, when set, is the view the member starts from, as if it had
	// adopted it from a snapshot (for example a persisted view, or a view
	// prebuilt by the simulator). The member still joins if it is not in it.
	Initial *view.View

	// Shared, when set, deduplicates views, encoded deltas, snapshots, and
	// candidate lists across nodes in one process (the simulator). It is
	// not safe for concurrent use.
	Shared *Shared

	Hooks Hooks
}

func (c *Config) defaults() {
	dur := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	num := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}

	if c.PartitionBits == 0 {
		c.PartitionBits = leader.DefaultPartitionBits
	}

	dur(&c.EpochPeriod, epoch.DefaultPeriod)
	dur(&c.PingInterval, DefaultPingInterval)
	dur(&c.PingTimeout, DefaultPingTimeout)
	num(&c.SuspectAfter, DefaultSuspectAfter)
	dur(&c.SuspectRefresh, DefaultSuspectRefresh)
	dur(&c.JoinRetry, DefaultJoinRetry)
	num(&c.CatchUpMaxDeltas, DefaultCatchUpMaxDeltas)
	dur(&c.CallTimeout, DefaultCallTimeout)
	num(&c.DeltaLogSize, view.DefaultDeltaLogSize)
	dur(&c.NeighborLinger, DefaultNeighborLinger)
	dur(&c.LeaderTick, DefaultLeaderTick)
	dur(&c.TickInterval, DefaultTickInterval)
	num(&c.MaxAhead, DefaultMaxAhead)

	c.Elect = c.Elect.WithDefaults()
	c.Leader.Self = leader.Member{ID: c.ID, Incarnation: c.Incarnation, Addr: c.Addr, Zone: c.Zone}
	c.Leader.PartitionBits = c.PartitionBits
	c.Leader.EpochPeriod = c.EpochPeriod
}

// CatchUpKind says how a catch-up was served.
type CatchUpKind uint8

// Catch-up kinds.
const (
	CatchUpDeltas CatchUpKind = iota + 1
	CatchUpSnapshot
	CatchUpEmpty
)

// Hooks observe a Node for metrics, tests, and simulation coverage. Nil
// fields are skipped. Hooks run synchronously inside Node methods and must
// not call back into the Node.
type Hooks struct {
	// OnView is called after every view change.
	OnView func(v *view.View)
	// OnNeighbors reports data-plane neighbors added and removed (after
	// NeighborLinger).
	OnNeighbors func(added, removed []view.Slot)
	// OnPublish is called when this member publishes a delta as leader.
	OnPublish func(d *view.Delta)
	// OnCatchUp is called when a catch-up response is applied.
	OnCatchUp func(kind CatchUpKind)
	// OnMessage is called for every handled inbound message kind.
	OnMessage func(kind wire.Kind)
	// OnEvent reports named internal events (for coverage), for example
	// "join-sent", "suspect-sent", "fork", "ahead-queued", "relay".
	OnEvent func(name string)
	// Leader and Elect are installed on the embedded leader and elector.
	Leader leader.Hooks
	Elect  elect.Hooks
}

// Call is an outbound control request.
type Call struct {
	ID      uint64
	To      string
	Msg     []byte
	Timeout time.Duration
}

type callKind uint8

const (
	callPing callKind = iota + 1
	callBroadcast
	callCatchUp
	callRouted
)

type pending struct {
	kind    callKind
	slot    view.Slot // ping subject or broadcast target
	key     uint64
	sentAt  int64
	level   int8
	cell    uint32
	attempt uint8
	enc     []byte // broadcast delta
	seq     uint64 // broadcast delta seq
	addr    string // catch-up peer
}

type subject struct {
	slot        view.Slot
	key         uint64
	fails       int
	inflight    bool
	lastSuspect int64
}

type neighbor struct {
	slot    view.Slot
	key     uint64
	cell    uint32
	lingers int64 // 0 while current; removal time while lingering
}

type aheadDelta struct {
	d     *view.Delta
	enc   []byte
	level int8
}

const ownLeaseBit = uint64(1) << 63

// Node is the control-plane state machine of one member.
type Node struct {
	cfg  Config
	rng  *rand.Rand
	key  uint64
	self wire.MemberRef

	v    *view.View
	log  *view.DeltaLog
	slot view.Slot

	el     *elect.Elector
	ld     *leader.Leader
	ep     epoch.Tracker
	hl     *health.Tracker
	shared *Shared

	started  bool
	now      int64
	wall     time.Time
	nextTick int64
	nextPing int64

	// Leadership.
	leading      bool
	term         uint64
	startTerm    bool
	nextLeader   int64
	heardAt      int64
	leaseSeeds   []string
	nextSeedsAt  int64
	candidate    bool
	candKey      view.Digest
	candEpoch    uint64
	releasing    bool
	bootstrapper bool

	// Cold start: a starter whose seeds and Lease holder are all dead.
	coldStart   bool
	coldSeen    bool
	coldRV      string
	coldSince   int64
	lastContact int64 // last reply from a peer that has a view

	// Joining and leaving.
	seeds       []string
	joined      bool // was a member of some view
	nextJoin    int64
	joinTries   int
	ownReadSeq  uint64
	ownReadDone bool
	leaving     bool
	left        bool
	handedOff   bool   // gave up leadership once while leaving
	leaveEpoch  uint64 // epoch at Leave
	nextLeave   int64
	leaderAddr  string // last known leader address hint

	// Catch-up.
	catchUpInFlight bool
	catchUpSentAt   int64
	ahead           []aheadDelta
	// snapFrom is the peer the current view's snapshot came from ("" if
	// it came from the leader of that view, or the view was built from
	// deltas). unconfirmed is set while such a snapshot has had no delta
	// applied on top of it, since its version and digest are trusted on
	// decode; badBase is set when a delta failed its digest check against
	// the current view.
	snapFrom      string
	unconfirmed   bool
	unconfirmedAt int64
	badBase       bool
	forwarded     view.Version

	// Monitoring and neighbors.
	subjects  []subject
	neighbors []neighbor
	lingerAt  int64 // earliest linger expiry, 0 if none

	// I/O.
	nextCall uint64
	calls    map[uint64]*pending
	outbox   []Call
	leaseOps []elect.Op

	// Snapshot cache.
	snapDigest view.Digest
	snapBytes  []byte

	msg  wire.Message
	resp wire.Message
	buf  []view.Slot
}

// New returns a Node. rng must not be nil.
func New(cfg Config, rng *rand.Rand) *Node {
	cfg.defaults()

	n := &Node{
		cfg:    cfg,
		rng:    rng,
		key:    hashing.MemberKey(cfg.ID, cfg.Incarnation),
		v:      view.Genesis(),
		log:    view.NewDeltaLog(cfg.DeltaLogSize),
		slot:   view.NoSlot,
		shared: cfg.Shared,
		calls:  map[uint64]*pending{},
		seeds:  slices.Clone(cfg.Seeds),
	}
	n.self = wire.MemberRef{ID: cfg.ID, Incarnation: cfg.Incarnation, Slot: view.NoSlot}
	n.el = elect.New(cfg.Elect, elect.Self{ID: cfg.ID, Incarnation: cfg.Incarnation, Key: n.key, Addr: cfg.Addr}, rng)
	n.el.SetHooks(cfg.Hooks.Elect)
	n.ld = leader.New(cfg.Leader, rng)
	n.ld.SetHooks(cfg.Hooks.Leader)
	n.hl = health.NewTracker(cfg.Health, rng)
	n.ep.Anchor(0, cfg.EpochPeriod, 0)

	return n
}

func (n *Node) event(name string) {
	if n.cfg.Hooks.OnEvent != nil {
		n.cfg.Hooks.OnEvent(name)
	}
}

// View returns the current view.
func (n *Node) View() *view.View { return n.v }

// Self returns this member's slot in the current view, or view.NoSlot.
func (n *Node) Self() view.Slot { return n.slot }

// Key returns this member's key.
func (n *Node) Key() uint64 { return n.key }

// Health returns the neighbor health tracker.
func (n *Node) Health() *health.Tracker { return n.hl }

// Epoch returns the epoch tracker.
func (n *Node) Epoch() *epoch.Tracker { return &n.ep }

// Elector returns the elector (read-only use).
func (n *Node) Elector() *elect.Elector { return n.el }

// Leading reports whether this member currently publishes deltas, and the
// term it publishes under.
func (n *Node) Leading() (bool, uint64) { return n.leading, n.term }

// Leaving reports whether Leave was called; Left whether the member has
// observed its own removal after leaving.
func (n *Node) Leaving() bool { return n.leaving }

// Left reports whether the member observed its removal after Leave.
func (n *Node) Left() bool { return n.left }

// Neighbors returns the current (non-lingering) data-plane neighbor slots
// in ascending order.
func (n *Node) Neighbors(dst []view.Slot) []view.Slot {
	for _, nb := range n.neighbors {
		if nb.lingers == 0 {
			dst = append(dst, nb.slot)
		}
	}

	return dst
}

// PendingCalls returns the number of outbound calls awaiting OnReply.
func (n *Node) PendingCalls() int { return len(n.calls) }

// Outbox drains queued outbound calls.
func (n *Node) Outbox() []Call {
	out := n.outbox
	n.outbox = nil

	return out
}

// LeaseOps drains queued Lease operations.
func (n *Node) LeaseOps() []elect.Op {
	out := n.leaseOps
	n.leaseOps = nil

	return out
}

// NextDeadline returns the earliest local time at which Tick must run.
func (n *Node) NextDeadline() int64 {
	d := min(n.nextTick, n.nextPing)

	if n.leading {
		d = min(d, n.nextLeader)
	}

	if !n.isMember() && n.started {
		d = min(d, n.nextJoin)
	}

	if n.leaving && !n.left {
		d = min(d, n.nextLeave)
	}

	return d
}

func (n *Node) isMember() bool { return n.slot != view.NoSlot }

// Start starts the member at local time now.
func (n *Node) Start(now int64, wall time.Time) {
	if n.started {
		return
	}

	n.started = true
	n.now, n.wall = now, wall

	if iv := n.cfg.Initial; iv != nil {
		n.cfg.Initial = nil
		n.adoptSnapshot(now, iv)
		n.heardAt = 0
	}

	n.nextTick = now
	n.nextPing = now + n.rng.Int64N(int64(n.cfg.PingInterval))
	n.nextJoin = now

	if len(n.seeds) == 0 && !n.joined {
		n.ownReadSeq = ownLeaseBit | 1
		n.leaseOps = append(n.leaseOps, elect.Op{Seq: n.ownReadSeq, Kind: elect.OpGetCached})
		n.event("lease-seed-read")
	}

	n.Tick(now, wall)
}

// Leave starts a graceful departure.
func (n *Node) Leave(now int64) {
	if n.leaving {
		return
	}

	n.leaving = true
	n.leaveEpoch = n.ep.Current()
	n.nextLeave = now
	n.event("leave")
}

// Tick runs due timers at local time now.
func (n *Node) Tick(now int64, wall time.Time) {
	if !n.started {
		return
	}

	n.now, n.wall = max(n.now, now), wall
	now = n.now

	n.ep.Advance(now)

	if now >= n.nextTick {
		n.nextTick = now + int64(n.cfg.TickInterval)
		n.hl.Tick(now)
		n.expireCalls(now)
		n.expireNeighbors(now)
		n.tickElector(now, wall)
	}

	if n.leading && now >= n.nextLeader {
		n.nextLeader = now + int64(n.cfg.LeaderTick)
		n.leaderTick(now)
	}

	if !n.isMember() && now >= n.nextJoin {
		n.nextJoin = now + int64(n.cfg.JoinRetry)
		n.sendJoin(now)
	}

	if n.leaving && !n.left && now >= n.nextLeave {
		n.nextLeave = now + int64(n.cfg.JoinRetry)
		n.sendLeave(now)
	}

	if now >= n.nextPing {
		n.nextPing = now + int64(n.cfg.PingInterval)
		n.pingSubjects(now)
	}

	if n.catchUpInFlight && now-n.catchUpSentAt > 2*int64(n.cfg.CallTimeout) {
		n.catchUpInFlight = false
	}

	if n.unconfirmed && now-n.unconfirmedAt > n.confirmAfter() {
		n.unconfirmedAt = now
		n.event("snapshot-unconfirmed")
		n.recoverBase(now, "")
	}
}

func (n *Node) expireCalls(now int64) {
	limit := 4 * int64(max(n.cfg.CallTimeout, n.cfg.PingTimeout))

	for id, p := range n.calls {
		if now-p.sentAt > limit {
			delete(n.calls, id)
			n.fail(p)
		}
	}
}

// --- Outbound ---

func (n *Node) header(m *wire.Message, k wire.Kind) {
	m.Reset()
	m.Kind = k
	m.From = n.self
	m.Version = n.v.Version()
	m.Digest = n.v.Digest()
	m.Epoch = n.ep.Current()
	m.LeaderSlot = n.v.Leader()
}

// MaxPendingCalls bounds outbound calls awaiting a reply. Beyond it new
// calls are dropped (the senders retry on their own timers), so a flood of
// inbound messages that each cause a relay cannot grow memory without
// bound.
const MaxPendingCalls = 4096

func (n *Node) send(to string, m *wire.Message, p *pending, timeout time.Duration) bool {
	if to == "" || to == n.cfg.Addr {
		return false
	}

	if len(n.calls) >= MaxPendingCalls {
		n.event("call-dropped")

		return false
	}

	n.nextCall++
	id := n.nextCall
	p.sentAt = n.now
	n.calls[id] = p
	n.outbox = append(n.outbox, Call{ID: id, To: to, Msg: wire.Append(nil, m), Timeout: timeout})

	return true
}

func (n *Node) addrOf(s view.Slot) string {
	m, ok := n.v.Member(s)
	if !ok {
		return ""
	}

	return m.Addr
}

// leaderTarget returns the address of the leader to route to, "" if this
// member is the leader, and false if no leader is known.
func (n *Node) leaderTarget() (string, bool) {
	if n.leading {
		return "", true
	}

	if l := n.v.Leader(); l != view.NoSlot && l != n.slot {
		if a := n.addrOf(l); a != "" {
			return a, true
		}
	}

	if li, ok := n.el.Leader(); ok && li.Addr != "" && li.Addr != n.cfg.Addr {
		return li.Addr, true
	}

	if n.leaderAddr != "" && n.leaderAddr != n.cfg.Addr {
		return n.leaderAddr, true
	}

	return "", false
}

// toLeader routes inner (an encoded Join, Leave, Suspect, or Invalidate)
// to the leader via addr, handling it locally when leading.
func (n *Node) toLeader(now int64, addr string, inner *wire.Message) {
	if n.leading && addr == "" {
		n.leaderInput(now, inner)

		return
	}

	var r wire.Message
	n.header(&r, wire.KindRouted)
	r.Origin = n.self
	r.ToLeader = true
	r.TTL = DefaultRoutedTTL
	r.Inner = wire.Append(nil, inner)
	n.send(addr, &r, &pending{kind: callRouted}, n.cfg.CallTimeout)
}

func (n *Node) sendJoin(now int64) {
	if len(n.seeds) == 0 && !n.leading {
		if n.ownReadDone && n.joinTries%16 == 15 {
			n.rereadLease()
		}

		n.joinTries++

		return
	}

	n.joinTries++

	var j wire.Message
	n.header(&j, wire.KindJoin)
	j.Member = n.self
	j.Addr, j.Zone, j.PartitionBits = n.cfg.Addr, n.cfg.Zone, n.cfg.PartitionBits

	if n.leading {
		n.leaderInput(now, &j)

		return
	}

	seed := n.seeds[n.rng.IntN(len(n.seeds))]
	if seed == n.cfg.Addr {
		return
	}

	// Send the join straight to the leader when one is known (from the
	// newest view, the Lease, or a Pong hint).
	to := seed

	if a, ok := n.leaderTarget(); ok && a != "" {
		to = a
	}

	n.toLeader(now, to, &j)
	n.catchUp(now, seed, false)
	n.event("join-sent")

	// Seeds may all be gone: refresh them from the Lease now and then.
	if n.joinTries%16 == 15 {
		n.rereadLease()
	}
}

// dropSeed forgets a join seed that did not answer, keeping at least one.
func (n *Node) dropSeed(addr string) {
	if n.isMember() || len(n.seeds) <= 1 {
		return
	}

	n.seeds = slices.DeleteFunc(n.seeds, func(s string) bool { return s == addr })
	if len(n.seeds) == 0 {
		n.seeds = append(n.seeds, addr)
	}
}

// addSeed remembers a live peer as a join seed while not a member.
func (n *Node) addSeed(addr string) {
	if n.isMember() || addr == "" || addr == n.cfg.Addr || slices.Contains(n.seeds, addr) {
		return
	}

	if len(n.seeds) >= maxSeeds {
		n.seeds = slices.Delete(n.seeds, 0, 1)
	}

	n.seeds = append(n.seeds, addr)
}

func (n *Node) rereadLease() {
	n.ownReadSeq = (n.ownReadSeq + 1) | ownLeaseBit
	n.ownReadDone = false
	n.leaseOps = append(n.leaseOps, elect.Op{Seq: n.ownReadSeq, Kind: elect.OpGetCached})
	n.event("lease-seed-read")
}

func (n *Node) sendLeave(now int64) {
	if n.isMember() && n.v.State(n.slot) == view.Draining {
		// Release once the partitions have moved away. After a first
		// hand-off that nobody took, release again only for a member
		// that became Active since this one started leaving, so is alive.
		if n.leading && n.v.Load(n.slot) == 0 && !n.releasing && (!n.handedOff || n.activeSince(n.leaveEpoch)) {
			n.releasing, n.handedOff = true, true
			n.leaseOps = append(n.leaseOps, n.el.Release(n.electInput(now, n.wall))...)
			n.event("leader-release")
		}

		return
	}

	if !n.isMember() {
		return
	}

	var m wire.Message
	n.header(&m, wire.KindLeave)
	m.Member = n.self

	addr, ok := n.leaderTarget()
	if !ok {
		return
	}

	n.toLeader(now, addr, &m)
}

// Invalidate asks the leader to bump the generation of keyspace seed.
func (n *Node) Invalidate(now int64, seed uint64) {
	var m wire.Message
	n.header(&m, wire.KindInvalidate)
	m.Seed = seed

	addr, ok := n.leaderTarget()
	if !ok {
		return
	}

	n.toLeader(now, addr, &m)
}

// catchUp asks addr for what follows the current view. At most one
// catch-up is in flight unless force is set (recovery from a bad base,
// which must not wait behind a request to the peer that caused it).
func (n *Node) catchUp(now int64, addr string, force bool) {
	if addr == "" || addr == n.cfg.Addr || (n.catchUpInFlight && !force) {
		return
	}

	var m wire.Message
	n.header(&m, wire.KindCatchUpRequest)
	m.Have, m.HaveDigest = n.v.Version(), n.v.Digest()
	m.MaxDeltas = uint32(n.cfg.CatchUpMaxDeltas)

	if n.send(addr, &m, &pending{kind: callCatchUp, addr: addr}, n.cfg.CallTimeout) {
		n.catchUpInFlight = true
		n.catchUpSentAt = now
	}
}

// NoteVersion reports that the data plane saw a member at version ver. The
// data-plane header carries only the low 32 bits of the term, so ver is a
// hint: it may trigger a catch-up but never a leader step-down (see
// observeTerm, which only trusts full 64-bit terms from control messages).
func (n *Node) NoteVersion(now int64, from view.Slot, ver view.Version) {
	if n.v.Version().Less(ver) {
		n.catchUp(now, n.addrOf(from), false)
	}
}

// --- Inbound ---

// Handle processes one inbound control request from transport address
// from and returns the encoded response.
func (n *Node) Handle(now int64, from string, msg []byte) []byte {
	n.now = max(n.now, now)
	now = n.now

	m := &n.msg
	if err := wire.Decode(msg, m); err != nil {
		return n.nack(wire.ReasonBadRequest)
	}

	if n.cfg.Hooks.OnMessage != nil {
		n.cfg.Hooks.OnMessage(m.Kind)
	}

	n.observeTerm(now, m.Version.Term, from)

	switch m.Kind {
	case wire.KindPing:
		n.noteRemote(now, from, m)

		var r wire.Message
		n.header(&r, wire.KindPong)

		if addr, ok := n.leaderTarget(); ok {
			if addr == "" {
				addr = n.cfg.Addr
			}

			r.LeaderAddr = addr
		}

		return wire.Append(nil, &r)
	case wire.KindBroadcast:
		n.onBroadcast(now, from, m)

		return n.ack()
	case wire.KindCatchUpRequest:
		return n.serveCatchUp(m)
	case wire.KindRouted:
		return n.onRouted(now, m)
	case wire.KindJoin, wire.KindLeave, wire.KindSuspect, wire.KindInvalidate:
		// Sent directly rather than routed: treat like a routed message.
		inner := *m
		inner.Detach()

		addr, ok := n.leaderTarget()
		if !ok {
			return n.nack(wire.ReasonNotLeader)
		}

		n.toLeader(now, addr, &inner)

		return n.ack()
	default:
		return n.nack(wire.ReasonBadRequest)
	}
}

func (n *Node) ack() []byte {
	n.header(&n.resp, wire.KindAck)

	return wire.Append(nil, &n.resp)
}

func (n *Node) nack(r wire.Reason) []byte {
	n.header(&n.resp, wire.KindNack)
	n.resp.Reason = r

	return wire.Append(nil, &n.resp)
}

// noteRemote compares a peer's version with ours and catches up if the
// peer is ahead.
func (n *Node) noteRemote(now int64, addr string, m *wire.Message) {
	mine := n.v.Version()

	switch {
	case mine.Less(m.Version):
		n.catchUp(now, addr, false)
	case mine == m.Version && m.Digest != n.v.Digest() && !mine.IsZero():
		// Same version, different digest: a fork. Resync from the leader.
		n.event("fork")

		if l := n.v.Leader(); l != n.slot {
			n.catchUp(now, n.addrOf(l), false)
		}
	}
}

func (n *Node) onRouted(now int64, m *wire.Message) []byte {
	if !m.ToLeader {
		return n.nack(wire.ReasonNoRoute)
	}

	var inner wire.Message
	if err := wire.Decode(m.Inner, &inner); err != nil {
		return n.nack(wire.ReasonBadRequest)
	}

	if n.leading {
		n.leaderInput(now, &inner)

		return n.ack()
	}

	if m.TTL <= 1 {
		return n.nack(wire.ReasonTTLExhausted)
	}

	addr, ok := n.leaderTarget()
	if !ok {
		return n.nack(wire.ReasonNotLeader)
	}

	var r wire.Message
	n.header(&r, wire.KindRouted)
	r.Origin = m.Origin
	r.ToLeader = true
	r.TTL = m.TTL - 1
	r.Hops = m.Hops + 1
	r.Inner = bytes.Clone(m.Inner)
	n.send(addr, &r, &pending{kind: callRouted}, n.cfg.CallTimeout)
	n.event("relay")

	return n.ack()
}

// leaderInput feeds a leader-bound message to the leader logic.
func (n *Node) leaderInput(now int64, m *wire.Message) {
	if n.cfg.Hooks.OnEvent != nil {
		n.event("leader-input-" + m.Kind.String())
	}

	switch m.Kind {
	case wire.KindJoin:
		if m.PartitionBits != n.cfg.PartitionBits {
			n.event("join-rejected")

			return
		}

		n.ld.Join(now, leader.Member{ID: m.Member.ID, Incarnation: m.Member.Incarnation, Addr: m.Addr, Zone: m.Zone})
	case wire.KindLeave:
		n.ld.Leave(now, m.Member.ID, m.Member.Incarnation)
	case wire.KindSuspect:
		n.ld.Suspect(now, m.Target.Slot, hashing.MemberKey(m.Target.ID, m.Target.Incarnation), m.Observer.Slot)
	case wire.KindInvalidate:
		n.ld.Invalidate(m.Seed)
	}
}

// OnReply completes outbound call id with its response or an error.
func (n *Node) OnReply(now int64, id uint64, resp []byte, err error) {
	n.now = max(n.now, now)
	now = n.now

	p, ok := n.calls[id]
	if !ok {
		return
	}

	delete(n.calls, id)

	var m *wire.Message

	if err == nil {
		m = &n.msg
		if wire.Decode(resp, m) != nil {
			m = nil
		}
	}

	if m != nil {
		addr := p.addr
		if addr == "" && p.slot != view.NoSlot && (p.kind == callPing || p.kind == callBroadcast) {
			addr = n.addrOf(p.slot)
		}

		// Contact with the cluster means a reply from a member of a view.
		// Peers that only hold a view they are not in (all restarted after
		// the members died) do not count, or they would hold each other
		// back from a cold start forever.
		if m.From.Slot != view.NoSlot {
			n.lastContact = now
		}

		n.observeTerm(now, m.Version.Term, addr)
	}

	if m == nil || m.Kind == wire.KindNack {
		if m != nil && p.kind == callCatchUp {
			n.catchUpInFlight = false
			n.event("catchup-nack")

			return
		}

		n.fail(p)

		return
	}

	switch p.kind {
	case callPing:
		n.pingDone(now, p, true, time.Duration(now-p.sentAt))

		if m.Kind == wire.KindPong {
			if m.LeaderAddr != "" && !n.isMember() {
				n.leaderAddr = string([]byte(m.LeaderAddr))
			}

			n.noteRemote(now, n.addrOf(p.slot), m)
		}
	case callCatchUp:
		n.catchUpInFlight = false

		if m.Kind == wire.KindCatchUpResponse {
			n.onCatchUp(now, m, p.addr)
		}
	case callBroadcast:
		if p.slot != view.NoSlot && n.hl != nil {
			n.hl.Observe(p.slot, health.Success, time.Duration(now-p.sentAt))
		}
	case callRouted:
	}
}

// fail handles a failed call.
func (n *Node) fail(p *pending) {
	switch p.kind {
	case callPing:
		n.pingDone(n.now, p, false, 0)
	case callCatchUp:
		n.catchUpInFlight = false
		n.event("catchup-failed")
		n.dropSeed(p.addr)
	case callBroadcast:
		if p.attempt == 0 && p.level != wire.LevelLocal {
			n.event("broadcast-retry")
			n.forwardCell(p.enc, p.seq, p.level, p.cell, 1, p.slot)
		}
	case callRouted:
	}
}

// --- Hardening ---

// confirmAfter is how long a snapshot from a peer other than its leader may
// go without a delta on top before the member re-fetches its view from
// another source. A live leader publishes at least one heartbeat per epoch
// period.
func (n *Node) confirmAfter() int64 {
	return 2*int64(n.cfg.EpochPeriod) + 2*int64(n.cfg.CallTimeout)
}

// recoverBase re-fetches the view after the current one proved bad (a delta
// failed its digest check against it) or stayed unconfirmed, from a source
// other than the peer that served it: from (the peer whose delta failed),
// then the leader, then a seed.
func (n *Node) recoverBase(now int64, from string) {
	n.badBase = true
	n.event("bad-base")
	n.catchUp(now, n.repairSource(from), true)
}

func (n *Node) repairSource(from string) string {
	cands := []string{from, n.addrOf(n.v.Leader())}

	if li, ok := n.el.Leader(); ok {
		cands = append(cands, li.Addr)
	}

	cands = append(cands, n.leaderAddr)
	cands = append(cands, n.seeds...)

	for _, a := range cands {
		if a != "" && a != n.cfg.Addr && a != n.snapFrom {
			return a
		}
	}

	return ""
}

// observeTerm handles a full 64-bit term seen in a control message or
// delta from addr. A leader that sees a term above its own steps down at
// once and gives the Lease back; every member remembers the term so its
// next acquisition starts above it, and catches up from addr when the term
// is above its view's. Data-plane notes, whose terms are truncated to 32
// bits, never come here.
func (n *Node) observeTerm(now int64, term uint64, addr string) {
	if term <= n.v.Term() && (!n.leading || term <= n.term) {
		return
	}

	if term >= elect.MaxSaneTerm {
		n.event("absurd-term")

		return
	}

	n.el.ObserveTerm(term)

	if n.leading && term > n.term {
		n.resign(now, "higher-term")
	}

	if term > n.v.Term() {
		n.catchUp(now, addr, false)
	}
}

// resign stops publishing and releases the Lease if the elector holds it.
func (n *Node) resign(now int64, reason string) {
	n.stopLeading(reason)
	n.leaseOps = append(n.leaseOps, n.el.Release(n.electInput(now, n.wall))...)
}

// noteColdRead tracks this member's own Lease reads while it is not a
// member of a view. It enables a cold start, which lets the member take
// part in the lottery and, if it wins, publish a first view (or a new term
// of the view it holds), once the Lease shows no sign of life and no member
// has answered: the record (or its absence) stayed unchanged for
// LeaseDuration of local time across reads, it is expired by wall time
// beyond LeaseDuration+ClockSkewAllowance, and no reply from a peer that is
// a member of its view arrived for LeaseDuration. Requiring an
// unchanged resourceVersion on the local monotonic clock, as candidates do,
// keeps a member with a skewed wall clock from stealing a live Lease.
func (n *Node) noteColdRead(now int64, wall time.Time, r elect.Result) {
	if n.isMember() || n.coldStart || n.bootstrapper || n.leaving {
		return
	}

	notFound := errors.Is(r.Err, elect.ErrNotFound)
	if r.Err != nil && !notFound {
		return
	}

	key := "-"
	if !notFound {
		key = r.Record.UID + "/" + r.Record.ResourceVersion
	}

	if !n.coldSeen || key != n.coldRV {
		n.coldSeen, n.coldRV, n.coldSince = true, key, now

		return
	}

	ld := n.cfg.Elect.LeaseDuration
	if !notFound {
		ld = max(ld, r.Record.LeaseDuration)
	}

	switch {
	case now-n.coldSince < int64(ld), now-n.lastContact < int64(ld):
		return
	case !notFound && r.Record.Holder != "" && !wall.After(r.Record.RenewTime.Add(ld+n.cfg.Elect.ClockSkewAllowance)):
		return
	}

	n.coldStart = true
	n.event("cold-start")
}

// Stats reports the sizes of the Node's internal collections, for tests
// and monitoring. Every one of them is bounded.
type Stats struct {
	Calls, Outbox, LeaseOps         int
	Ahead, Seeds, LeaseSeeds, Log   int
	Subjects, Neighbors             int
	Joins, Leaves, Suspects, Invals int
	Unconfirmed, BadBase, ColdStart bool
	// Lingering counts removed neighbors kept for NeighborLinger;
	// LingerOverdue reports one kept past its expiry by more than a tick.
	Lingering     int
	LingerOverdue bool
}

// Stats returns the current collection sizes.
func (n *Node) Stats() Stats {
	j, l, s, i := n.ld.Pending()
	lingering, overdue := 0, false

	for _, nb := range n.neighbors {
		if nb.lingers != 0 {
			lingering++
			overdue = overdue || n.now > nb.lingers+int64(n.cfg.TickInterval)
		}
	}

	return Stats{
		Lingering: lingering, LingerOverdue: overdue,
		Calls: len(n.calls), Outbox: len(n.outbox), LeaseOps: len(n.leaseOps),
		Ahead: len(n.ahead), Seeds: len(n.seeds), LeaseSeeds: len(n.leaseSeeds), Log: n.log.Len(),
		Subjects: len(n.subjects), Neighbors: len(n.neighbors),
		Joins: j, Leaves: l, Suspects: s, Invals: i,
		Unconfirmed: n.unconfirmed, BadBase: n.badBase, ColdStart: n.coldStart,
	}
}

// activeSince reports whether the view has another Active member whose
// state changed at or after epoch ep.
func (n *Node) activeSince(ep uint64) bool {
	found := false

	n.v.ForEachMember(func(s view.Slot, m *view.Member) bool {
		found = s != n.slot && m.State == view.Active && m.Since >= ep

		return !found
	})

	return found
}
