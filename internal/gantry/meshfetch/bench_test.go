// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

const (
	benchChunk = 16 << 20
	benchBlob  = 256 << 20
)

func benchCluster(b *testing.B, n int) (*cluster, []byte) {
	b.Helper()

	c := newCluster(b, clusterOpts{
		n:         n,
		chunkSize: benchChunk,
		lookahead: 8,
	})

	return c, randomBlob(42, benchBlob)
}

// addAs registers data under a synthetic digest, so iterations get cold
// keys without generating new data. Verification must stay off.
func (o *fakeOrigin) addAs(i int, data []byte) digest.Digest {
	d := digest.MustParse(fmt.Sprintf("sha256:%064x", i+1))

	o.mu.Lock()
	o.blobs[d.String()] = data
	o.mu.Unlock()

	return d
}

func benchFetch(b *testing.B, n *testNode, d digest.Digest) {
	b.Helper()

	ref := ifaces.OriginRef{Registry: "reg", Repository: "library/app", Digest: d}

	started, written, err := n.node.FetchBlob(context.Background(), io.Discard, ref, func(int64, string) {})
	if err != nil || !started || written != benchBlob {
		b.Fatalf("FetchBlob: started=%v written=%d err=%v", started, written, err)
	}
}

// BenchmarkFetchBlobCold measures a cold blob: owners fill chunks from an
// in-memory origin and the requester streams them over loopback HTTP.
func BenchmarkFetchBlobCold(b *testing.B) {
	c, data := benchCluster(b, 4)

	b.SetBytes(benchBlob)
	b.ResetTimer()

	for i := range b.N {
		d := c.origin.addAs(i, data)
		benchFetch(b, c.nodes[i%len(c.nodes)], d)

		b.StopTimer()

		for _, n := range c.nodes {
			n.node.store.evictDigest(d.String())
		}

		b.StartTimer()
	}
}

// BenchmarkFetchBlobFromPeerCache measures the peer hop alone: every chunk
// is already cached at its owner, and the requester's own copy is dropped
// before each iteration.
func BenchmarkFetchBlobFromPeerCache(b *testing.B) {
	c, data := benchCluster(b, 4)

	d := c.origin.addAs(0, data)
	benchFetch(b, c.nodes[1], d)

	req := c.nodes[0]

	b.SetBytes(benchBlob)
	b.ResetTimer()

	for range b.N {
		b.StopTimer()
		req.node.store.evictDigest(d.String())
		b.StartTimer()

		benchFetch(b, req, d)
	}
}

// benchContentStore writes a one-chunk blob to a content root on disk and
// returns a DirContent over it with the blob's digest.
func benchContentStore(b *testing.B) (*DirContent, string) {
	b.Helper()

	root := b.TempDir()
	dir := filepath.Join(root, "blobs", "sha256")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}

	d := fmt.Sprintf("sha256:%064x", 1)
	if err := os.WriteFile(filepath.Join(dir, hexOf(d)), make([]byte, benchChunk), 0o444); err != nil {
		b.Fatal(err)
	}

	dc, err := NewDirContent(root, nil)
	if err != nil {
		b.Fatal(err)
	}

	return dc, d
}

// localReader returns a reader of a complete chunk of k whose pages are
// all behind the window, so every page is read from the content store.
func localReader(s *store, k chunkKey) *reader {
	f, r := s.newLocalFill(k, 0)
	_ = f.setMeta(benchChunk, "", benchChunk)

	f.mu.Lock()
	f.n = benchChunk
	f.winStart = benchChunk / s.cfg.pageSize
	f.localEnd = benchChunk
	f.mu.Unlock()

	f.finish(nil)
	f.release()

	return r
}

// BenchmarkStreamFromLocalContent measures copying a chunk from the
// content store (page cache warm, as right after containerd wrote it) to a
// writer: the per-hop serve cost of a page behind the window.
func BenchmarkStreamFromLocalContent(b *testing.B) {
	dc, d := benchContentStore(b)
	s := newTestStore(b, func(c *storeConfig) {
		c.chunkSize = benchChunk
		c.pageSize = 1 << 20
		c.hotBudget = 0
		c.content = dc
	})
	k := chunkKey{digest: d}

	b.SetBytes(benchChunk)
	b.ResetTimer()

	for range b.N {
		r := localReader(s, k)

		if _, err := r.streamTo(context.Background(), &copySink{buf: make([]byte, 1<<20)}); err != nil {
			b.Fatal(err)
		}

		r.close()
	}
}

// BenchmarkFanOutFromLocalContent measures 32 concurrent readers of one
// chunk behind the window: pages are read from the content store once and
// shared.
func BenchmarkFanOutFromLocalContent(b *testing.B) {
	const readers = 32

	dc, d := benchContentStore(b)
	s := newTestStore(b, func(c *storeConfig) {
		c.chunkSize = benchChunk
		c.pageSize = 1 << 20
		c.hotBudget = 64 << 20
		c.content = dc
	})
	k := chunkKey{digest: d}

	b.SetBytes(benchChunk * readers)
	b.ResetTimer()

	for range b.N {
		var wg sync.WaitGroup

		for range readers {
			r := localReader(s, k)

			wg.Go(func() {
				defer r.close()

				_, _ = r.streamTo(context.Background(), &copySink{buf: make([]byte, 1<<20)})
			})
		}

		wg.Wait()
		s.pages.purge(d)
	}

	b.ReportMetric(float64(s.stats().LocalReads)/float64(b.N), "local-reads/op")
}

// BenchmarkFillWithFollowers measures a fill read from memory while 8
// readers follow it (cut-through).
func BenchmarkFillWithFollowers(b *testing.B) {
	s := newTestStore(b, func(c *storeConfig) {
		c.chunkSize = benchChunk
		c.pageSize = 1 << 20
		c.window = 4
	})

	b.SetBytes(benchChunk)
	b.ResetTimer()

	for i := range b.N {
		r, _, _ := s.acquire(context.Background(), key(int64(i)), 0, false, false)
		f := r.f

		var wg sync.WaitGroup

		for range 8 {
			fr := s.get(key(int64(i)), 0)

			wg.Go(func() {
				defer fr.close()

				_, _ = fr.streamTo(context.Background(), &copySink{buf: make([]byte, 1<<20)})
			})
		}

		r.close()

		_ = f.setMeta(benchChunk, "", benchChunk)
		_ = f.readFrom(context.Background(), io.LimitReader(zeroReader{}, benchChunk), nil)
		f.finish(nil)
		f.release()
		wg.Wait()
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// copySink copies every write, like a socket write would.
type copySink struct{ buf []byte }

func (s *copySink) Write(p []byte) (int, error) {
	for off := 0; off < len(p); off += len(s.buf) {
		copy(s.buf, p[off:])
	}

	return len(p), nil
}

// BenchmarkFetchBlobFromPeerCacheVerified is FetchBlobFromPeerCache with
// full-blob digest verification on.
func BenchmarkFetchBlobFromPeerCacheVerified(b *testing.B) {
	c := newCluster(b, clusterOpts{n: 4, chunkSize: benchChunk, lookahead: 8, verify: true})
	d := c.origin.add(randomBlob(43, benchBlob), "")

	benchFetch(b, c.nodes[1], d)

	req := c.nodes[0]

	b.SetBytes(benchBlob)
	b.ResetTimer()

	for range b.N {
		b.StopTimer()
		req.node.store.evictDigest(d.String())
		b.StartTimer()

		benchFetch(b, req, d)
	}
}
