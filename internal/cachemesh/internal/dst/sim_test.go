// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/elect"
)

type recTarget struct{ got []uint64 }

func (r *recTarget) Fire(a, b uint64) { r.got = append(r.got, a*100+b) }

// drain runs every pending event.
func drain(s *Sim) {
	for s.Step() {
	}
}

func TestSimOrdering(t *testing.T) {
	s := New(1)

	var got []int

	s.At(20, func() { got = append(got, 3) })
	s.At(10, func() { got = append(got, 1) })
	s.At(10, func() { got = append(got, 2) })
	s.At(30, func() {
		got = append(got, 4)

		s.After(0, func() { got = append(got, 6) })
		s.At(5, func() { got = append(got, 7) }) // past: clamps to now, after 6
	})
	s.At(30, func() { got = append(got, 5) })

	drain(s)

	want := []int{1, 2, 3, 4, 5, 6, 7}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}

	if s.Now() != 30 {
		t.Fatalf("now = %d", s.Now())
	}
}

func TestSimStabilityManySameTime(t *testing.T) {
	s := New(2)
	tg := &recTarget{}

	for i := range 1000 {
		s.AtTarget(int64(i%3), tg, uint64(i), 0)
	}

	drain(s)

	var want []uint64

	for tm := range 3 {
		for i := range 1000 {
			if i%3 == tm {
				want = append(want, uint64(i)*100)
			}
		}
	}

	for i := range want {
		if tg.got[i] != want[i] {
			t.Fatalf("event %d: got %d want %d", i, tg.got[i], want[i])
		}
	}
}

func TestSimCancel(t *testing.T) {
	s := New(3)

	var ran []int

	h1 := s.After(10, func() { ran = append(ran, 1) })
	h2 := s.After(20, func() { ran = append(ran, 2) })
	s.After(30, func() { ran = append(ran, 3) })

	if !s.Cancel(h2) || s.Cancel(h2) {
		t.Fatal("cancel should succeed once")
	}

	s.RunUntil(15)

	if s.Cancel(h1) {
		t.Fatal("cancel of run event")
	}

	// Reuse of the canceled slot must not let the stale handle cancel it.
	h4 := s.After(1, func() { ran = append(ran, 4) })
	if s.Cancel(h2) {
		t.Fatal("stale handle canceled a new event")
	}

	_ = h4

	drain(s)

	if len(ran) != 3 || ran[0] != 1 || ran[1] != 4 || ran[2] != 3 {
		t.Fatalf("ran %v", ran)
	}

	if s.Cancel(Handle{}) {
		t.Fatal("zero handle")
	}
}

func TestSimRunUntilAdvancesClock(t *testing.T) {
	s := New(4)
	s.At(100, func() {})
	s.RunFor(50)

	if next, ok := s.Next(); s.Now() != 50 || !ok || next != 100 {
		t.Fatalf("now %d next %d %v", s.Now(), next, ok)
	}

	s.RunUntil(100)

	if s.Steps() != 1 {
		t.Fatal("event at boundary should run")
	}

	if _, ok := s.Next(); ok {
		t.Fatal("next on empty")
	}
}

func TestSimHeapRandom(t *testing.T) {
	s := New(5)
	r := s.Stream("test")

	var handles []Handle

	last := int64(-1)
	check := func() {
		if s.Now() < last {
			t.Fatal("time went backwards")
		}

		last = s.Now()
	}

	for i := range 20000 {
		at := r.Int64N(1_000_000)
		handles = append(handles, s.At(at, check))

		if i%3 == 0 {
			s.Cancel(handles[r.IntN(len(handles))])
		}
	}

	drain(s)

	if _, ok := s.Next(); ok {
		t.Fatal("pending after run")
	}
}

func TestStreamsIndependent(t *testing.T) {
	a, b := New(9), New(9)
	_ = a.Stream("net").Uint64()
	_ = a.Stream("net").Uint64()

	if a.Stream("workload").Uint64() != b.Stream("workload").Uint64() {
		t.Fatal("draws on one stream perturbed another")
	}

	if New(9).Stream("sim").Uint64() == New(10).Stream("sim").Uint64() {
		t.Fatal("different seeds gave the same draw")
	}
}

func TestTrace(t *testing.T) {
	run := func(seed uint64) uint64 {
		s := New(seed)
		tr := NewTrace()
		r := s.Stream("x")

		for i := range 100 {
			s.After(r.Int64N(1000), func() { tr.Add(1, uint64(i), uint64(s.Now()), 0) })
		}

		drain(s)

		return tr.Sum()
	}

	if first := run(1); first != run(1) {
		t.Fatal("same seed, different trace")
	}

	if run(1) == run(2) {
		t.Fatal("different seed, same trace")
	}

	tr := NewTrace()

	if n := testing.AllocsPerRun(1000, func() { tr.Add(1, 2, 3, 4) }); n != 0 {
		t.Fatalf("Add allocates %v", n)
	}
}

func TestClockDrift(t *testing.T) {
	c := NewClock(12345)
	if c.Mono(0) != 12345 {
		t.Fatal("base")
	}

	c.SetDrift(0, 1000) // +1ppm

	h := int64(time.Hour)
	if got := c.Mono(h) - 12345; got != h+h/1_000_000 {
		t.Fatalf("drift elapsed %d", got)
	}

	prev := c.Mono(h)

	for i, ppb := range []int64{-MaxDriftPPB, 0, MaxDriftPPB, -7, 2 * MaxDriftPPB} {
		now := h + int64(i+1)*int64(time.Minute)
		c.SetDrift(now, ppb)

		for dt := int64(0); dt < 3*nsPerSec; dt += 333_333_333 {
			m := c.Mono(now + dt)
			if m < prev {
				t.Fatalf("mono decreased: %d < %d", m, prev)
			}

			prev = m
		}
	}

	if c.DriftPPB() != MaxDriftPPB {
		t.Fatal("drift not clamped")
	}

	// Long runs do not overflow.
	c.SetDrift(0, MaxDriftPPB)

	if c.Mono(1000*h) < c.Mono(999*h) {
		t.Fatal("overflow")
	}
}

func TestClockMonoStepwise(t *testing.T) {
	c := NewClock(0)
	c.SetDrift(0, -MaxDriftPPB+1)

	prev := c.Mono(0)
	for now := int64(0); now < 3*nsPerSec; now += 7_777_777 {
		if m := c.Mono(now); m < prev {
			t.Fatal("decreased")
		} else {
			prev = m
		}
	}
}

func TestClockWallAndSimTime(t *testing.T) {
	c := NewClock(-5)

	if !c.Wall(0).Equal(Epoch) {
		t.Fatal("epoch")
	}

	c.SetOffset(int64(time.Second), 3*time.Second)

	if got := c.Wall(int64(2 * time.Second)); !got.Equal(Epoch.Add(5 * time.Second)) {
		t.Fatalf("wall %v", got)
	}

	before := c.Mono(int64(2 * time.Second))
	c.SetOffset(int64(2*time.Second), -time.Second)

	if c.Mono(int64(2*time.Second)) != before {
		t.Fatal("offset moved mono")
	}

	if c.Offset() != -time.Second {
		t.Fatal("offset")
	}

	for _, ppb := range []int64{0, 1, 999, -1_234_567, MaxDriftPPB, -MaxDriftPPB} {
		c.SetDrift(int64(10*time.Second), ppb)

		for _, d := range []int64{1, 999, nsPerSec, 12345678901, 3600 * nsPerSec} {
			target := c.Mono(int64(10*time.Second)) + d
			at := c.SimTime(target)

			if c.Mono(at) < target || c.Mono(at-1) >= target {
				t.Fatalf("ppb %d d %d: SimTime %d not earliest", ppb, d, at)
			}
		}

		if c.SimTime(0) != int64(10*time.Second) {
			t.Fatal("past deadline")
		}
	}
}

func TestCoverage(t *testing.T) {
	c := NewCoverage()
	c.Register("b", "a", "c")
	h := c.Handle("a")
	h.Hit()
	h.Hit()
	c.Hit("extra")

	if got := c.Missing(); len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("missing %v", got)
	}

	o := NewCoverage()
	o.Register("d")
	o.Hit("b")
	c.Merge(o)

	if got := c.Missing(); len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Fatalf("missing %v", got)
	}

	if c.Report() == "" {
		t.Fatal("report")
	}

	if n := testing.AllocsPerRun(100, h.Hit); n != 0 {
		t.Fatal("Hit allocates")
	}
}

type inbox struct {
	got   []Message
	net   *Net
	reply bool
}

func (b *inbox) Deliver(m *Message) {
	b.got = append(b.got, *m)
	if b.reply && m.Call != 0 {
		b.net.Reply(m, Message{Kind: 2, Payload: []byte("pong")})
	}
}

func newTestNet(seed uint64, cfg NetConfig) (*Sim, *Net, *inbox) {
	s := New(seed)
	b := &inbox{}
	n := NewNet(s, 4, b, cfg)
	b.net = n

	return s, n, b
}

func TestNetDeliveryAndCounters(t *testing.T) {
	s, n, b := newTestNet(1, NetConfig{Latency: 1000, NoJitter: true})

	for i := range 10 {
		n.Send(Message{From: 0, To: 1, Kind: 7, Payload: make([]byte, i)})
	}

	drain(s)

	if len(b.got) != 10 || s.Now() != 1000 {
		t.Fatalf("got %d at %d", len(b.got), s.Now())
	}

	for i, m := range b.got {
		if len(m.Payload) != i {
			t.Fatal("no-jitter messages reordered")
		}
	}

	if st := n.Stats(); st.Sent != 10 || st.Delivered != 10 {
		t.Fatalf("counters %+v", st)
	}
}

func TestNetJitterReorders(t *testing.T) {
	s, n, b := newTestNet(2, NetConfig{Latency: 1000, Jitter: 100_000})

	for i := range 100 {
		n.Send(Message{From: 0, To: 1, ID: uint64(i)})
	}

	drain(s)

	reordered := false

	for i := 1; i < len(b.got); i++ {
		if b.got[i].ID < b.got[i-1].ID {
			reordered = true
		}
	}

	if !reordered || len(b.got) != 100 {
		t.Fatal("expected reordering with jitter")
	}
}

func TestNetLossAndDup(t *testing.T) {
	s, n, b := newTestNet(3, NetConfig{LossPPM: 200_000})

	for range 10_000 {
		n.Send(Message{From: 0, To: 1})
	}

	drain(s)

	if got := len(b.got); got < 7500 || got > 8500 || uint64(10_000-got) != n.Stats().Lost {
		t.Fatalf("delivered %d lost %d", got, n.Stats().Lost)
	}

	s, n, b = newTestNet(3, NetConfig{DupPPM: 500_000})

	for range 1000 {
		n.Send(Message{From: 0, To: 1})
	}

	drain(s)

	dups := 0

	for _, m := range b.got {
		if m.Dup {
			dups++
		}
	}

	if dups < 400 || dups > 600 || len(b.got) != 1000+dups {
		t.Fatalf("dups %d delivered %d", dups, len(b.got))
	}
}

func TestNetPartition(t *testing.T) {
	s, n, b := newTestNet(4, NetConfig{})
	n.SetGroup(1, 1)
	n.Send(Message{From: 0, To: 1})
	n.Send(Message{From: 0, To: 2})
	drain(s)

	if len(b.got) != 1 || b.got[0].To != 2 || n.Stats().Partitioned != 1 {
		t.Fatal("cross-group message delivered")
	}

	// Partition at delivery time drops in-flight messages; heal restores.
	n.Heal()
	n.Send(Message{From: 0, To: 1})
	n.SetGroup(0, 5)
	drain(s)

	if len(b.got) != 1 {
		t.Fatal("in-flight message crossed partition")
	}

	n.Heal()
	n.Send(Message{From: 0, To: 1})
	drain(s)

	if len(b.got) != 2 || n.Group(0) != 0 {
		t.Fatal("heal")
	}
}

func TestNetPause(t *testing.T) {
	s, n, b := newTestNet(5, NetConfig{})
	n.Pause(1)
	n.Send(Message{From: 0, To: 1, Kind: 1})
	n.Send(Message{From: 0, To: 1, Kind: 2})
	s.RunFor(1_000_000_000)

	if len(b.got) != 0 || !n.Paused(1) {
		t.Fatal("delivered to paused member")
	}

	n.Resume(1)
	drain(s)

	if len(b.got) != 2 {
		t.Fatal("held messages not delivered")
	}

	s, n, b = newTestNet(5, NetConfig{})
	n.Pause(1)
	n.Send(Message{From: 0, To: 1})
	drain(s)
	n.DropHeld(1)
	n.Resume(1)
	drain(s)

	if len(b.got) != 0 {
		t.Fatal("DropHeld")
	}
}

func TestNetSlow(t *testing.T) {
	s, n, _ := newTestNet(6, NetConfig{Latency: 1000, NoJitter: true})
	n.SetSlow(1, 3000)
	n.Send(Message{From: 0, To: 1})
	drain(s)

	if s.Now() != 3000 {
		t.Fatalf("slow latency %d", s.Now())
	}
}

type completions struct {
	n    map[uint64]int
	errs int
}

func (c *completions) Complete(call uint64, resp *Message, err error) {
	c.n[call]++
	if err != nil {
		c.errs++
	} else if string(resp.Payload) != "pong" {
		panic("bad response")
	}
}

func TestNetCallExactlyOnce(t *testing.T) {
	s, n, b := newTestNet(7, NetConfig{Latency: 1000, Jitter: 3000, DupPPM: 300_000, LossPPM: 100_000})
	b.reply = true
	c := &completions{n: map[uint64]int{}}

	var calls []uint64

	for i := range 2000 {
		// Timeouts near the round-trip time make responses race the timer.
		calls = append(calls, n.Call(Message{From: 0, To: 1 + uint32(i%3)}, 2000+int64(i%5)*1000, c))
	}

	drain(s)

	for _, id := range calls {
		if c.n[id] != 1 {
			t.Fatalf("call %d completed %d times", id, c.n[id])
		}
	}

	st := n.Stats()
	if c.errs == 0 || c.errs == len(calls) || st.LateResponses == 0 || n.PendingCalls() != 0 {
		t.Fatalf("errs %d late %d", c.errs, st.LateResponses)
	}
}

func TestNetDeterminism(t *testing.T) {
	run := func(seed uint64) uint64 {
		s := New(seed)
		tr := NewTrace()
		h := traceHandler{tr: tr, s: s}
		n := NewNet(s, 100, &h, NetConfig{LossPPM: 10_000, DupPPM: 10_000})
		r := s.Stream("load")

		for range 5000 {
			n.Send(Message{From: r.Uint32N(100), To: r.Uint32N(100), Kind: uint16(r.IntN(4))})
		}

		drain(s)

		return tr.Sum()
	}

	if first := run(11); first != run(11) {
		t.Fatal("nondeterministic")
	}

	if run(11) == run(12) {
		t.Fatal("seed ignored")
	}
}

type traceHandler struct {
	tr *Trace
	s  *Sim
}

func (h *traceHandler) Deliver(m *Message) {
	h.tr.Add(1, uint64(h.s.Now()), m.ID, uint64(m.From)<<32|uint64(m.To))
}

func TestLeaseCAS(t *testing.T) {
	s := New(1)
	l := NewLeaseServer(s, LeaseConfig{})

	if r := l.Do(NoMember, elect.Op{Kind: elect.OpGet}, 0); !errors.Is(r.Err, elect.ErrNotFound) {
		t.Fatal("get missing")
	}

	if r := l.Do(NoMember, elect.Op{Kind: elect.OpUpdate}, 0); !errors.Is(r.Err, elect.ErrNotFound) {
		t.Fatal("update missing")
	}

	c := l.Do(3, elect.Op{Seq: 9, Kind: elect.OpCreate, Record: elect.Record{Holder: "a", Term: 1, Incarnation: 1}}, 10)
	if c.Err != nil || c.Seq != 9 || c.Record.ResourceVersion != "1" || c.Record.UID == "" {
		t.Fatalf("create %+v", c)
	}

	if r := l.Do(NoMember, elect.Op{Kind: elect.OpCreate}, 11); !errors.Is(r.Err, elect.ErrConflict) {
		t.Fatal("double create")
	}

	upd := c.Record
	upd.Holder = "b"
	upd.Term = 2
	u := l.Do(NoMember, elect.Op{Kind: elect.OpUpdate, Record: upd}, 20)

	if u.Err != nil || u.Record.ResourceVersion != "2" || u.Record.UID != c.Record.UID {
		t.Fatalf("update %+v", u)
	}

	if r := l.Do(NoMember, elect.Op{Kind: elect.OpUpdate, Record: upd}, 21); !errors.Is(r.Err, elect.ErrConflict) {
		t.Fatal("stale rv accepted")
	}

	if err := CheckAudit(l.Audit()); err != nil {
		t.Fatal(err)
	}

	// Returned records are copies.
	u.Record.Seeds = append(u.Record.Seeds, "x")

	if cur := l.Do(NoMember, elect.Op{Kind: elect.OpGetCached}, 22); cur.Err != nil || len(cur.Record.Seeds) != 0 {
		t.Fatal("aliasing")
	}
}

func TestLeaseOutageConflictsAsync(t *testing.T) {
	s := New(3)
	l := NewLeaseServer(s, LeaseConfig{ConflictPPM: 500_000})
	l.AddOutage(100, 200)

	if r := l.Do(NoMember, elect.Op{Kind: elect.OpGet}, 150); !errors.Is(r.Err, ErrUnavailable) {
		t.Fatal("outage")
	}

	conflicts := 0

	for i := range 100 {
		if r := l.Do(NoMember, elect.Op{Kind: elect.OpCreate}, int64(300+i)); errors.Is(r.Err, elect.ErrConflict) {
			conflicts++
		}
	}

	if conflicts < 99 { // at most one create can succeed
		t.Fatal("create conflicts")
	}

	l.SetConflictPPM(0)

	var got []elect.Result

	l.Submit(1, elect.Op{Seq: 5, Kind: elect.OpGet}, func(r elect.Result) { got = append(got, r) })
	drain(s)

	if len(got) != 1 || got[0].Seq != 5 || s.Now() < 5_000_000 {
		t.Fatalf("submit %v at %d", got, s.Now())
	}
}

func TestCheckAudit(t *testing.T) {
	ok := []Write{
		{Holder: "a", Incarnation: 1, Term: 1},
		{Holder: "a", Incarnation: 1, Term: 1},
		{Holder: "", Term: 1},
		{Holder: "b", Incarnation: 1, Term: 2},
	}
	if err := CheckAudit(ok); err != nil {
		t.Fatal(err)
	}

	bad := [][]Write{
		{{Holder: "a", Term: 2}, {Holder: "a", Term: 1}},
		{{Holder: "a", Incarnation: 1, Term: 1}, {Holder: "a", Incarnation: 2, Term: 1}},
		{{Holder: "a", Term: 1}, {Holder: "", Term: 1}, {Holder: "b", Term: 1}},
	}
	for i, ws := range bad {
		if CheckAudit(ws) == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

type hookLog struct{ log []string }

func (h *hookLog) OnStart(m uint32, inc uint64) {
	if inc == 0 {
		panic("zero incarnation")
	}

	h.log = append(h.log, "start")
}
func (h *hookLog) OnCrash(uint32)  { h.log = append(h.log, "crash") }
func (h *hookLog) OnLeave(uint32)  { h.log = append(h.log, "leave") }
func (h *hookLog) OnPause(uint32)  { h.log = append(h.log, "pause") }
func (h *hookLog) OnResume(uint32) { h.log = append(h.log, "resume") }

func TestProcesses(t *testing.T) {
	s := New(1)
	h := &hookLog{}
	p := NewProcesses(s, 3, h)

	if !p.Start(0) || p.Start(0) || p.State(0) != Starting {
		t.Fatal("start")
	}

	inc := p.Get(0).Incarnation

	if !p.MarkUp(0) || !p.Pause(0) || p.State(0) != Paused || p.MarkUp(0) {
		t.Fatal("pause")
	}

	if !p.Resume(0) || p.State(0) != Up || p.Resume(0) {
		t.Fatal("resume")
	}

	if !p.Crash(0) || !p.Start(0) || p.Get(0).Incarnation == inc || p.Get(0).Starts != 2 {
		t.Fatal("restart")
	}

	if !p.Leave(0) || p.Leave(0) || !p.Exited(0) || p.State(0) != Down {
		t.Fatal("leave")
	}

	if p.Crash(1) || p.Pause(1) {
		t.Fatal("transitions from down")
	}

	p.Start(1)
	p.Start(2)
	p.Crash(2)

	if live := p.Live(nil); len(live) != 1 || live[0] != 1 {
		t.Fatalf("live %v", live)
	}

	if len(p.InState(nil, Crashed)) != 1 || !Starting.Alive() || Crashed.Alive() {
		t.Fatal("count")
	}

	want := "start pause resume crash start leave start start crash"

	got := ""

	for i, l := range h.log {
		if i > 0 {
			got += " "
		}

		got += l
	}

	if got != want {
		t.Fatalf("hooks %q", got)
	}

	for st := Down; st <= Crashed+1; st++ {
		if st.String() == "" {
			t.Fatal("name")
		}
	}

	// Incarnations are deterministic per seed.
	p2 := NewProcesses(New(1), 1, nil)
	p2.Start(0)

	if p2.Get(0).Incarnation != inc {
		t.Fatal("incarnation not deterministic")
	}
}

func TestScenarioDeterministic(t *testing.T) {
	for _, sz := range []SizeClass{SizeTiny, SizeSmall, SizeMedium, SizeLarge, SizeHuge} {
		a, b := Generate(42, sz), Generate(42, sz)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("size %d not deterministic", sz)
		}

		if reflect.DeepEqual(a, Generate(43, sz)) {
			t.Fatal("seed ignored")
		}
	}

	if Generate(1, SizeLarge).Members != 10_000 || Generate(1, SizeHuge).Members != 100_000 {
		t.Fatal("size classes")
	}
}

func TestScenarioCoversEveryKind(t *testing.T) {
	seen := map[FaultKind]bool{}

	for seed := range uint64(200) {
		s := Generate(seed, SizeTiny)
		if len(s.Faults) == 0 {
			t.Fatal("no faults")
		}

		capacity := s.Members

		var prev int64

		for _, f := range s.Faults {
			seen[f.Kind] = true

			if f.Start < prev || f.Start < s.Warmup {
				t.Fatal("faults not sorted or before warmup")
			}

			prev = f.Start

			if f.End() > s.Duration-s.Settle {
				t.Fatal("fault overlaps settle period")
			}

			if f.Kind == MassJoin {
				capacity += f.Count
			}
		}

		if capacity != s.Capacity || s.Settle <= 0 {
			t.Fatal("capacity/settle")
		}
	}

	for _, k := range AllFaultKinds() {
		if !seen[k] {
			t.Fatalf("kind %s never generated", k)
		}

		s := GenerateKinds(7, SizeSmall, k)
		if got := s.Kinds(); len(got) != 1 || got[0] != k {
			t.Fatalf("GenerateKinds(%s) = %v", k, got)
		}

		if k.String() == "unknown" {
			t.Fatal("name")
		}
	}

	if len(AllFaultKinds()) != 17 || FaultKind(0).String() != "unknown" {
		t.Fatal("kinds")
	}

	s := Generate(3, SizeSmall)
	if !strings.Contains(s.String(), "seed=3") {
		t.Fatal(s.String())
	}
}

func TestKeyGen(t *testing.T) {
	g := NewKeyGen(NewRand(1, "w"), 1000)
	counts := map[KeyKind]int{}
	ranks := make([]int, 1000)

	for range 20_000 {
		k, kind, rank := g.Next()
		counts[kind]++
		ranks[rank]++

		switch kind {
		case KeyBlob:
			if len(k) != 32 {
				t.Fatal("blob len")
			}
		case KeyStripe:
			if len(k) != 36 {
				t.Fatal("stripe len")
			}
		case KeyMeta:
			if !bytes.HasPrefix(k, []byte("repo-")) || !bytes.Contains(k, []byte(":tag-")) {
				t.Fatalf("meta %q", k)
			}
		}
	}

	if counts[KeyBlob] == 0 || counts[KeyStripe] == 0 || counts[KeyMeta] == 0 {
		t.Fatal(counts)
	}

	if ranks[0] < ranks[100]*5 {
		t.Fatal("not skewed")
	}

	a := append([]byte(nil), g.Blob(5)...)
	if !bytes.Equal(a, g.Blob(5)) || bytes.Equal(a, g.Blob(6)) || !bytes.Equal(g.Stripe(5, 1)[:32], a) {
		t.Fatal("digests")
	}

	if n := testing.AllocsPerRun(1000, func() { g.Next() }); n != 0 {
		t.Fatalf("Next allocates %v", n)
	}

	h := NewKeyGen(NewRand(1, "w"), 1000)
	g2 := NewKeyGen(NewRand(1, "w"), 1000)

	for range 100 {
		x, _, _ := h.Next()
		y, _, _ := g2.Next()

		if !bytes.Equal(x, y) {
			t.Fatal("nondeterministic")
		}
	}
}

func TestSeedsFromEnv(t *testing.T) {
	def := []uint64{1, 2}

	t.Setenv(EnvSeed, "")
	t.Setenv(EnvSeeds, "")

	if got, err := SeedsFromEnv(def); err != nil || !reflect.DeepEqual(got, def) {
		t.Fatal(got, err)
	}

	t.Setenv(EnvSeeds, "3")

	if got, err := SeedsFromEnv(def); err != nil || len(got) != 5 || got[0] != 1 {
		t.Fatal(got, err)
	}

	t.Setenv(EnvSeeds, "7, 0x10,")

	if got, err := SeedsFromEnv(def); err != nil || !reflect.DeepEqual(got, []uint64{7, 16}) {
		t.Fatal(got, err)
	}

	t.Setenv(EnvSeeds, "x,1")

	if _, err := SeedsFromEnv(def); err == nil {
		t.Fatal("bad list accepted")
	}

	t.Setenv(EnvSeed, "99")

	if got, err := SeedsFromEnv(def); err != nil || !reflect.DeepEqual(got, []uint64{99}) {
		t.Fatal(got, err)
	}

	t.Setenv(EnvSeed, "nope")

	if _, err := SeedsFromEnv(def); err == nil {
		t.Fatal("bad seed accepted")
	}

	if def[0] != 1 || def[1] != 2 {
		t.Fatal("defaults modified")
	}

	t.Setenv(EnvLarge, "1")

	if !Large() {
		t.Fatal("large")
	}

	if got := ReplayCommand(5); got != "CACHEMESH_DST_SEED=5 go test ./internal/cachemesh/internal/dst -run TestDST -count=1 -v" {
		t.Fatal(got)
	}
}
