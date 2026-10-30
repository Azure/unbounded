// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

const integrationSize = 40<<20 + 123

var integrationKey = racersdk.Key{1, 2, 3}

// patternOrigin serves pattern data of the given size as version "v1".
func patternOrigin(size int64, gets *atomic.Int64) racersdk.Origin {
	return func(_ context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		md := racersdk.Metadata{Size: size, ETag: `"v1"`, ExpiresAt: time.Now().Add(time.Hour)}

		if r.Key != integrationKey {
			return racersdk.Metadata{}, nil, racersdk.ErrNotFound
		}

		if r.ETag != "" && r.ETag != md.ETag {
			return racersdk.Metadata{}, nil, racersdk.ErrVersionMismatch
		}

		if r.Head {
			return md, nil, nil
		}

		gets.Add(1)

		start := min(r.Offset, size)
		end := min(start+r.Length, size)

		if end == start {
			return md, nil, nil
		}

		return md, io.NopCloser(&patternReader{off: start, end: end}), nil
	}
}

func TestIntegrationRacerSDK(t *testing.T) {
	var gets atomic.Int64

	client := racersdktest.NewClient(t, patternOrigin(integrationSize, &gets))
	src := FromClient(client, racersdk.Request{Key: integrationKey})

	b := mustOpen(t, src, Config{ChunkSize: 1 << 20, CacheBytes: 24 << 20, ReadaheadMax: 8 << 20, Logger: quietLogger()})
	if b.Size() != integrationSize || b.ETag() != `"v1"` {
		t.Fatalf("size=%d etag=%s", b.Size(), b.ETag())
	}

	// Sequential pass crossing the 16 MiB page boundaries.
	for off := int64(0); off < b.Size(); off += 1 << 20 {
		checkRead(t, b, off, 1<<20)
	}

	// Random concurrent reads, including past the end.
	var wg sync.WaitGroup
	for w := range 16 {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 7)) //nolint:gosec // Deterministic test data.
			for range 40 {
				n := 512 * (1 + rng.Int64N(512))
				off := rng.Int64N(b.Size()+4096) &^ 511
				p := make([]byte, n)

				if err := b.ReadAt(t.Context(), p, off); err != nil {
					t.Error(err)
					return
				}

				if !bytes.Equal(p, expected(off, n, b.Size())) {
					t.Errorf("mismatch at %d+%d", off, n)
					return
				}
			}
		})
	}

	wg.Wait()

	if s := b.Stats(); s.PeakBytes > 24<<20+16<<20 || s.Fetches == 0 {
		t.Fatalf("stats=%+v", s)
	}
}

func TestIntegrationPinnedETag(t *testing.T) {
	var gets atomic.Int64

	client := racersdktest.NewClient(t, patternOrigin(integrationSize, &gets))
	src := FromClient(client, racersdk.Request{Key: integrationKey})

	cfg := Config{ETag: `"v1"`, ChunkSize: 1 << 20, CacheBytes: 16 << 20, ReadaheadMax: -1, WarmTTL: time.Hour, Logger: quietLogger()}
	b := mustOpen(t, src, cfg)
	checkRead(t, b, 16<<20-100, 200)

	cfg.ETag = `"v2"`
	if _, err := Open(t.Context(), src, cfg); !errors.Is(err, racersdk.ErrVersionMismatch) {
		t.Fatalf("warm pin err=%v", err)
	}

	cfg.WarmTTL = 0
	if _, err := Open(t.Context(), src, cfg); !errors.Is(err, racersdk.ErrVersionMismatch) {
		t.Fatalf("probe pin err=%v", err)
	}

	missing := FromClient(client, racersdk.Request{Key: racersdk.Key{9}})
	if _, err := Open(t.Context(), missing, Config{Logger: quietLogger()}); !errors.Is(err, racersdk.ErrNotFound) {
		t.Fatalf("missing err=%v", err)
	}
}

func TestIntegrationSmallObject(t *testing.T) {
	var gets atomic.Int64

	client := racersdktest.NewClient(t, patternOrigin(1000, &gets))
	src := FromClient(client, racersdk.Request{Key: integrationKey})
	b := mustOpen(t, src, testConfig())

	// The read is padded to a 512-byte multiple past the end.
	checkRead(t, b, 0, 1024)
	checkRead(t, b, 999, 25)
}

func TestFromClientETagCheck(t *testing.T) {
	var gets atomic.Int64

	client := racersdktest.NewClient(t, patternOrigin(1000, &gets))
	src := FromClient(client, racersdk.Request{Key: integrationKey})

	r, err := src.Open(t.Context(), 0, 10, `"v1"`)
	if err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, expected(0, 10, 1000)) {
		t.Fatalf("got=%v err=%v", got, err)
	}

	if mr, ok := r.(MetadataReader); !ok || mr.Metadata().Size != 1000 {
		t.Fatal("reader does not report metadata")
	}

	_ = r.Close()

	if _, err := src.Open(t.Context(), 0, 10, `"v9"`); !errors.Is(err, racersdk.ErrVersionMismatch) {
		t.Fatalf("err=%v", err)
	}
}

func TestStress(t *testing.T) {
	const cs = MinChunkSize

	f := newFake(300*cs + 4321)

	var opens atomic.Int64

	f.onOpen = func(_ context.Context, n int, c openCall) (io.ReadCloser, error) {
		opens.Add(1)

		switch {
		case n%17 == 0:
			return nil, racersdk.ErrUnavailable
		case n%23 == 0:
			return truncReader{&patternReader{off: c.off, end: c.off + c.length/2}}, nil
		}

		return nil, nil
	}
	b := mustOpen(t, f, Config{ChunkSize: cs, CacheBytes: 16 * cs, ReadaheadMax: 8 * cs, MaxAttempts: 10, Logger: quietLogger()})

	var wg sync.WaitGroup
	for w := range 64 {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 99)) //nolint:gosec // Deterministic test data.
			seq := w%2 == 0
			off := rng.Int64N(b.Size())

			for range 100 {
				n := 512 * (1 + rng.Int64N(64))
				if !seq {
					off = rng.Int64N(b.Size() + 1000)
				}

				p := make([]byte, n)
				if err := b.ReadAt(context.Background(), p, off); err != nil {
					t.Error(err)
					return
				}

				if !bytes.Equal(p, expected(off, n, b.Size())) {
					t.Errorf("mismatch at %d+%d", off, n)
					return
				}

				off = (off + n) % b.Size()
			}
		})
	}

	wg.Wait()

	s := b.Stats()
	t.Logf("stats=%+v", s)

	// Usage may exceed the budget only through demand overcommit, bounded by
	// the chunks of concurrent requests (64 readers, at most 2 chunks each).
	if s.PeakBytes > 16*cs+64*2*cs {
		t.Fatalf("peak %d too high", s.PeakBytes)
	}

	if s.Retries == 0 || s.Hits == 0 {
		t.Fatalf("stats=%+v", s)
	}
}

func BenchmarkReadAtHit(b *testing.B) {
	f := newFake(64 << 20)

	be, err := Open(context.Background(), f, Config{ChunkSize: 1 << 20, CacheBytes: 128 << 20, ReadaheadMax: -1, Logger: quietLogger()})
	if err != nil {
		b.Fatal(err)
	}
	defer be.Close() //nolint:errcheck // Benchmark cleanup.

	p := make([]byte, 64<<20)
	if err := be.ReadAt(context.Background(), p, 0); err != nil {
		b.Fatal(err)
	}

	b.SetBytes(4096)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]byte, 4096)
		rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // Benchmark offsets.

		for pb.Next() {
			if err := be.ReadAt(context.Background(), buf, rng.Int64N(64<<20-4096)); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
