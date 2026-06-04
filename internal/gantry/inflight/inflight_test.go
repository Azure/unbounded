// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package inflight

import (
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestStart_FirstStartGetsOwnership(t *testing.T) {
	m := New(DefaultStalls(), nil)
	d := digest.MustParse("sha256:" + zeros(64))
	h, e, alreadyPulling := m.Start(d, ifaces.KindBlob, 0)
	if alreadyPulling {
		t.Fatal("alreadyPulling should be false on first Start")
	}
	if h == nil {
		t.Fatal("Handle nil")
	}
	if e.StartedAt.IsZero() {
		t.Error("StartedAt not set")
	}
	if m.Len() != 1 {
		t.Errorf("Len = %d, want 1", m.Len())
	}
	h.Done()
	if m.Len() != 0 {
		t.Errorf("Len after Done = %d, want 0", m.Len())
	}
}

func TestStart_SecondStartReportsAlreadyPulling(t *testing.T) {
	m := New(DefaultStalls(), nil)
	d := digest.MustParse("sha256:" + zeros(64))
	h1, _, _ := m.Start(d, ifaces.KindBlob, 0)
	h2, e2, alreadyPulling := m.Start(d, ifaces.KindBlob, 0)
	if !alreadyPulling {
		t.Error("alreadyPulling = false on second Start")
	}
	if e2.StartedAt.IsZero() {
		t.Error("second Start did not surface existing StartedAt")
	}
	// Releasing h2 (the noop handle) must NOT clear the entry.
	h2.Done()
	if m.Len() != 1 {
		t.Errorf("Len after noop-Done = %d, want 1", m.Len())
	}
	// Releasing h1 (the owning handle) clears it.
	h1.Done()
	if m.Len() != 0 {
		t.Errorf("Len after owner Done = %d, want 0", m.Len())
	}
}

func TestStart_RaceParallelClaimsExactlyOneWinner(t *testing.T) {
	m := New(DefaultStalls(), nil)
	d := digest.MustParse("sha256:" + zeros(64))

	var winners, losers int
	var winnerMu sync.Mutex

	const goroutines = 64
	// Hold every goroutine at the start line so they all race Start
	// against each other before any of them releases the entry.
	start := make(chan struct{})
	handles := make(chan *Handle, goroutines)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			h, _, already := m.Start(d, ifaces.KindBlob, 0)
			handles <- h
			winnerMu.Lock()
			if already {
				losers++
			} else {
				winners++
			}
			winnerMu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	close(handles)
	for h := range handles {
		h.Done()
	}
	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1", winners)
	}
	if winners+losers != goroutines {
		t.Errorf("total = %d, want %d", winners+losers, goroutines)
	}
}

func TestLookupForIntent(t *testing.T) {
	m := New(DefaultStalls(), nil)
	d := digest.MustParse("sha256:" + zeros(64))
	if _, ok := m.LookupForIntent(d); ok {
		t.Fatal("LookupForIntent should be false for unknown digest")
	}
	h, _, _ := m.Start(d, ifaces.KindManifest, 0)
	defer h.Done()
	e, ok := m.LookupForIntent(d)
	if !ok {
		t.Fatal("LookupForIntent should be true after Start")
	}
	if e.ExpectedClass != ifaces.KindManifest {
		t.Errorf("ExpectedClass = %v; want KindManifest", e.ExpectedClass)
	}
}

func TestIsStale_ManifestConfig(t *testing.T) {
	fixed := time.Now()
	clock := fixed
	m := New(DefaultStalls(), func() time.Time { return clock })

	startedAt := fixed.Add(-6 * time.Second) // older than 5s threshold
	if !m.IsStale(ifaces.KindManifest, 0, startedAt) {
		t.Error("6s-old manifest pull should be stale (threshold 5s)")
	}
	startedAt = fixed.Add(-3 * time.Second) // newer
	if m.IsStale(ifaces.KindManifest, 0, startedAt) {
		t.Error("3s-old manifest pull should NOT be stale")
	}
}

func TestIsStale_LayerSizeAware(t *testing.T) {
	fixed := time.Now()
	clock := fixed
	m := New(DefaultStalls(), func() time.Time { return clock })

	// 100 MB layer at 50 MB/s = 2s expected; × 3 = 6s stall threshold,
	// but floor is 10s. So threshold = max(10s, 2s) × 3 = 30s.
	startedAt := fixed.Add(-25 * time.Second)
	if m.IsStale(ifaces.KindBlob, 100*1024*1024, startedAt) {
		t.Error("25s-old 100MB layer should NOT be stale (threshold 30s)")
	}
	startedAt = fixed.Add(-31 * time.Second)
	if !m.IsStale(ifaces.KindBlob, 100*1024*1024, startedAt) {
		t.Error("31s-old 100MB layer SHOULD be stale (threshold 30s)")
	}

	// 5 GB layer at 50 MB/s = 100s expected; × 3 = 300s threshold.
	startedAt = fixed.Add(-200 * time.Second)
	if m.IsStale(ifaces.KindBlob, 5*1024*1024*1024, startedAt) {
		t.Error("200s-old 5GB layer should NOT be stale (threshold 300s)")
	}
}

func TestIsStale_ZeroTimeNotStale(t *testing.T) {
	m := New(DefaultStalls(), nil)
	if m.IsStale(ifaces.KindBlob, 0, time.Time{}) {
		t.Error("zero startedAt must not be reported as stale")
	}
}

func TestResolveStall_UnknownSize(t *testing.T) {
	s := DefaultStalls()
	got := s.ResolveStall(ifaces.KindBlob, 0)
	want := s.LayerFloor * time.Duration(s.LayerMultiplier)
	if got != want {
		t.Errorf("unknown-size stall = %v; want %v", got, want)
	}
}

func zeros(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}
