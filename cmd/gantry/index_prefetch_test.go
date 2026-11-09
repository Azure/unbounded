// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sync/atomic"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/httprange"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/manifest"
	"github.com/Azure/unbounded/internal/gantry/rangecache"
)

type prefetchPeerStub struct {
	body  []byte
	total int64
	calls int
	fail  bool
}

func (s *prefetchPeerStub) FetchRangeFromPeer(_ context.Context, _ string, d digest.Digest, requested httprange.Range) (io.ReadCloser, int64, string, error) {
	s.calls++

	if s.fail {
		return nil, 0, "", &ifaces.ErrNotFound{Digest: d}
	}

	return io.NopCloser(bytes.NewReader(s.body[:requested.Length()])), s.total, "", nil
}

type prefetchProviderStub struct{ addrs []string }

func (s prefetchProviderStub) FindProviders(context.Context, digest.Digest) ([]ifaces.Provider, error) {
	out := make([]ifaces.Provider, 0, len(s.addrs))
	for _, a := range s.addrs {
		out = append(out, ifaces.Provider{Addr: a})
	}

	return out, nil
}

type prefetchOriginStub struct {
	calls      atomic.Int64
	beforePull func(context.Context, ifaces.OriginRef) error
}

func (s *prefetchOriginStub) Pull(ctx context.Context, ref ifaces.OriginRef) (io.ReadCloser, int64, error) {
	s.calls.Add(1)

	if s.beforePull != nil {
		if err := s.beforePull(ctx, ref); err != nil {
			return nil, 0, err
		}
	}

	return io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("o"), 1<<20))), 1 << 20, nil
}

func (s *prefetchOriginStub) Head(context.Context, ifaces.OriginRef) (int64, string, error) {
	return 0, "", nil
}

func prefetchDigest(t *testing.T, s string) digest.Digest {
	t.Helper()

	sum := sha256.Sum256([]byte(s))

	return digest.MustParse("sha256:" + hex.EncodeToString(sum[:]))
}

// A 1.08 GB layer only needs its head and trailing index, so the prefetcher
// must ask for exactly those two windows and nothing in between.
func TestIndexPrefetchRegionsCoverHeadAndTailOnly(t *testing.T) {
	p := newIndexPrefetcher(indexPrefetchOptions{HeadBytes: 64 << 10, TailBytes: 2 << 20})

	const size = 1082920618

	got := p.regions(size)
	if len(got) != 2 {
		t.Fatalf("regions = %+v, want head and tail", got)
	}

	if got[0].Start != 0 || got[0].End != (64<<10)-1 {
		t.Errorf("head = %+v, want 0-%d", got[0], (64<<10)-1)
	}

	if got[1].Start != size-(2<<20) || got[1].End != size-1 {
		t.Errorf("tail = %+v, want %d-%d", got[1], size-(2<<20), size-1)
	}

	total := got[0].Length() + got[1].Length()
	if pct := 100 * float64(total) / float64(size); pct > 1 {
		t.Errorf("prefetch covers %.3f%% of the layer, want well under 1%%", pct)
	}
}

// A layer smaller than head+tail must be fetched once, not as two overlapping
// windows that would double-count the middle.
func TestIndexPrefetchRegionsCollapseForSmallLayers(t *testing.T) {
	p := newIndexPrefetcher(indexPrefetchOptions{HeadBytes: 64 << 10, TailBytes: 2 << 20})

	got := p.regions(4096)
	if len(got) != 1 || got[0].Start != 0 || got[0].End != 4095 {
		t.Fatalf("regions = %+v, want a single 0-4095 range", got)
	}
}

func TestIndexPrefetchPrefersPeerAndSkipsOrigin(t *testing.T) {
	cache, err := rangecache.New(t.TempDir()+"/ranges", 0)
	if err != nil {
		t.Fatalf("rangecache.New: %v", err)
	}

	t.Cleanup(func() { _ = cache.Close() })

	const size = 8 << 20

	peer := &prefetchPeerStub{body: bytes.Repeat([]byte("p"), 4<<20), total: size}
	origin := &prefetchOriginStub{}

	var announced int

	p := newIndexPrefetcher(indexPrefetchOptions{
		Cache:      cache,
		DHT:        prefetchProviderStub{addrs: []string{"10.0.0.1:5001"}},
		Peer:       peer,
		Origin:     origin,
		MarkCached: func(digest.Digest) { announced++ },
		HeadBytes:  64 << 10,
		TailBytes:  2 << 20,
	})

	d := prefetchDigest(t, "layer")
	p.Prefetch(context.Background(), "reg", "repo", prefetchDigest(t, "manifest"), []manifest.TypedChild{{Digest: d, Kind: ifaces.KindBlob, Size: size}})

	if origin.calls.Load() != 0 {
		t.Errorf("origin.calls = %d, want 0 when a peer has the range", origin.calls.Load())
	}

	head, _ := httprange.New(0, (64<<10)-1)
	tail, _ := httprange.New(size-(2<<20), size-1)

	if !cache.Has(d, head) || !cache.Has(d, tail) {
		t.Error("index regions were not cached after prefetch")
	}

	if announced == 0 {
		t.Error("prefetched ranges were never announced to the swarm")
	}
}

func TestIndexPrefetchFallsBackToOriginWhenNoPeerHasIt(t *testing.T) {
	cache, err := rangecache.New(t.TempDir()+"/ranges", 0)
	if err != nil {
		t.Fatalf("rangecache.New: %v", err)
	}

	t.Cleanup(func() { _ = cache.Close() })

	origin := &prefetchOriginStub{}
	p := newIndexPrefetcher(indexPrefetchOptions{
		Cache:     cache,
		DHT:       prefetchProviderStub{},
		Peer:      &prefetchPeerStub{fail: true},
		Origin:    origin,
		HeadBytes: 4096,
		TailBytes: 4096,
	})

	d := prefetchDigest(t, "cold")
	p.Prefetch(context.Background(), "reg", "repo", prefetchDigest(t, "manifest"), []manifest.TypedChild{{Digest: d, Kind: ifaces.KindBlob, Size: 1 << 20}})

	if origin.calls.Load() == 0 {
		t.Error("origin was never used even though no peer had the layer")
	}

	head, _ := httprange.New(0, 4095)
	if !cache.Has(d, head) {
		t.Error("head region missing after origin fallback")
	}
}
