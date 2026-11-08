// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package elect is the cachemesh leader election state machine.
//
// # Design
//
// One Kubernetes Lease names the cachemesh leader. Almost every member is a
// Follower and makes no API calls at all. A handful of members (the top
// Config.Candidates by hashing.Score(leaseSeed, member.Key) among Active
// members admitted at least MinCandidateAge ago, see TopCandidates) are
// Candidates: they read the Lease with a consistent Get every RetryPeriod
// (jittered) and take it with a compare-and-swap Update once it has not
// changed for LeaseDuration of their own local monotonic time, or as soon as
// it is missing (Create) or released (empty Holder). The Leader renews every
// RetryPeriod. When no leader has been heard from for NoLeaderTimeout (plus a
// per-elector jitter), or the member has no view at all, a non-candidate
// enters the Lottery: in each LotteryRound it is eligible with probability
// p = min(1, LotteryBudgetQPS*LotteryRound/N), decided by
// hashing.Below(hashing.Score(self.Key, round), p64), and an eligible member
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
package elect

import (
	"errors"
	"math"
	"math/bits"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
)

// Config tunes the Elector. Zero values select defaults.
type Config struct {
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
	// ExpectedClusterSize is N for the lottery when there is no view.
	// Default 1000.
	ExpectedClusterSize int
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
	DefaultExpectedClusterSize   = 1000
)

// WithDefaults returns c with zero fields replaced by defaults.
func (c Config) WithDefaults() Config {
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
	setInt(&c.ExpectedClusterSize, DefaultExpectedClusterSize)

	return c
}

// Validate checks c after defaults are applied.
func (c Config) Validate() error {
	c = c.WithDefaults()

	switch {
	case c.LeaseDuration < 0 || c.RenewDeadline < 0 || c.RetryPeriod < 0 || c.OpTimeout < 0 ||
		c.NoLeaderTimeout < 0 || c.LotteryRound < 0 || c.ClockSkewAllowance < 0 || c.MinCandidateAge < 0:
		return errors.New("elect: durations must not be negative")
	case c.RenewDeadline >= c.LeaseDuration:
		return errors.New("elect: RenewDeadline must be less than LeaseDuration")
	case c.RetryPeriod*6/5 >= c.RenewDeadline:
		return errors.New("elect: RetryPeriod*1.2 must be less than RenewDeadline")
	case c.LotteryBudgetQPS < 0 || c.Candidates < 0 || c.ExpectedClusterSize < 0:
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
	// Key is hashing.MemberKey(ID, Incarnation).
	Key uint64
	// Addr is the control-plane address published when leader.
	Addr string
}

// State is the elector state.
type State uint8

// States.
const (
	Follower State = iota
	Candidate
	Leader
	Lottery
)

// String returns the state name.
func (s State) String() string {
	switch s {
	case Follower:
		return "follower"
	case Candidate:
		return "candidate"
	case Leader:
		return "leader"
	case Lottery:
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
	// ClusterSize is the member count of the view. Zero or negative selects
	// Config.ExpectedClusterSize.
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

// Hooks observe the Elector for metrics and coverage. Nil fields are skipped.
type Hooks struct {
	OnState func(from, to State)
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
	cfg   Config
	self  Self
	rng   *rand.Rand
	hooks Hooks

	state   State
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

// New returns an Elector. It panics if cfg is invalid (see Config.Validate)
// or rng is nil.
func New(cfg Config, self Self, rng *rand.Rand) *Elector {
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
func (e *Elector) SetHooks(h Hooks) { e.hooks = h }

// State returns the current state.
func (e *Elector) State() State { return e.state }

// Term returns the term while leader, 0 otherwise.
func (e *Elector) Term() uint64 {
	if e.state != Leader {
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
	return e.state == Leader && now-e.anchor < int64(e.cfg.RenewDeadline)
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

func (e *Elector) setState(to State) {
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
	e.setState(Follower)
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

func (e *Elector) desiredState(in Input) State {
	if in.IsCandidate {
		return Candidate
	}

	heard := max(in.LeaderHeardAt, e.startAt)
	if !in.HaveView || in.Now-heard > int64(e.cfg.NoLeaderTimeout)+e.lotteryJitter {
		return Lottery
	}

	return Follower
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

	if e.state == Leader {
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
		if to == Lottery {
			e.roundDone = false
		}

		if to == Candidate {
			e.nextOpAt = in.Now
		}

		e.setState(to)
	}

	if e.outstanding {
		return nil
	}

	switch e.state {
	case Candidate:
		if in.Now < e.nextOpAt {
			return nil
		}

		if e.takeNow {
			e.takeNow = false
			return e.acquire(in, e.observed, e.observedNotFound)
		}

		e.nextOpAt = e.retryAt(in.Now)

		return e.issue(in.Now, OpGet, purposeRead, Record{})
	case Lottery:
		if e.takeNow {
			e.takeNow = false
			return e.acquire(in, e.observed, e.observedNotFound)
		}

		round := uint64(in.Now / int64(e.cfg.LotteryRound))
		if e.roundDone && round == e.lastRound {
			return nil
		}

		e.lastRound, e.roundDone = round, true

		// An empty or unset ClusterSize would make p=1 and every member
		// read every round, so it falls back to ExpectedClusterSize.
		n := e.cfg.ExpectedClusterSize
		if in.HaveView && in.ClusterSize > 0 {
			n = in.ClusterSize
		}

		p64 := LotteryP64(e.cfg.LotteryBudgetQPS, e.cfg.LotteryRound, n)
		if !hashing.Below(hashing.Score(e.self.Key, round), p64) {
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
	r.Seeds = capSeeds(in.Seeds)
	r.LeaseDuration = e.cfg.LeaseDuration
	r.RenewTime = in.Wall

	return r
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
	r.Seeds = capSeeds(in.Seeds)
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
func (e *Elector) Release(in Input) []Op {
	if e.state != Leader {
		return nil
	}

	r := e.held.Clone()
	r.Holder = ""
	r.RenewTime = in.Wall
	r.Seeds = nil
	// Carry the highest term this member has seen (see ObserveTerm) so the
	// next holder starts above every lineage, even one whose term never
	// reached the Lease.
	r.Term = max(e.term, e.maxTerm)

	ops := e.issue(in.Now, OpUpdate, purposeRelease, r)
	e.events = append(e.events, Event{Kind: Lost, Term: e.term, Reason: LostReleased})
	e.setState(Follower)

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
	if e.state != Leader {
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
	e.setState(Leader)
	e.events = append(e.events, Event{Kind: Acquired, Term: e.term})
	e.nextOpAt = e.retryAt(in.Now)
}

func (e *Elector) onRead(in Input, res Result) {
	if e.state != Candidate && e.state != Lottery {
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

	if e.state == Lottery {
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

// TopCandidates returns the n best keys by hashing.Score(leaseSeed, key),
// ordered best first with hashing.Better. The caller passes only eligible
// keys (Active members admitted at least MinCandidateAge ago). keys is not
// modified; duplicates are kept.
func TopCandidates(leaseSeed uint64, keys []uint64, n int) []uint64 {
	if n <= 0 {
		return nil
	}

	out := make([]uint64, 0, min(n, len(keys)))
	for _, k := range keys {
		s := hashing.Score(leaseSeed, k)

		i := len(out)
		for i > 0 && hashing.Better(s, k, hashing.Score(leaseSeed, out[i-1]), out[i-1]) {
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

// IsTopCandidate reports whether self is among TopCandidates(leaseSeed,
// keys, n).
func IsTopCandidate(leaseSeed, self uint64, keys []uint64, n int) bool {
	return slices.Contains(TopCandidates(leaseSeed, keys, n), self)
}
