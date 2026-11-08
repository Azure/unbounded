// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package route

import (
	"math/bits"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/epoch"
	"github.com/Azure/unbounded/internal/cachemesh/internal/health"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// fakeCell groups every peer a test ejects into one health cell together
// with a never-ejected dummy, so the tracker's last-usable-in-cell rule does
// not get in the way.
const (
	fakeCell  = 1 << 30
	dummySlot = 1 << 30
)

// newTracker returns a tracker that can eject any number of peers and
// never panics unless panicAt is set.
func newTracker(panicAt int) *health.Tracker {
	if panicAt <= 0 {
		panicAt = 100
	}

	tr := health.NewTracker(health.Config{MaxEjectionPercent: 100, PanicThreshold: panicAt}, rand.New(rand.NewPCG(1, 2)))
	tr.Track(dummySlot, 1, fakeCell)

	return tr
}

func eject(t testing.TB, tr *health.Tracker, v *view.View, slots ...view.Slot) {
	t.Helper()

	for _, s := range slots {
		tr.Track(s, v.Key(s), fakeCell)
	}

	for _, s := range slots {
		for range 5 {
			tr.Observe(s, health.Failure, time.Millisecond)
		}

		if tr.Usable(s) != health.Unusable {
			t.Fatalf("slot %d not ejected", s)
		}
	}
}

func degrade(t testing.TB, tr *health.Tracker, v *view.View, s view.Slot) {
	t.Helper()
	tr.Track(s, v.Key(s), fakeCell)
	tr.Observe(s, health.Overload, 0)

	if tr.Usable(s) != health.Degraded {
		t.Fatalf("slot %d not degraded", s)
	}
}

type fixture struct {
	v   *view.View
	env *Env
	st  []*State
}

func newFixture(v *view.View, env *Env) *fixture {
	f := &fixture{v: v, env: env, st: make([]*State, v.SlotCap())}
	for i := range f.st {
		f.st[i] = NewState(v, view.Slot(i), env)
	}

	return f
}

func newEnv(tr *health.Tracker, cur uint64) *Env {
	e := &epoch.Tracker{}
	e.Anchor(cur, 15*time.Second, 0)

	return NewEnv(Config{Health: tr, Epoch: e})
}

// findKind returns a partition for which st routes with kind k.
func findKind(t testing.TB, st *State, k Kind) uint32 {
	t.Helper()

	n := uint32(1) << st.PartitionBits()
	for p := range n {
		if st.RoutePartition(p).Kind == k {
			return p
		}
	}

	t.Fatalf("no partition with kind %v", k)

	return 0
}

func diffDims(v *view.View, a, b view.Slot) int {
	return bits.OnesCount8(v.Shape().DiffMask(v.Cell(a), v.Cell(b)))
}

// walk follows a request for partition p from origin hop by hop. It returns
// the slots visited after the origin and the final decision.
func walk(t testing.TB, f *fixture, origin view.Slot, p uint32) ([]view.Slot, Decision) {
	t.Helper()

	d := f.st[origin].RoutePartition(p)

	var path []view.Slot

	for !d.Next.IsZero() {
		next := d.Next.Slot
		for _, s := range path {
			if s == next {
				t.Fatalf("loop at %d for p=%d from %d: %v", next, p, origin, path)
			}
		}

		if next == origin {
			t.Fatalf("routed back to origin %d", origin)
		}

		path = append(path, next)

		var err error

		d, err = f.st[next].Forward(d.Header)
		if err != nil {
			t.Fatalf("forward at %d (p=%d from %d, path %v): %v", next, p, origin, path, err)
		}
	}

	return path, d
}
