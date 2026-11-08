// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cachemesh

import (
	"strconv"
	"sync"
	"sync/atomic"
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

	return benchMeshAt(b, 12345)
}

// benchMeshAt is benchMesh for the member at slot self.
func benchMeshAt(b *testing.B, self view.Slot) *Mesh {
	b.Helper()
	benchOnce.Do(func() { benchView = viewtest.Build(b, 100_000, viewtest.Options{}) })

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
	// Each goroutine reports on its own neighbor, as when traffic spreads
	// over many peers; ObserveParallel above is the worst case of every
	// goroutine reporting on one peer.
	b.Run("ObserveParallelSpread", func(b *testing.B) {
		var peers []PeerRef

		seen := map[uint32]bool{}

		for _, k := range keys {
			if d := m.Route(ks, k); !d.Next.IsZero() && !seen[d.Next.Slot] {
				seen[d.Next.Slot] = true
				peers = append(peers, d.Next)
			}
		}

		for _, p := range peers {
			m.Observe(p, Success, time.Millisecond)

			if _, _, ewma := m.health.PeerState(p.Slot); ewma == 0 {
				b.Fatalf("peer %d is not tracked", p.Slot)
			}
		}

		var next atomic.Uint32

		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			p := peers[int(next.Add(1))%len(peers)]
			for pb.Next() {
				m.Observe(p, Success, time.Millisecond)
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

// BenchmarkRouteWide routes keys spread over the whole keyspace, so the
// partition table, member records, and cell lists are read cold: 1M
// distinct keys from one member (the table and records exceed the per-core
// caches), and 64k keys from 16 different members in turn (each member
// relays through a different set of neighbor cells).
func BenchmarkRouteWide(b *testing.B) {
	ks := MustKeyspace("blobs", 0)

	keys := func(n int) [][]byte {
		buf := make([]byte, 0, n*12)
		out := make([][]byte, n)

		for i := range out {
			start := len(buf)
			buf = strconv.AppendInt(append(buf, "k:"...), int64(i)*7919, 10)
			out[i] = buf[start:len(buf):len(buf)]
		}

		return out
	}

	b.Run("Keys1M", func(b *testing.B) {
		m := benchMesh(b)
		ks1m := keys(1 << 20)

		b.ReportAllocs()
		b.ResetTimer()

		i := 0
		for b.Loop() {
			_ = m.Route(ks, ks1m[i&(1<<20-1)])
			i++
		}
	})
	b.Run("Selves16", func(b *testing.B) {
		ms := make([]*Mesh, 16)
		for i := range ms {
			ms[i] = benchMeshAt(b, view.Slot(i*6151+17))
		}

		k64 := keys(1 << 16)

		b.ReportAllocs()
		b.ResetTimer()

		i := 0
		for b.Loop() {
			_ = ms[i&15].Route(ks, k64[(i>>4)&(1<<16-1)])
			i++
		}
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
