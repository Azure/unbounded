// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/unbounded/internal/cachemesh/internal/elect"
)

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

	s.Run(0)

	if len(b.got) != 10 || s.Now() != 1000 {
		t.Fatalf("got %d at %d", len(b.got), s.Now())
	}

	for i, m := range b.got {
		if len(m.Payload) != i {
			t.Fatal("no-jitter messages reordered")
		}
	}

	if n.Sent(0).Msgs != 10 || n.Sent(0).Bytes != 45 || n.Received(1).Bytes != 45 || n.Kind(7).Delivered.Msgs != 10 {
		t.Fatal("counters")
	}

	if m, st := n.MaxSent(); m != 0 || st.Msgs != 10 {
		t.Fatal("max sent")
	}
}

func TestNetJitterReorders(t *testing.T) {
	s, n, b := newTestNet(2, NetConfig{Latency: 1000, Jitter: 100_000})

	for i := range 100 {
		n.Send(Message{From: 0, To: 1, ID: uint64(i)})
	}

	s.Run(0)

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

	s.Run(0)

	if got := len(b.got); got < 7500 || got > 8500 || uint64(10_000-got) != n.Stats().Lost {
		t.Fatalf("delivered %d lost %d", got, n.Stats().Lost)
	}

	s, n, b = newTestNet(3, NetConfig{DupPPM: 500_000})

	for range 1000 {
		n.Send(Message{From: 0, To: 1})
	}

	s.Run(0)

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
	s.Run(0)

	if len(b.got) != 1 || b.got[0].To != 2 || n.Stats().Partitioned != 1 {
		t.Fatal("cross-group message delivered")
	}

	// Partition at delivery time drops in-flight messages; heal restores.
	n.Heal()
	n.Send(Message{From: 0, To: 1})
	n.SetGroup(0, 5)
	s.Run(0)

	if len(b.got) != 1 {
		t.Fatal("in-flight message crossed partition")
	}

	n.Heal()
	n.Send(Message{From: 0, To: 1})
	s.Run(0)

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
	s.Run(0)

	if len(b.got) != 2 {
		t.Fatal("held messages not delivered")
	}

	s, n, b = newTestNet(5, NetConfig{DropWhilePaused: true})
	n.Pause(1)
	n.Send(Message{From: 0, To: 1})
	s.Run(0)
	n.Resume(1)
	s.Run(0)

	if len(b.got) != 0 || n.Stats().Paused != 1 {
		t.Fatal("drop while paused")
	}

	s, n, b = newTestNet(5, NetConfig{})
	n.Pause(1)
	n.Send(Message{From: 0, To: 1})
	s.Run(0)
	n.DropHeld(1)
	n.Resume(1)
	s.Run(0)

	if len(b.got) != 0 {
		t.Fatal("DropHeld")
	}
}

func TestNetSlowAndLink(t *testing.T) {
	s, n, _ := newTestNet(6, NetConfig{Latency: 1000, NoJitter: true})
	n.SetSlow(1, 3000)
	n.Send(Message{From: 0, To: 1})
	s.Run(0)

	if s.Now() != 3000 {
		t.Fatalf("slow latency %d", s.Now())
	}

	n.SetLink(0, 2, Link{Latency: 50})

	start := s.Now()

	n.Send(Message{From: 0, To: 2})
	s.Run(0)

	if s.Now()-start != 50 {
		t.Fatal("link override")
	}

	n.ClearLink(0, 2)
	n.SetLink(0, 2, Link{Latency: 1, LossPPM: 1_000_000})
	n.Send(Message{From: 0, To: 2})
	s.Run(0)

	if n.Stats().Lost != 1 {
		t.Fatal("link loss")
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

	s.Run(0)

	for _, id := range calls {
		if c.n[id] != 1 {
			t.Fatalf("call %d completed %d times", id, c.n[id])
		}
	}

	st := n.Stats()
	if c.errs == 0 || c.errs == len(calls) || st.LateResponses == 0 || n.PendingCalls() != 0 {
		t.Fatalf("errs %d late %d", c.errs, st.LateResponses)
	}

	// CancelCall suppresses completion.
	id := n.Call(Message{From: 0, To: 1}, 10, c)
	if !n.CancelCall(id) || n.CancelCall(id) {
		t.Fatal("cancel call")
	}

	s.Run(0)

	if c.n[id] != 0 {
		t.Fatal("canceled call completed")
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

		s.Run(0)

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

	if r := l.Do(elect.Op{Kind: elect.OpGet}, 0); !errors.Is(r.Err, elect.ErrNotFound) {
		t.Fatal("get missing")
	}

	if r := l.Do(elect.Op{Kind: elect.OpUpdate}, 0); !errors.Is(r.Err, elect.ErrNotFound) {
		t.Fatal("update missing")
	}

	c := l.DoAs(3, elect.Op{Seq: 9, Kind: elect.OpCreate, Record: elect.Record{Holder: "a", Term: 1, Incarnation: 1}}, 10)
	if c.Err != nil || c.Seq != 9 || c.Record.ResourceVersion != "1" || c.Record.UID == "" {
		t.Fatalf("create %+v", c)
	}

	if r := l.Do(elect.Op{Kind: elect.OpCreate}, 11); !errors.Is(r.Err, elect.ErrConflict) {
		t.Fatal("double create")
	}

	upd := c.Record
	upd.Holder = "b"
	upd.Term = 2
	u := l.Do(elect.Op{Kind: elect.OpUpdate, Record: upd}, 20)

	if u.Err != nil || u.Record.ResourceVersion != "2" || u.Record.UID != c.Record.UID {
		t.Fatalf("update %+v", u)
	}

	if r := l.Do(elect.Op{Kind: elect.OpUpdate, Record: upd}, 21); !errors.Is(r.Err, elect.ErrConflict) {
		t.Fatal("stale rv accepted")
	}

	if err := CheckAudit(l.Audit()); err != nil {
		t.Fatal(err)
	}

	// Returned records are copies.
	u.Record.Seeds = append(u.Record.Seeds, "x")

	if cur, ok := l.Current(); !ok || len(cur.Seeds) != 0 {
		t.Fatal("aliasing")
	}

	st := l.Store(1)
	ctx := context.Background()

	got, err := st.Get(ctx)
	if err != nil || got.Holder != "b" {
		t.Fatal("store get")
	}

	got.Holder = "c"
	got.Term = 3

	if _, err := st.Update(ctx, got); err != nil {
		t.Fatal(err)
	}

	if _, err := st.Create(ctx, got); !errors.Is(err, elect.ErrConflict) {
		t.Fatal("store create")
	}

	if _, err := st.GetCached(ctx); err != nil {
		t.Fatal(err)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := st.Get(cctx); err == nil {
		t.Fatal("canceled ctx")
	}

	if res := elect.Execute(ctx, st, elect.Op{Seq: 4, Kind: elect.OpGet}); res.Err != nil || res.Seq != 4 {
		t.Fatal("execute")
	}
}

func TestLeaseCachedStale(t *testing.T) {
	s := New(2)
	l := NewLeaseServer(s, LeaseConfig{CacheLag: 1000})
	c := l.Do(elect.Op{Kind: elect.OpCreate, Record: elect.Record{Holder: "a"}}, 0)
	r := c.Record

	for i := int64(1); i <= 10; i++ {
		r.Term = uint64(i)
		r = l.Do(elect.Op{Kind: elect.OpUpdate, Record: r}, i*100).Record
	}

	stale, fresh := 0, 0

	for range 200 {
		g := l.Do(elect.Op{Kind: elect.OpGetCached}, 1500)
		if g.Err != nil {
			t.Fatal(g.Err)
		}

		if g.Record.Term < 10 {
			stale++
		} else {
			fresh++
		}
	}

	if stale == 0 || fresh == 0 {
		t.Fatalf("stale %d fresh %d", stale, fresh)
	}

	if g := l.Do(elect.Op{Kind: elect.OpGet}, 1000); g.Record.Term != 10 {
		t.Fatal("consistent read stale")
	}

	// Much later the cache has caught up and history was trimmed.
	r.Term = 11
	l.Do(elect.Op{Kind: elect.OpUpdate, Record: r}, 100_000)

	for range 50 {
		if g := l.Do(elect.Op{Kind: elect.OpGetCached}, 200_000); g.Record.Term != 11 {
			t.Fatal("cache never caught up")
		}
	}

	if len(l.hist) > 2 {
		t.Fatalf("history not trimmed: %d", len(l.hist))
	}

	// A cached read before creation sees not-found.
	l2 := NewLeaseServer(s, LeaseConfig{CacheLag: 1_000_000})
	l2.Do(elect.Op{Kind: elect.OpCreate}, 10)

	seenMissing := false

	for range 50 {
		if g := l2.Do(elect.Op{Kind: elect.OpGetCached}, 20); errors.Is(g.Err, elect.ErrNotFound) {
			seenMissing = true
		}
	}

	if !seenMissing {
		t.Fatal("cached read never stale before create")
	}
}

func TestLeaseOutageConflictsAsync(t *testing.T) {
	s := New(3)
	l := NewLeaseServer(s, LeaseConfig{ConflictPPM: 500_000})
	l.AddOutage(100, 200)

	if r := l.Do(elect.Op{Kind: elect.OpGet}, 150); !errors.Is(r.Err, ErrUnavailable) {
		t.Fatal("outage")
	}

	conflicts := 0

	for i := range 100 {
		if r := l.Do(elect.Op{Kind: elect.OpCreate}, int64(300+i)); errors.Is(r.Err, elect.ErrConflict) {
			conflicts++
		}
	}

	if conflicts < 99 { // at most one create can succeed
		t.Fatal("create conflicts")
	}

	l.SetConflictPPM(0)

	var got []elect.Result

	l.Submit(1, elect.Op{Seq: 5, Kind: elect.OpGet}, func(r elect.Result) { got = append(got, r) })
	s.Run(0)

	if len(got) != 1 || got[0].Seq != 5 || s.Now() < 5_000_000 {
		t.Fatalf("submit %v at %d", got, s.Now())
	}
}

func TestLeaseQPS(t *testing.T) {
	s := New(4)
	l := NewLeaseServer(s, LeaseConfig{})

	// Member 1: 10 ops spread over 10s. Member 2: 30 ops within 0.5s
	// straddling a second boundary.
	for i := range int64(10) {
		l.DoAs(1, elect.Op{Kind: elect.OpGetCached}, i*nsPerSec)
	}

	for i := range int64(30) {
		l.DoAs(2, elect.Op{Kind: elect.OpGet}, 4_800_000_000+i*15_000_000)
	}

	q := l.QPS()
	if q.Member(1).Max != 1 || q.Member(1).Total != 10 {
		t.Fatalf("member 1 %+v", q.Member(1))
	}

	if q.Member(2).Max != 30 || q.Kind(elect.OpGet).Total != 30 {
		t.Fatalf("member 2 %+v", q.Member(2))
	}

	if q.All().Max != 31 || q.All().Total != 40 {
		t.Fatalf("all %+v", q.All())
	}

	if m := q.Members(); len(m) != 2 || m[0] != 1 {
		t.Fatal("members")
	}

	series, first := q.Series(3)
	if first != 7 || len(series) != 3 || series[0] != 1 {
		t.Fatalf("series %v from %d", series, first)
	}

	all, first := q.Series(0)
	if first != 0 || all[4] != 15 || all[5] != 17 {
		t.Fatalf("series %v", all)
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

	if !p.Restart(0) || p.Get(0).Incarnation == inc || p.Get(0).Starts != 2 {
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

	if p.Count(Crashed) != 1 || len(p.InState(nil, Crashed)) != 1 || !Starting.Alive() || Crashed.Alive() {
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
