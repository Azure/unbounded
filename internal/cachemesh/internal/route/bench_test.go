// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package route

import (
	"sync"
	"testing"

	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view/viewtest"
)

var (
	benchOnce sync.Once
	benchView *view.View
)

// big returns a shared 100k-member view with 2^20 partitions.
func big(b *testing.B) *view.View {
	b.Helper()
	benchOnce.Do(func() { benchView = viewtest.Build(b, 100_000, viewtest.Options{}) })

	return benchView
}

func BenchmarkRoute(b *testing.B) {
	v := big(b)
	tr := newTracker(0)
	env := newEnv(tr, 1)
	self := view.Slot(12345)
	st := NewState(v, self, env)

	// Pick a partition per kind; the backup case ejects an adjacent owner.
	parts := map[Kind]uint32{}
	for _, k := range []Kind{KindSelf, KindOwner, KindRelay} {
		parts[k] = findKind(b, st, k)
	}

	for p := range uint32(1) << 20 {
		o := v.Table().Owner(p)
		if o != self && v.Cell(o) != v.Cell(self) && diffDims(v, self, o) == 1 && o != v.Table().Owner(parts[KindOwner]) {
			eject(b, tr, v, o)

			parts[KindBackup] = p

			break
		}
	}

	for _, k := range []Kind{KindSelf, KindOwner, KindRelay, KindBackup} {
		p := parts[k]
		if got := st.RoutePartition(p).Kind; got != k {
			b.Fatalf("%v: got %v", k, got)
		}

		// Find a key that hashes to the partition's kind, to include
		// hashing in the measurement.
		var key [8]byte

		seed := uint64(7)

		for i := uint64(0); ; i++ {
			key = [8]byte{byte(i), byte(i >> 8), byte(i >> 16), byte(i >> 24), byte(i >> 32)}
			if st.Route(seed, key[:]).Kind == k {
				break
			}
		}

		b.Run("Route/"+k.String(), func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				_ = st.Route(seed, key[:])
			}
		})

		d := st.RoutePartition(p)
		if d.Next.IsZero() {
			continue
		}

		next := NewState(v, d.Next.Slot, env)
		h := d.Header

		b.Run("Forward/"+k.String(), func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				_, _ = next.Forward(h)
			}
		})
	}
}

func BenchmarkFresh(b *testing.B) {
	v := big(b)
	st := NewState(v, 1, newEnv(nil, 1))
	seed := hashing.Seed("ks")
	s := st.Stamp(seed, 0)

	b.ReportAllocs()

	for b.Loop() {
		if !st.Fresh(seed, s) {
			b.Fatal("stale")
		}
	}
}

func BenchmarkNewState(b *testing.B) {
	v := big(b)
	env := newEnv(nil, 1)

	b.ReportAllocs()

	for b.Loop() {
		_ = NewState(v, 777, env)
	}
}

func BenchmarkRouteParallel(b *testing.B) {
	v := big(b)
	st := NewState(v, 12345, newEnv(nil, 1))

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var k [8]byte

		i := uint64(0)
		for pb.Next() {
			i++
			k[0], k[1], k[2] = byte(i), byte(i>>8), byte(i>>16)
			_ = st.Route(3, k[:])
		}
	})
}
