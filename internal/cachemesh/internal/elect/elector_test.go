// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package elect

import (
	"errors"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// memStore is a deterministic in-memory Lease server.
type memStore struct {
	rec    Record
	exists bool
	rv     int
}

func (m *memStore) apply(op Op) Result {
	res := Result{Seq: op.Seq, Kind: op.Kind}

	switch op.Kind {
	case OpGet, OpGetCached:
		if !m.exists {
			res.Err = ErrNotFound
			return res
		}

		res.Record = m.rec.Clone()
	case OpCreate:
		if m.exists {
			res.Err = ErrConflict
			return res
		}

		m.store(op.Record)
		res.Record = m.rec.Clone()
	case OpUpdate:
		if !m.exists {
			res.Err = ErrNotFound
			return res
		}

		if op.Record.ResourceVersion != m.rec.ResourceVersion {
			res.Err = ErrConflict
			return res
		}

		m.store(op.Record)
		res.Record = m.rec.Clone()
	}

	return res
}

func (m *memStore) store(r Record) {
	m.rv++
	r = r.Clone()
	r.ResourceVersion = strconv.Itoa(m.rv)
	r.UID = "uid"
	m.rec, m.exists = r, true
}

func self(i int) Self {
	id := "m" + strconv.Itoa(i)
	return Self{ID: id, Incarnation: uint64(i + 1), Key: view.MemberKey(id, uint64(i+1)), Addr: id + ":1"}
}

func newRNG(s uint64) *rand.Rand { return rand.New(rand.NewPCG(s, s^0x9e37)) }

func sec(n float64) int64 { return int64(n * float64(time.Second)) }

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"defaults", Config{}, true},
		{"renew>=lease", Config{LeaseDuration: 5 * time.Second, RenewDeadline: 5 * time.Second}, false},
		{"retry too big", Config{RetryPeriod: 9 * time.Second}, false},
		{"negative", Config{LotteryRound: -1}, false},
		{"jitter", Config{RetryJitterPercent: 101}, false},
		{"custom", Config{LeaseDuration: 3 * time.Second, RenewDeadline: 2 * time.Second, RetryPeriod: time.Second}, true},
	}

	for _, c := range cases {
		if err := c.cfg.Validate(); (err == nil) != c.ok {
			t.Errorf("%s: Validate() = %v, want ok=%v", c.name, err, c.ok)
		}
	}

	d := Config{}.WithDefaults()
	if d.LeaseDuration != DefaultLeaseDuration || d.OpTimeout != DefaultRetryPeriod || d.Candidates != 3 {
		t.Errorf("defaults: %+v", d)
	}

	defer func() {
		if recover() == nil {
			t.Error("New with invalid config did not panic")
		}
	}()

	New(Config{RetryPeriod: time.Hour}, self(0), newRNG(1))
}

func TestStateStrings(t *testing.T) {
	t.Parallel()

	for s, want := range map[State]string{Follower: "follower", Candidate: "candidate", Leader: "leader", Lottery: "lottery", 9: "unknown"} {
		if s.String() != want {
			t.Errorf("%d: %q", s, s.String())
		}
	}

	for r, want := range map[LostReason]string{LostDeadline: "deadline", LostConflict: "conflict", LostReleased: "released", 0: "unknown"} {
		if r.String() != want {
			t.Errorf("%d: %q", r, r.String())
		}
	}
}

func TestLotteryP64(t *testing.T) {
	t.Parallel()

	if got := LotteryP64(20, time.Second, 10); got != ^uint64(0) {
		t.Errorf("p>=1: %x", got)
	}

	if got := LotteryP64(20, time.Second, 40); got != 1<<63 {
		t.Errorf("p=0.5: %x", got)
	}

	if got := LotteryP64(20, time.Second, 0); got != ^uint64(0) {
		t.Errorf("n=0: %x", got)
	}

	if got := LotteryP64(0, time.Second, 10); got != 0 {
		t.Errorf("qps=0: %x", got)
	}

	if got := LotteryP64(1<<40, time.Hour, 1); got != ^uint64(0) {
		t.Errorf("overflow: %x", got)
	}
}

func TestTopCandidates(t *testing.T) {
	t.Parallel()

	rng := newRNG(7)
	for iter := range 200 {
		seed := rng.Uint64()
		keys := make([]uint64, rng.IntN(20))

		for i := range keys {
			keys[i] = rng.Uint64() % 50 // force duplicates and ties
		}

		n := rng.IntN(5)
		got := TopCandidates(seed, keys, n)

		ref := slices.Clone(keys)
		slices.SortFunc(ref, func(a, b uint64) int {
			sa, sb := view.Score(seed, a), view.Score(seed, b)
			if view.Better(sa, a, sb, b) {
				return -1
			}

			if view.Better(sb, b, sa, a) {
				return 1
			}

			return 0
		})

		if len(ref) > n {
			ref = ref[:n]
		}

		if !slices.Equal(got, ref) {
			t.Fatalf("iter %d: got %v want %v", iter, got, ref)
		}

		// Order independence.
		perm := slices.Clone(keys)
		rng.Shuffle(len(perm), func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })

		if !slices.Equal(TopCandidates(seed, perm, n), got) {
			t.Fatalf("iter %d: order dependent", iter)
		}
	}
}

// harness drives one elector synchronously against a memStore.
type harness struct {
	t   *testing.T
	e   *Elector
	s   *memStore
	in  Input
	ops int
}

func newHarness(t *testing.T, s *memStore, i int) *harness {
	t.Helper()

	h := &harness{t: t, s: s, e: New(Config{ExpectedClusterSize: 1}, self(i), newRNG(uint64(i)))}
	h.e.SetHooks(Hooks{OnOp: func(OpKind) { h.ops++ }})
	h.in = Input{Wall: epoch, HaveView: true, ClusterSize: 10, IsCandidate: true}

	return h
}

func (h *harness) advance(d int64) {
	h.in.Now += d
	h.in.Wall = h.in.Wall.Add(time.Duration(d))
}

// step ticks once and executes any op synchronously.
func (h *harness) step() []Op {
	ops := h.e.Tick(h.in)
	for _, op := range ops {
		h.e.OnResult(h.in, h.s.apply(op))
	}

	return ops
}

func (h *harness) run(until int64) {
	for h.in.Now < until {
		h.advance(sec(0.1))
		h.step()
	}
}

func kinds(ev []Event) []EventKind {
	var out []EventKind
	for _, e := range ev {
		out = append(out, e.Kind)
	}

	return out
}

func TestCandidateCreatesMissingLease(t *testing.T) {
	t.Parallel()

	h := newHarness(t, &memStore{}, 0)
	h.in.ViewTerm = 7
	h.run(sec(1))

	if h.e.State() != Leader || h.e.Term() != 8 {
		t.Fatalf("state %v term %d", h.e.State(), h.e.Term())
	}

	if h.s.rec.Holder != "m0" || h.s.rec.Term != 8 || h.s.rec.Addr != "m0:1" {
		t.Fatalf("record %+v", h.s.rec)
	}

	if ev := h.e.Events(); !slices.Equal(kinds(ev), []EventKind{Acquired}) || ev[0].Term != 8 {
		t.Fatalf("events %+v", ev)
	}

	if !h.e.CanPublish(h.in.Now) {
		t.Fatal("cannot publish")
	}

	// Renewals keep it.
	h.in.Seeds = []string{"a", "b"}
	h.run(sec(60))

	if h.e.State() != Leader || len(h.e.Events()) != 0 || !slices.Equal(h.s.rec.Seeds, []string{"a", "b"}) {
		t.Fatalf("lost leadership: %v %+v", h.e.State(), h.s.rec)
	}

	if h.ops > 2+60/2 {
		t.Fatalf("too many ops: %d", h.ops)
	}
}

func TestCandidateWaitsLeaseDurationForStaleHolder(t *testing.T) {
	t.Parallel()

	s := &memStore{}
	s.store(Record{Holder: "other", Incarnation: 1, Term: 4, RenewTime: epoch, LeaseDuration: DefaultLeaseDuration})

	h := newHarness(t, s, 0)
	h.run(sec(14))

	if h.e.State() != Candidate {
		t.Fatalf("took over early: %v", h.e.State())
	}

	if ev := h.e.Events(); !slices.Equal(kinds(ev), []EventKind{Discovered}) {
		t.Fatalf("events %+v", ev)
	}

	if li, ok := h.e.Leader(); !ok || li.ID != "other" || li.Term != 4 {
		t.Fatalf("leader %+v", li)
	}

	h.run(sec(20))

	if h.e.State() != Leader || h.e.Term() != 5 || s.rec.Transitions != 1 {
		t.Fatalf("state %v term %d rec %+v", h.e.State(), h.e.Term(), s.rec)
	}
}

func TestSameIDOtherIncarnationWaits(t *testing.T) {
	t.Parallel()

	s := &memStore{}
	s.store(Record{Holder: "m0", Incarnation: 99, Term: 1, RenewTime: epoch})

	h := newHarness(t, s, 0)
	h.run(sec(10))

	if h.e.State() == Leader {
		t.Fatal("took over a different incarnation without waiting")
	}

	h.run(sec(25))

	if h.e.State() != Leader {
		t.Fatal("never took over")
	}
}

func TestSameIncarnationRetakesImmediately(t *testing.T) {
	t.Parallel()

	s := &memStore{}
	s.store(Record{Holder: "m0", Incarnation: 1, Term: 3, RenewTime: epoch})

	h := newHarness(t, s, 0)
	h.run(sec(1))

	if h.e.State() != Leader || h.e.Term() != 4 || s.rec.Transitions != 0 {
		t.Fatalf("state %v term %d rec %+v", h.e.State(), h.e.Term(), s.rec)
	}
}

func TestReleasedLeaseTakenImmediately(t *testing.T) {
	t.Parallel()

	s := &memStore{}
	s.store(Record{Holder: "", Term: 9, RenewTime: epoch})

	h := newHarness(t, s, 0)
	h.run(sec(1))

	if h.e.State() != Leader || h.e.Term() != 10 {
		t.Fatalf("state %v term %d", h.e.State(), h.e.Term())
	}
}

func TestReleaseAndHandoff(t *testing.T) {
	t.Parallel()

	s := &memStore{}
	a := newHarness(t, s, 0)
	a.run(sec(1))

	b := newHarness(t, s, 1)
	b.in = a.in
	b.run(a.in.Now + sec(5))

	if b.e.State() != Candidate {
		t.Fatalf("b %v", b.e.State())
	}

	if (&Elector{}).Release(a.in) != nil {
		t.Fatal("non-leader release returned ops")
	}

	ops := a.e.Release(a.in)
	if len(ops) != 1 || ops[0].Record.Holder != "" {
		t.Fatalf("release ops %+v", ops)
	}

	if a.e.State() != Follower || a.e.CanPublish(a.in.Now) {
		t.Fatal("still leader after release")
	}

	if ev := a.e.Events(); ev[len(ev)-1].Kind != Lost || ev[len(ev)-1].Reason != LostReleased {
		t.Fatalf("events %+v", ev)
	}

	a.e.OnResult(a.in, s.apply(ops[0]))

	start := b.in.Now
	for b.e.State() != Leader && b.in.Now-start < sec(10) {
		b.advance(sec(0.1))
		b.step()
	}

	if got := b.in.Now - start; got > int64(DefaultRetryPeriod)*6/5+sec(0.2) {
		t.Fatalf("handoff took %v", time.Duration(got))
	}

	if b.e.Term() != 2 {
		t.Fatalf("term %d", b.e.Term())
	}
}

func TestLeaderStepsDownOnOutage(t *testing.T) {
	t.Parallel()

	h := newHarness(t, &memStore{}, 0)
	h.run(sec(1))
	h.e.Events()

	acquiredAt := h.in.Now
	// Outage: every op fails.
	for h.e.State() == Leader {
		h.advance(sec(0.1))

		for _, op := range h.e.Tick(h.in) {
			h.e.OnResult(h.in, Result{Seq: op.Seq, Kind: op.Kind, Err: errors.New("outage")})
		}
	}

	if d := h.in.Now - acquiredAt; d > int64(DefaultRenewDeadline)+sec(0.1) {
		t.Fatalf("stepped down after %v", time.Duration(d))
	}

	if ev := h.e.Events(); len(ev) != 1 || ev[0].Reason != LostDeadline {
		t.Fatalf("events %+v", ev)
	}
}

func TestLeaderStepsDownWithOpOutstandingAndIgnoresLateResult(t *testing.T) {
	t.Parallel()

	s := &memStore{}
	h := newHarness(t, s, 0)
	h.run(sec(1))

	var pending []Op

	for h.e.State() == Leader {
		h.advance(sec(0.1))
		pending = append(pending, h.e.Tick(h.in)...)
	}

	if len(pending) == 0 {
		t.Fatal("no renewals issued")
	}

	for _, op := range pending {
		h.e.OnResult(h.in, s.apply(op))
	}

	if h.e.State() == Leader || h.e.CanPublish(h.in.Now) {
		t.Fatal("late result restored leadership")
	}
}

func TestPausedLeaderCannotPublishOnWake(t *testing.T) {
	t.Parallel()

	h := newHarness(t, &memStore{}, 0)
	h.run(sec(1))

	h.advance(sec(30)) // stop the world

	if h.e.CanPublish(h.in.Now) {
		t.Fatal("paused leader may publish")
	}

	h.step()

	if h.e.State() == Leader {
		t.Fatal("still leader after pause")
	}
}

func TestLeaderConflictStepsDown(t *testing.T) {
	t.Parallel()

	s := &memStore{}
	h := newHarness(t, s, 0)
	h.run(sec(1))
	h.e.Events()

	s.store(Record{Holder: "thief", Incarnation: 1, Term: 50, RenewTime: epoch})
	h.run(h.in.Now + sec(3))

	if ev := h.e.Events(); len(ev) < 1 || ev[0].Kind != Lost || ev[0].Reason != LostConflict {
		t.Fatalf("events %+v", ev)
	}

	if h.e.State() != Candidate {
		t.Fatalf("state %v", h.e.State())
	}

	if li, _ := h.e.Leader(); li.ID != "thief" {
		t.Fatalf("leader %+v", li)
	}
}

func TestStaleResultsIgnored(t *testing.T) {
	t.Parallel()

	s := &memStore{}
	h := newHarness(t, s, 0)

	h.advance(sec(0.1))

	ops := h.e.Tick(h.in)
	if len(ops) != 1 || ops[0].Kind != OpGet {
		t.Fatalf("ops %+v", ops)
	}

	if len(h.e.Tick(h.in)) != 0 {
		t.Fatal("second op while outstanding")
	}

	// Wrong seq: ignored, still outstanding.
	h.e.OnResult(h.in, Result{Seq: ops[0].Seq + 5, Err: ErrNotFound})

	if len(h.e.Tick(h.in)) != 0 {
		t.Fatal("stale result cleared the outstanding op")
	}

	// After OpTimeout the op is abandoned and a new one issued.
	h.advance(int64(DefaultRetryPeriod) * 2)

	ops2 := h.e.Tick(h.in)
	if len(ops2) != 1 || ops2[0].Seq == ops[0].Seq {
		t.Fatalf("ops2 %+v", ops2)
	}

	h.e.OnResult(h.in, Result{Seq: ops[0].Seq, Err: ErrNotFound})

	if len(h.e.Tick(h.in)) != 0 {
		t.Fatal("abandoned result accepted")
	}
}

func TestFollowerMakesNoOps(t *testing.T) {
	t.Parallel()

	h := newHarness(t, &memStore{}, 0)
	h.in.IsCandidate = false

	for h.in.Now < sec(300) {
		h.advance(sec(0.1))
		h.in.LeaderHeardAt = h.in.Now
		h.step()
	}

	if h.ops != 0 || h.e.State() != Follower {
		t.Fatalf("ops %d state %v", h.ops, h.e.State())
	}
}

func TestLotteryDiscoversLiveLeader(t *testing.T) {
	t.Parallel()

	s := &memStore{}
	s.store(Record{Holder: "L", Incarnation: 1, Term: 3, Addr: "l:1", Seeds: []string{"s1"}, RenewTime: epoch.Add(time.Hour), LeaseDuration: DefaultLeaseDuration})

	h := newHarness(t, s, 0)
	h.in.IsCandidate = false
	h.in.HaveView = false
	h.in.Wall = epoch.Add(time.Hour)
	h.run(sec(2))

	if h.e.State() != Lottery {
		t.Fatalf("state %v", h.e.State())
	}

	ev := h.e.Events()
	if !slices.Equal(kinds(ev), []EventKind{Discovered}) || ev[0].Record.Addr != "l:1" {
		t.Fatalf("events %+v", ev)
	}

	h.in.HaveView = true
	h.in.LeaderHeardAt = h.in.Now
	h.step()

	if h.e.State() != Follower {
		t.Fatalf("state %v", h.e.State())
	}
}

func TestLotteryTakesExpiredLease(t *testing.T) {
	t.Parallel()

	s := &memStore{}
	s.store(Record{Holder: "L", Incarnation: 1, Term: 3, RenewTime: epoch, LeaseDuration: DefaultLeaseDuration})

	h := newHarness(t, s, 0)
	h.in.IsCandidate = false
	h.in.HaveView = false
	h.in.Wall = epoch.Add(DefaultLeaseDuration + DefaultClockSkewAllowance - 3*time.Second)
	h.run(sec(1.5))

	if h.e.State() != Lottery {
		t.Fatalf("took a lease within skew allowance: %v", h.e.State())
	}

	h.in.Wall = epoch.Add(DefaultLeaseDuration + DefaultClockSkewAllowance + time.Second)
	h.run(sec(3))

	if h.e.State() != Leader || h.e.Term() != 4 {
		t.Fatalf("state %v term %d", h.e.State(), h.e.Term())
	}
}

func TestLotteryEntryAfterNoLeaderTimeout(t *testing.T) {
	t.Parallel()

	h := newHarness(t, &memStore{}, 0)
	h.in.IsCandidate = false
	h.in.LeaderHeardAt = 1
	h.run(sec(30))

	if h.e.State() != Follower || h.ops != 0 {
		t.Fatalf("state %v ops %d", h.e.State(), h.ops)
	}

	h.run(sec(46))

	if h.e.State() == Follower {
		t.Fatal("never entered lottery")
	}
}

// TestLotteryBudget checks that N lottery members read at about
// LotteryBudgetQPS in aggregate.
func TestLotteryBudget(t *testing.T) {
	t.Parallel()

	const n, rounds = 2000, 50

	s := &memStore{}
	s.store(Record{Holder: "L", RenewTime: epoch.Add(time.Hour)})

	total := 0

	for i := range n {
		e := New(Config{ExpectedClusterSize: n}, self(i), newRNG(uint64(i)))
		e.SetHooks(Hooks{OnOp: func(OpKind) { total++ }})

		in := Input{Wall: epoch}
		for r := range rounds * 10 {
			in.Now = sec(100) + int64(r)*sec(0.1)
			for _, op := range e.Tick(in) {
				e.OnResult(in, s.apply(op))
			}
		}
	}

	qps := float64(total) / rounds
	if qps < 15 || qps > 25 {
		t.Fatalf("lottery qps %.1f, want ~20", qps)
	}
}

// Simulation.

type simEvent struct {
	at     int64
	seq    int
	member int
	op     Op
	server bool // true: apply at server; false: deliver result
	res    Result
}

type simMember struct {
	e         *Elector
	off       int64         // mono offset
	skew      time.Duration // wall skew
	cand      bool
	pausedTil int64
	queued    []Result
	ops       int
	opsAsFoll int
	alive     bool
}

func TestSimulation(t *testing.T) {
	t.Parallel()

	for seed := uint64(1); seed <= 40; seed++ {
		runSim(t, seed)
	}
}

func runSim(t *testing.T, seed uint64) {
	t.Helper()

	rng := newRNG(seed * 1000)
	nm := 3 + rng.IntN(8)
	store := &memStore{}
	members := make([]*simMember, nm)

	var acquisitions []uint64

	for i := range members {
		m := &simMember{
			off:   rng.Int64N(sec(1000)),
			skew:  time.Duration(rng.Int64N(sec(4)) - sec(2)),
			cand:  i < 3 && seed%4 != 0, // every fourth seed elects by lottery only
			alive: true,
		}
		m.e = New(Config{}, self(i), newRNG(seed*100+uint64(i)))
		m.e.SetHooks(Hooks{OnOp: func(OpKind) {
			m.ops++
			if m.e.State() == Follower {
				m.opsAsFoll++
			}
		}})
		members[i] = m
	}

	var (
		queue   []simEvent
		qseq    int
		outage  [2]int64
		heardAt int64 // global time a leader last could publish
	)

	push := func(ev simEvent) {
		qseq++
		ev.seq = qseq
		queue = append(queue, ev)
	}

	const step = int64(100 * time.Millisecond)

	end := sec(600)
	lossP := rng.IntN(20)
	conflictP := rng.IntN(5)
	outage[0] = sec(100) + rng.Int64N(sec(300))
	outage[1] = outage[0] + rng.Int64N(sec(30))

	lastLeader := -1

	input := func(i int, now int64) Input {
		m := members[i]

		in := Input{
			Now:         now + m.off,
			Wall:        epoch.Add(time.Duration(now) + m.skew),
			HaveView:    true,
			ClusterSize: nm,
			IsCandidate: m.cand,
			ViewTerm:    0,
		}
		if heardAt > 0 {
			in.LeaderHeardAt = heardAt + m.off
		}

		return in
	}

	for now := int64(0); now < end; now += step {
		// Random faults.
		if rng.IntN(2000) == 0 {
			i := rng.IntN(nm)
			members[i].pausedTil = now + rng.Int64N(sec(40))
		}

		if rng.IntN(3000) == 0 && lastLeader >= 0 {
			// Graceful release by the leader.
			m := members[lastLeader]
			for _, op := range m.e.Release(input(lastLeader, now)) {
				push(simEvent{at: now + rng.Int64N(sec(0.2)), member: lastLeader, op: op, server: true})
			}
		}

		// Deliver due events in (time, seq) order.
		slices.SortFunc(queue, func(a, b simEvent) int {
			if a.at != b.at {
				return int(a.at - b.at)
			}

			return a.seq - b.seq
		})

		for len(queue) > 0 && queue[0].at <= now {
			ev := queue[0]
			queue = queue[1:]

			if ev.server {
				var res Result

				switch {
				case now >= outage[0] && now < outage[1]:
					res = Result{Seq: ev.op.Seq, Kind: ev.op.Kind, Err: errors.New("outage")}
				case ev.op.Kind == OpUpdate && rng.IntN(100) < conflictP:
					store.rv++ // external writer bumps RV
					store.rec.ResourceVersion = strconv.Itoa(store.rv)
					res = store.apply(ev.op)
				default:
					res = store.apply(ev.op)
				}

				if rng.IntN(100) >= lossP {
					push(simEvent{at: now + rng.Int64N(sec(1)), member: ev.member, res: res})
				}

				continue
			}

			m := members[ev.member]
			if now < m.pausedTil {
				m.queued = append(m.queued, ev.res)
				continue
			}

			m.e.OnResult(input(ev.member, now), ev.res)
		}

		publishers := 0

		for i, m := range members {
			if now < m.pausedTil {
				continue
			}

			in := input(i, now)
			for _, r := range m.queued {
				m.e.OnResult(in, r)
			}

			m.queued = nil

			for _, op := range m.e.Tick(in) {
				push(simEvent{at: now + rng.Int64N(sec(1)), member: i, op: op, server: true})
			}

			for _, ev := range m.e.Events() {
				if ev.Kind == Acquired {
					acquisitions = append(acquisitions, ev.Term)
				}
			}

			if m.e.CanPublish(in.Now) {
				publishers++
				lastLeader = i
				heardAt = now
			}
		}

		if publishers > 1 {
			t.Fatalf("seed %d: %d publishers at %v", seed, publishers, time.Duration(now))
		}
	}

	for i := 1; i < len(acquisitions); i++ {
		if acquisitions[i] <= acquisitions[i-1] {
			t.Fatalf("seed %d: terms not increasing: %v", seed, acquisitions)
		}
	}

	t.Logf("seed %d: members %d acquisitions %d", seed, nm, len(acquisitions))

	if len(acquisitions) == 0 {
		t.Fatalf("seed %d: no leader elected", seed)
	}

	for i, m := range members {
		if m.opsAsFoll != 0 {
			t.Fatalf("seed %d: follower %d made %d ops", seed, i, m.opsAsFoll)
		}

		if m.cand && seed%4 != 0 {
			// Reads every RetryPeriod (+20%) plus acquisition attempts.
			if limit := int(end/int64(DefaultRetryPeriod)) + 20; m.ops > limit && m.e.State() != Leader {
				t.Fatalf("seed %d: candidate %d made %d ops > %d", seed, i, m.ops, limit)
			}
		}
	}
}

// TestLotteryZeroClusterSize checks that a view reporting no members does
// not make every lottery member read every round.
func TestLotteryZeroClusterSize(t *testing.T) {
	t.Parallel()

	s := &memStore{}
	s.store(Record{Holder: "L", RenewTime: epoch.Add(time.Hour)})

	// Members enter the lottery by 45s (NoLeaderTimeout plus jitter);
	// reads are counted from 50s on.
	const n, rounds = 500, 20

	total := 0

	for i := range n {
		e := New(Config{ExpectedClusterSize: n}, self(i), newRNG(uint64(i)))
		in := Input{Wall: epoch, HaveView: true, ClusterSize: 0}

		e.SetHooks(Hooks{OnOp: func(OpKind) {
			if in.Now >= sec(50) {
				total++
			}
		}})

		for r := range (50 + rounds) * 10 {
			in.Now = int64(r) * sec(0.1)
			for _, op := range e.Tick(in) {
				e.OnResult(in, s.apply(op))
			}
		}
	}

	if qps := float64(total) / rounds; qps > 40 {
		t.Fatalf("lottery qps %.1f with ClusterSize 0, want ~20", qps)
	}
}

// TestReleaseKeepsClusterSeeds is a regression test: a member without
// seeds that took the Lease (a lottery winner without a view, which then
// declines) wrote and released a record without seeds, which told the next
// starting member that no cluster existed, and it bootstrapped a second
// cluster next to the live one (found by the DST data-plane workload: the
// epoch and keyspace generations restarted).
func TestReleaseKeepsClusterSeeds(t *testing.T) {
	t.Parallel()

	s := &memStore{}
	s.store(Record{Holder: "", Term: 9, RenewTime: epoch, Seeds: []string{"a", "b"}})

	h := newHarness(t, s, 0)
	h.run(sec(1))

	if h.e.State() != Leader || !slices.Equal(s.rec.Seeds, []string{"a", "b"}) {
		t.Fatalf("acquire without seeds: state %v seeds %v", h.e.State(), s.rec.Seeds)
	}

	for _, op := range h.e.Release(h.in) {
		h.e.OnResult(h.in, s.apply(op))
	}

	if s.rec.Holder != "" || !slices.Equal(s.rec.Seeds, []string{"a", "b"}) {
		t.Fatalf("release: %+v", s.rec)
	}
}

func TestReleaseSeedsExcludeSelf(t *testing.T) {
	t.Parallel()

	s := &memStore{}
	h := newHarness(t, s, 0)
	me := self(0).Addr
	h.in.Seeds = []string{me, "c"}
	h.run(sec(1))

	if !slices.Equal(s.rec.Seeds, []string{me, "c"}) {
		t.Fatalf("acquire seeds %v", s.rec.Seeds)
	}

	// A leaving leader points joiners at the remaining members.
	for _, op := range h.e.Release(h.in) {
		h.e.OnResult(h.in, s.apply(op))
	}

	if !slices.Equal(s.rec.Seeds, []string{"c"}) {
		t.Fatalf("release seeds %v", s.rec.Seeds)
	}

	// The last member leaving clears them, so a later cluster can start.
	s2 := &memStore{}
	h2 := newHarness(t, s2, 0)
	h2.in.Seeds = []string{me}
	h2.run(sec(1))

	for _, op := range h2.e.Release(h2.in) {
		h2.e.OnResult(h2.in, s2.apply(op))
	}

	if len(s2.rec.Seeds) != 0 {
		t.Fatalf("last member release seeds %v", s2.rec.Seeds)
	}
}
