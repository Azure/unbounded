// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     Config
		wantErr string
		check   func(t *testing.T, c Config)
	}{
		{name: "defaults", check: func(t *testing.T, c Config) {
			if c.ChunkSize != DefaultChunkSize || c.CacheBytes != DefaultCacheBytes ||
				c.ReadaheadMax != DefaultReadaheadMax || c.MaxAttempts != DefaultMaxAttempts || c.Logger == nil {
				t.Fatalf("unexpected defaults %+v", c)
			}
		}},
		{name: "chunk too small", cfg: Config{ChunkSize: 32 << 10}, wantErr: "power of two"},
		{name: "chunk too large", cfg: Config{ChunkSize: 32 << 20}, wantErr: "power of two"},
		{name: "chunk not power of two", cfg: Config{ChunkSize: 3 << 20}, wantErr: "power of two"},
		{name: "chunk max", cfg: Config{ChunkSize: MaxChunkSize, ReadaheadMax: -1}},
		{name: "negative cache", cfg: Config{CacheBytes: -1}, wantErr: "negative cache"},
		{name: "cache too small", cfg: Config{CacheBytes: 64 << 20}, wantErr: "at least"},
		{name: "cache just enough", cfg: Config{CacheBytes: 80 << 20}},
		{name: "negative readahead disables", cfg: Config{ReadaheadMax: -5, CacheBytes: 16 << 20}, check: func(t *testing.T, c Config) {
			if c.ReadaheadMax != 0 {
				t.Fatalf("ReadaheadMax=%d", c.ReadaheadMax)
			}
		}},
		{name: "readahead rounds down", cfg: Config{ReadaheadMax: 9 << 20}, check: func(t *testing.T, c Config) {
			if c.ReadaheadMax != 8<<20 {
				t.Fatalf("ReadaheadMax=%d", c.ReadaheadMax)
			}
		}},
		{name: "readahead rounds up to one chunk", cfg: Config{ReadaheadMax: 1}, check: func(t *testing.T, c Config) {
			if c.ReadaheadMax != DefaultChunkSize {
				t.Fatalf("ReadaheadMax=%d", c.ReadaheadMax)
			}
		}},
		{name: "negative ttl", cfg: Config{WarmTTL: -1}, wantErr: "warm TTL"},
		{name: "negative attempts", cfg: Config{MaxAttempts: -1}, wantErr: "attempts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := tc.cfg.normalize()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want %q", err, tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if tc.check != nil {
				tc.check(t, c)
			}
		})
	}

	if _, err := Open(t.Context(), newFake(10), Config{ChunkSize: 1}); err == nil {
		t.Fatal("Open accepted an invalid config")
	}
}

func TestOpen(t *testing.T) {
	t.Run("stat pins current", func(t *testing.T) {
		f := newFake(1000)
		b := mustOpen(t, f, testConfig())

		if b.Size() != 1000 || b.ETag() != `"v1"` || f.stats.Load() != 1 || f.warms.Load() != 0 {
			t.Fatalf("size=%d etag=%s stats=%d warms=%d", b.Size(), b.ETag(), f.stats.Load(), f.warms.Load())
		}
	})

	t.Run("zero size", func(t *testing.T) {
		if _, err := Open(t.Context(), newFake(0), testConfig()); err == nil {
			t.Fatal("want error")
		}
	})

	t.Run("no etag", func(t *testing.T) {
		f := newFake(10)
		f.etag = ""

		if _, err := Open(t.Context(), f, testConfig()); err == nil {
			t.Fatal("want error")
		}
	})

	t.Run("stat error", func(t *testing.T) {
		f := newFake(10)
		f.statErr = racersdk.ErrNotFound

		if _, err := Open(t.Context(), f, testConfig()); !errors.Is(err, racersdk.ErrNotFound) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("pinned current etag skips probe", func(t *testing.T) {
		f := newFake(10)
		cfg := testConfig()
		cfg.ETag = `"v1"`
		mustOpen(t, f, cfg)

		if n := len(f.openCalls()); n != 0 {
			t.Fatalf("opens=%d", n)
		}
	})

	t.Run("pinned older etag probes for size", func(t *testing.T) {
		f := newFake(5000)
		f.onOpen = func(_ context.Context, _ int, c openCall) (io.ReadCloser, error) {
			if c.etag == `"old"` {
				md := racersdk.Metadata{Size: 4000, ETag: `"old"`}
				return &patternReader{off: c.off, end: c.off + c.length, md: md}, nil
			}

			return nil, nil
		}
		cfg := testConfig()
		cfg.ETag = `"old"`
		b := mustOpen(t, f, cfg)

		if b.Size() != 4000 || b.ETag() != `"old"` {
			t.Fatalf("size=%d etag=%s", b.Size(), b.ETag())
		}

		if calls := f.openCalls(); len(calls) != 1 || calls[0].length != 1 {
			t.Fatalf("calls=%v", calls)
		}

		checkRead(t, b, 3990, 20)
	})

	t.Run("pinned missing etag fails fast", func(t *testing.T) {
		cfg := testConfig()
		cfg.ETag = `"gone"`

		if _, err := Open(t.Context(), newFake(10), cfg); !errors.Is(err, racersdk.ErrVersionMismatch) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("pinned probe without metadata", func(t *testing.T) {
		f := newFake(10)
		f.onOpen = func(context.Context, int, openCall) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("x")), nil
		}
		cfg := testConfig()
		cfg.ETag = `"other"`

		if _, err := Open(t.Context(), f, cfg); err == nil || !strings.Contains(err.Error(), "metadata") {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("warm pins", func(t *testing.T) {
		f := newFake(10)
		cfg := testConfig()
		cfg.WarmTTL = time.Hour
		cfg.ETag = `"v1"`
		mustOpen(t, f, cfg)

		if f.warms.Load() != 1 || f.stats.Load() != 0 {
			t.Fatalf("warms=%d stats=%d", f.warms.Load(), f.stats.Load())
		}
	})

	t.Run("warm mismatch", func(t *testing.T) {
		cfg := testConfig()
		cfg.WarmTTL = time.Hour
		cfg.ETag = `"v2"`

		if _, err := Open(t.Context(), newFake(10), cfg); !errors.Is(err, racersdk.ErrVersionMismatch) {
			t.Fatalf("err=%v", err)
		}
	})

	t.Run("warm error", func(t *testing.T) {
		f := newFake(10)
		f.warmErr = racersdk.ErrForbidden
		cfg := testConfig()
		cfg.WarmTTL = time.Hour

		if _, err := Open(t.Context(), f, cfg); !errors.Is(err, racersdk.ErrForbidden) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestReadShapes(t *testing.T) {
	const cs = MinChunkSize

	page := int64(racersdk.PageSize)

	for _, tc := range []struct {
		name  string
		size  int64
		chunk int64
		reads [][2]int64
	}{
		{name: "tiny", size: 100, chunk: cs, reads: [][2]int64{{0, 512}, {0, 100}, {99, 1}, {50, 462}, {100, 412}, {512, 512}}},
		{name: "under one chunk", size: cs - 7, chunk: cs, reads: [][2]int64{{0, cs}, {cs - 512, 512}, {1000, 3000}}},
		{name: "odd size", size: 5*cs + 123, chunk: cs, reads: [][2]int64{
			{0, 4096},
			{4096, 4096},
			{cs - 100, 200},
			{cs - 4096, 3 * cs},
			{5 * cs, 512},
			{5*cs + 100, 4096},
			{5*cs + 123, 389},
			{6 * cs, 4096},
			{1 << 40, 512},
			{0, 6 * cs},
			{7, 13},
		}},
		{name: "page boundaries", size: 2*page + 4097, chunk: 4 << 20, reads: [][2]int64{
			{page - 4096, 8192},
			{page - 1, 2},
			{page - 512, 1 << 20},
			{2*page - 100, 4296},
			{2 * page, 8192},
			{page - 3<<20, 8 << 20},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(tc.size)
			cfg := testConfig()
			cfg.ChunkSize = tc.chunk
			cfg.CacheBytes = 16 * tc.chunk
			b := mustOpen(t, f, cfg)

			for _, r := range tc.reads {
				checkRead(t, b, r[0], r[1])
			}

			// Second pass is served from cache.
			for _, r := range tc.reads {
				checkRead(t, b, r[0], r[1])
			}

			for _, c := range f.openCalls() {
				if c.etag != `"v1"` || c.off%tc.chunk != 0 || c.off+c.length > tc.size {
					t.Fatalf("bad open %+v", c)
				}
			}
		})
	}
}

func TestReadArgs(t *testing.T) {
	b := mustOpen(t, newFake(1000), testConfig())

	if err := b.ReadAt(t.Context(), make([]byte, 1), -1); err == nil {
		t.Fatal("negative offset accepted")
	}

	if err := b.ReadAt(t.Context(), make([]byte, 2), 1<<63-1); err == nil {
		t.Fatal("overflow accepted")
	}

	if err := b.ReadAt(t.Context(), nil, 10); err != nil {
		t.Fatalf("empty read: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := b.ReadAt(ctx, make([]byte, 1), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestCoalescingOneChunk(t *testing.T) {
	g := newGate()
	f := newFake(10 * MinChunkSize)
	f.onOpen = func(ctx context.Context, _ int, c openCall) (io.ReadCloser, error) {
		<-g.ch
		return nil, nil
	}
	b := mustOpen(t, f, testConfig())

	const readers = 32

	var wg sync.WaitGroup

	errs := make(chan error, readers)

	for i := range readers {
		wg.Go(func() {
			off := int64(i * 1000)
			p := make([]byte, 4096)

			if err := b.ReadAt(t.Context(), p, off); err != nil {
				errs <- err
				return
			}

			if string(p) != string(expected(off, 4096, b.Size())) {
				errs <- fmt.Errorf("mismatch at %d", off)
			}
		})
	}

	waitFor(t, "all readers to queue", func() bool {
		s := b.Stats()
		return s.Misses == 1 && s.CoalescedWaits == readers-1
	})
	g.open()
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}

	if n := len(f.openCalls()); n != 1 {
		t.Fatalf("opens=%d, want 1", n)
	}
}

func TestMultiChunkMissOneOpen(t *testing.T) {
	f := newFake(10 * MinChunkSize)
	b := mustOpen(t, f, testConfig())

	checkRead(t, b, 100, 4*MinChunkSize)

	calls := f.openCalls()
	if len(calls) != 1 || calls[0].off != 0 || calls[0].length != 5*MinChunkSize {
		t.Fatalf("calls=%+v", calls)
	}

	// A request spanning cached and missing chunks fetches only the gaps.
	checkRead(t, b, 6*MinChunkSize, MinChunkSize)
	checkRead(t, b, 3*MinChunkSize, 5*MinChunkSize)

	// Chunks 5 and 7 are missing, separated by cached chunk 6.
	calls = f.openCalls()
	if len(calls) != 4 || calls[2].off+calls[3].off != 12*MinChunkSize ||
		calls[2].length != MinChunkSize || calls[3].length != MinChunkSize {
		t.Fatalf("calls=%+v", calls)
	}

	s := b.Stats()
	if s.Hits != 3 || s.Fetches != 4 || s.FetchedBytes != 8*MinChunkSize {
		t.Fatalf("stats=%+v", s)
	}
}

func TestReadaheadSequential(t *testing.T) {
	const cs = MinChunkSize

	f := newFake(200 * cs)
	cfg := testConfig()
	cfg.ReadaheadMax = 16 * cs
	b := mustOpen(t, f, cfg)

	for off := int64(0); off < b.Size(); off += cs / 2 {
		checkRead(t, b, off, cs/2)
	}

	s := b.Stats()
	if s.ReadaheadFetches == 0 || s.Hits < 300 || s.Misses > 20 {
		t.Fatalf("stats=%+v", s)
	}

	// The window grows from 2 chunks up to ReadaheadMax.
	var lengths []int64
	for _, c := range f.openCalls() {
		lengths = append(lengths, c.length/cs)
	}

	sawMax := false

	for _, l := range lengths {
		if l > 16 {
			t.Fatalf("open of %d chunks exceeds ReadaheadMax: %v", l, lengths)
		}

		sawMax = sawMax || l == 16
	}

	growth := []int64{2, 4, 8, 16}
	for _, l := range lengths {
		if len(growth) > 0 && l == growth[0] {
			growth = growth[1:]
		}
	}

	if !sawMax || len(growth) != 0 {
		t.Fatalf("window did not grow: %v", lengths)
	}

	// Read-ahead never fetches past the end.
	for _, c := range f.openCalls() {
		if c.off+c.length > b.Size() {
			t.Fatalf("open past end: %+v", c)
		}
	}
}

func TestReadaheadRandomDoesNotTrigger(t *testing.T) {
	f := newFake(1 << 30)
	cfg := testConfig()
	cfg.ReadaheadMax = 16 * MinChunkSize
	b := mustOpen(t, f, cfg)

	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // Deterministic test data.
	for range 500 {
		checkRead(t, b, rng.Int64N(b.Size()-4096), 4096)
	}

	if s := b.Stats(); s.ReadaheadFetches != 0 {
		t.Fatalf("stats=%+v", s)
	}
}

func TestReadaheadDisabled(t *testing.T) {
	f := newFake(100 * MinChunkSize)
	b := mustOpen(t, f, testConfig())

	for off := int64(0); off < b.Size(); off += 4096 {
		checkRead(t, b, off, 4096)
	}

	if s := b.Stats(); s.ReadaheadFetches != 0 || s.Misses != 100 {
		t.Fatalf("stats=%+v", s)
	}
}

func TestReadaheadRespectsBudget(t *testing.T) {
	const cs = MinChunkSize

	g := newGate()
	f := newFake(1000 * cs)
	f.onOpen = func(context.Context, int, openCall) (io.ReadCloser, error) {
		<-g.ch
		return nil, nil
	}
	cfg := testConfig()
	cfg.ReadaheadMax = 4 * cs
	cfg.CacheBytes = 8 * cs
	b := mustOpen(t, f, cfg)

	// Fill the budget with pending demand fetches from many readers.
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if err := b.ReadAt(t.Context(), make([]byte, 512), int64(100+i*10)*cs); err != nil {
				t.Error(err)
			}
		})
	}

	waitFor(t, "pending fetches", func() bool { return b.Stats().Misses == 8 })

	// No room: read-ahead is skipped and nothing is overcommitted for it.
	b.prefetch(0, 4*cs)

	if s := b.Stats(); s.ReadaheadFetches != 0 || s.ResidentBytes != 8*cs {
		t.Fatalf("stats=%+v", s)
	}

	g.open()
	wg.Wait()
}

func TestEvictionBounded(t *testing.T) {
	const cs = MinChunkSize

	f := newFake(300*cs + 77)
	cfg := testConfig()
	cfg.CacheBytes = MinCacheChunks * cs
	b := mustOpen(t, f, cfg)

	for round := range 2 {
		for off := int64(0); off < b.Size(); off += 3000 {
			checkRead(t, b, off, 5000)
		}

		s := b.Stats()
		if s.PeakBytes > cfg.CacheBytes || s.ResidentBytes > cfg.CacheBytes || s.Evictions == 0 || s.Overcommits != 0 {
			t.Fatalf("round %d stats=%+v", round, s)
		}
	}

	// Freed buffers are reused, so the free list stays within budget.
	b.mu.Lock()
	total := b.used + int64(len(b.free))*cs
	b.mu.Unlock()

	if total > cfg.CacheBytes {
		t.Fatalf("buffers %d exceed budget", total)
	}
}

func TestDemandOvercommit(t *testing.T) {
	const cs = MinChunkSize

	g := newGate()
	f := newFake(100 * cs)
	f.onOpen = func(_ context.Context, n int, _ openCall) (io.ReadCloser, error) {
		if n <= 8 {
			<-g.ch
		}

		return nil, nil
	}
	cfg := testConfig()
	cfg.CacheBytes = MinCacheChunks * cs
	b := mustOpen(t, f, cfg)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() { checkReadErr(t, b, int64(i*10)*cs, 4096) })
	}

	waitFor(t, "8 pending fetches", func() bool { return b.Stats().Misses == 8 })

	s := b.Stats()
	if s.Overcommits != 4 || s.ResidentBytes != 8*cs {
		t.Fatalf("stats=%+v", s)
	}

	g.open()
	wg.Wait()

	// The next completed fetch trims usage back to the budget.
	checkRead(t, b, 99*cs, 10)

	if s := b.Stats(); s.ResidentBytes > cfg.CacheBytes {
		t.Fatalf("stats=%+v", s)
	}
}

func checkReadErr(t *testing.T, b *Backend, off, n int64) {
	t.Helper()

	p := make([]byte, n)
	if err := b.ReadAt(context.Background(), p, off); err != nil {
		t.Errorf("ReadAt(%d): %v", off, err)
		return
	}

	if string(p) != string(expected(off, n, b.Size())) {
		t.Errorf("ReadAt(%d): mismatch", off)
	}
}

type truncReader struct {
	io.Reader
}

func (truncReader) Close() error { return nil }

func TestRetry(t *testing.T) {
	t.Run("unavailable then success", func(t *testing.T) {
		f := newFake(4 * MinChunkSize)
		f.onOpen = func(_ context.Context, n int, _ openCall) (io.ReadCloser, error) {
			if n <= 2 {
				return nil, fmt.Errorf("busy: %w", racersdk.ErrUnavailable)
			}

			return nil, nil
		}
		b := mustOpen(t, f, testConfig())
		checkRead(t, b, 0, 4*MinChunkSize)

		if s := b.Stats(); s.Retries != 2 || s.Fetches != 3 || s.Errors != 0 {
			t.Fatalf("stats=%+v", s)
		}
	})

	t.Run("truncated stream resumes at first incomplete chunk", func(t *testing.T) {
		const cs = MinChunkSize

		f := newFake(4 * cs)
		f.onOpen = func(_ context.Context, n int, c openCall) (io.ReadCloser, error) {
			if n == 1 {
				// One and a half chunks, then EOF.
				return truncReader{&patternReader{off: c.off, end: c.off + cs + cs/2}}, nil
			}

			return nil, nil
		}
		b := mustOpen(t, f, testConfig())
		checkRead(t, b, 0, 4*cs)

		calls := f.openCalls()
		if len(calls) != 2 || calls[1].off != cs || calls[1].length != 3*cs {
			t.Fatalf("calls=%+v", calls)
		}
	})

	t.Run("exhaustion", func(t *testing.T) {
		f := newFake(MinChunkSize)
		f.onOpen = func(context.Context, int, openCall) (io.ReadCloser, error) {
			return nil, racersdk.ErrUnavailable
		}
		cfg := testConfig()
		cfg.MaxAttempts = 3
		b := mustOpen(t, f, cfg)

		err := b.ReadAt(t.Context(), make([]byte, 10), 0)
		if !errors.Is(err, racersdk.ErrUnavailable) {
			t.Fatalf("err=%v", err)
		}

		if n := len(f.openCalls()); n != 3 {
			t.Fatalf("opens=%d", n)
		}

		// Failed chunks are not cached; the next read tries again.
		f.onOpen = nil

		checkRead(t, b, 0, 10)

		if s := b.Stats(); s.Errors != 1 || s.Retries != 2 {
			t.Fatalf("stats=%+v", s)
		}
	})

	for _, perm := range []error{
		racersdk.ErrVersionMismatch, racersdk.ErrNotFound, racersdk.ErrForbidden,
		racersdk.ErrUnauthorized, racersdk.ErrRangeNotSatisfiable, racersdk.ErrInvalidRequest,
	} {
		t.Run("permanent "+perm.Error(), func(t *testing.T) {
			f := newFake(MinChunkSize)
			f.onOpen = func(context.Context, int, openCall) (io.ReadCloser, error) {
				return nil, fmt.Errorf("origin: %w", perm)
			}
			b := mustOpen(t, f, testConfig())

			err := b.ReadAt(t.Context(), make([]byte, 10), 0)
			if !errors.Is(err, perm) || !strings.Contains(err.Error(), "chunk 0") {
				t.Fatalf("err=%v", err)
			}

			if n := len(f.openCalls()); n != 1 {
				t.Fatalf("opens=%d", n)
			}
		})
	}
}

func TestVersionMismatchSurfaces(t *testing.T) {
	f := newFake(MinChunkSize)
	b := mustOpen(t, f, testConfig())
	f.etag = `"v2"` // the pinned version disappeared

	if err := b.ReadAt(t.Context(), make([]byte, 10), 0); !errors.Is(err, racersdk.ErrVersionMismatch) {
		t.Fatalf("err=%v", err)
	}
}

func TestWaiterCancel(t *testing.T) {
	g := newGate()
	f := newFake(MinChunkSize)
	f.onOpen = func(context.Context, int, openCall) (io.ReadCloser, error) {
		<-g.ch
		return nil, nil
	}
	b := mustOpen(t, f, testConfig())

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- b.ReadAt(ctx, make([]byte, 10), 0) }()

	waitFor(t, "fetch start", func() bool { return len(f.openCalls()) == 1 })
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}

	// The fetch continues and serves the next reader.
	g.open()
	checkRead(t, b, 0, 10)

	if n := len(f.openCalls()); n != 1 {
		t.Fatalf("opens=%d", n)
	}
}

func TestCloseCancelsFetches(t *testing.T) {
	f := newFake(MinChunkSize)
	started := make(chan struct{})
	f.onOpen = func(ctx context.Context, _ int, _ openCall) (io.ReadCloser, error) {
		close(started)
		<-ctx.Done()

		return nil, ctx.Err()
	}

	b, err := Open(t.Context(), f, testConfig())
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { done <- b.ReadAt(t.Context(), make([]byte, 10), 0) }()

	<-started

	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatalf("in-flight err=%v", err)
	}

	if err := b.ReadAt(t.Context(), make([]byte, 10), 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("after close err=%v", err)
	}

	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestReadaheadFailureDoesNotFailDemand(t *testing.T) {
	const cs = MinChunkSize

	f := newFake(100 * cs)

	var (
		mu     sync.Mutex
		failed bool
	)

	f.onOpen = func(_ context.Context, _ int, c openCall) (io.ReadCloser, error) {
		mu.Lock()
		defer mu.Unlock()

		// The first fetch beyond chunk 2 is read-ahead; fail it permanently.
		if !failed && c.off >= 3*cs {
			failed = true
			return nil, racersdk.ErrForbidden
		}

		return nil, nil
	}
	cfg := testConfig()
	cfg.ReadaheadMax = 8 * cs
	b := mustOpen(t, f, cfg)

	for off := int64(0); off < b.Size(); off += 4096 {
		checkRead(t, b, off, 4096)
	}

	if s := b.Stats(); s.Errors != 1 || s.ReadaheadFetches == 0 {
		t.Fatalf("stats=%+v", s)
	}
}

func TestWarmRefresh(t *testing.T) {
	f := newFake(MinChunkSize)
	cfg := testConfig()
	cfg.WarmTTL = 20 * time.Millisecond

	b, err := Open(t.Context(), f, cfg)
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "warm refreshes", func() bool { return f.warms.Load() >= 4 })

	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	n := f.warms.Load()

	time.Sleep(60 * time.Millisecond)

	if got := f.warms.Load(); got != n {
		t.Fatalf("warm called after Close: %d -> %d", n, got)
	}
}

func TestWarmRefreshErrorsAreTolerated(t *testing.T) {
	f := newFake(MinChunkSize)
	f.warmFailAfterFirst = true
	cfg := testConfig()
	cfg.WarmTTL = 10 * time.Millisecond
	b := mustOpen(t, f, cfg)

	waitFor(t, "failing warm refreshes", func() bool { return f.warms.Load() >= 3 })
	checkRead(t, b, 0, 100)
}

func TestLogLimiter(t *testing.T) {
	var l logLimiter

	l.last = make(map[string]int64)

	if !l.allow("a") || l.allow("a") || !l.allow("b") {
		t.Fatal("unexpected limiter result")
	}

	for i := range logKeys + 1 {
		l.allow(fmt.Sprint(i))
	}

	if len(l.last) > logKeys {
		t.Fatalf("limiter grew to %d", len(l.last))
	}
}

func TestBackoff(t *testing.T) {
	for attempt := 1; attempt < 20; attempt++ {
		d := backoff(attempt)
		if d <= 0 || d > backoffMax {
			t.Fatalf("backoff(%d)=%v", attempt, d)
		}
	}

	if d := backoff(1); d >= backoffBase {
		t.Fatalf("backoff(1)=%v", d)
	}
}

func TestRetryable(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{racersdk.ErrUnavailable, true},
		{io.ErrUnexpectedEOF, true},
		{errors.New("protocol"), true},
		{racersdk.ErrNotFound, false},
		{context.Canceled, false},
		{fmt.Errorf("x: %w", racersdk.ErrVersionMismatch), false},
		{expiredDialError(t), true},
	} {
		if got := retryable(tc.err); got != tc.want {
			t.Errorf("retryable(%v)=%v", tc.err, got)
		}
	}
}

// expiredDialError returns the error net.Dialer reports when its timeout
// expires, which matches context.DeadlineExceeded while no caller context has
// ended.
func expiredDialError(t *testing.T) error {
	t.Helper()

	d := net.Dialer{Deadline: time.Now().Add(-time.Second)}

	conn, err := d.DialContext(t.Context(), "unix", filepath.Join(t.TempDir(), "racer.sock"))
	if err == nil {
		_ = conn.Close()

		t.Fatal("dial with an expired deadline succeeded")
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired dial error %v does not match context.DeadlineExceeded", err)
	}

	return fmt.Errorf("%w: %w", racersdk.ErrUnavailable, err)
}
