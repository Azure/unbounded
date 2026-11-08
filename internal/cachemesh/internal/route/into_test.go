// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package route

import (
	"errors"
	"math/rand/v2"
	"strconv"
	"testing"

	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
	"github.com/Azure/unbounded/internal/cachemesh/internal/topology"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view/viewtest"
)

// TestInPlaceVariantsMatch checks that RouteTo, RouteHashTo, and ForwardTo
// produce exactly what Route, RouteHash, and Forward return, with and
// without filtered peers.
func TestInPlaceVariantsMatch(t *testing.T) {
	v := viewtest.Build(t, 400, viewtest.Options{PartitionBits: 12})

	for _, filtered := range []bool{false, true} {
		tr := newTracker(0)
		if filtered {
			eject(t, tr, v, 3, 50, 77)
			degrade(t, tr, v, 120)
		}

		st := NewState(v, 7, newEnv(tr, 1))

		for i := range 2000 {
			key := []byte("k" + strconv.Itoa(i))

			var d Decision

			st.RouteTo(&d, 9, key)

			if want := st.Route(9, key); d != want {
				t.Fatalf("RouteTo %q: %+v, want %+v", key, d, want)
			}

			var dh Decision

			h := hashing.KeyHash(9, key)
			st.RouteHashTo(&dh, h)

			if want := st.RouteHash(h); dh != want || dh != d {
				t.Fatalf("RouteHashTo %q: %+v, want %+v", key, dh, want)
			}

			in := d.Header
			in.Sender = 11

			var df Decision

			err := st.ForwardTo(&df, in)

			want, werr := st.Forward(in)
			if df != want || !errors.Is(err, werr) {
				t.Fatalf("ForwardTo %q: %+v %v, want %+v %v", key, df, err, want, werr)
			}
		}

		var d Decision

		if err := st.ForwardTo(&d, Header{}); err == nil || d != (Decision{}) {
			t.Fatalf("bad header: %v %+v", err, d)
		}
	}
}

// TestDiffMaskMatchesShape checks State.diffMask and the relay cell
// arithmetic against topology.Shape.
func TestDiffMaskMatchesShape(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))

	for _, sizes := range [][]uint32{{7}, {5, 3}, {4, 6, 5}, {2, 3, 2, 5, 3}} {
		sh, err := topology.NewShape(sizes...)
		if err != nil {
			t.Fatal(err)
		}

		for range 500 {
			self := rng.Uint32N(sh.Cells())
			st := &State{shape: &sh, selfCell: self}
			sh.Coords(&st.selfCoords, self)

			c := rng.Uint32N(sh.Cells())

			var oc [topology.MaxDims]uint32

			if got, want := st.diffMask(&oc, c), sh.DiffMask(self, c); got != want {
				t.Fatalf("%v: diffMask(%d, %d) = %b, want %b", sizes, self, c, got, want)
			}

			for dim := range int(sh.Dims) {
				got := self + (oc[dim]-st.selfCoords[dim])*sh.Stride(dim)
				if want := sh.Replace(self, dim, sh.Coord(c, dim)); got != want {
					t.Fatalf("%v: replace dim %d: %d, want %d", sizes, dim, got, want)
				}
			}
		}
	}
}
