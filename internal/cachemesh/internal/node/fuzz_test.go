// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"bytes"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/elect"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/wire"
)

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

func (r *fuzzReader) ref() wire.MemberRef {
	i := int(r.u8()) % (len(fixture.ids) + 1)
	if i == len(fixture.ids) {
		return wire.MemberRef{ID: string(r.bytes(int(r.u8() % 16))), Incarnation: r.u64(), Slot: view.Slot(r.u8())}
	}

	return wire.MemberRef{ID: fixture.ids[i], Incarnation: fixture.incs[i], Slot: view.Slot(r.u8() % 8)}
}

// message builds a mostly valid control message from fuzz bytes.
func (r *fuzzReader) message() []byte {
	var m wire.Message

	m.Kind = wire.Kind(1 + r.u8()%16)
	m.From = r.ref()
	m.Version = view.Version{Term: r.term(), Seq: uint64(r.u8())}
	m.Epoch = uint64(r.u8())
	m.LeaderSlot = view.Slot(r.u8() % 8)

	if r.u8()%2 == 0 {
		m.Digest = fixture.base.Digest()
	}

	switch m.Kind {
	case wire.KindBroadcast:
		m.Level = int8(r.u8()%6) - 2
		if len(fixture.deltas) > 0 {
			m.Delta = r.mutate(fixture.deltas[int(r.u8())%len(fixture.deltas)])
		}
	case wire.KindCatchUpRequest:
		m.Have = view.Version{Term: r.term(), Seq: uint64(r.u8())}
		m.MaxDeltas = uint32(r.u8())
	case wire.KindCatchUpResponse:
		if r.u8()%2 == 0 {
			m.Snapshot = r.mutate(fixture.snap)
		} else {
			for range r.u8() % 4 {
				if len(fixture.deltas) > 0 {
					m.Deltas = append(m.Deltas, r.mutate(fixture.deltas[int(r.u8())%len(fixture.deltas)]))
				}
			}
		}
	case wire.KindRouted:
		m.Origin = r.ref()
		m.ToLeader = r.u8()%4 != 0
		m.TTL = r.u8() % 6
		m.Inner = r.message()
	case wire.KindJoin, wire.KindLeave:
		m.Member = r.ref()
		m.Addr = "f" + strconv.Itoa(int(r.u8()))
		m.PartitionBits = 10 - r.u8()%2
	case wire.KindSuspect:
		m.Target, m.Observer = r.ref(), r.ref()
	case wire.KindInvalidate:
		m.Seed = uint64(r.u8())
	case wire.KindPong:
		m.LeaderAddr = "f" + strconv.Itoa(int(r.u8()))
	}

	return wire.Append(nil, &m)
}

// fuzzNode starts a node in one of three states: a joiner with a seed, a
// member started from the fixture view, or a member that won the Lease and
// leads.
func fuzzNode(state byte) (*Node, *memLease, int64) {
	l := &memLease{}
	now := 1000 * sec
	cfg := Config{PartitionBits: 10, Elect: elect.Config{ExpectedClusterSize: 10}, DeltaLogSize: 32}

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
			ops    []elect.Op
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
			if s.Calls > MaxPendingCalls || s.Ahead > n.cfg.MaxAhead || s.Seeds > 4*elect.MaxSeeds ||
				s.Log > n.cfg.DeltaLogSize || s.LeaseSeeds > elect.MaxSeeds {
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
