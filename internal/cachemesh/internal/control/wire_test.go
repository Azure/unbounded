// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package control

import (
	"bytes"
	"errors"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

func env(k Kind) Message {
	m := Message{
		Kind:       k,
		From:       MemberRef{ID: "node-a", Incarnation: 7, Slot: 3},
		Version:    view.Version{Term: 2, Seq: 99},
		Epoch:      12,
		LeaderSlot: 0,
	}
	m.Digest[0], m.Digest[31] = 1, 0xff

	return m
}

func samples() map[string]Message {
	out := map[string]Message{}

	m := env(KindPing)
	out["ping"] = m

	m = env(KindPing)
	m.From = MemberRef{Slot: view.NoSlot}
	m.LeaderSlot = view.NoSlot
	m.Version = view.Version{Term: math.MaxUint64, Seq: math.MaxUint64}
	m.Epoch = math.MaxUint64
	out["ping-edge"] = m

	m = env(KindPong)
	m.LeaderAddr = "10.0.0.1:7000"
	out["pong"] = m

	out["ack"] = env(KindAck)

	m = env(KindNack)
	m.Reason = ReasonNotLeader
	out["nack"] = m

	for _, l := range []int8{LevelLeader, LevelLocal, 0, 7} {
		m = env(KindBroadcast)
		m.Level = l
		m.Delta = bytes.Repeat([]byte{byte(l)}, 100)
		out["broadcast"+string(rune('a'+l+2))] = m
	}

	m = env(KindBroadcast)
	m.Delta = []byte{}
	out["broadcast-empty"] = m

	m = env(KindCatchUpRequest)
	m.Have = view.Version{Term: 1, Seq: 5}
	m.HaveDigest[3] = 9
	m.MaxDeltas = math.MaxUint32
	out["catchup-req"] = m

	m = env(KindCatchUpResponse)
	m.Deltas = [][]byte{}
	out["catchup-empty"] = m

	m = env(KindCatchUpResponse)
	for i := range MaxDeltas {
		m.Deltas = append(m.Deltas, []byte{byte(i)})
	}

	m.Deltas[5] = []byte{}
	out["catchup-deltas"] = m

	m = env(KindCatchUpResponse)
	m.Snapshot = bytes.Repeat([]byte{4}, 1<<20)
	out["catchup-snapshot"] = m

	inner := env(KindJoin)
	inner.Member = MemberRef{ID: "new", Incarnation: 1, Slot: view.NoSlot}
	inner.Addr = "x:1"
	inner.Zone = "z1"
	inner.PartitionBits = 20
	out["join"] = inner

	m = env(KindRouted)
	m.Origin = MemberRef{ID: "o", Incarnation: 1, Slot: view.NoSlot}
	m.ToLeader = true
	m.TTL, m.Hops = math.MaxUint8, math.MaxUint8
	m.Inner = Append(nil, &inner)
	out["routed-leader"] = m

	m = env(KindRouted)
	m.Target = MemberRef{ID: "t", Incarnation: 2, Slot: 4}
	m.TTL = 3
	m.Inner = []byte{}
	out["routed-target"] = m

	m = env(KindLeave)
	m.Member = MemberRef{ID: "node-a", Incarnation: 7, Slot: 3}
	out["leave"] = m

	m = env(KindSuspect)
	m.Target = MemberRef{ID: "bad", Incarnation: 3, Slot: 10}
	m.Observer = MemberRef{ID: "node-a", Incarnation: 7, Slot: 3}
	out["suspect"] = m

	m = env(KindInvalidate)
	m.Seed = math.MaxUint64
	out["invalidate"] = m

	return out
}

func TestRoundTrip(t *testing.T) {
	for name, m := range samples() {
		t.Run(name, func(t *testing.T) {
			if err := m.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}

			b := Append(nil, &m)

			var got Message
			if err := Decode(b, &got); err != nil {
				t.Fatalf("Decode: %v", err)
			}

			if !reflect.DeepEqual(norm(m), norm(got)) {
				t.Fatalf("mismatch\n got %+v\nwant %+v", got.From, m.From)
			}

			if b2 := Append(nil, &got); !bytes.Equal(b, b2) {
				t.Fatal("re-encoding differs")
			}

			if k, err := PeekKind(b); err != nil || k != m.Kind {
				t.Fatalf("PeekKind = %v, %v", k, err)
			}

			// Detach must make got independent of b.
			got.Detach()
			clear(b)

			if !bytes.Equal(Append(nil, &got), Append(nil, &m)) {
				t.Fatal("Detach did not copy")
			}
		})
	}
}

// norm maps empty-but-non-nil slices to nil for comparison.
func norm(m Message) Message {
	if len(m.Deltas) == 0 {
		m.Deltas = nil
	}

	for i := range m.Deltas {
		if len(m.Deltas[i]) == 0 {
			m.Deltas[i] = nil
		}
	}

	for _, p := range []*[]byte{&m.Delta, &m.Snapshot, &m.Inner} {
		if len(*p) == 0 {
			*p = nil
		}
	}

	return m
}

func TestDecodeReuse(t *testing.T) {
	s := samples()
	r := s["catchup-deltas"]
	p := s["ping"]

	var m Message
	if err := Decode(Append(nil, &r), &m); err != nil {
		t.Fatal(err)
	}

	if err := Decode(Append(nil, &p), &m); err != nil {
		t.Fatal(err)
	}

	if len(m.Deltas) != 0 || cap(m.Deltas) < MaxDeltas {
		t.Fatalf("Deltas not reset/reused: len %d cap %d", len(m.Deltas), cap(m.Deltas))
	}

	if !reflect.DeepEqual(norm(m), norm(p)) {
		t.Fatal("stale fields after reuse")
	}
}

func TestReject(t *testing.T) {
	s := samples()
	enc := func(m Message) []byte { return Append(nil, &m) }
	ping := enc(s["ping"])

	mut := func(name string, f func(*Message)) []byte {
		m := s[name]
		f(&m)

		return enc(m)
	}

	cases := map[string]struct {
		b    []byte
		want error
	}{
		"empty":         {nil, view.ErrShort},
		"one-byte":      {ping[:1], view.ErrShort},
		"truncated":     {ping[:len(ping)-1], view.ErrShort},
		"trailing":      {append(append([]byte{}, ping...), 0), view.ErrTrailing},
		"bad-version":   {append([]byte{2}, ping[1:]...), ErrVersion},
		"zero-kind":     {append([]byte{1, 0}, ping[2:]...), ErrUnknownKind},
		"unknown-kind":  {append([]byte{1, 200}, ping[2:]...), ErrUnknownKind},
		"long-id":       {mut("ping", func(m *Message) { m.From.ID = string(make([]byte, MaxIDLen+1)) }), view.ErrTooLarge},
		"long-addr":     {mut("pong", func(m *Message) { m.LeaderAddr = string(make([]byte, MaxAddrLen+1)) }), view.ErrTooLarge},
		"long-zone":     {mut("join", func(m *Message) { m.Zone = string(make([]byte, MaxZoneLen+1)) }), view.ErrTooLarge},
		"bad-pbits":     {mut("join", func(m *Message) { m.PartitionBits = MaxPartitionBits + 1 }), view.ErrInvalidValue},
		"level-low":     {mut("broadcasta", func(m *Message) { m.Level = -3 }), view.ErrInvalidValue},
		"level-high":    {mut("broadcasta", func(m *Message) { m.Level = MaxDimensions }), view.ErrInvalidValue},
		"nack-zero":     {mut("nack", func(m *Message) { m.Reason = 0 }), view.ErrInvalidValue},
		"nack-high":     {mut("nack", func(m *Message) { m.Reason = reasonMax + 1 }), view.ErrInvalidValue},
		"both":          {mut("catchup-snapshot", func(m *Message) { m.Deltas = [][]byte{{1}} }), view.ErrInvalidValue},
		"too-many":      {mut("catchup-deltas", func(m *Message) { m.Deltas = append(m.Deltas, nil) }), view.ErrTooLarge},
		"big-inner":     {mut("routed-target", func(m *Message) { m.Inner = make([]byte, MaxInnerSize+1) }), view.ErrTooLarge},
		"zero-deltas":   {append(enc(s["catchup-empty"])[:len(enc(s["catchup-empty"]))-1], 1, 0), view.ErrInvalidValue},
		"empty-snap":    {append(enc(s["catchup-empty"])[:len(enc(s["catchup-empty"]))-1], 2, 0), view.ErrInvalidValue},
		"bad-bool":      {badBool(enc(s["routed-leader"]), len(enc(s["ack"]))+7), view.ErrInvalidValue},
		"oversize":      {append([]byte{1, 1}, make([]byte, MaxMessageSize)...), ErrTooLarge},
		"maxdeltas-u33": {append(enc(s["catchup-req"])[:len(enc(s["catchup-req"]))-5], view.AppendUvarint(nil, 1<<32)...), view.ErrInvalidValue},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var m Message
			if err := Decode(c.b, &m); !errors.Is(err, c.want) {
				t.Fatalf("Decode = %v, want %v", err, c.want)
			}
		})
	}
}

// badBool corrupts the Routed ToLeader byte at offset i.
func badBool(b []byte, i int) []byte {
	b[i] = 2

	return b
}

func TestValidate(t *testing.T) {
	m := env(Kind(99))
	if err := m.Validate(); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("Validate = %v", err)
	}
}

func TestKindString(t *testing.T) {
	if KindPing.String() != "Ping" || Kind(0).String() != "Kind(0)" || KindInvalidate.String() != "Invalidate" {
		t.Fatal("bad String")
	}
}

func FuzzDecode(f *testing.F) {
	for _, m := range samples() {
		if len(m.Snapshot) > 4096 || len(m.Deltas) > 8 {
			continue
		}

		f.Add(Append(nil, &m))
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		var m Message
		if err := Decode(b, &m); err != nil {
			return
		}

		if got := Append(nil, &m); !bytes.Equal(got, b) {
			t.Fatalf("non-canonical: %x -> %x", b, got)
		}

		if len(m.Deltas) > len(b) {
			t.Fatal("deltas exceed input")
		}
	})
}

func BenchmarkPingAppend(b *testing.B) {
	m := env(KindPing)
	buf := make([]byte, 0, 128)

	b.ReportAllocs()

	for b.Loop() {
		buf = Append(buf[:0], &m)
	}
}

func BenchmarkPingDecode(b *testing.B) {
	m := env(KindPing)
	buf := Append(nil, &m)

	var out Message

	b.ReportAllocs()

	for b.Loop() {
		if err := Decode(buf, &out); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBroadcastAppend(b *testing.B) {
	m := env(KindBroadcast)
	m.Delta = make([]byte, 4096)
	buf := make([]byte, 0, 8192)

	b.ReportAllocs()
	b.SetBytes(4096)

	for b.Loop() {
		buf = Append(buf[:0], &m)
	}
}

func BenchmarkBroadcastDecode(b *testing.B) {
	m := env(KindBroadcast)
	m.Delta = make([]byte, 4096)
	buf := Append(nil, &m)

	var out Message

	b.ReportAllocs()
	b.SetBytes(4096)

	for b.Loop() {
		if err := Decode(buf, &out); err != nil {
			b.Fatal(err)
		}
	}
}

// fuzzFixture is a small real cluster's history: a view, the deltas
// published after it, a snapshot, and the identities of its members.
type fuzzFixture struct {
	base   *view.View
	deltas [][]byte
	snap   []byte
	ids    []string
	incs   []uint64
}

var (
	fixtureOnce sync.Once
	fixture     fuzzFixture
)

func buildFixture() {
	c := newCluster(nil)

	var raws [][]byte

	record := false

	c.add(c.cfg(0, nil))

	prev := c.nodes[0].cfg.Hooks.OnPublish
	c.nodes[0].cfg.Hooks.OnPublish = func(d *view.Delta) {
		prev(d)

		if record {
			raws = append(raws, view.EncodeDelta(d))
		}
	}
	c.run(5 * time.Second)

	for i := 1; i < 6; i++ {
		c.add(c.cfg(i, []string{addr(0)}))
	}

	c.run(30 * time.Second)

	fixture.base = c.nodes[0].View()
	record = true

	for i := range 8 {
		c.nodes[0].Invalidate(c.now, uint64(i))
		c.run(1500 * time.Millisecond)
	}

	c.run(30 * time.Second)

	fixture.deltas = raws
	fixture.snap = view.EncodeSnapshot(c.nodes[0].View())

	for i, n := range c.nodes {
		fixture.ids = append(fixture.ids, "n"+strconv.Itoa(i))
		fixture.incs = append(fixture.incs, n.cfg.Incarnation)
	}
}

// fuzzReader hands out fuzz bytes, then zeros.
type fuzzReader struct{ b []byte }

func (r *fuzzReader) more() bool { return len(r.b) > 0 }

func (r *fuzzReader) u8() byte {
	if len(r.b) == 0 {
		return 0
	}

	v := r.b[0]
	r.b = r.b[1:]

	return v
}

func (r *fuzzReader) u64() uint64 {
	var v uint64
	for range 8 {
		v = v<<8 | uint64(r.u8())
	}

	return v
}

func (r *fuzzReader) bytes(n int) []byte {
	n = min(n, len(r.b))
	out := bytes.Clone(r.b[:n])
	r.b = r.b[n:]

	return out
}

// term draws a term near the fixture's, or occasionally far above it.
func (r *fuzzReader) term() uint64 {
	switch r.u8() % 8 {
	case 0:
		return r.u64()
	case 1:
		return fixture.base.Term() + 1<<32
	default:
		return fixture.base.Term() + uint64(r.u8()%4)
	}
}

func (r *fuzzReader) mutate(b []byte) []byte {
	b = bytes.Clone(b)
	if len(b) > 0 && r.u8()%2 == 0 {
		b[int(r.u8())%len(b)] ^= r.u8() | 1
	}

	return b
}

func (r *fuzzReader) ref() MemberRef {
	i := int(r.u8()) % (len(fixture.ids) + 1)
	if i == len(fixture.ids) {
		return MemberRef{ID: string(r.bytes(int(r.u8() % 16))), Incarnation: r.u64(), Slot: view.Slot(r.u8())}
	}

	return MemberRef{ID: fixture.ids[i], Incarnation: fixture.incs[i], Slot: view.Slot(r.u8() % 8)}
}

// message builds a mostly valid control message from fuzz bytes.
func (r *fuzzReader) message() []byte {
	var m Message

	m.Kind = Kind(1 + r.u8()%16)
	m.From = r.ref()
	m.Version = view.Version{Term: r.term(), Seq: uint64(r.u8())}
	m.Epoch = uint64(r.u8())
	m.LeaderSlot = view.Slot(r.u8() % 8)

	if r.u8()%2 == 0 {
		m.Digest = fixture.base.Digest()
	}

	switch m.Kind {
	case KindBroadcast:
		m.Level = int8(r.u8()%6) - 2
		if len(fixture.deltas) > 0 {
			m.Delta = r.mutate(fixture.deltas[int(r.u8())%len(fixture.deltas)])
		}
	case KindCatchUpRequest:
		m.Have = view.Version{Term: r.term(), Seq: uint64(r.u8())}
		m.MaxDeltas = uint32(r.u8())
	case KindCatchUpResponse:
		if r.u8()%2 == 0 {
			m.Snapshot = r.mutate(fixture.snap)
		} else {
			for range r.u8() % 4 {
				if len(fixture.deltas) > 0 {
					m.Deltas = append(m.Deltas, r.mutate(fixture.deltas[int(r.u8())%len(fixture.deltas)]))
				}
			}
		}
	case KindRouted:
		m.Origin = r.ref()
		m.ToLeader = r.u8()%4 != 0
		m.TTL = r.u8() % 6
		m.Inner = r.message()
	case KindJoin, KindLeave:
		m.Member = r.ref()
		m.Addr = "f" + strconv.Itoa(int(r.u8()))
		m.PartitionBits = 10 - r.u8()%2
	case KindSuspect:
		m.Target, m.Observer = r.ref(), r.ref()
	case KindInvalidate:
		m.Seed = uint64(r.u8())
	case KindPong:
		m.LeaderAddr = "f" + strconv.Itoa(int(r.u8()))
	}

	return Append(nil, &m)
}

// fuzzNode starts a node in one of three states: a joiner with a seed, a
// member started from the fixture view, or a member that won the Lease and
// leads.
func fuzzNode(state byte) (*Node, *memLease, int64) {
	l := &memLease{}
	now := 1000 * sec
	cfg := Config{PartitionBits: 10, Elect: ElectionConfig{ExpectedClusterSize: 10}, DeltaLogSize: 32}

	switch state % 3 {
	case 0:
		cfg.ID, cfg.Incarnation, cfg.Addr, cfg.Seeds = "fz", 99, "fz:1", []string{addr(0)}
	default:
		i := 1 + int(state)%(len(fixture.ids)-1)
		cfg.ID, cfg.Incarnation, cfg.Addr, cfg.Initial = fixture.ids[i], fixture.incs[i], addr(i), fixture.base
	}

	n := New(cfg, rand.New(rand.NewPCG(uint64(state), 1)))
	n.Start(now, time.Unix(0, now))

	if state%3 == 2 {
		for i := 0; i < 120; i++ {
			if ok, _ := n.Leading(); ok {
				break
			}

			for _, op := range n.LeaseOps() {
				n.OnLease(now, time.Unix(0, now), l.do(op, time.Unix(0, now)))
			}

			now += sec
			n.Tick(now, time.Unix(0, now))
			n.Outbox()
		}
	}

	return n, l, now
}

// FuzzHandle feeds arbitrary bytes and mostly valid messages, replies,
// Lease results, and timer ticks into a node in various states. The node
// must not panic, its state must stay bounded, and its view must stay
// valid. Run with go test -fuzz=FuzzHandle -fuzztime=60s.
func FuzzHandle(f *testing.F) {
	fixtureOnce.Do(buildFixture)

	f.Add([]byte{0, 1, 3, 2, 2, 2})
	f.Add([]byte{1, 1, 2, 0, 0, 0, 0, 1, 1, 1, 0, 5, 1, 2, 3})
	f.Add([]byte{2, 1, 1, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 3, 0, 4, 1})
	f.Add([]byte{5, 1, 6, 1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 1, 1, 1})

	if len(fixture.deltas) == 0 {
		f.Fatal("fixture has no deltas")
	}

	f.Fuzz(runFuzzOne)
}

func runFuzzOne(t *testing.T, data []byte) {
	{
		r := &fuzzReader{b: data}
		n, lease, now := fuzzNode(r.u8())

		var (
			calls  []uint64
			ops    []Op
			valid  = map[*view.View]bool{}
			floods int
		)

		check := func() {
			for _, c := range n.Outbox() {
				if len(calls) < 64 {
					calls = append(calls, c.ID)
				}
			}

			ops = append(ops, n.LeaseOps()...)
			if len(ops) > 64 {
				ops = ops[len(ops)-64:]
			}

			s := n.Stats()
			if s.Calls > MaxPendingCalls || s.Ahead > n.cfg.MaxAhead || s.Seeds > 4*MaxSeeds ||
				s.Log > n.cfg.DeltaLogSize || s.LeaseSeeds > MaxSeeds {
				t.Fatalf("unbounded state: %+v", s)
			}

			if v := n.View(); !valid[v] {
				if err := view.Validate(v); err != nil {
					t.Fatalf("invalid view %v: %v", v.Version(), err)
				}

				valid[v] = true
			}
		}

		check()

		for steps := 0; r.more() && steps < 256; steps++ {
			switch r.u8() % 8 {
			case 0:
				n.Handle(now, "f"+strconv.Itoa(int(r.u8()%4)), r.bytes(int(r.u8())))
			case 1, 2:
				n.Handle(now, "f"+strconv.Itoa(int(r.u8()%4)), r.message())
			case 3:
				now += int64(r.u8()) * int64(100*time.Millisecond)
				n.Tick(now, time.Unix(0, now))
			case 4:
				if len(calls) > 0 {
					i := int(r.u8()) % len(calls)
					id := calls[i]
					calls = slices.Delete(calls, i, i+1)

					switch r.u8() % 3 {
					case 0:
						n.OnReply(now, id, nil, errDown)
					case 1:
						n.OnReply(now, id, r.bytes(int(r.u8())), nil)
					default:
						n.OnReply(now, id, r.message(), nil)
					}
				}
			case 5:
				if len(ops) > 0 {
					op := ops[0]
					ops = ops[1:]

					res := lease.do(op, time.Unix(0, now))
					if r.u8()%4 == 0 {
						res.Record.Term = r.term()
						res.Record.Holder = string(r.bytes(int(r.u8() % 8)))
						res.Record.RenewTime = time.Unix(0, now-int64(r.u8())*sec)
					}

					n.OnLease(now, time.Unix(0, now), res)
				}
			case 6:
				switch r.u8() % 3 {
				case 0:
					n.NoteVersion(now, view.Slot(r.u8()%8), view.Version{Term: r.term(), Seq: uint64(r.u8())})
				case 1:
					n.Invalidate(now, uint64(r.u8()))
				default:
					n.Leave(now)
				}
			case 7:
				// A flood of one message, at most twice per input, large
				// enough to reach MaxPendingCalls.
				msg := r.message()

				if floods < 2 {
					floods++

					for range int(r.u8()) * 32 {
						n.Handle(now, "f0", msg)
					}
				}
			}

			check()
		}
	}
}

// TestFuzzStates checks that fuzzNode reaches the states it promises.
func TestFuzzStates(t *testing.T) {
	fixtureOnce.Do(buildFixture)

	if n, _, _ := fuzzNode(0); n.Self() != view.NoSlot {
		t.Fatal("state 0 should be a joiner")
	}

	if n, _, _ := fuzzNode(1); n.Self() == view.NoSlot {
		t.Fatal("state 1 should be a member")
	}

	if n, _, _ := fuzzNode(2); !func() bool { ok, _ := n.Leading(); return ok }() {
		t.Fatal("state 2 should lead")
	}
}
