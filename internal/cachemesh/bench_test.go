// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cachemesh

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view/viewtest"
)

var (
	benchOnce sync.Once
	benchView *view.View
)

// benchMesh returns a started (but not running) member at slot 12345 of a
// shared 100k-member view with 2^20 partitions.
func benchMesh(b *testing.B) *Mesh {
	b.Helper()
	benchOnce.Do(func() { benchView = viewtest.Build(b, 100_000, viewtest.Options{}) })

	const self = 12345

	mem, _ := benchView.Member(self)
	cfg := Config{
		ID: mem.ID, Incarnation: mem.Incarnation, Addr: mem.Addr,
		Transport: nopTransport{}, Seeds: []string{"seed:1"},
	}
	SetInitial(&cfg, benchView)

	m, err := New(cfg)
	if err != nil {
		b.Fatal(err)
	}

	m.mu.Lock()
	m.n.Start(m.mono(), time.Now())
	m.afterLocked()
	m.mu.Unlock()

	if s, ok := m.Self(); !ok || s.Slot != self || m.Members() != 100_000 {
		b.Fatalf("not a member: %+v %v", s, ok)
	}

	return m
}

func benchKeys() [][]byte {
	keys := make([][]byte, 1024)
	for i := range keys {
		keys[i] = []byte("sha256:" + strconv.Itoa(i))
	}

	return keys
}

func BenchmarkMesh(b *testing.B) {
	m := benchMesh(b)
	ks := MustKeyspace("blobs", 0)
	meta := MustKeyspace("meta", MinTTL)
	keys := benchKeys()

	var relay Decision

	for _, k := range keys {
		if d := m.Route(ks, k); d.Kind == KindRelay {
			relay = d

			break
		}
	}

	// A header as received from a neighbor at the same version.
	in := relay.Header
	in.Sender = relay.Next.Slot

	st := m.Stamp(meta)

	b.Run("Route", func(b *testing.B) {
		b.ReportAllocs()

		i := 0
		for b.Loop() {
			_ = m.Route(ks, keys[i&1023])
			i++
		}
	})
	// Same key every time: the routing cost without table cache misses.
	b.Run("RouteRelayKey", func(b *testing.B) {
		b.ReportAllocs()

		var key []byte

		for _, k := range keys {
			if m.Route(ks, k).Kind == KindRelay {
				key = k

				break
			}
		}

		for b.Loop() {
			_ = m.Route(ks, key)
		}
	})
	b.Run("RouteHash", func(b *testing.B) {
		b.ReportAllocs()

		h := ks.Hash(keys[0])
		for b.Loop() {
			_ = m.RouteHash(ks, h)
		}
	})
	b.Run("Forward", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			_, _ = m.Forward(in)
		}
	})
	b.Run("Observe", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			m.Observe(relay.Next, Success, time.Millisecond)
		}
	})
	b.Run("Fresh", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			_ = m.Fresh(meta, st)
		}
	})
	b.Run("Stamp", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			_ = m.Stamp(meta)
		}
	})
	b.Run("TryHedge", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			_ = m.TryHedge(relay.Header)
		}
	})
	b.Run("RouteParallel", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				_ = m.Route(ks, keys[i&1023])
				i++
			}
		})
	})
	b.Run("ForwardParallel", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, _ = m.Forward(in)
			}
		})
	})
	b.Run("ObserveParallel", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				m.Observe(relay.Next, Success, time.Millisecond)
			}
		})
	})
	b.Run("FreshParallel", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_ = m.Fresh(meta, st)
			}
		})
	})
}

// TestHotPathAllocs checks the zero-allocation contract on a small view.
func TestHotPathAllocs(t *testing.T) {
	v := viewtest.Build(t, 64, viewtest.Options{PartitionBits: 12})
	mem, _ := v.Member(5)
	cfg := Config{ID: mem.ID, Incarnation: mem.Incarnation, Addr: mem.Addr, Transport: nopTransport{}, Seeds: []string{"s"}}
	SetInitial(&cfg, v)

	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	m.n.Start(m.mono(), time.Now())
	m.afterLocked()
	m.mu.Unlock()

	ks := MustKeyspace("meta", MinTTL)
	key := []byte("k")
	d := m.Route(ks, key)
	st := m.Stamp(ks)
	out := make([]Decision, 1)
	keys := [][]byte{key}
	h := hashing.KeyHash(ks.Seed(), key)

	if n := testing.AllocsPerRun(100, func() {
		_ = m.Route(ks, key)
		_ = m.RouteHash(ks, h)
		m.RouteMany(ks, keys, out)
		_, _ = m.Forward(d.Header)
		m.Observe(d.Next, Success, time.Millisecond)
		_ = m.TryHedge(d.Header)
		_ = m.Fresh(ks, m.Stamp(ks))
		_ = st
	}); n != 0 {
		t.Fatalf("allocs = %v", n)
	}
}
