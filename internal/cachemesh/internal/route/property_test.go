// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package route

import (
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view/viewtest"
)

func TestHeaderRoundTrip(t *testing.T) {
	h := Header{
		Format: HeaderFormat, Flags: FlagHedge | FlagBackup, Hops: 2, TTL: 6,
		Partition: 0xdeadbeef, Sender: 12345, Term: 7, Seq: 1<<40 + 3, Epoch: 1<<63 + 9,
	}

	b := h.Append([]byte("x"))
	if len(b) != 1+HeaderSize || b[0] != 'x' {
		t.Fatalf("len %d", len(b))
	}

	got, err := ParseHeader(b[1:])
	if err != nil || got != h {
		t.Fatalf("%+v %v", got, err)
	}

	var a [HeaderSize]byte

	h.Put(&a)

	if string(a[:]) != string(b[1:]) {
		t.Fatal("Put and Append differ")
	}

	// Big-endian partition at offset 4.
	if a[4] != 0xde || a[7] != 0xef {
		t.Fatalf("% x", a)
	}
}

func TestParseHeaderMalformed(t *testing.T) {
	good := Header{Format: HeaderFormat, TTL: 4}.Append(nil)

	for name, mut := range map[string]func([]byte) []byte{
		"short":  func(b []byte) []byte { return b[:HeaderSize-1] },
		"empty":  func([]byte) []byte { return nil },
		"format": func(b []byte) []byte { b[0] = 0; return b },
		"flags":  func(b []byte) []byte { b[1] = 0x40; return b },
		"hops":   func(b []byte) []byte { b[2] = 5; return b },
	} {
		b := mut(append([]byte(nil), good...))
		if _, err := ParseHeader(b); !errors.Is(err, ErrBadHeader) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func FuzzParseHeader(f *testing.F) {
	f.Add(Header{Format: HeaderFormat, TTL: 4, Partition: 3}.Append(nil))
	f.Add([]byte{1, 2, 3})
	f.Fuzz(func(t *testing.T, b []byte) {
		h, err := ParseHeader(b)
		if err != nil {
			return
		}

		if h.Hops > h.TTL || h.Format != HeaderFormat {
			t.Fatalf("accepted %+v", h)
		}

		if got, err := ParseHeader(h.Append(nil)); err != nil || got != h {
			t.Fatalf("round trip %+v %v", got, err)
		}

		if string(h.Append(nil)) != string(b[:HeaderSize]) {
			t.Fatal("not canonical")
		}
	})
}

// propertyWalk walks every origin in origins to sampled partitions and
// returns the hop count histogram.
func propertyWalk(t *testing.T, f *fixture, origins []view.Slot, parts int, rng *rand.Rand, maxHops int) []int {
	t.Helper()

	hist := make([]int, 16)
	bits := f.v.PartitionBits()

	for _, o := range origins {
		for range parts {
			p := rng.Uint32() >> (32 - bits)
			path, d := walk(t, f, o, p)

			if d.Kind != KindSelf {
				t.Fatalf("ended with %v", d.Kind)
			}

			last := o
			if len(path) > 0 {
				last = path[len(path)-1]
			}

			owner := f.v.Table().Owner(p)
			if last != owner && d.Header.Flags&FlagBackup == 0 {
				t.Fatalf("ended at %d, owner %d", last, owner)
			}

			if len(path) > maxHops {
				t.Fatalf("%d hops > %d: %v", len(path), maxHops, path)
			}

			hist[len(path)]++
		}
	}

	return hist
}

func testProperty(t *testing.T, n, originStep int) {
	t.Helper()

	v := viewtest.Build(t, n, viewtest.Options{Seed: uint64(n)})
	d := int(v.Shape().Dims)
	rng := rand.New(rand.NewPCG(uint64(n), 7))

	var origins []view.Slot
	for o := 0; o < n; o += originStep {
		origins = append(origins, view.Slot(o))
	}

	f := newFixture(v, newEnv(nil, 1))
	hist := propertyWalk(t, f, origins, 4, rng, d)
	t.Logf("n=%d D=%d healthy hops: %v", n, d, hist[:d+2])

	// Random unusable members (10%, at most 2000 to keep tracking cheap):
	// loop-free and within TTL.
	tr := newTracker(0)
	bad := make([]view.Slot, 0, min(n/10, 2000))

	for _, s := range rng.Perm(n)[:cap(bad)] {
		bad = append(bad, view.Slot(s))
	}

	eject(t, tr, v, bad...)

	f = newFixture(v, newEnv(tr, 1))
	hist = propertyWalk(t, f, origins, 4, rng, d+2)
	t.Logf("n=%d D=%d with %d unusable hops: %v", n, d, len(bad), hist[:d+3])
}

func TestPropertyHops10k(t *testing.T) {
	testProperty(t, 10_000, 1)
}

func TestPropertyHops100k(t *testing.T) {
	if testing.Short() {
		t.Skip("large")
	}

	testProperty(t, 100_000, 10)
}
