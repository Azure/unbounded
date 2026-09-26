// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/coldstart"
	"github.com/Azure/unbounded/internal/gantry/containerdstore"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

// errSeedNoProvider means no peer advertised the digest before the peer
// patience budget expired.
var errSeedNoProvider = errors.New("layer seed: no peer provider")

// providerLookup is the DHT subset the seeder needs.
type providerLookup interface {
	FindProviders(ctx context.Context, d digest.Digest) ([]ifaces.Provider, error)
}

// layerSeeder downloads every child digest of a served manifest into this
// node's own containerd content store. Chair prefetch seeds a bounded cohort
// from origin; the seeder is what makes every other node converge to complete
// local content instead of reaching back over the network per read.
//
// Peers are preferred and origin is used only after the peer patience budget
// expires, so a 1000-node rollout does not turn into 1000 origin pulls.
type layerSeeder struct {
	store       ifaces.LocalContentStore
	dht         providerLookup
	peer        ifaces.PeerDialer
	origin      ifaces.OriginPuller
	markPresent func(context.Context, digest.Digest) bool
	logger      *slog.Logger

	concurrency  int
	peerPatience time.Duration
	pollInterval time.Duration
	fetchTimeout time.Duration
	budget       time.Duration

	onOutcome func(outcome string)

	mu     sync.Mutex
	active map[digest.Digest]struct{}
}

type layerSeederOptions struct {
	Store       ifaces.LocalContentStore
	DHT         providerLookup
	Peer        ifaces.PeerDialer
	Origin      ifaces.OriginPuller
	MarkPresent func(context.Context, digest.Digest) bool
	Logger      *slog.Logger

	Concurrency  int
	PeerPatience time.Duration
	PollInterval time.Duration
	FetchTimeout time.Duration
	Budget       time.Duration

	OnOutcome func(outcome string)
}

func newLayerSeeder(opts layerSeederOptions) *layerSeeder {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}

	if opts.PeerPatience <= 0 {
		opts.PeerPatience = 10 * time.Minute
	}

	if opts.PollInterval <= 0 {
		opts.PollInterval = time.Second
	}

	if opts.FetchTimeout <= 0 {
		opts.FetchTimeout = 30 * time.Minute
	}

	if opts.Budget <= 0 {
		opts.Budget = time.Hour
	}

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	return &layerSeeder{
		store:        opts.Store,
		dht:          opts.DHT,
		peer:         opts.Peer,
		origin:       opts.Origin,
		markPresent:  opts.MarkPresent,
		logger:       opts.Logger.With(slog.String("subsystem", "layer-seed")),
		concurrency:  opts.Concurrency,
		peerPatience: opts.PeerPatience,
		pollInterval: opts.PollInterval,
		fetchTimeout: opts.FetchTimeout,
		budget:       opts.Budget,
		onOutcome:    opts.OnOutcome,
		active:       map[digest.Digest]struct{}{},
	}
}

// Seed fetches every child into the local store. It blocks until all children
// are resolved or the budget expires, so callers run it in their own goroutine.
func (s *layerSeeder) Seed(ctx context.Context, registry, repository string, children []coldstart.ChildDigest) {
	if s == nil || s.store == nil || len(children) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, s.budget)
	defer cancel()

	slots := make(chan struct{}, s.concurrency)

	var wg sync.WaitGroup

	for _, child := range children {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()

			return
		}

		wg.Add(1)

		go func(child coldstart.ChildDigest) {
			defer wg.Done()
			defer func() { <-slots }()

			s.seedOne(ctx, registry, repository, child.Digest, child.Kind)
		}(child)
	}

	wg.Wait()
}

func (s *layerSeeder) seedOne(ctx context.Context, registry, repository string, d digest.Digest, kind ifaces.OriginRefKind) {
	if !s.claim(d) {
		return
	}
	defer s.release(d)

	if s.has(ctx, d) {
		s.observe("present")

		return
	}

	err := s.seedFromPeers(ctx, registry, repository, d, kind)
	if err == nil {
		s.observe("peer")

		return
	}

	if ctx.Err() != nil {
		s.observe("canceled")

		return
	}

	if !errors.Is(err, errSeedNoProvider) || s.origin == nil {
		s.observe("failed")
		s.logger.Debug("layer seed failed",
			slog.String("digest", d.String()),
			slog.Any("err", err),
		)

		return
	}

	if s.has(ctx, d) {
		s.observe("present")

		return
	}

	if err := s.seedFromOrigin(ctx, registry, repository, d, kind); err != nil {
		s.observe("failed")
		s.logger.Debug("layer seed origin fallback failed",
			slog.String("digest", d.String()),
			slog.Any("err", err),
		)

		return
	}

	s.observe("origin")
}

// seedFromPeers polls the DHT until a provider serves the digest or the peer
// patience budget expires. Chairs are pulling concurrently, so an empty
// provider set early in a rollout is expected rather than terminal.
func (s *layerSeeder) seedFromPeers(ctx context.Context, registry, repository string, d digest.Digest, kind ifaces.OriginRefKind) error {
	deadline := time.Now().Add(s.peerPatience)

	for {
		if s.has(ctx, d) {
			return nil
		}

		if s.dht != nil && s.peer != nil {
			lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			providers, err := s.dht.FindProviders(lookupCtx, d)

			cancel()

			if err != nil {
				s.logger.Debug("layer seed provider lookup failed",
					slog.String("digest", d.String()),
					slog.Any("err", err),
				)
			}

			for _, provider := range providers {
				if ctx.Err() != nil {
					return ctx.Err()
				}

				if err := s.fetchFromPeer(ctx, provider, registry, repository, d, kind); err == nil {
					return nil
				}
			}
		}

		if time.Now().After(deadline) {
			return errSeedNoProvider
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.pollInterval):
		}
	}
}

func (s *layerSeeder) fetchFromPeer(ctx context.Context, provider ifaces.Provider, registry, repository string, d digest.Digest, kind ifaces.OriginRefKind) error {
	fetchCtx, cancel := context.WithTimeout(ctx, s.fetchTimeout)
	defer cancel()

	ref := ifaces.OriginRef{Registry: registry, Repository: repository, Digest: d, Kind: kind}

	body, _, _, err := s.peer.FetchFromPeer(fetchCtx, provider.Addr, ref)
	if err != nil {
		return err
	}

	defer func() { _ = body.Close() }() //nolint:errcheck // best-effort close

	return s.commit(fetchCtx, registry, repository, d, body)
}

func (s *layerSeeder) seedFromOrigin(ctx context.Context, registry, repository string, d digest.Digest, kind ifaces.OriginRefKind) error {
	fetchCtx, cancel := context.WithTimeout(ctx, s.fetchTimeout)
	defer cancel()

	ref := ifaces.OriginRef{Registry: registry, Repository: repository, Digest: d, Kind: kind}

	body, _, err := s.origin.Pull(fetchCtx, ref)
	if err != nil {
		return err
	}

	defer func() { _ = body.Close() }() //nolint:errcheck // best-effort close

	return s.commit(fetchCtx, registry, repository, d, body)
}

// commit streams body into the content store under a containerd lease so the
// seeded blob is not reclaimed before a snapshot references it. Commit fails
// if the bytes do not hash to d.
func (s *layerSeeder) commit(ctx context.Context, registry, repository string, d digest.Digest, body io.Reader) error {
	var leaseGuard *containerdstore.LeaseGuard

	if leased, ok := s.store.(preIngestLeaseStore); ok {
		leaseCtx, leaseCancel := context.WithTimeout(context.Background(), 10*time.Second)
		guard, err := leased.CreateLease(leaseCtx, d, registry, repository)

		leaseCancel()

		if err != nil {
			return err
		}

		leaseGuard = guard
	}

	releaseLease := func() {
		if leaseGuard == nil {
			return
		}

		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = leaseGuard.Release(releaseCtx) //nolint:errcheck // best-effort release

		releaseCancel()
	}

	writer, err := s.store.Writer(ctx, d)
	if err != nil {
		releaseLease()

		return err
	}

	if _, err := io.Copy(writer, body); err != nil {
		_ = writer.Abort(ctx) //nolint:errcheck // best-effort abort

		releaseLease()

		return err
	}

	if err := writer.Commit(ctx); err != nil {
		_ = writer.Abort(ctx) //nolint:errcheck // best-effort abort

		releaseLease()

		return err
	}

	if s.markPresent != nil {
		advertiseCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		s.markPresent(advertiseCtx, d)

		cancel()
	}

	return nil
}

func (s *layerSeeder) has(ctx context.Context, d digest.Digest) bool {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	has, err := s.store.Has(checkCtx, d)

	return err == nil && has
}

// claim keeps concurrent manifest serves on the same node from downloading the
// same digest twice.
func (s *layerSeeder) claim(d digest.Digest) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, busy := s.active[d]; busy {
		return false
	}

	s.active[d] = struct{}{}

	return true
}

func (s *layerSeeder) release(d digest.Digest) {
	s.mu.Lock()
	delete(s.active, d)
	s.mu.Unlock()
}

func (s *layerSeeder) observe(outcome string) {
	if s.onOutcome != nil {
		s.onOutcome(outcome)
	}
}
