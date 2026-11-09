// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testPage  = 4096
	testChunk = 4 * testPage
)

func key(i int64) chunkKey { return chunkKey{digest: "sha256:x", index: i} }

// newTestStore returns a store with 4KiB pages, 16KiB chunks, a two-page
// window, and a four-chunk disk budget in a temporary directory.
func newTestStore(t testing.TB, mut func(*storeConfig)) *store {
	t.Helper()

	cfg := storeConfig{
		chunkSize:    testChunk,
		pageSize:     testPage,
		window:       2,
		maxFills:     8,
		dir:          t.TempDir(),
		diskBudget:   4 * testChunk,
		hotBudget:    4 * testPage,
		stallTimeout: 5 * time.Second,
		logger:       slog.New(slog.DiscardHandler),
	}
	if mut != nil {
		mut(&cfg)
	}

	s := newStore(cfg)
	t.Cleanup(s.close)

	return s
}

func pattern(seed byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i*7)
	}

	return b
}

// waitFor polls cond for up to 5s.
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(time.Millisecond)
	}
}

// onDisk reports whether f is complete on disk with its window freed.
func onDisk(f *fill) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.done && f.durable == f.chunkLen && len(f.win) == 0
}

// completeFill fills k with data and waits until it is on disk (when it
// is written at all). It returns the fill; the caller holds no reference.
func completeFill(t testing.TB, s *store, k chunkKey, data []byte, owned bool) *fill {
	t.Helper()

	r, created, err := s.acquire(context.Background(), k, 0, false, false)
	if err != nil || !created {
		t.Fatalf("acquire %v: created=%v err=%v", k, created, err)
	}

	f := r.f
	if owned {
		s.markOwned(f)
	}

	r.close()

	if err := f.setMeta(int64(len(data)), "", len(data)); err != nil {
		t.Fatal(err)
	}

	if err := f.readFrom(context.Background(), bytes.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}

	f.finish(nil)

	if f.wantDisk.Load() {
		waitFor(t, "chunk on disk", func() bool { return onDisk(f) || !cached(s, k) })
	}

	f.release() // filler

	return f
}

func cached(s *store, k chunkKey) bool {
	r := s.get(k, 0)
	if r == nil {
		return false
	}

	r.close()

	return true
}

func readAll(t testing.TB, r *reader) []byte {
	t.Helper()

	var buf bytes.Buffer
	if _, err := r.streamTo(context.Background(), &buf); err != nil {
		t.Fatalf("streamTo: %v", err)
	}

	return buf.Bytes()
}

func TestStoreDiskBudgetEvictsLRU(t *testing.T) {
	var evicted int64

	s := newTestStore(t, func(c *storeConfig) {
		c.diskBudget = 3 * testChunk
		c.onEvict = func(n int64) { evicted += n }
	})

	for i := range int64(3) {
		completeFill(t, s, key(i), pattern(byte(i), testChunk), false)
	}

	// Touch chunk 0 so chunk 1 is the least recently used.
	if !cached(s, key(0)) {
		t.Fatal("chunk 0 not cached")
	}

	completeFill(t, s, key(3), pattern(3, testChunk), false)

	if cached(s, key(1)) {
		t.Fatal("chunk 1 should have been evicted")
	}

	if !cached(s, key(0)) || !cached(s, key(3)) {
		t.Fatal("recently used chunks evicted")
	}

	if evicted != testChunk {
		t.Fatalf("evicted %d bytes, want %d", evicted, testChunk)
	}

	// The evicted chunk's file is gone; three remain.
	entries, err := os.ReadDir(s.cfg.dir)
	if err != nil || len(entries) != 3 {
		t.Fatalf("chunk files: %d, %v; want 3", len(entries), err)
	}

	if st := s.stats(); st.DiskBytes > 3*testChunk || st.Chunks != 3 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestStoreFillsBeyondDiskBudgetStreamThrough(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) { c.diskBudget = 2 * testChunk })

	// Two fills in progress hold the whole disk budget. A third one is
	// not refused: it streams through memory.
	var rs []*reader

	for i := range int64(3) {
		r, created, err := s.acquire(context.Background(), key(i), 0, false, false)
		if err != nil || !created {
			t.Fatalf("acquire %d: %v", i, err)
		}

		rs = append(rs, r)
	}

	if !rs[0].f.wantDisk.Load() || !rs[1].f.wantDisk.Load() || rs[2].f.wantDisk.Load() {
		t.Fatal("only the first two fills should get disk")
	}

	data := pattern(9, testChunk)
	f := rs[2].f

	go func() {
		_ = f.setMeta(testChunk, "", testChunk)
		_ = f.readFrom(context.Background(), bytes.NewReader(data), nil)
		f.finish(nil)
		f.release()
	}()

	if got := readAll(t, rs[2]); !bytes.Equal(got, data) {
		t.Fatal("stream-through data mismatch")
	}

	rs[2].close()

	if cached(s, key(2)) {
		t.Fatal("a stream-through chunk must not stay cached")
	}

	if st := s.stats(); st.StreamFills != 1 || st.DiskBytes != 2*testChunk {
		t.Fatalf("stats: %+v", st)
	}

	for _, r := range rs[:2] {
		r.f.finish(errors.New("done"))
		r.f.release()
		r.close()
	}
}

func TestStoreFillCapRefusesAndWaits(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) { c.maxFills = 1 })

	r, _, err := s.acquire(context.Background(), key(1), 0, false, false)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.acquire(context.Background(), key(2), 0, false, true); !errors.Is(err, errBusy) {
		t.Fatalf("want errBusy, got %v", err)
	}

	done := make(chan error, 1)

	go func() {
		r2, _, err := s.acquire(context.Background(), key(2), 0, true, false)
		if err == nil {
			r2.f.finish(errors.New("done"))
			r2.f.release()
			r2.close()
		}

		done <- err
	}()

	time.Sleep(20 * time.Millisecond)
	r.f.finish(errors.New("done"))
	r.f.release()
	r.close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiting acquire: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting acquire never proceeded")
	}
}

func TestStoreFailedFillIsDroppedAndReadersSeeError(t *testing.T) {
	s := newTestStore(t, nil)

	r, _, err := s.acquire(context.Background(), key(1), 0, false, false)
	if err != nil {
		t.Fatal(err)
	}

	f := r.f

	if err := f.setMeta(100, "", 100); err != nil {
		t.Fatal(err)
	}

	_ = f.readFrom(context.Background(), strings.NewReader(strings.Repeat("a", 40)), nil)

	boom := errors.New("boom")
	f.finish(boom)
	f.release()

	var buf bytes.Buffer

	n, err := r.streamTo(context.Background(), &buf)
	if !errors.Is(err, boom) || n != 40 {
		t.Fatalf("streamTo = %d, %v; want 40 bytes then boom", n, err)
	}

	r.close()

	if cached(s, key(1)) {
		t.Fatal("failed fill still cached")
	}

	if st := s.stats(); st.DiskBytes != 0 || st.Inflight != 0 || st.PageBytes != 0 {
		t.Fatalf("stats after failure: %+v", st)
	}

	if entries, _ := os.ReadDir(s.cfg.dir); len(entries) != 0 {
		t.Fatalf("failed fill left %d files", len(entries))
	}
}

// A reader follows a fill as it grows: it gets bytes before the fill
// completes, from an offset inside the chunk.
func TestStreamToFollowsTheFill(t *testing.T) {
	s := newTestStore(t, nil)

	r, _, _ := s.acquire(context.Background(), key(1), 0, false, false)
	f := r.f
	follower := s.get(key(1), 10)

	pr, pw := io.Pipe()
	got := make(chan []byte, 1)

	go func() {
		var buf bytes.Buffer

		_, err := follower.streamTo(context.Background(), &buf)
		if err != nil {
			t.Errorf("follower: %v", err)
		}

		got <- buf.Bytes()
	}()

	go func() {
		_ = f.setMeta(1000, "", 30)
		_ = f.readFrom(context.Background(), pr, nil)
		f.finish(nil)
		f.release()
	}()

	_, _ = pw.Write([]byte("0123456789abcdefghij"))

	// The follower sees the first bytes while the fill is still open.
	waitFor(t, "cut-through bytes", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()

		return follower.pos == 20
	})

	_, _ = pw.Write([]byte("klmnopqrst"))

	if b := <-got; string(b) != "abcdefghijklmnopqrst" {
		t.Fatalf("got %q", b)
	}

	follower.close()
	r.close()
}

// Concurrent readers of a chunk on disk share one disk read per page.
func TestDiskPageReadsCoalesce(t *testing.T) {
	const readers = 6

	s := newTestStore(t, func(c *storeConfig) { c.hotBudget = 0 })
	data := pattern(1, testChunk)
	completeFill(t, s, key(1), data, false)

	base := s.stats().DiskReads

	// Each reader writes one page per Write call; the barrier holds every
	// reader inside Write of the same page, so all of them hold the page
	// at once.
	b := newBarrier(readers)

	var wg sync.WaitGroup

	for range readers {
		r := s.get(key(1), 0)
		if r == nil {
			t.Fatal("chunk not cached")
		}

		wg.Go(func() {
			defer r.close()

			var buf bytes.Buffer

			if _, err := r.streamTo(context.Background(), barrierWriter{b: b, w: &buf}); err != nil {
				t.Errorf("streamTo: %v", err)
			}

			if !bytes.Equal(buf.Bytes(), data) {
				t.Error("data mismatch")
			}
		})
	}

	wg.Wait()

	st := s.stats()
	if reads := st.DiskReads - base; reads != testChunk/testPage {
		t.Fatalf("disk reads = %d, want one per page (%d)", reads, testChunk/testPage)
	}

	if st.PageReadsShared != (readers-1)*testChunk/testPage {
		t.Fatalf("shared reads = %d", st.PageReadsShared)
	}

	if st.HotBytes != 0 || st.PageBytes != 0 {
		t.Fatalf("pages retained without a hot tier: %+v", st)
	}
}

func TestHotTierAdmitsOnlySharedPagesAndEvictsLRU(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) { c.hotBudget = 2 * testPage })
	data := pattern(2, testChunk)
	completeFill(t, s, key(1), data, false)

	pages := int64(testChunk / testPage)

	// One reader at a time: nothing is retained, each read goes to disk.
	for range 2 {
		r := s.get(key(1), 0)
		if got := readAll(t, r); !bytes.Equal(got, data) {
			t.Fatal("data mismatch")
		}

		r.close()
	}

	st := s.stats()
	if st.HotBytes != 0 || st.DiskReads != 2*pages {
		t.Fatalf("single readers: %+v", st)
	}

	// Two concurrent readers: shared pages are admitted, and the LRU keeps
	// the last two within the budget.
	b := newBarrier(2)

	var wg sync.WaitGroup

	for range 2 {
		r := s.get(key(1), 0)

		wg.Go(func() {
			defer r.close()

			_, _ = r.streamTo(context.Background(), barrierWriter{b: b, w: io.Discard})
		})
	}

	wg.Wait()

	st = s.stats()
	if st.HotBytes != 2*testPage || st.DiskReads != 3*pages {
		t.Fatalf("after shared read: %+v", st)
	}

	// A later reader of the last two pages hits the hot tier.
	r := s.get(key(1), 2*testPage)

	var buf bytes.Buffer
	if _, err := r.streamTo(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}

	r.close()

	if !bytes.Equal(buf.Bytes(), data[2*testPage:]) {
		t.Fatal("hot tier data mismatch")
	}

	if got := s.stats().DiskReads; got != 3*pages {
		t.Fatalf("hot pages read from disk again: %d reads", got)
	}

	// Evicting the chunk drops its hot pages.
	s.evictDigest("sha256:x")

	if st := s.stats(); st.HotBytes != 0 || st.PageBytes != 0 {
		t.Fatalf("after evict: %+v", st)
	}
}

// A reader that does not keep up does not hold back a fill that is
// written to disk; it reads the pages it missed from disk.
func TestSlowReaderFallsBackToDisk(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) { c.chunkSize = 16 * testPage })
	data := pattern(3, 16*testPage)

	r, _, _ := s.acquire(context.Background(), key(1), 0, false, false)
	f := r.f

	_ = f.setMeta(int64(len(data)), "", len(data))

	maxPages := 0
	tr := &trackingReader{r: bytes.NewReader(data), each: func() {
		if live, _ := s.pool.stats(); live > maxPages {
			maxPages = live
		}
	}}

	if err := f.readFrom(context.Background(), tr, nil); err != nil {
		t.Fatal(err)
	}

	f.finish(nil)
	f.release()

	if maxPages > s.cfg.window+1 {
		t.Fatalf("fill held %d pages, window is %d", maxPages, s.cfg.window)
	}

	if got := readAll(t, r); !bytes.Equal(got, data) {
		t.Fatal("slow reader data mismatch")
	}

	r.close()

	if st := s.stats(); st.DiskReads == 0 {
		t.Fatalf("slow reader did not read from disk: %+v", st)
	}
}

// Without disk, the slowest reader paces the fill, and a reader that
// stops reading is dropped after the stall timeout.
func TestStreamThroughPacingAndKick(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) {
		c.dir = ""
		c.stallTimeout = 100 * time.Millisecond
	})
	data := pattern(4, testChunk)

	r, _, _ := s.acquire(context.Background(), key(1), 0, false, false)
	f := r.f
	stuck := s.get(key(1), 0)

	filled := make(chan error, 1)

	go func() {
		_ = f.setMeta(int64(len(data)), "", len(data))
		filled <- f.readFrom(context.Background(), bytes.NewReader(data), nil)
	}()

	// The fill stops at the window while both readers sit at 0.
	waitFor(t, "window full", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()

		return f.n == 2*testPage && f.fillerWaiting
	})

	got := make(chan []byte, 1)

	go func() { got <- readAll(t, r) }()

	if err := <-filled; err != nil {
		t.Fatalf("fill: %v", err)
	}

	f.finish(nil)
	f.release()

	if b := <-got; !bytes.Equal(b, data) {
		t.Fatal("paced reader data mismatch")
	}

	r.close()

	if _, err := stuck.streamTo(context.Background(), io.Discard); !errors.Is(err, errLost) {
		t.Fatalf("stuck reader: want errLost, got %v", err)
	}

	stuck.close()

	if cached(s, key(1)) {
		t.Fatal("stream-through chunk cached")
	}
}

// A reader that needs bytes a stream-through fill dropped gets a new fill.
func TestAcquireReplacesFillMissingPrefix(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) { c.dir = "" })
	data := pattern(5, testChunk)

	r, _, _ := s.acquire(context.Background(), key(1), 0, false, false)
	f := r.f
	_ = f.setMeta(int64(len(data)), "", len(data))

	done := make(chan []byte, 1)

	go func() { done <- readAll(t, r) }()

	pr, pw := io.Pipe()

	go func() {
		_ = f.readFrom(context.Background(), pr, nil)
		f.finish(nil)
		f.release()
	}()

	_, _ = pw.Write(data[:3*testPage])

	waitFor(t, "window slides", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()

		return f.winStart > 0
	})

	if s.get(key(1), 0) != nil {
		t.Fatal("get must not join a fill missing the prefix")
	}

	// A late joiner at the start gets a new fill; one inside the window
	// joins the old one.
	if late := s.get(key(1), 3*testPage); late == nil {
		t.Fatal("reader inside the window should join")
	} else {
		late.close()
	}

	r2, created, err := s.acquire(context.Background(), key(1), 0, false, false)
	if err != nil || !created || r2.f == f {
		t.Fatalf("want a new fill: created=%v err=%v", created, err)
	}

	r2.f.finish(errors.New("done"))
	r2.f.release()
	r2.close()

	_, _ = pw.Write(data[3*testPage:])
	_ = pw.Close()

	if b := <-done; !bytes.Equal(b, data) {
		t.Fatal("original reader data mismatch")
	}

	r.close()
}

func TestDiskFullFallsBackToStreamThrough(t *testing.T) {
	s := newTestStore(t, nil)

	creates := 0
	s.faults = &diskFaults{create: func() error {
		creates++
		return errDiskFull
	}}

	data := pattern(6, testChunk)

	r, _, _ := s.acquire(context.Background(), key(1), 0, false, false)
	f := r.f

	go func() {
		_ = f.setMeta(int64(len(data)), "", len(data))
		_ = f.readFrom(context.Background(), bytes.NewReader(data), nil)
		f.finish(nil)
		f.release()
	}()

	if got := readAll(t, r); !bytes.Equal(got, data) {
		t.Fatal("data mismatch")
	}

	r.close()

	if cached(s, key(1)) {
		t.Fatal("chunk without a file cached")
	}

	// During the backoff new fills do not try the disk.
	r2, _, _ := s.acquire(context.Background(), key(2), 0, false, false)
	if r2.f.wantDisk.Load() || creates != 1 {
		t.Fatalf("disk retried during backoff: creates=%d", creates)
	}

	r2.f.finish(errors.New("done"))
	r2.f.release()
	r2.close()

	if st := s.stats(); st.DiskErrors != 1 || st.DiskBytes != 0 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestDiskWriteErrorFallsBackToStreamThrough(t *testing.T) {
	s := newTestStore(t, nil)
	s.faults = &diskFaults{write: func(idx int) error {
		if idx == 1 {
			return errors.New("EIO")
		}

		return nil
	}}

	data := pattern(7, testChunk)

	r, _, _ := s.acquire(context.Background(), key(1), 0, false, false)
	f := r.f

	go func() {
		_ = f.setMeta(int64(len(data)), "", len(data))
		_ = f.readFrom(context.Background(), bytes.NewReader(data), nil)
		f.finish(nil)
		f.release()
	}()

	if got := readAll(t, r); !bytes.Equal(got, data) {
		t.Fatal("data mismatch")
	}

	r.close()

	waitFor(t, "chunk dropped", func() bool { return !cached(s, key(1)) })

	if st := s.stats(); st.DiskErrors != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestDiskReadErrorDropsChunk(t *testing.T) {
	s := newTestStore(t, nil)
	data := pattern(8, testChunk)
	completeFill(t, s, key(1), data, false)

	s.faults = &diskFaults{read: func(int) error { return errors.New("EIO") }}

	r := s.get(key(1), 0)
	if _, err := r.streamTo(context.Background(), io.Discard); !errors.Is(err, errLost) {
		t.Fatalf("want errLost, got %v", err)
	}

	r.close()

	if cached(s, key(1)) {
		t.Fatal("unreadable chunk still cached")
	}
}

func TestStoreWipesChunkDirAtStartup(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{"c1", "stale"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}

	s := newTestStore(t, func(c *storeConfig) { c.dir = dir })

	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("chunk dir not wiped: %d entries", len(entries))
	}

	// New chunk files do not collide with anything.
	completeFill(t, s, key(1), pattern(1, testChunk), false)

	if !cached(s, key(1)) {
		t.Fatal("chunk not cached after wipe")
	}
}

func TestStoreUnusableDirDisablesDisk(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	s := newTestStore(t, func(c *storeConfig) { c.dir = filepath.Join(file, "sub") })
	if s.disk != nil {
		t.Fatal("disk enabled on an unusable dir")
	}

	r, _, _ := s.acquire(context.Background(), key(1), 0, false, false)
	if r.f.wantDisk.Load() {
		t.Fatal("fill wants disk without a disk tier")
	}

	r.f.finish(errors.New("done"))
	r.f.release()
	r.close()
}

// The final chunk of a blob ends inside a page; it round-trips through
// disk (including O_DIRECT padding).
func TestPartialFinalPageRoundTrip(t *testing.T) {
	for _, mode := range []PageCacheMode{PageCacheBypass, PageCacheDrop, PageCacheKeep} {
		t.Run(string(mode), func(t *testing.T) {
			s := newTestStore(t, func(c *storeConfig) { c.pageCache = mode })
			data := pattern(9, 2*testPage+100)
			completeFill(t, s, key(1), data, false)

			for _, off := range []int{0, 2 * testPage, 2*testPage + 99} {
				r := s.get(key(1), off)
				if r == nil {
					t.Fatal("chunk not cached")
				}

				if got := readAll(t, r); !bytes.Equal(got, data[off:]) {
					t.Fatalf("off %d: data mismatch", off)
				}

				r.close()
			}

			if st := s.stats(); st.DiskBytes != alignUp(int64(len(data))) {
				t.Fatalf("charged %d bytes for %d", st.DiskBytes, len(data))
			}
		})
	}
}

func TestPersistSharedWritesOnlySharedChunks(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) { c.persist = PersistShared })

	// Only this member consumes chunk 1: it is never written.
	completeFill(t, s, key(1), pattern(1, testChunk), false)

	if cached(s, key(1)) || s.stats().DiskWrites != 0 {
		t.Fatal("local-only chunk written to disk")
	}

	// Owned chunks are written.
	completeFill(t, s, key(2), pattern(2, testChunk), true)

	if !cached(s, key(2)) {
		t.Fatal("owned chunk not cached")
	}

	// A peer joining a local fill before its first page is gone makes it
	// a disk chunk.
	data := pattern(3, testChunk)

	r, _, _ := s.acquire(context.Background(), key(3), 0, false, false)
	f := r.f
	_ = f.setMeta(int64(len(data)), "", len(data))

	peer, created, err := s.acquire(context.Background(), key(3), 0, false, true)
	if err != nil || created {
		t.Fatalf("peer join: created=%v err=%v", created, err)
	}

	go func() {
		_ = f.readFrom(context.Background(), bytes.NewReader(data), nil)
		f.finish(nil)
		f.release()
	}()

	for _, rd := range []*reader{r, peer} {
		if got := readAll(t, rd); !bytes.Equal(got, data) {
			t.Fatal("data mismatch")
		}

		rd.close()
	}

	waitFor(t, "shared chunk on disk", func() bool { return onDisk(f) })

	if !cached(s, key(3)) {
		t.Fatal("shared chunk not cached")
	}
}

func TestObtainRefusesLoop(t *testing.T) {
	c := newCluster(t, clusterOpts{n: 1})
	n := c.nodes[0].node

	r, _, err := n.store.acquire(context.Background(), key(5), 0, false, false)
	if err != nil {
		t.Fatal(err)
	}

	f := r.f
	f.mu.Lock()
	f.upstream = 7
	f.mu.Unlock()

	if _, err := n.obtain(context.Background(), chunkReq{key: key(5)}, 0, nil, "peer", 7); !errors.Is(err, errLoop) {
		t.Fatalf("want errLoop, got %v", err)
	}

	g, err := n.obtain(context.Background(), chunkReq{key: key(5)}, 0, nil, "peer", 8)
	if err != nil {
		t.Fatalf("other sender: %v", err)
	}

	g.close()
	f.finish(errors.New("done"))
	f.release()
	r.close()
}

func TestStoreEvictsOwnedChunksLast(t *testing.T) {
	s := newTestStore(t, nil)

	// Owned chunk 0 is the oldest; transit chunks 1-3 are newer.
	completeFill(t, s, key(0), pattern(0, testChunk), true)

	for i := int64(1); i < 4; i++ {
		completeFill(t, s, key(i), pattern(byte(i), testChunk), false)
	}

	// Two more transit chunks evict the two oldest transit chunks, not
	// the older owned one.
	completeFill(t, s, key(4), pattern(4, testChunk), false)
	completeFill(t, s, key(5), pattern(5, testChunk), false)

	if !cached(s, key(0)) {
		t.Fatal("owned chunk evicted before transit chunks")
	}

	if cached(s, key(1)) || cached(s, key(2)) {
		t.Fatal("oldest transit chunks should have been evicted")
	}

	if s.ownedUsed != testChunk {
		t.Fatalf("ownedUsed = %d, want %d", s.ownedUsed, testChunk)
	}
}

func TestStoreCapsOwnedChunksAtHalfTheBudget(t *testing.T) {
	s := newTestStore(t, nil)

	// Three owned chunks hold more than half of the budget.
	for i := range int64(3) {
		completeFill(t, s, key(i), pattern(byte(i), testChunk), true)
	}

	completeFill(t, s, key(10), pattern(10, testChunk), false)

	// The next chunk evicts the oldest owned chunk, since owned chunks are
	// over their share, and keeps the transit chunk.
	completeFill(t, s, key(11), pattern(11, testChunk), false)

	if cached(s, key(0)) {
		t.Fatal("owned chunks over half the budget must yield first")
	}

	if !cached(s, key(10)) || !cached(s, key(1)) {
		t.Fatal("unexpected eviction")
	}

	if s.ownedUsed != 2*testChunk {
		t.Fatalf("ownedUsed = %d, want %d", s.ownedUsed, 2*testChunk)
	}

	// With only owned chunks left they are still evicted LRU.
	s2 := newTestStore(t, func(c *storeConfig) { c.diskBudget = 2 * testChunk })
	for i := range int64(3) {
		completeFill(t, s2, key(i), pattern(byte(i), testChunk), true)
	}

	if cached(s2, key(0)) || !cached(s2, key(2)) {
		t.Fatal("owned-only store must evict its oldest chunk")
	}

	// Evicting a whole digest clears owned accounting.
	s2.evictDigest("sha256:x")

	if s2.ownedUsed != 0 || s2.used != 0 {
		t.Fatalf("after evictDigest: ownedUsed=%d used=%d", s2.ownedUsed, s2.used)
	}
}

// barrier releases a group of n waiters at a time.
type barrier struct {
	mu    sync.Mutex
	n     int
	count int
	gen   chan struct{}
}

func newBarrier(n int) *barrier { return &barrier{n: n, gen: make(chan struct{})} }

func (b *barrier) wait() {
	b.mu.Lock()
	b.count++

	ch := b.gen
	if b.count == b.n {
		b.count = 0
		close(b.gen)
		b.gen = make(chan struct{})
	}
	b.mu.Unlock()

	<-ch
}

type barrierWriter struct {
	b *barrier
	w io.Writer
}

func (bw barrierWriter) Write(p []byte) (int, error) {
	bw.b.wait()
	return bw.w.Write(p)
}

type trackingReader struct {
	r    io.Reader
	each func()
}

func (t *trackingReader) Read(p []byte) (int, error) {
	t.each()
	return t.r.Read(p)
}

func TestPersistOwnedWritesOnlyOwnedChunks(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) { c.persist = PersistOwned })

	completeFill(t, s, key(1), pattern(1, testChunk), true)

	if !cached(s, key(1)) {
		t.Fatal("owned chunk not cached")
	}

	// A peer-initiated fill of a chunk this member does not own streams.
	r, _, err := s.acquire(context.Background(), key(2), 0, false, true)
	if err != nil {
		t.Fatal(err)
	}

	if r.f.wantDisk.Load() {
		t.Fatal("peer fill of a chunk not owned wants disk")
	}

	r.f.finish(errors.New("done"))
	r.f.release()
	r.close()
}

// A fill that a peer reads borrows pages from the in-flight budget instead
// of waiting for a lagging reader, and returns them when it is done.
func TestFillBorrowsInflightPagesForPeers(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) {
		c.dir = ""
		c.extraBudget = 2 * testPage
	})
	data := pattern(11, testChunk)

	r, _, _ := s.acquire(context.Background(), key(1), 0, false, false)
	f := r.f
	peer := s.get(key(1), 0)

	_ = f.setMeta(int64(len(data)), "", len(data))

	// Neither reader reads: the four-page chunk fits the two-page window
	// plus two borrowed pages, so the fill never waits.
	if err := f.readFrom(context.Background(), bytes.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}

	if st := s.stats(); st.ExtraPageBytes != 2*testPage {
		t.Fatalf("borrowed %d bytes, want %d", st.ExtraPageBytes, 2*testPage)
	}

	f.finish(nil)
	f.release()

	for _, rd := range []*reader{r, peer} {
		if got := readAll(t, rd); !bytes.Equal(got, data) {
			t.Fatal("data mismatch")
		}

		rd.close()
	}

	if st := s.stats(); st.ExtraPageBytes != 0 || st.PageBytes != 0 {
		t.Fatalf("pages not returned: %+v", st)
	}
}

// Without a peer reader a lagging local reader paces the fill rather than
// borrowing memory.
func TestLocalOnlyFillDoesNotBorrow(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) {
		c.dir = ""
		c.extraBudget = 8 * testPage
	})
	data := pattern(12, testChunk)

	r, _, _ := s.acquire(context.Background(), key(1), 0, false, false)
	f := r.f
	_ = f.setMeta(int64(len(data)), "", len(data))

	done := make(chan error, 1)

	go func() { done <- f.readFrom(context.Background(), bytes.NewReader(data), nil) }()

	waitFor(t, "fill paced", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()

		return f.fillerWaiting && f.n == 2*testPage
	})

	if st := s.stats(); st.ExtraPageBytes != 0 {
		t.Fatalf("local-only fill borrowed: %+v", st)
	}

	if got := readAll(t, r); !bytes.Equal(got, data) {
		t.Fatal("data mismatch")
	}

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	f.finish(nil)
	f.release()
	r.close()
}

// When the in-flight budget is exhausted a fill with a lagging peer spills
// to disk from its oldest window page; the reader then reads from disk.
func TestFillSpillsWhenBudgetExhausted(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) {
		c.chunkSize = 16 * testPage
		c.persist = PersistOwned
		c.extraBudget = testPage
	})
	data := pattern(13, 16*testPage)

	r, _, _ := s.acquire(context.Background(), key(1), 3*testPage, false, true)
	f := r.f

	if f.wantDisk.Load() {
		t.Fatal("PersistOwned fill wants disk before spilling")
	}

	_ = f.setMeta(int64(len(data)), "", len(data))

	if err := f.readFrom(context.Background(), bytes.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}

	f.finish(nil)
	f.release()

	waitFor(t, "spilled pages on disk", func() bool { return onDisk(f) })

	st := s.stats()
	if st.Spills != 1 || st.DiskWrites != 13 {
		t.Fatalf("stats: %+v", st)
	}

	if got := readAll(t, r); !bytes.Equal(got, data[3*testPage:]) {
		t.Fatal("spilled data mismatch")
	}

	r.close()

	// A spilled chunk lacks its first pages: it is not cached.
	if cached(s, key(1)) {
		t.Fatal("partial chunk cached")
	}

	if st := s.stats(); st.ExtraPageBytes != 0 || st.DiskBytes != 0 {
		t.Fatalf("after close: %+v", st)
	}
}
