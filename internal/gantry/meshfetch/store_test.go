// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func key(i int64) chunkKey { return chunkKey{digest: "sha256:x", index: i} }

func completeFill(t *testing.T, s *store, k chunkKey, data []byte) *fill {
	t.Helper()

	f, created, err := s.acquire(context.Background(), k, false)
	if err != nil || !created {
		t.Fatalf("acquire %v: created=%v err=%v", k, created, err)
	}

	if err := f.setMeta(int64(len(data)), "", len(data)); err != nil {
		t.Fatal(err)
	}

	if err := f.readFrom(bytes.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}

	f.finish(nil)
	f.release() // filler

	return f
}

func TestStoreBudgetEvictsLRUAndPinsFills(t *testing.T) {
	const cs = 1024

	s := newStore(cs, 3*cs, 8, nil)

	for i := range int64(3) {
		completeFill(t, s, key(i), bytes.Repeat([]byte{byte(i)}, cs)).release()
	}

	// Touch chunk 0 so chunk 1 is the least recently used.
	s.get(key(0)).release()

	completeFill(t, s, key(3), make([]byte, cs)).release()

	if f := s.get(key(1)); f != nil {
		t.Fatal("chunk 1 should have been evicted")
	}

	if f := s.get(key(0)); f == nil {
		t.Fatal("recently used chunk 0 evicted")
	} else {
		f.release()
	}

	// Three fills in progress pin the whole budget: complete chunks are
	// evicted to make room, and a fourth fill is refused.
	var pinned []*fill

	for i := int64(10); i < 13; i++ {
		f, created, err := s.acquire(context.Background(), key(i), false)
		if err != nil || !created {
			t.Fatalf("acquire %d: %v", i, err)
		}

		pinned = append(pinned, f)
	}

	if _, _, err := s.acquire(context.Background(), key(20), false); !errors.Is(err, errBusy) {
		t.Fatalf("want errBusy, got %v", err)
	}

	// A waiting acquire proceeds once a pinned fill completes.
	done := make(chan error, 1)

	go func() {
		f, _, err := s.acquire(context.Background(), key(21), true)
		if err == nil {
			f.release()
		}

		done <- err
	}()

	time.Sleep(20 * time.Millisecond)

	f := pinned[0]
	if err := f.setMeta(cs, "", cs); err != nil {
		t.Fatal(err)
	}

	_ = f.readFrom(bytes.NewReader(make([]byte, cs)), nil)
	f.finish(nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiting acquire: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting acquire never proceeded")
	}

	if st := s.stats(); st.Bytes > 3*cs {
		t.Fatalf("budget exceeded: %+v", st)
	}
}

func TestStoreFailedFillIsDroppedAndReadersSeeError(t *testing.T) {
	s := newStore(1024, 4096, 4, nil)

	f, _, err := s.acquire(context.Background(), key(1), false)
	if err != nil {
		t.Fatal(err)
	}

	reader := s.get(key(1))

	if err := f.setMeta(100, "", 100); err != nil {
		t.Fatal(err)
	}

	_ = f.readFrom(strings.NewReader(strings.Repeat("a", 40)), nil)

	boom := errors.New("boom")
	f.finish(boom)

	var buf bytes.Buffer

	n, err := reader.streamTo(context.Background(), &buf, 0)
	if !errors.Is(err, boom) || n != 40 {
		t.Fatalf("streamTo = %d, %v; want 40 bytes then boom", n, err)
	}

	reader.release()

	if s.get(key(1)) != nil {
		t.Fatal("failed fill still cached")
	}

	if st := s.stats(); st.Bytes != 0 || st.Inflight != 0 {
		t.Fatalf("stats after failure: %+v", st)
	}
}

func TestStoreRecyclesChunkBuffers(t *testing.T) {
	const cs = 1024

	s := newStore(cs, 2*cs, 4, nil)

	completeFill(t, s, key(1), make([]byte, cs)).release()
	completeFill(t, s, key(2), make([]byte, cs)).release()
	// Evicts chunk 1; its buffer goes back to the pool.
	completeFill(t, s, key(3), make([]byte, cs)).release()

	if st := s.stats(); st.PooledBuffers != 0 {
		// The new fill reused the evicted buffer.
		t.Fatalf("pooled buffers = %d, want 0 after reuse", st.PooledBuffers)
	}

	s.evictDigest("sha256:x")

	if st := s.stats(); st.PooledBuffers != 2 || st.Chunks != 0 {
		t.Fatalf("after evictDigest: %+v", st)
	}
}

func TestStreamToCutThrough(t *testing.T) {
	s := newStore(1024, 4096, 4, nil)

	f, _, _ := s.acquire(context.Background(), key(1), false)
	reader := s.get(key(1))

	var (
		buf  bytes.Buffer
		done = make(chan error, 1)
	)

	go func() {
		_, err := reader.streamTo(context.Background(), &buf, 10)
		done <- err
	}()

	if err := f.setMeta(1000, "", 30); err != nil {
		t.Fatal(err)
	}

	_ = f.readFrom(strings.NewReader("0123456789abcdefghijklmnopqrst"), nil)

	if err := <-done; err != nil {
		t.Fatalf("streamTo: %v", err)
	}

	if buf.String() != "abcdefghijklmnopqrst" {
		t.Fatalf("got %q", buf.String())
	}

	f.finish(nil)
	f.release()
	reader.release()
}

func TestObtainRefusesLoop(t *testing.T) {
	c := newCluster(t, clusterOpts{n: 1})
	n := c.nodes[0].node

	f, _, err := n.store.acquire(context.Background(), key(5), false)
	if err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	f.upstream = 7
	f.mu.Unlock()

	if _, err := n.obtain(context.Background(), chunkReq{key: key(5)}, nil, "peer", 7); !errors.Is(err, errLoop) {
		t.Fatalf("want errLoop, got %v", err)
	}

	g, err := n.obtain(context.Background(), chunkReq{key: key(5)}, nil, "peer", 8)
	if err != nil {
		t.Fatalf("other sender: %v", err)
	}

	g.release()
	f.finish(errors.New("done"))
	f.release()
	f.release()
}

// ownedFill completes a fill for k that this member owns.
func ownedFill(t *testing.T, s *store, k chunkKey, data []byte) {
	t.Helper()

	f, created, err := s.acquire(context.Background(), k, false)
	if err != nil || !created {
		t.Fatalf("acquire %v: created=%v err=%v", k, created, err)
	}

	s.markOwned(f)

	if err := f.setMeta(int64(len(data)), "", len(data)); err != nil {
		t.Fatal(err)
	}

	if err := f.readFrom(bytes.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}

	f.finish(nil)
	f.release() // filler
	f.release() // caller
}

func cached(s *store, k chunkKey) bool {
	f := s.get(k)
	if f == nil {
		return false
	}

	f.release()

	return true
}

func TestStoreEvictsOwnedChunksLast(t *testing.T) {
	const cs = 1024

	s := newStore(cs, 4*cs, 8, nil)

	// Owned chunk 0 is the oldest; transit chunks 1-3 are newer.
	ownedFill(t, s, key(0), make([]byte, cs))

	for i := int64(1); i < 4; i++ {
		completeFill(t, s, key(i), make([]byte, cs)).release()
	}

	// Two more transit chunks evict the two oldest transit chunks, not
	// the older owned one.
	completeFill(t, s, key(4), make([]byte, cs)).release()
	completeFill(t, s, key(5), make([]byte, cs)).release()

	if !cached(s, key(0)) {
		t.Fatal("owned chunk evicted before transit chunks")
	}

	if cached(s, key(1)) || cached(s, key(2)) {
		t.Fatal("oldest transit chunks should have been evicted")
	}

	if s.ownedUsed != cs {
		t.Fatalf("ownedUsed = %d, want %d", s.ownedUsed, cs)
	}
}

func TestStoreCapsOwnedChunksAtHalfTheBudget(t *testing.T) {
	const cs = 1024

	s := newStore(cs, 4*cs, 8, nil)

	// Three owned chunks hold more than half of the budget.
	for i := range int64(3) {
		ownedFill(t, s, key(i), make([]byte, cs))
	}

	completeFill(t, s, key(10), make([]byte, cs)).release()

	// The next chunk evicts the oldest owned chunk, since owned chunks are
	// over their share, and keeps the transit chunk.
	completeFill(t, s, key(11), make([]byte, cs)).release()

	if cached(s, key(0)) {
		t.Fatal("owned chunks over half the budget must yield first")
	}

	if !cached(s, key(10)) || !cached(s, key(1)) {
		t.Fatal("unexpected eviction")
	}

	if s.ownedUsed != 2*cs {
		t.Fatalf("ownedUsed = %d, want %d", s.ownedUsed, 2*cs)
	}

	// With only owned chunks left they are still evicted LRU.
	s2 := newStore(cs, 2*cs, 8, nil)
	ownedFill(t, s2, key(0), make([]byte, cs))
	ownedFill(t, s2, key(1), make([]byte, cs))
	ownedFill(t, s2, key(2), make([]byte, cs))

	if cached(s2, key(0)) || !cached(s2, key(2)) {
		t.Fatal("owned-only store must evict its oldest chunk")
	}

	// Evicting a whole digest clears owned accounting.
	s2.evictDigest("sha256:x")

	if s2.ownedUsed != 0 || s2.used != 0 {
		t.Fatalf("after evictDigest: ownedUsed=%d used=%d", s2.ownedUsed, s2.used)
	}
}
