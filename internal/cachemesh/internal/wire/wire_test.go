// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/Azure/unbounded/internal/cachemesh/internal/codec"
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
		"empty":         {nil, codec.ErrShort},
		"one-byte":      {ping[:1], codec.ErrShort},
		"truncated":     {ping[:len(ping)-1], codec.ErrShort},
		"trailing":      {append(append([]byte{}, ping...), 0), codec.ErrTrailing},
		"bad-version":   {append([]byte{2}, ping[1:]...), ErrVersion},
		"zero-kind":     {append([]byte{1, 0}, ping[2:]...), ErrUnknownKind},
		"unknown-kind":  {append([]byte{1, 200}, ping[2:]...), ErrUnknownKind},
		"long-id":       {mut("ping", func(m *Message) { m.From.ID = string(make([]byte, MaxIDLen+1)) }), codec.ErrTooLarge},
		"long-addr":     {mut("pong", func(m *Message) { m.LeaderAddr = string(make([]byte, MaxAddrLen+1)) }), codec.ErrTooLarge},
		"long-zone":     {mut("join", func(m *Message) { m.Zone = string(make([]byte, MaxZoneLen+1)) }), codec.ErrTooLarge},
		"bad-pbits":     {mut("join", func(m *Message) { m.PartitionBits = MaxPartitionBits + 1 }), codec.ErrInvalid},
		"level-low":     {mut("broadcasta", func(m *Message) { m.Level = -3 }), codec.ErrInvalid},
		"level-high":    {mut("broadcasta", func(m *Message) { m.Level = MaxDimensions }), codec.ErrInvalid},
		"nack-zero":     {mut("nack", func(m *Message) { m.Reason = 0 }), codec.ErrInvalid},
		"nack-high":     {mut("nack", func(m *Message) { m.Reason = reasonMax + 1 }), codec.ErrInvalid},
		"both":          {mut("catchup-snapshot", func(m *Message) { m.Deltas = [][]byte{{1}} }), codec.ErrInvalid},
		"too-many":      {mut("catchup-deltas", func(m *Message) { m.Deltas = append(m.Deltas, nil) }), codec.ErrTooLarge},
		"big-inner":     {mut("routed-target", func(m *Message) { m.Inner = make([]byte, MaxInnerSize+1) }), codec.ErrTooLarge},
		"zero-deltas":   {append(enc(s["catchup-empty"])[:len(enc(s["catchup-empty"]))-1], 1, 0), codec.ErrInvalid},
		"empty-snap":    {append(enc(s["catchup-empty"])[:len(enc(s["catchup-empty"]))-1], 2, 0), codec.ErrInvalid},
		"bad-bool":      {badBool(enc(s["routed-leader"]), len(enc(s["ack"]))+7), codec.ErrInvalid},
		"oversize":      {append([]byte{1, 1}, make([]byte, MaxMessageSize)...), ErrTooLarge},
		"maxdeltas-u33": {append(enc(s["catchup-req"])[:len(enc(s["catchup-req"]))-5], codec.AppendUvarint(nil, 1<<32)...), codec.ErrInvalid},
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
