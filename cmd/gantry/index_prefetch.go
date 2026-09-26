// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/httprange"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/manifest"
	"github.com/Azure/unbounded/internal/gantry/rangecache"
)

// rangePeerFetcher is the bounded-range half of the peer dialer.
type rangePeerFetcher interface {
	FetchRangeFromPeer(ctx context.Context, peerAddr string, d digest.Digest, requested httprange.Range) (io.ReadCloser, int64, string, error)
}

// indexPrefetcher pulls the regions OverlayBD reads at mount time into the
// range cache before containerd ever asks for them.
//
// Measured on a 1.08 GB layer: the only bytes a container needs are the head
// and the last ~1 MB - about 0.13% of the image. Fetching them on demand makes
// the first mount attempt fail, and the container then waits out kubelet's
// CreateContainer backoff, which is what puts a ~130s floor under startup.
type indexPrefetcher struct {
	cache      *rangecache.Cache
	dht        providerLookup
	peer       rangePeerFetcher
	origin     ifaces.OriginPuller
	markCached func(d digest.Digest)
	logger     *slog.Logger

	head        int64
	tail        int64
	concurrency int
	maxJitter   time.Duration
	budget      time.Duration
}

type indexPrefetchOptions struct {
	Cache       *rangecache.Cache
	DHT         providerLookup
	Peer        rangePeerFetcher
	Origin      ifaces.OriginPuller
	MarkCached  func(d digest.Digest)
	Logger      *slog.Logger
	HeadBytes   int64
	TailBytes   int64
	Concurrency int
	MaxJitter   time.Duration
	Budget      time.Duration
}

func newIndexPrefetcher(o indexPrefetchOptions) *indexPrefetcher {
	p := &indexPrefetcher{
		cache:       o.Cache,
		dht:         o.DHT,
		peer:        o.Peer,
		origin:      o.Origin,
		markCached:  o.MarkCached,
		logger:      o.Logger,
		head:        o.HeadBytes,
		tail:        o.TailBytes,
		concurrency: o.Concurrency,
		maxJitter:   o.MaxJitter,
		budget:      o.Budget,
	}

	if p.logger == nil {
		p.logger = slog.Default()
	}

	p.logger = p.logger.With(slog.String("subsystem", "index-prefetch"))

	if p.head <= 0 {
		p.head = 64 << 10
	}

	if p.tail <= 0 {
		p.tail = 2 << 20
	}

	if p.concurrency <= 0 {
		p.concurrency = 8
	}

	if p.budget <= 0 {
		p.budget = 5 * time.Minute
	}

	return p
}

// Prefetch warms the index regions for every sized layer in children.
func (p *indexPrefetcher) Prefetch(ctx context.Context, registry, repository string, children []manifest.TypedChild) {
	if p == nil || p.cache == nil || len(children) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, p.budget)
	defer cancel()

	// Spread the fleet's first wave; 1000 nodes asking origin in the same
	// instant is what produces 429s.
	if p.maxJitter > 0 {
		select {
		case <-time.After(time.Duration(rand.Int63n(int64(p.maxJitter)))): //nolint:gosec // jitter, not security
		case <-ctx.Done():
			return
		}
	}

	sem := make(chan struct{}, p.concurrency)

	var wg sync.WaitGroup

	for _, child := range children {
		if child.Size <= 0 {
			continue
		}

		wg.Add(1)

		go func(c manifest.TypedChild) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			p.warm(ctx, registry, repository, c)
		}(child)
	}

	wg.Wait()
}

func (p *indexPrefetcher) warm(ctx context.Context, registry, repository string, c manifest.TypedChild) {
	for _, want := range p.regions(c.Size) {
		if ctx.Err() != nil {
			return
		}

		if p.cache.Has(c.Digest, want) {
			continue
		}

		if p.fromPeers(ctx, c.Digest, want) {
			continue
		}

		p.fromOrigin(ctx, registry, repository, c, want)
	}
}

// regions returns the head and tail intervals worth warming, clamped and
// de-duplicated for layers smaller than head+tail.
func (p *indexPrefetcher) regions(size int64) []httprange.Range {
	if size <= 0 {
		return nil
	}

	if size <= p.head+p.tail {
		whole, err := httprange.New(0, size-1)
		if err != nil {
			return nil
		}

		return []httprange.Range{whole}
	}

	head, err := httprange.New(0, p.head-1)
	if err != nil {
		return nil
	}

	tail, err := httprange.New(size-p.tail, size-1)
	if err != nil {
		return []httprange.Range{head}
	}

	return []httprange.Range{head, tail}
}

func (p *indexPrefetcher) fromPeers(ctx context.Context, d digest.Digest, want httprange.Range) bool {
	if p.dht == nil || p.peer == nil {
		return false
	}

	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	providers, err := p.dht.FindProviders(lookupCtx, d)

	cancel()

	if err != nil {
		return false
	}

	for _, provider := range providers {
		if ctx.Err() != nil {
			return false
		}

		body, total, _, fetchErr := p.peer.FetchRangeFromPeer(ctx, provider.Addr, d, want)
		if fetchErr != nil {
			continue
		}

		stored, putErr := p.cache.Put(d, total, want.Start, io.LimitReader(body, want.Length()))
		_ = body.Close() //nolint:errcheck // best-effort close

		if putErr == nil && stored == want.Length() {
			p.announce(d)

			return true
		}
	}

	return false
}

func (p *indexPrefetcher) fromOrigin(ctx context.Context, registry, repository string, c manifest.TypedChild, want httprange.Range) {
	if p.origin == nil {
		return
	}

	// Pull streams from Offset to the end of the blob; for the tail that is
	// exactly the region, and for the head we stop reading after Length.
	body, _, err := p.origin.Pull(ctx, ifaces.OriginRef{
		Registry:   registry,
		Repository: repository,
		Digest:     c.Digest,
		Offset:     want.Start,
		Kind:       ifaces.KindBlob,
	})
	if err != nil {
		p.logger.Debug("index prefetch origin failed",
			slog.String("digest", c.Digest.String()),
			slog.Int64("offset", want.Start),
			slog.Any("err", err),
		)

		return
	}

	stored, putErr := p.cache.Put(c.Digest, c.Size, want.Start, io.LimitReader(body, want.Length()))
	_ = body.Close() //nolint:errcheck // stop the transfer once the region is in

	if putErr != nil || stored != want.Length() {
		p.logger.Debug("index prefetch store incomplete",
			slog.String("digest", c.Digest.String()),
			slog.Int64("stored", stored),
			slog.Any("err", putErr),
		)

		return
	}

	p.announce(c.Digest)
}

func (p *indexPrefetcher) announce(d digest.Digest) {
	if p.markCached != nil {
		p.markCached(d)
	}
}
