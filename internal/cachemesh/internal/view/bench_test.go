// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view_test

import (
	"sync"
	"testing"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view/viewtest"
)

var (
	largeOnce sync.Once
	large     *view.View
)

// largeView returns a shared 100k-member, 2^20-partition view.
func largeView(b *testing.B) *view.View {
	b.Helper()
	largeOnce.Do(func() { large = viewtest.Build(b, 100_000, viewtest.Options{}) })

	return large
}

var sink uint64

func BenchmarkAccessors(b *testing.B) {
	v := largeView(b)
	n := v.SlotCap()

	b.Run("Member", func(b *testing.B) {
		for i := range b.N {
			m, _ := v.Member(uint32(i) % n)
			sink += m.Key
		}
	})
	b.Run("Key", func(b *testing.B) {
		for i := range b.N {
			sink += v.Key(uint32(i) % n)
		}
	})
	b.Run("Cell", func(b *testing.B) {
		for i := range b.N {
			sink += uint64(v.Cell(uint32(i) % n))
		}
	})
	b.Run("CellMembers", func(b *testing.B) {
		c := v.Shape().Cells()
		for i := range b.N {
			sink += uint64(len(v.CellMembers(uint32(i) % c)))
		}
	})
	b.Run("Load", func(b *testing.B) {
		for i := range b.N {
			sink += uint64(v.Load(uint32(i) % n))
		}
	})
	b.Run("Subjects", func(b *testing.B) {
		var dst []uint32
		for i := range b.N {
			dst = view.Subjects(v, uint32(i)%n, dst[:0])
		}
	})
	b.Run("Observers", func(b *testing.B) {
		var dst []uint32
		for i := range b.N {
			dst = view.Observers(v, uint32(i)%n, dst[:0])
		}
	})
	b.Run("Neighbors", func(b *testing.B) {
		var dst []uint32
		for i := range b.N {
			dst = view.Neighbors(v, uint32(i)%n, dst[:0])
		}
	})
}

// BenchmarkApply applies a delta with 5 admissions, 5 removals (with their
// partitions reassigned), an epoch op, and 4096 partition moves.
// The Members case has no optional moves (only the ~50 partitions of the
// removed members), and Moves4096 has only the moves, which touch nearly
// every 1024-entry table chunk.
func BenchmarkApply(b *testing.B) {
	v := largeView(b)

	for _, c := range []struct {
		name  string
		churn viewtest.Churn
	}{
		{"Members", viewtest.Churn{Admit: 5, Remove: 5, Epoch: true}},
		{"Moves4096", viewtest.Churn{Moves: 4096}},
		{"Full", viewtest.Churn{Admit: 5, Remove: 5, Moves: 4096, Epoch: true}},
	} {
		d := viewtest.Delta(b, v, c.churn)

		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()

			for range b.N {
				if _, err := view.Apply(v, d); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkValidate(b *testing.B) {
	v := largeView(b)

	for range b.N {
		if err := view.Validate(v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSnapshot(b *testing.B) {
	v := largeView(b)
	enc := view.EncodeSnapshot(v)

	b.Run("Encode", func(b *testing.B) {
		buf := make([]byte, 0, len(enc))
		for range b.N {
			buf = view.AppendSnapshot(buf[:0], v)
		}

		b.ReportMetric(float64(len(enc)), "bytes")
	})
	b.Run("Decode", func(b *testing.B) {
		for range b.N {
			if _, err := view.DecodeSnapshot(enc); err != nil {
				b.Fatal(err)
			}
		}

		b.ReportMetric(float64(len(enc)), "bytes")
	})
}

func BenchmarkBuild(b *testing.B) {
	for range b.N {
		viewtest.Build(b, 100_000, viewtest.Options{})
	}
}
