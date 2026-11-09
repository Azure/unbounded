// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Leader election. This file is the cachemesh leader election state machine.
//
// # Design
//
// One Kubernetes Lease names the cachemesh leader. Almost every member is a
// Follower and makes no API calls at all. A handful of members (the top
// Config.Candidates by view.Score(leaseSeed, member.Key) among Active
// members admitted at least MinCandidateAge ago, see TopCandidates) are
// Candidates: they read the Lease with a consistent Get every RetryPeriod
// (jittered) and take it with a compare-and-swap Update once it has not
// changed for LeaseDuration of their own local monotonic time, or as soon as
// it is missing (Create) or released (empty Holder). The Leader renews every
// RetryPeriod. When no leader has been heard from for NoLeaderTimeout (plus a
// per-elector jitter), or the member has no view at all, a non-candidate
// enters the Lottery: in each LotteryRound it is eligible with probability
// p = min(1, LotteryBudgetQPS*LotteryRound/N), decided by
// view.Below(view.Score(self.Key, round), p64), and an eligible member
// issues one cached read. A live holder is reported with a Discovered event
// so the node can catch up from its Addr and Seeds; a holder expired by
// RenewTime+LeaseDuration+ClockSkewAllowance of wall time is taken with a
// compare-and-swap Update. Across N members this bounds reads during leader
// loss at about LotteryBudgetQPS.
//
// # Contract
//
// The Elector is sans-I/O and deterministic. It never reads a clock, never
// uses global randomness, starts no goroutines and keeps no maps. The caller
// passes an Input to Tick and OnResult, executes the returned Op against a
// Store (see Execute), and feeds the Result back with OnResult. At most one
// Op is outstanding; each Op has a fresh Seq and results with any other Seq
// are ignored, as are results for an Op that was abandoned after OpTimeout or
// by a state change. Events (Acquired, Lost, Discovered) are queued and
// drained with Events. On Acquired the node publishes NewTerm; on Lost it
// stops publishing at once. Before every publish the node must call
// CanPublish with its current monotonic time.
//
// # Safety
//
// A leadership is anchored at the local monotonic send time of the last
// successful write (the acquiring Update or Create, or a renewal). The leader
// considers itself leader only while now-anchor < RenewDeadline: CanPublish
// returns false and the next Tick emits Lost as soon as that bound passes,
// even if a renewal is still outstanding, and late results are ignored.
//
// A candidate only takes a Lease whose ResourceVersion it has observed
// unchanged for LeaseDuration of its own local time, measured from when the
// read result arrived. That read happened after the leader's last successful
// write was applied, which happened after the leader sent it. So the takeover
// is sent at least LeaseDuration after the old anchor, while the old leader
// stopped by anchor+RenewDeadline. With RenewDeadline < LeaseDuration, and
// clock rate drift small enough that LeaseDuration-RenewDeadline covers it,
// the two leaderships do not overlap in global time. Precisely, if local
// monotonic clocks run at rates in [rmin, rmax] of real time, safety needs
// LeaseDuration/rmax > RenewDeadline/rmin; with the defaults (15s, 10s) the
// rate ratio rmax/rmin must stay below 1.5, about +-20% drift. A simulator
// that drifts clocks further must widen the gap between the two. The compare-and-swap
// guarantees that no write (renewal or other takeover) landed between the
// observation and the takeover; a renewal still in flight fails with a
// conflict on one side or the other.
//
// A lottery member compares wall clocks instead, because it has not watched
// the Lease. The holder writes RenewTime as its wall time at send. The lottery
// member takes the Lease only when its wall time exceeds
// RenewTime+LeaseDuration+ClockSkewAllowance and its compare-and-swap against
// the read ResourceVersion succeeds (a stale cached read simply conflicts).
// If wall skew between members is below ClockSkewAllowance, the takeover is
// sent more than LeaseDuration after the holder's anchor, so the same
// argument applies.
//
// A leader paused by the runtime (stop-the-world, SIGSTOP, VM migration)
// resumes with a monotonic clock that has advanced past its anchor, so
// CanPublish fails on wake and the first Tick emits Lost. The node must call
// CanPublish immediately before each publish; a pause between that call and
// the actual send is outside what this package can prevent and is covered by
// receivers fencing on terms.
//
// Terms increase across acquisitions: the new term is
// max(record.Term, ViewTerm, last term held)+1, written in the same
// compare-and-swap that takes the Lease, so every stored term is larger than
// the one it replaced.
//
// A record held by self.ID with a different Incarnation belongs to a previous
// process of this member, or to another process if IDs collide. It is treated
// like any other holder: the candidate waits LeaseDuration. Only a record held
// by this exact incarnation (left behind after this Elector stepped down or
// abandoned an Update whose result was lost) may be retaken immediately,
// because the only process that could publish under it is this one, and it is
// not publishing.
package control

import (
	"context"
	"errors"
	"math"
	"math/bits"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// ElectionConfig tunes the Elector. Zero values select defaults.
type ElectionConfig struct {
	// LeaseDuration is how long a record stays valid after a renewal.
	// Default 15s.
	LeaseDuration time.Duration
	// RenewDeadline bounds the time a leader keeps leading without a
	// successful renewal. Must be below LeaseDuration. Default 10s.
	RenewDeadline time.Duration
	// RetryPeriod is the candidate read and leader renewal interval.
	// RetryPeriod*1.2 must be below RenewDeadline. Default 2s.
	RetryPeriod time.Duration
	// RetryJitterPercent adds up to this percentage of RetryPeriod to each
	// interval. Default 20; negative disables jitter.
	RetryJitterPercent int
	// OpTimeout abandons an outstanding operation whose result has not
	// arrived. Default RetryPeriod.
	OpTimeout time.Duration
	// NoLeaderTimeout is how long a non-candidate waits without hearing
	// from a leader before entering the lottery. Default 30s.
	NoLeaderTimeout time.Duration
	// NoLeaderJitterPercent adds up to this percentage of NoLeaderTimeout,
	// chosen once per Elector. Default 50; negative disables jitter.
	NoLeaderJitterPercent int
	// LotteryRound is the lottery round length. Default 1s.
	LotteryRound time.Duration
	// LotteryBudgetQPS is the target cluster-wide read rate during leader
	// loss. Default 20.
	LotteryBudgetQPS int
	// ClockSkewAllowance is the wall clock skew tolerated between members
	// when judging Lease expiry from RenewTime. Default 5s.
	ClockSkewAllowance time.Duration
	// Candidates is the number of candidates. Default 3.
	Candidates int
	// MinCandidateAge is how long a member must have been admitted before it
	// can be a candidate. The caller applies it. Default 60s.
	MinCandidateAge time.Duration

	// expectedSize is the least N for the lottery (see Tick). The Node sets
	// it from Config.ExpectedSize. Default 1000.
	expectedSize int
}

// Defaults.
const (
	DefaultLeaseDuration         = 15 * time.Second
	DefaultRenewDeadline         = 10 * time.Second
	DefaultRetryPeriod           = 2 * time.Second
	DefaultRetryJitterPercent    = 20
	DefaultNoLeaderTimeout       = 30 * time.Second
	DefaultNoLeaderJitterPercent = 50
	DefaultLotteryRound          = time.Second
	DefaultLotteryBudgetQPS      = 20
	DefaultClockSkewAllowance    = 5 * time.Second
	DefaultCandidates            = 3
	DefaultMinCandidateAge       = 60 * time.Second
	DefaultExpectedSize          = 1000
)

// WithDefaults returns c with zero fields replaced by defaults.
func (c ElectionConfig) WithDefaults() ElectionConfig {
	setDur := func(v *time.Duration, d time.Duration) {
		if *v == 0 {
			*v = d
		}
	}
	setInt := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}

	setDur(&c.LeaseDuration, DefaultLeaseDuration)
	setDur(&c.RenewDeadline, DefaultRenewDeadline)
	setDur(&c.RetryPeriod, DefaultRetryPeriod)
	setInt(&c.RetryJitterPercent, DefaultRetryJitterPercent)
	setDur(&c.OpTimeout, c.RetryPeriod)
	setDur(&c.NoLeaderTimeout, DefaultNoLeaderTimeout)
	setInt(&c.NoLeaderJitterPercent, DefaultNoLeaderJitterPercent)
	setDur(&c.LotteryRound, DefaultLotteryRound)
	setInt(&c.LotteryBudgetQPS, DefaultLotteryBudgetQPS)
	setDur(&c.ClockSkewAllowance, DefaultClockSkewAllowance)
	setInt(&c.Candidates, DefaultCandidates)
	setDur(&c.MinCandidateAge, DefaultMinCandidateAge)
	setInt(&c.expectedSize, DefaultExpectedSize)

	return c
}

// Validate checks c after defaults are applied.
func (c ElectionConfig) Validate() error {
	c = c.WithDefaults()

	switch {
	case c.LeaseDuration < 0 || c.RenewDeadline < 0 || c.RetryPeriod < 0 || c.OpTimeout < 0 ||
		c.NoLeaderTimeout < 0 || c.LotteryRound < 0 || c.ClockSkewAllowance < 0 || c.MinCandidateAge < 0:
		return errors.New("elect: durations must not be negative")
	case c.RenewDeadline >= c.LeaseDuration:
		return errors.New("elect: RenewDeadline must be less than LeaseDuration")
	case c.RetryPeriod*6/5 >= c.RenewDeadline:
		return errors.New("elect: RetryPeriod*1.2 must be less than RenewDeadline")
	case c.LotteryBudgetQPS < 0 || c.Candidates < 0 || c.expectedSize < 0:
		return errors.New("elect: counts must not be negative")
	case c.RetryJitterPercent > 100 || c.NoLeaderJitterPercent > 100:
		return errors.New("elect: jitter percentages must be at most 100")
	}

	return nil
}

// Self identifies the local member.
type Self struct {
	ID          string
	Incarnation uint64
	// Key is view.MemberKey(ID, Incarnation).
	Key uint64
	// Addr is the control-plane address published when leader.
	Addr string
}

// ElectorState is the elector state.
type ElectorState uint8

// States.
const (
	StateFollower ElectorState = iota
	StateCandidate
	StateLeader
	StateLottery
)

// String returns the state name.
func (s ElectorState) String() string {
	switch s {
	case StateFollower:
		return "follower"
	case StateCandidate:
		return "candidate"
	case StateLeader:
		return "leader"
	case StateLottery:
		return "lottery"
	default:
		return "unknown"
	}
}

// Input is the caller's view of the world at one call.
type Input struct {
	// Now is the local monotonic time in nanoseconds.
	Now int64
	// Wall is the local wall clock, possibly skewed.
	Wall time.Time
	// ViewTerm is the term of the member's view, 0 without a view.
	ViewTerm uint64
	// HaveView reports whether the member has a view.
	HaveView bool
	// ClusterSize is the member count of the view. The lottery uses the
	// larger of it and Config.ExpectedSize.
	ClusterSize int
	// IsCandidate reports whether the member is a candidate in its view
	// (see TopCandidates).
	IsCandidate bool
	// LeaderHeardAt is the monotonic time of the last delta or heartbeat
	// with a current term, 0 if never.
	LeaderHeardAt int64
	// Seeds are published in the Lease while leader, at most MaxSeeds.
	Seeds []string
}

// LeaderInfo is the last known Lease holder.
type LeaderInfo struct {
	ID          string
	Incarnation uint64
	Term        uint64
	Addr        string
	Seeds       []string
	RenewTime   time.Time
}

// EventKind identifies an Event.
type EventKind uint8

// Event kinds.
const (
	// Acquired: this member became leader for Event.Term.
	Acquired EventKind = iota + 1
	// Lost: this member stopped being leader for Event.Term.
	Lost
	// Discovered: a read revealed a holder, in Event.Record.
	Discovered
)

// LostReason explains a Lost event.
type LostReason uint8

// Lost reasons.
const (
	// LostDeadline: no successful renewal within RenewDeadline.
	LostDeadline LostReason = iota + 1
	// LostConflict: a renewal hit a compare-and-swap conflict or the Lease
	// was deleted.
	LostConflict
	// LostReleased: Release was called.
	LostReleased
)

// String returns the reason name.
func (r LostReason) String() string {
	switch r {
	case LostDeadline:
		return "deadline"
	case LostConflict:
		return "conflict"
	case LostReleased:
		return "released"
	default:
		return "unknown"
	}
}

// Event is emitted by the Elector and drained with Events.
type Event struct {
	Kind   EventKind
	Term   uint64
	Reason LostReason
	Record Record
}

// ElectorHooks observe the Elector for metrics and coverage. Nil fields are skipped.
type ElectorHooks struct {
	OnState func(from, to ElectorState)
	OnOp    func(kind OpKind)
}

type opPurpose uint8

const (
	purposeRead opPurpose = iota + 1
	purposeAcquire
	purposeRenew
	purposeRelease
)

// Elector is the election state machine. It is not safe for concurrent use.
type Elector struct {
	cfg   ElectionConfig
	self  Self
	rng   *rand.Rand
	hooks ElectorHooks

	state   ElectorState
	started bool
	startAt int64

	seq         uint64
	outstanding bool
	outPurpose  opPurpose
	outSentAt   int64
	nextOpAt    int64

	// Candidate observation.
	observed          Record
	haveObserved      bool
	observedNotFound  bool
	observedChangedAt int64
	takeNow           bool

	// Lottery.
	lotteryJitter int64
	lastRound     uint64
	roundDone     bool

	// Leadership.
	term     uint64
	maxTerm  uint64
	anchor   int64
	held     Record
	leader   LeaderInfo
	haveLead bool

	events []Event
}

// NewElector returns an Elector. It panics if cfg is invalid (see Config.Validate)
// or rng is nil.
func NewElector(cfg ElectionConfig, self Self, rng *rand.Rand) *Elector {
	if err := cfg.Validate(); err != nil {
		panic(err)
	}

	if rng == nil {
		panic("elect: nil rng")
	}

	cfg = cfg.WithDefaults()
	e := &Elector{cfg: cfg, self: self, rng: rng}
	e.lotteryJitter = jitter(rng, int64(cfg.NoLeaderTimeout), cfg.NoLeaderJitterPercent)

	return e
}

// SetHooks installs hooks.
func (e *Elector) SetHooks(h ElectorHooks) { e.hooks = h }

// State returns the current state.
func (e *Elector) State() ElectorState { return e.state }

// Term returns the term while leader, 0 otherwise.
func (e *Elector) Term() uint64 {
	if e.state != StateLeader {
		return 0
	}

	return e.term
}

// Leader returns the last known holder as of the last read or write, and
// whether one is known. A released Lease reports no holder.
func (e *Elector) Leader() (LeaderInfo, bool) {
	li := e.leader
	li.Seeds = slices.Clone(li.Seeds)

	return li, e.haveLead
}

// Events drains queued events.
func (e *Elector) Events() []Event {
	ev := e.events
	e.events = nil

	return ev
}

// CanPublish reports whether the member is leader and may publish at local
// monotonic time now. It does not change state; Tick does.
func (e *Elector) CanPublish(now int64) bool {
	return e.state == StateLeader && now-e.anchor < int64(e.cfg.RenewDeadline)
}

func jitter(rng *rand.Rand, d int64, percent int) int64 {
	if percent <= 0 || d <= 0 {
		return 0
	}

	return rng.Int64N(d*int64(percent)/100 + 1)
}

func (e *Elector) retryAt(now int64) int64 {
	p := int64(e.cfg.RetryPeriod)
	return now + p + jitter(e.rng, p, e.cfg.RetryJitterPercent)
}

func (e *Elector) setState(to ElectorState) {
	if e.state == to {
		return
	}

	from := e.state
	e.state = to

	if e.hooks.OnState != nil {
		e.hooks.OnState(from, to)
	}
}

func (e *Elector) stepDown(reason LostReason) {
	e.events = append(e.events, Event{Kind: Lost, Term: e.term, Reason: reason})
	e.outstanding = false
	e.setState(StateFollower)
}

func (e *Elector) issue(now int64, kind OpKind, purpose opPurpose, r Record) []Op {
	e.seq++
	e.outstanding = true
	e.outPurpose = purpose
	e.outSentAt = now

	if e.hooks.OnOp != nil {
		e.hooks.OnOp(kind)
	}

	return []Op{{Seq: e.seq, Kind: kind, Record: r}}
}

// LotteryP64 returns p*2^64 (saturating) for the lottery probability
// p = min(1, qps*round/n), using integer math only.
func LotteryP64(qps int, round time.Duration, n int) uint64 {
	if n < 1 {
		n = 1
	}

	if qps <= 0 || round <= 0 {
		return 0
	}

	num := uint64(qps) * uint64(round)
	den := uint64(time.Second) * uint64(n)

	hi, _ := bits.Mul64(uint64(qps), uint64(round))
	if hi != 0 || num >= den {
		return math.MaxUint64
	}

	q, _ := bits.Div64(num, 0, den)

	return q
}

func (e *Elector) desiredState(in Input) ElectorState {
	if in.IsCandidate {
		return StateCandidate
	}

	heard := max(in.LeaderHeardAt, e.startAt)
	if !in.HaveView || in.Now-heard > int64(e.cfg.NoLeaderTimeout)+e.lotteryJitter {
		return StateLottery
	}

	return StateFollower
}

// Tick advances the Elector to in.Now and returns at most one Op to execute.
func (e *Elector) Tick(in Input) []Op {
	if !e.started {
		e.started = true
		e.startAt = in.Now
	}

	if e.outstanding && in.Now-e.outSentAt >= int64(e.cfg.OpTimeout) {
		e.outstanding = false
	}

	if e.state == StateLeader {
		if !e.CanPublish(in.Now) {
			e.stepDown(LostDeadline)
		} else {
			if e.outstanding || in.Now < e.nextOpAt {
				return nil
			}

			e.nextOpAt = e.retryAt(in.Now)

			return e.issue(in.Now, OpUpdate, purposeRenew, e.renewRecord(in))
		}
	}

	to := e.desiredState(in)
	if to != e.state {
		if to == StateLottery {
			e.roundDone = false
		}

		if to == StateCandidate {
			e.nextOpAt = in.Now
		}

		e.setState(to)
	}

	if e.outstanding {
		return nil
	}

	switch e.state {
	case StateCandidate:
		if in.Now < e.nextOpAt {
			return nil
		}

		if e.takeNow {
			e.takeNow = false
			return e.acquire(in, e.observed, e.observedNotFound)
		}

		e.nextOpAt = e.retryAt(in.Now)

		return e.issue(in.Now, OpGet, purposeRead, Record{})
	case StateLottery:
		if e.takeNow {
			e.takeNow = false
			return e.acquire(in, e.observed, e.observedNotFound)
		}

		round := uint64(in.Now / int64(e.cfg.LotteryRound))
		if e.roundDone && round == e.lastRound {
			return nil
		}

		e.lastRound, e.roundDone = round, true

		// N is the larger of the view's size and expectedSize. A
		// view can be far smaller than the cluster (a fresh lineage after
		// a cold restart or a partition, a member that missed joins), and
		// with N from it alone every member of such a view would read every
		// round.
		n := e.cfg.expectedSize
		if in.HaveView {
			n = max(n, in.ClusterSize)
		}

		p64 := LotteryP64(e.cfg.LotteryBudgetQPS, e.cfg.LotteryRound, n)
		if !view.Below(view.Score(e.self.Key, round), p64) {
			return nil
		}

		return e.issue(in.Now, OpGetCached, purposeRead, Record{})
	default:
		return nil
	}
}

func (e *Elector) renewRecord(in Input) Record {
	r := e.held.Clone()
	r.Holder = e.self.ID
	r.Incarnation = e.self.Incarnation
	r.Term = e.term
	r.Addr = e.self.Addr
	r.Seeds = seedsOr(in.Seeds, r.Seeds)
	r.LeaseDuration = e.cfg.LeaseDuration
	r.RenewTime = in.Wall

	return r
}

// seedsOr returns s capped, or keep when s is empty: a holder that knows
// no seeds (a member without a view that won the lottery) must not erase
// the seeds through which joiners reach the existing cluster.
func seedsOr(s, keep []string) []string {
	if len(s) == 0 {
		return capSeeds(keep)
	}

	return capSeeds(s)
}

// otherSeeds returns s without this member's address, capped.
func (e *Elector) otherSeeds(s []string) []string {
	var out []string

	for _, a := range s {
		if a != e.self.Addr && len(out) < MaxSeeds {
			out = append(out, a)
		}
	}

	return out
}

func capSeeds(s []string) []string {
	if len(s) > MaxSeeds {
		s = s[:MaxSeeds]
	}

	return slices.Clone(s)
}

func (e *Elector) acquire(in Input, base Record, notFound bool) []Op {
	bt := base.Term
	if bt >= MaxSaneTerm {
		// A corrupt annotation; reading it as 0 is safe (see ObserveTerm).
		bt = 0
	}

	term := max(bt, in.ViewTerm, e.maxTerm) + 1
	if notFound {
		term = max(in.ViewTerm, e.maxTerm) + 1
		base = Record{}
	}

	r := base.Clone()
	if r.Holder != e.self.ID || r.Incarnation != e.self.Incarnation {
		r.Transitions++
	}

	r.Holder = e.self.ID
	r.Incarnation = e.self.Incarnation
	r.Term = term
	r.Addr = e.self.Addr
	r.Seeds = seedsOr(in.Seeds, r.Seeds)
	r.LeaseDuration = e.cfg.LeaseDuration
	r.AcquireTime = in.Wall
	r.RenewTime = in.Wall

	if notFound {
		r.Transitions = 0
		return e.issue(in.Now, OpCreate, purposeAcquire, r)
	}

	return e.issue(in.Now, OpUpdate, purposeAcquire, r)
}

// MaxSaneTerm bounds the terms the Elector believes. A real cluster never
// gets near it (one term per election); a larger value can only come from a
// corrupt or hostile record or message, and accepting it would let the
// next term overflow.
const MaxSaneTerm = uint64(1) << 62

// ObserveTerm records a term seen outside the Lease, for example in a view
// or control message of another lineage. The next acquisition uses a term
// above it, and a Release carries it, so a Lease whose term annotation was
// lost or corrupted (read as 0) cannot make a new leader reuse a term.
// Terms at or above MaxSaneTerm are ignored so the next term cannot
// overflow.
func (e *Elector) ObserveTerm(t uint64) {
	if t >= MaxSaneTerm {
		return
	}

	e.maxTerm = max(e.maxTerm, t)
}

// MaxTerm returns the highest term seen in the Lease, held, or observed.
func (e *Elector) MaxTerm() uint64 { return max(e.maxTerm, e.term) }

// Release gives up leadership for a graceful leave. If leader, it steps down
// at once (emitting Lost{LostReleased}) and returns an Update that clears
// Holder so candidates can take the Lease without waiting. Otherwise it
// returns nil.
//
// The released record keeps seeds other than this member: in.Seeds, or the
// held record's seeds when in.Seeds names no other member. A released Lease
// without seeds tells a starting member that no cluster exists and that it
// may bootstrap one, so seeds are cleared only when this member was the
// last one known (it leaves, and nobody else is listed). Clearing them
// whenever a member released (for example a member without a view that won
// the lottery and declined) let a fresh member bootstrap a second cluster
// next to the existing one.
func (e *Elector) Release(in Input) []Op {
	if e.state != StateLeader {
		return nil
	}

	r := e.held.Clone()
	r.Holder = ""
	r.RenewTime = in.Wall
	// Seeds stay (see above): a member starting without seeds must still
	// find the cluster through a released Lease rather than bootstrap a
	// new one.
	r.Seeds = e.otherSeeds(in.Seeds)
	if len(r.Seeds) == 0 {
		r.Seeds = e.otherSeeds(e.held.Seeds)
	}
	// Carry the highest term this member has seen (see ObserveTerm) so the
	// next holder starts above every lineage, even one whose term never
	// reached the Lease.
	r.Term = max(e.term, e.maxTerm)

	ops := e.issue(in.Now, OpUpdate, purposeRelease, r)
	e.events = append(e.events, Event{Kind: Lost, Term: e.term, Reason: LostReleased})
	e.setState(StateFollower)

	return ops
}

func sameObservation(a, b Record) bool {
	if a.ResourceVersion != "" || b.ResourceVersion != "" {
		return a.ResourceVersion == b.ResourceVersion && a.UID == b.UID
	}

	return a.Holder == b.Holder && a.Incarnation == b.Incarnation && a.Term == b.Term && a.RenewTime.Equal(b.RenewTime)
}

func (e *Elector) noteHolder(r Record) {
	if r.Holder == "" {
		e.haveLead = false
		e.leader = LeaderInfo{}

		return
	}

	changed := !e.haveLead || e.leader.ID != r.Holder || e.leader.Incarnation != r.Incarnation || e.leader.Term != r.Term
	e.leader = LeaderInfo{
		ID: r.Holder, Incarnation: r.Incarnation, Term: r.Term,
		Addr: r.Addr, Seeds: slices.Clone(r.Seeds), RenewTime: r.RenewTime,
	}
	e.haveLead = true

	if r.Term < MaxSaneTerm {
		e.maxTerm = max(e.maxTerm, r.Term)
	}

	if changed && (r.Holder != e.self.ID || r.Incarnation != e.self.Incarnation) {
		e.events = append(e.events, Event{Kind: Discovered, Term: r.Term, Record: r.Clone()})
	}
}

// OnResult feeds back the result of an Op returned by Tick or Release.
func (e *Elector) OnResult(in Input, res Result) {
	if !e.outstanding || res.Seq != e.seq {
		return
	}

	e.outstanding = false

	switch e.outPurpose {
	case purposeRelease:
		if res.Err == nil {
			e.noteHolder(res.Record)
		}
	case purposeRenew:
		e.onRenew(in, res)
	case purposeAcquire:
		e.onAcquire(in, res)
	case purposeRead:
		e.onRead(in, res)
	}
}

func (e *Elector) onRenew(in Input, res Result) {
	if e.state != StateLeader {
		return
	}

	switch {
	case res.Err == nil:
		e.anchor = e.outSentAt
		e.held = res.Record.Clone()
		e.noteHolder(res.Record)
	case errors.Is(res.Err, ErrConflict) || errors.Is(res.Err, ErrNotFound):
		e.stepDown(LostConflict)
		e.nextOpAt = in.Now
	default:
		// Transient: retry at the next RetryPeriod. The deadline check in
		// Tick bounds how long this can go on.
	}
}

func (e *Elector) onAcquire(in Input, res Result) {
	if res.Err != nil {
		e.haveObserved = false
		e.nextOpAt = min(e.nextOpAt, in.Now)

		return
	}

	// A late success is not trusted: the deadline counts from send time.
	if in.Now-e.outSentAt >= int64(e.cfg.RenewDeadline) {
		e.haveObserved = false
		return
	}

	e.held = res.Record.Clone()
	e.term = res.Record.Term
	e.maxTerm = max(e.maxTerm, e.term)
	e.anchor = e.outSentAt
	e.noteHolder(res.Record)
	e.haveObserved = false
	e.setState(StateLeader)
	e.events = append(e.events, Event{Kind: Acquired, Term: e.term})
	e.nextOpAt = e.retryAt(in.Now)
}

func (e *Elector) onRead(in Input, res Result) {
	if e.state != StateCandidate && e.state != StateLottery {
		return
	}

	notFound := errors.Is(res.Err, ErrNotFound)
	if res.Err != nil && !notFound {
		return
	}

	if notFound {
		if !e.haveObserved || !e.observedNotFound {
			e.observedChangedAt = in.Now
		}

		e.observed, e.haveObserved, e.observedNotFound = Record{}, true, true
		e.takeNow = true
		e.nextOpAt = in.Now

		return
	}

	r := res.Record
	if !e.haveObserved || e.observedNotFound || !sameObservation(e.observed, r) {
		e.observedChangedAt = in.Now
	}

	e.observed, e.haveObserved, e.observedNotFound = r.Clone(), true, false

	ld := max(r.LeaseDuration, e.cfg.LeaseDuration)
	self := r.Holder == e.self.ID && r.Incarnation == e.self.Incarnation

	if e.state == StateLottery {
		live := r.Holder != "" && in.Wall.Before(r.RenewTime.Add(ld+e.cfg.ClockSkewAllowance))
		if live {
			e.noteHolder(r)
		}

		e.takeNow = !live || r.Holder == "" || self

		return
	}

	e.noteHolder(r)

	if r.Holder == "" || self || in.Now-e.observedChangedAt >= int64(ld) {
		e.takeNow = true
		e.nextOpAt = in.Now
	}
}

// TopCandidates returns the n best keys by view.Score(leaseSeed, key),
// ordered best first with view.Better. The caller passes only eligible
// keys (Active members admitted at least MinCandidateAge ago). keys is not
// modified; duplicates are kept.
func TopCandidates(leaseSeed uint64, keys []uint64, n int) []uint64 {
	if n <= 0 {
		return nil
	}

	out := make([]uint64, 0, min(n, len(keys)))
	for _, k := range keys {
		s := view.Score(leaseSeed, k)

		i := len(out)
		for i > 0 && view.Better(s, k, view.Score(leaseSeed, out[i-1]), out[i-1]) {
			i--
		}

		if i >= n {
			continue
		}

		if len(out) < n {
			out = append(out, 0)
		}

		copy(out[i+1:], out[i:len(out)-1])
		out[i] = k
	}

	return out
}

// Record is the cachemesh view of the leader Lease. kubelease maps it onto a
// coordination/v1 Lease: Holder, LeaseDuration, AcquireTime, RenewTime and
// Transitions live in the Lease spec; Incarnation, Term, Addr and Seeds live
// in annotations; ResourceVersion and UID come from object metadata.
type Record struct {
	// Holder is the MemberID of the leader, empty if the Lease was released.
	Holder string
	// Incarnation of the holder process.
	Incarnation uint64
	// Term is the cachemesh leader term the holder publishes under.
	Term uint64
	// Addr is the holder's control-plane address.
	Addr string
	// Seeds are up to MaxSeeds control addresses of members chosen by the
	// leader for joiners to contact.
	Seeds []string
	// LeaseDuration is how long the record stays valid after RenewTime.
	LeaseDuration time.Duration
	// AcquireTime is the wall time the holder took the Lease.
	AcquireTime time.Time
	// RenewTime is the wall time of the last renewal.
	RenewTime time.Time
	// Transitions counts holder changes.
	Transitions int32
	// ResourceVersion is the server-assigned version used for compare-and-swap.
	ResourceVersion string
	// UID is the server-assigned object identity. A Lease that is deleted and
	// recreated gets a new UID.
	UID string
}

// MaxSeeds bounds Record.Seeds.
const MaxSeeds = 16

// Clone returns a deep copy of r.
func (r Record) Clone() Record {
	r.Seeds = slices.Clone(r.Seeds)
	return r
}

// OpKind identifies a Lease operation.
type OpKind uint8

const (
	// OpGet is a consistent (quorum) read.
	OpGet OpKind = iota + 1
	// OpGetCached is a read that may be served from the API server's watch
	// cache (resourceVersion=0). It may return stale data.
	OpGetCached
	// OpCreate creates the Lease. It fails with ErrConflict if it exists.
	OpCreate
	// OpUpdate replaces the Lease if its ResourceVersion still equals
	// Op.Record.ResourceVersion. It fails with ErrConflict on mismatch and
	// ErrNotFound if the Lease does not exist.
	OpUpdate
)

// String returns the operation name.
func (k OpKind) String() string {
	switch k {
	case OpGet:
		return "get"
	case OpGetCached:
		return "get-cached"
	case OpCreate:
		return "create"
	case OpUpdate:
		return "update"
	default:
		return "unknown"
	}
}

// Op is a Lease operation requested by the Elector. The runtime executes it
// against a Store and feeds the Result back with the same Seq.
type Op struct {
	// Seq identifies the operation; results with a stale Seq are ignored.
	Seq  uint64
	Kind OpKind
	// Record is the desired state for OpCreate and OpUpdate.
	Record Record
}

// Result is the outcome of an Op.
type Result struct {
	Seq  uint64
	Kind OpKind
	// Record is the Lease as stored after the operation, valid when Err is nil.
	Record Record
	// Err is nil, ErrNotFound, ErrConflict, or any other (transient) error.
	Err error
}

var (
	// ErrNotFound reports that the Lease does not exist.
	ErrNotFound = errors.New("cachemesh: lease not found")
	// ErrConflict reports a failed compare-and-swap or a create of an
	// existing Lease.
	ErrConflict = errors.New("cachemesh: lease conflict")
)

// Store executes Lease operations. Implementations must map "not found" to
// ErrNotFound and optimistic-concurrency failures to ErrConflict (wrapping is
// allowed; callers use errors.Is).
type Store interface {
	// Get reads the Lease with a consistent read.
	Get(ctx context.Context) (Record, error)
	// GetCached reads the Lease from the API server cache.
	GetCached(ctx context.Context) (Record, error)
	// Create creates the Lease and returns the stored record.
	Create(ctx context.Context, r Record) (Record, error)
	// Update replaces the Lease if r.ResourceVersion matches.
	Update(ctx context.Context, r Record) (Record, error)
}

// Execute runs op against s.
func Execute(ctx context.Context, s Store, op Op) Result {
	res := Result{Seq: op.Seq, Kind: op.Kind}

	switch op.Kind {
	case OpGet:
		res.Record, res.Err = s.Get(ctx)
	case OpGetCached:
		res.Record, res.Err = s.GetCached(ctx)
	case OpCreate:
		res.Record, res.Err = s.Create(ctx, op.Record)
	case OpUpdate:
		res.Record, res.Err = s.Update(ctx, op.Record)
	default:
		res.Err = errors.New("cachemesh: unknown lease op")
	}

	return res
}

// --- Election and leadership ---

func (n *Node) electInput(now int64, wall time.Time) Input {
	_, has := n.v.Config()
	in := Input{
		Now: now, Wall: wall,
		ViewTerm:    n.v.Term(),
		HaveView:    has,
		ClusterSize: n.v.Len(),
		// A bootstrapper (no seeds, no Lease) is not a candidate: it takes
		// the Lease through the lottery, so a cluster whose members all
		// start at once without seeds makes about LotteryBudgetQPS Lease
		// calls, not one per member per RetryPeriod.
		IsCandidate:   n.isCandidate(),
		LeaderHeardAt: n.heardAt,
		Seeds:         n.leaseSeeds,
	}

	// A joiner that has seeds is in contact with the cluster and needs no
	// Lease reads: keep it a follower. Otherwise a joiner without a view
	// would sit in the lottery and, with a skewed wall clock, take a live
	// leader's Lease only to give it back. A joiner that holds a configured
	// view reports when it last heard the leader (heardAt is refreshed by
	// every version it applies, heartbeats included), so if the whole
	// membership of its lineage is gone (every member restarted with a new
	// incarnation while no leader could be elected) it enters the lottery
	// after NoLeaderTimeout and the winner admits itself into that lineage.
	if !n.isMember() && len(n.seeds) > 0 && !n.bootstrapper && !n.coldStart && !n.leading {
		in.HaveView, in.LeaderHeardAt = true, now

		if has {
			in.LeaderHeardAt = n.heardAt // zero counts from the elector's start
		}
	}

	// Likewise while this member's own Lease read is outstanding: its
	// result decides whether to join, bootstrap, or cold start, and a
	// lottery read in the meantime would only duplicate it.
	if !n.isMember() && n.ownReadSeq != 0 && !n.ownReadDone && !n.bootstrapper && !n.coldStart && !n.leading {
		in.HaveView, in.LeaderHeardAt = true, now
	}

	return in
}

func (n *Node) isCandidate() bool {
	if !n.isMember() || n.leaving {
		return false
	}

	ep := n.ep.Current()
	if n.candKey == n.v.Digest() && n.candEpoch == ep {
		return n.candidate
	}

	n.candKey, n.candEpoch = n.v.Digest(), ep
	top := n.topCandidates(ep)
	n.candidate = slices.Contains(top, n.key)

	return n.candidate
}

func (n *Node) topCandidates(ep uint64) []uint64 {
	k := candKey{digest: n.v.Digest(), epoch: ep}
	if s := n.shared; s != nil {
		if t, ok := s.cand.Get(k); ok {
			return t
		}
	}

	per := n.v.Period()
	if per <= 0 {
		per = n.cfg.EpochPeriod
	}

	minAge := uint64(n.cfg.Election.MinCandidateAge / per)

	var keys []uint64

	n.v.ForEachMember(func(_ view.Slot, m *view.Member) bool {
		if m.State == view.Active && m.Joined+minAge <= ep {
			keys = append(keys, m.Key)
		}

		return true
	})

	top := TopCandidates(LeaseSeed, keys, n.cfg.Election.Candidates)

	if s := n.shared; s != nil {
		s.cand.Put(k, top)
	}

	return top
}

func (n *Node) tickElector(now int64, wall time.Time) {
	if n.leading && now >= n.nextSeedsAt {
		n.nextSeedsAt = now + int64(DefaultSeedRefresh)
		n.pickSeeds()
	}

	n.leaseOps = append(n.leaseOps, n.el.Tick(n.electInput(now, wall))...)
	n.electEvents(now)

	// The Lease names a leader of a later term than our view (for example
	// after a partition healed into two lineages): catch up from it. This
	// costs no API calls; the elector already read the Lease.
	if li, ok := n.el.Leader(); ok && !n.leading && li.Term > n.v.Term() && li.Addr != "" && li.Addr != n.cfg.Addr {
		n.event("lease-newer-term")
		n.catchUp(now, li.Addr, false)
	}
}

// seedsFromView uses the leader and a few random members of the current
// view as join seeds.
func (n *Node) seedsFromView() {
	n.addSeed(n.addrOf(n.v.Leader()))

	if c := n.v.SlotCap(); c > 0 {
		for range 4 {
			n.addSeed(n.addrOf(view.Slot(n.rng.Uint32N(c))))
		}
	}
}

// maxSeeds bounds the join seed list.
const maxSeeds = 32

// pickSeeds chooses up to MaxSeeds random member addresses for the
// Lease.
func (n *Node) pickSeeds() {
	n.leaseSeeds = n.leaseSeeds[:0]

	cap := n.v.SlotCap()
	if cap == 0 {
		return
	}

	for range 4 * MaxSeeds {
		if len(n.leaseSeeds) == MaxSeeds {
			break
		}

		s := view.Slot(n.rng.Uint32N(cap))
		if m, ok := n.v.Member(s); ok && m.State == view.Active && !slices.Contains(n.leaseSeeds, m.Addr) {
			n.leaseSeeds = append(n.leaseSeeds, m.Addr)
		}
	}
}

// OnLease reports the result of a Lease operation from LeaseOps.
func (n *Node) OnLease(now int64, wall time.Time, r Result) {
	n.now = max(n.now, now)
	now = n.now

	if r.Seq&ownLeaseBit != 0 {
		if r.Seq != n.ownReadSeq || n.ownReadDone {
			return
		}

		n.ownReadDone = true
		defer n.noteColdRead(now, wall, r)

		// Any record that names a holder or seeds means a cluster exists
		// (or existed): join through it rather than judging liveness with
		// a possibly skewed wall clock. Only a missing Lease, or a released
		// one without seeds, makes this member bootstrap a cluster.
		if r.Err == nil && (r.Record.Holder != "" || len(r.Record.Seeds) > 0) {
			n.learnSeeds(r.Record.Addr, r.Record.Seeds)
		}

		switch {
		case len(n.seeds) > 0:
		case r.Err == nil || errors.Is(r.Err, ErrNotFound):
			if _, has := n.v.Config(); !has && len(n.cfg.Seeds) == 0 && !n.joined {
				n.bootstrapper = true
				n.event("bootstrap-candidate")
			}
		}

		return
	}

	n.el.OnResult(n.electInput(now, wall), r)
	n.electEvents(now)
}

func (n *Node) learnSeeds(addr string, seeds []string) {
	if addr != "" && addr != n.cfg.Addr {
		n.leaderAddr = addr
	}

	if n.isMember() {
		return
	}

	for _, s := range append([]string{addr}, seeds...) {
		if s != "" && s != n.cfg.Addr && !slices.Contains(n.seeds, s) && len(n.seeds) < 4*MaxSeeds {
			n.seeds = append(n.seeds, s)
		}
	}

	n.bootstrapper = false
}

func (n *Node) electEvents(now int64) {
	for _, e := range n.el.Events() {
		switch e.Kind {
		case Acquired:
			n.startLeading(now, e.Term)
		case Lost:
			n.stopLeading("lost")
		case Discovered:
			// A live holder: join it rather than cold start.
			n.coldStart = false
			n.learnSeeds(e.Record.Addr, e.Record.Seeds)

			if n.isMember() && n.v.Term() < e.Record.Term {
				n.catchUp(now, e.Record.Addr, false)
			}
		}
	}
}

func (n *Node) startLeading(now int64, term uint64) {
	_, has := n.v.Config()
	if n.leaving && (!n.isMember() || !n.handedOff) {
		// The acquire was in flight when Leave was called (or a lottery
		// read found the Lease free): a leaving member hands leadership
		// off once by giving the Lease straight back, which any candidate
		// takes at once. If it wins the Lease again, nobody else could
		// take it, so it leads (while still a member) rather than leave
		// the cluster without a leader; see sendLeave for when it lets go.
		n.handedOff = true
		n.event("acquire-declined-leaving")
		n.leaseOps = append(n.leaseOps, n.el.Release(n.electInput(now, n.wall))...)

		return
	}

	if term <= n.v.Term() || (!has && !n.bootstrapper && !n.coldStart) {
		// Cannot start this term from our view (or must not bootstrap a
		// second cluster): give the Lease back.
		n.event("acquire-declined")
		n.leaseOps = append(n.leaseOps, n.el.Release(n.electInput(now, n.wall))...)

		return
	}

	n.leading, n.term, n.startTerm = true, term, true
	n.releasing = false
	n.ld.Reset()
	n.nextLeader = now
	n.nextSeedsAt = now
	n.event("term-start")
}

func (n *Node) stopLeading(reason string) {
	if !n.leading {
		return
	}

	n.leading, n.startTerm = false, false
	n.event("leader-stop-" + reason)
}

func (n *Node) leaderTick(now int64) {
	if !n.el.CanPublish(now) {
		n.stopLeading("deadline")

		return
	}

	var newTerm uint64

	if n.startTerm {
		newTerm = n.term
	} else if n.v.Term() != n.term {
		n.resign(now, "fenced")

		return
	}

	ops := n.ld.Tick(now, n.ep.Advance(now), n.v, newTerm)
	if ops == nil {
		return
	}

	d := view.NewDelta(n.v, ops...)
	raw := view.EncodeDelta(d)

	if err := n.applyOne(now, d, raw); err != nil {
		// A bug in the leader logic; never publish an invalid delta.
		n.event("leader-invalid-delta")

		return
	}

	n.startTerm = false

	if n.cfg.Hooks.OnPublish != nil {
		n.cfg.Hooks.OnPublish(d)
	}

	n.forward(d, n.encoded(d, raw), LevelLeader)
}
