// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package meshfetch

import (
	"context"
	"fmt"
	"io"
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
		n:          n,
		chunkSize:  benchChunk,
		cacheBytes: 2 * benchBlob,
		lookahead:  8,
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

// BenchmarkStreamFromCache measures copying a cached chunk to a writer,
// the per-hop serve cost without the network.
func BenchmarkStreamFromCache(b *testing.B) {
	s := newStore(benchChunk, 4*benchChunk, 4, nil)

	f, _, _ := s.acquire(context.Background(), key(1), false)
	_ = f.setMeta(benchChunk, "", benchChunk)
	_ = f.readFrom(zeroReader{}, nil)
	f.finish(nil)

	b.SetBytes(benchChunk)
	b.ResetTimer()

	for range b.N {
		if _, err := f.streamTo(context.Background(), &copySink{buf: make([]byte, 1<<20)}, 0); err != nil {
			b.Fatal(err)
		}
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
	c := newCluster(b, clusterOpts{n: 4, chunkSize: benchChunk, cacheBytes: 2 * benchBlob, lookahead: 8, verify: true})
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
