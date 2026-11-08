// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package elect

import (
	"testing"
	"time"
)

// TestObserveTermRaisesNextTerm: a term seen outside the Lease (another
// lineage) makes the next acquisition start above it, even when the Lease
// annotation reads as 0.
func TestObserveTermRaisesNextTerm(t *testing.T) {
	t.Parallel()

	s := &memStore{}
	s.store(Record{Holder: "", Term: 0})

	h := newHarness(t, s, 0)
	h.in.ViewTerm = 3
	h.e.ObserveTerm(1<<32 + 3)
	h.run(sec(1))

	if h.e.State() != Leader || h.e.Term() != 1<<32+4 {
		t.Fatalf("state %v term %d", h.e.State(), h.e.Term())
	}
}

// TestObserveTermIgnoresAbsurd: terms at or above MaxSaneTerm, from a
// message or a corrupt annotation, are ignored so terms cannot overflow.
func TestObserveTermIgnoresAbsurd(t *testing.T) {
	t.Parallel()

	s := &memStore{}
	s.store(Record{Holder: "", Term: ^uint64(0)})

	h := newHarness(t, s, 0)
	h.in.ViewTerm = 5
	h.e.ObserveTerm(MaxSaneTerm)
	h.e.ObserveTerm(^uint64(0))
	h.run(sec(1))

	if h.e.State() != Leader || h.e.Term() != 6 {
		t.Fatalf("state %v term %d", h.e.State(), h.e.Term())
	}

	if h.e.MaxTerm() != 6 {
		t.Fatalf("max term %d", h.e.MaxTerm())
	}
}

// TestReleaseCarriesObservedTerm: a leader that steps down because it saw
// a higher term releases the Lease with that term, so whoever takes it
// next starts above both lineages.
func TestReleaseCarriesObservedTerm(t *testing.T) {
	t.Parallel()

	s := &memStore{}
	h := newHarness(t, s, 0)
	h.run(sec(1))

	if h.e.State() != Leader {
		t.Fatal("not leader")
	}

	h.e.ObserveTerm(40)

	for _, op := range h.e.Release(h.in) {
		h.e.OnResult(h.in, s.apply(op))
	}

	if s.rec.Holder != "" || s.rec.Term != 40 {
		t.Fatalf("record %+v", s.rec)
	}

	h2 := newHarness(t, s, 1)
	h2.run(sec(1))

	if h2.e.State() != Leader || h2.e.Term() != 41 {
		t.Fatalf("next leader %v term %d", h2.e.State(), h2.e.Term())
	}
}

// TestLotteryNFloor: a member whose view is far smaller than the cluster
// still uses ExpectedClusterSize for N, so a small lineage cannot make
// every member read every round.
func TestLotteryNFloor(t *testing.T) {
	t.Parallel()

	reads := 0

	for i := range 50 {
		s := &memStore{}
		s.store(Record{Holder: "other", Incarnation: 1, Term: 9, RenewTime: epoch.Add(time.Hour), LeaseDuration: time.Hour})

		e := New(Config{ExpectedClusterSize: 1000}, self(i), newRNG(uint64(i)))
		in := Input{Wall: epoch, HaveView: true, ClusterSize: 5, ViewTerm: 9}

		for r := range 200 {
			in.Now = sec(60 + float64(r))
			for _, op := range e.Tick(in) {
				reads++

				e.OnResult(in, s.apply(op))
			}
		}
	}

	// 50 members * 200 rounds * 20/1000 = 200 expected; with N=5 every
	// member would read every round (10000).
	if reads < 100 || reads > 400 {
		t.Fatalf("%d lottery reads", reads)
	}
}
