// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import "testing"

type nopTarget struct{}

func (nopTarget) Fire(_, _ uint64) {}

// BenchmarkSimPushPop1M measures one schedule plus one step with about one
// million events pending.
func BenchmarkSimPushPop1M(b *testing.B) {
	s := New(1)
	r := s.Stream("bench")

	var tg nopTarget

	for range 1_000_000 {
		s.AfterTarget(r.Int64N(1_000_000_000), tg, 0, 0)
	}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		s.AfterTarget(r.Int64N(1_000_000_000), tg, 0, 0)
		s.Step()
	}
}

// BenchmarkSimClosure measures closure events with a small queue.
func BenchmarkSimClosure(b *testing.B) {
	s := New(1)
	fn := func() {}

	b.ReportAllocs()

	for range b.N {
		s.After(10, fn)
		s.Step()
	}
}

type nopHandler struct{}

func (nopHandler) Deliver(*Message) {}

// BenchmarkNetSendDeliver measures one send and its delivery among 100k
// members with about 100k messages in flight.
func BenchmarkNetSendDeliver(b *testing.B) {
	const members = 100_000

	s := New(1)
	n := NewNet(s, members, nopHandler{}, NetConfig{LossPPM: 1000, DupPPM: 1000})
	r := s.Stream("bench")
	payload := make([]byte, 64)

	for range 100_000 {
		n.Send(Message{From: r.Uint32N(members), To: r.Uint32N(members), Payload: payload})
	}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		n.Send(Message{From: r.Uint32N(members), To: r.Uint32N(members), Payload: payload})
		s.Step()
	}
}

func BenchmarkHistogramRecord(b *testing.B) {
	var h Histogram

	b.ReportAllocs()

	for i := range b.N {
		h.Record(int64(i) * 997)
	}
}
