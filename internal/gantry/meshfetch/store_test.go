// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testPage  = 4096
	testChunk = 4 * testPage
	testBlob  = "sha256:x"
)

func key(i int64) chunkKey { return chunkKey{digest: testBlob, index: i} }

// newTestStore returns a store with 4KiB pages, 16KiB chunks, a two-page
// window, and an empty in-memory content store (testContent).
func newTestStore(t testing.TB, mut func(*storeConfig)) *store {
	t.Helper()

	cfg := storeConfig{
		chunkSize:    testChunk,
		pageSize:     testPage,
		window:       2,
		maxFills:     8,
		content:      newMemContent(),
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

// testContent returns the store's in-memory content store.
func testContent(s *store) *memContent { return s.cfg.content.(*memContent) }

// putChunk stores a blob in mc whose chunk idx (of chunkSize bytes) is
// data and that ends with it, as if containerd had pulled it.
func putChunk(mc *memContent, chunkSize, idx int64, data []byte) {
	b := make([]byte, idx*chunkSize+int64(len(data)))
	copy(b[idx*chunkSize:], data)
	mc.put(testBlob, b)
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

// behind fills chunk k with data, which the content store already holds,
// while n readers sit at offset 0 without reading. It returns the readers:
// the fill has dropped every page but its last window from memory, so the
// readers read the rest from the content store.
func behind(t testing.TB, s *store, k chunkKey, data []byte, n int) []*reader {
	t.Helper()

	putChunk(testContent(s), s.cfg.chunkSize, k.index, data)

	r, created, err := s.acquire(context.Background(), k, 0, false, false)
	if err != nil || !created {
		t.Fatalf("acquire %v: created=%v err=%v", k, created, err)
	}

	f := r.f
	rs := []*reader{r}

	for len(rs) < n {
		rs = append(rs, s.get(k, 0))
	}

	if err := f.setMeta(int64(k.index)*s.cfg.chunkSize+int64(len(data)), "", len(data)); err != nil {
		t.Fatal(err)
	}

	if err := f.readFrom(context.Background(), bytes.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}

	f.finish(nil)
	f.release()

	f.mu.Lock()
	ws := f.winStart
	f.mu.Unlock()

	if want := len(data)/s.cfg.pageSize - s.cfg.window; ws != want {
		t.Fatalf("window starts at page %d, want %d", ws, want)
	}

	return rs
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

// TestStoreLocalFillHonorsCap checks that fills read from the local
// content store count against the fill cap: a peer cannot start an
// unbounded number of them.
func TestStoreLocalFillHonorsCap(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) { c.maxFills = 1 })

	f, r, err := s.newLocalFill(key(1), 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := s.newLocalFill(key(2), 0); !errors.Is(err, errBusy) {
		t.Fatalf("second local fill: want errBusy, got %v", err)
	}

	if _, _, err := s.acquire(context.Background(), key(3), 0, false, true); !errors.Is(err, errBusy) {
		t.Fatalf("acquire beside a local fill: want errBusy, got %v", err)
	}

	f.finish(errors.New("done"))
	f.release()
	r.close()

	f2, r2, err := s.newLocalFill(key(2), 0)
	if err != nil {
		t.Fatalf("local fill after the first ended: %v", err)
	}

	f2.finish(errors.New("done"))
	f2.release()
	r2.close()
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

	if st := s.stats(); st.Inflight != 0 || st.PageBytes != 0 {
		t.Fatalf("stats after failure: %+v", st)
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

// Without local content, the slowest reader paces the fill, and a reader
// that stops reading is dropped after the stall timeout.
func TestStreamThroughPacingAndKick(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) {
		c.content = nil
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
	s := newTestStore(t, func(c *storeConfig) { c.content = nil })
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

// A fill that a peer reads borrows pages from the in-flight budget instead
// of waiting for a lagging reader, and returns them when it is done.
func TestFillBorrowsInflightPagesForPeers(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) {
		c.content = nil
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

	// The whole chunk is still in memory, so it lingers for later peers
	// until its pages are needed.
	if st := s.stats(); st.MemoryChunks != 1 {
		t.Fatalf("chunk does not linger: %+v", st)
	}

	s.evictDigest("sha256:x")

	if st := s.stats(); st.ExtraPageBytes != 0 || st.PageBytes != 0 || st.MemoryChunks != 0 {
		t.Fatalf("pages not returned: %+v", st)
	}
}

// A stream fill that peers read keeps its whole chunk in borrowed pages;
// a later peer reads it from memory, and a fill that needs the budget
// reclaims it.
func TestPeerReadChunkLingersInMemory(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) {
		c.content = nil
		c.extraBudget = 2 * testPage
	})

	fillWithPeer := func(k chunkKey, data []byte) {
		t.Helper()

		r, _, err := s.acquire(context.Background(), k, 0, false, true)
		if err != nil {
			t.Fatal(err)
		}

		f := r.f
		_ = f.setMeta(int64(len(data)), "", len(data))

		go func() {
			_ = f.readFrom(context.Background(), bytes.NewReader(data), nil)
			f.finish(nil)
			f.release()
		}()

		if got := readAll(t, r); !bytes.Equal(got, data) {
			t.Fatal("data mismatch")
		}

		r.close()
	}

	a := pattern(15, testChunk)
	fillWithPeer(key(1), a)

	late := s.get(key(1), 0)
	if late == nil {
		t.Fatal("peer-read chunk does not linger")
	}

	if got := readAll(t, late); !bytes.Equal(got, a) {
		t.Fatal("lingering data mismatch")
	}

	late.close()

	// The next peer-read fill needs the budget: chunk 1 is reclaimed.
	fillWithPeer(key(2), pattern(16, testChunk))

	if cached(s, key(1)) || !cached(s, key(2)) {
		t.Fatal("lingering chunk not reclaimed for the next fill")
	}

	if st := s.stats(); st.ExtraPageBytes > 2*testPage || st.MemoryChunks != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

// An unused lingering chunk is dropped after the linger TTL, returning
// its memory.
func TestLingeringChunkExpires(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) {
		c.content = nil
		c.extraBudget = 4 * testPage
		c.lingerTTL = time.Hour
	})
	data := pattern(17, testChunk)

	r, _, _ := s.acquire(context.Background(), key(1), 0, false, true)
	f := r.f
	_ = f.setMeta(int64(len(data)), "", len(data))
	_ = f.readFrom(context.Background(), bytes.NewReader(data), nil)
	f.finish(nil)
	f.release()
	r.close()

	if st := s.stats(); st.MemoryChunks != 1 {
		t.Fatalf("chunk does not linger: %+v", st)
	}

	// Not yet expired.
	s.expire(time.Now().Add(30 * time.Minute))

	if !cached(s, key(1)) {
		t.Fatal("chunk expired early")
	}

	// cached touched it; expire well after the TTL.
	s.expire(time.Now().Add(2 * time.Hour))

	if st := s.stats(); st.MemoryChunks != 0 || st.PageBytes != 0 || st.ExtraPageBytes != 0 {
		t.Fatalf("after expiry: %+v", st)
	}
}

// Without a peer reader a lagging local reader paces the fill rather than
// borrowing memory.
func TestLocalOnlyFillDoesNotBorrow(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) {
		c.content = nil
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

// Concurrent readers behind the window share one content store read per
// page.
func TestLocalPageReadsCoalesce(t *testing.T) {
	const readers = 6

	s := newTestStore(t, func(c *storeConfig) { c.chunkSize = 8 * testPage })
	data := pattern(1, 8*testPage)
	rs := behind(t, s, key(1), data, readers)

	// Each reader writes one page per Write call; the barrier holds every
	// reader inside Write of the same page, so all of them hold the page
	// at once.
	b := newBarrier(readers)

	var wg sync.WaitGroup

	for _, r := range rs {
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

	local := int64(8 - s.cfg.window)

	st := s.stats()
	if st.LocalReads != local {
		t.Fatalf("local reads = %d, want one per page behind the window (%d)", st.LocalReads, local)
	}

	if st.PageReadsShared != (readers-1)*local {
		t.Fatalf("shared reads = %d", st.PageReadsShared)
	}

	// The pages stay on the recent ring until pushed out or evicted.
	if st.PageBytes != local*testPage {
		t.Fatalf("recent pages: %+v", st)
	}

	s.evictDigest(testBlob)

	if st := s.stats(); st.PageBytes != 0 {
		t.Fatalf("pages retained after evict: %+v", st)
	}
}

// Pages of one blob are shared across fills: they are keyed by digest and
// absolute page, not by fill. A reader that arrives after the others are
// done shares their read through the recent ring, and evicting the digest
// frees its pages.
func TestLocalPagesSharedAcrossFills(t *testing.T) {
	s := newTestStore(t, nil)
	data := pattern(2, testChunk)

	a := behind(t, s, key(1), data, 1)[0]
	b := behind(t, s, key(1), data, 1)[0]

	if a.f == b.f {
		t.Fatal("want two fills")
	}

	bar := newBarrier(2)

	var wg sync.WaitGroup

	for _, r := range []*reader{a, b} {
		wg.Go(func() {
			defer r.close()

			var buf bytes.Buffer

			if _, err := r.streamTo(context.Background(), barrierWriter{b: bar, w: &buf}); err != nil {
				t.Errorf("streamTo: %v", err)
			}

			if !bytes.Equal(buf.Bytes(), data) {
				t.Error("data mismatch")
			}
		})
	}

	wg.Wait()

	// Both fills read the same two pages behind their windows once.
	if st := s.stats(); st.LocalReads != 2 || st.PageReadsShared != 2 || st.PageBytes != 2*testPage {
		t.Fatalf("stats: %+v", st)
	}

	// A later reader shares the recent pages.
	c := behind(t, s, key(1), data, 1)[0]
	if got := readAll(t, c); !bytes.Equal(got, data) {
		t.Fatal("data mismatch")
	}

	c.close()

	if st := s.stats(); st.LocalReads != 2 {
		t.Fatalf("recent pages read again: %+v", st)
	}

	// Evicting the digest frees its recent pages.
	s.evictDigest(testBlob)

	if st := s.stats(); st.PageBytes != 0 {
		t.Fatalf("after evict: %+v", st)
	}
}

// The recent ring holds at most recentPages released pages.
func TestRecentPagesBounded(t *testing.T) {
	s := newTestStore(t, nil)
	mc := testContent(s)
	mc.put(testBlob, pattern(5, 2*recentPages*testPage))

	for i := range int64(2 * recentPages) {
		e, err := s.pages.get(mc, testBlob, i, testPage)
		if err != nil {
			t.Fatal(err)
		}

		s.pages.put(e)
	}

	if st := s.stats(); st.PageBytes != recentPages*testPage || st.LocalReads != 2*recentPages {
		t.Fatalf("stats: %+v", st)
	}

	// The newest pages are still shared; the oldest were freed.
	e, _ := s.pages.get(mc, testBlob, 2*recentPages-1, testPage)
	s.pages.put(e)

	e, _ = s.pages.get(mc, testBlob, 0, testPage)
	s.pages.put(e)

	if st := s.stats(); st.LocalReads != 2*recentPages+1 || st.PageBytes != recentPages*testPage {
		t.Fatalf("stats: %+v", st)
	}
}

// A reader that does not keep up does not hold back a fill whose bytes
// the content store holds; it reads the pages it missed from there, and
// the fill keeps no more than its window in memory.
func TestSlowReaderFallsBackToLocalContent(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) { c.chunkSize = 16 * testPage })
	data := pattern(3, 16*testPage)
	putChunk(testContent(s), s.cfg.chunkSize, 1, data)

	r, _, _ := s.acquire(context.Background(), key(1), 0, false, false)
	f := r.f

	_ = f.setMeta(s.cfg.chunkSize+int64(len(data)), "", len(data))

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

	if st := s.stats(); st.LocalReads != 14 {
		t.Fatalf("slow reader local reads: %+v", st)
	}
}

// A fill notices bytes containerd writes while it waits for a lagging
// reader: once the content store covers the reader, the fill moves on.
func TestFillWaitsForLocalContentToCatchUp(t *testing.T) {
	s := newTestStore(t, nil)
	mc := testContent(s)
	data := pattern(4, testChunk)

	r, _, _ := s.acquire(context.Background(), key(0), 0, false, false)
	f := r.f
	_ = f.setMeta(int64(len(data)), "", len(data))

	done := make(chan error, 1)

	go func() { done <- f.readFrom(context.Background(), bytes.NewReader(data), nil) }()

	waitFor(t, "fill waits for the reader", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()

		return f.fillerWaiting && f.n == 2*testPage
	})

	// containerd writes the first page: the reader at 0 is covered.
	w := mc.ingest(testBlob, int64(len(data)))
	_, _ = w.Write(data[:testPage])

	select {
	case err := <-done:
		t.Fatalf("fill finished early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	waitFor(t, "fill moves on", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()

		return f.n > 2*testPage
	})

	_, _ = w.Write(data[testPage:])

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	f.finish(nil)
	f.release()

	if got := readAll(t, r); !bytes.Equal(got, data) {
		t.Fatal("data mismatch")
	}

	r.close()
}

// A failed content store read loses the reader (it retries elsewhere) and
// stops trusting the content store for that fill.
func TestLocalReadErrorLosesReader(t *testing.T) {
	s := newTestStore(t, nil)
	rs := behind(t, s, key(1), pattern(8, testChunk), 1)

	testContent(s).fail.Store(true)

	r := rs[0]
	if _, err := r.streamTo(context.Background(), io.Discard); !errors.Is(err, errLost) {
		t.Fatalf("want errLost, got %v", err)
	}

	r.f.mu.Lock()
	end := r.f.localEnd
	r.f.mu.Unlock()

	if end != 0 {
		t.Fatalf("localEnd = %d after a read error", end)
	}

	r.close()

	if st := s.stats(); st.LocalErrors != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

// Content that disappears (containerd garbage collection) loses the reader.
func TestLocalContentGoneLosesReader(t *testing.T) {
	s := newTestStore(t, nil)
	rs := behind(t, s, key(1), pattern(9, testChunk), 1)

	testContent(s).drop(testBlob)

	if _, err := rs[0].streamTo(context.Background(), io.Discard); !errors.Is(err, errLost) {
		t.Fatalf("want errLost, got %v", err)
	}

	rs[0].close()
}

// The final chunk of a blob ends inside a page; readers behind the window
// read it from the content store at any offset.
func TestPartialFinalPageRoundTrip(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) { c.chunkSize = 8 * testPage })
	data := pattern(9, 4*testPage+100)

	putChunk(testContent(s), s.cfg.chunkSize, 2, data)

	for _, off := range []int{0, 2 * testPage, 2*testPage + 99, 4*testPage + 99} {
		r, _, _ := s.acquire(context.Background(), key(2), off, false, false)
		f := r.f
		_ = f.setMeta(2*s.cfg.chunkSize+int64(len(data)), "", len(data))
		_ = f.readFrom(context.Background(), bytes.NewReader(data), nil)
		f.finish(nil)
		f.release()

		if got := readAll(t, r); !bytes.Equal(got, data[off:]) {
			t.Fatalf("off %d: data mismatch", off)
		}

		r.close()
	}
}

// A fill that peers read does not keep its whole chunk in borrowed pages
// when the content store holds it: later peers read it from there.
func TestLocallyHeldChunkDoesNotLinger(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) { c.extraBudget = 8 * testPage })
	data := pattern(10, testChunk)
	putChunk(testContent(s), s.cfg.chunkSize, 1, data)

	r, _, _ := s.acquire(context.Background(), key(1), 0, false, true)
	f := r.f
	_ = f.setMeta(s.cfg.chunkSize+int64(len(data)), "", len(data))

	// The peer reader sits at 0, but the content store covers it.
	if err := f.readFrom(context.Background(), bytes.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}

	f.finish(nil)
	f.release()

	if st := s.stats(); st.ExtraPageBytes != 0 || st.MemoryChunks != 0 {
		t.Fatalf("locally held chunk kept in memory: %+v", st)
	}

	if got := readAll(t, r); !bytes.Equal(got, data) {
		t.Fatal("data mismatch")
	}

	r.close()
}

// The budget reclaims lingering chunks this member does not own first.
func TestEvictLingeringPrefersNotOwned(t *testing.T) {
	s := newTestStore(t, func(c *storeConfig) {
		c.content = nil
		c.extraBudget = 64 * testPage
	})

	linger := func(k chunkKey, owned bool) {
		t.Helper()

		r, _, _ := s.acquire(context.Background(), k, 0, false, true)
		f := r.f

		if owned {
			f.owned.Store(true)
		}

		data := pattern(byte(k.index), testChunk)
		_ = f.setMeta(int64(len(data)), "", len(data))
		_ = f.readFrom(context.Background(), bytes.NewReader(data), nil)
		f.finish(nil)
		f.release()
		r.close()
	}

	linger(key(1), true)
	linger(key(2), false)
	linger(key(3), true)

	if !s.evictLingering() {
		t.Fatal("nothing to evict")
	}

	if cached(s, key(2)) || !cached(s, key(1)) || !cached(s, key(3)) {
		t.Fatal("want the chunk not owned evicted first")
	}

	// With only owned chunks left, the least recently used goes.
	if !s.evictLingering() || cached(s, key(1)) || !cached(s, key(3)) {
		t.Fatal("want the least recently used owned chunk evicted")
	}
}
