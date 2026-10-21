// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package providerprobe verifies that DHT provider candidates currently serve
// a requested digest.
package providerprobe

import (
	"context"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

const DefaultConcurrency = 4

// Attempted records provider identities and addresses already probed during
// one resolution. A changed address for the same peer remains eligible.
type Attempted map[ifaces.Provider]struct{}

// Finder is the discovery surface needed to check provider candidates.
type Finder interface {
	FindProviders(context.Context, digest.Digest) ([]ifaces.Provider, error)
}

// Candidates prefers an uncapped lookup when discovery supports streaming.
// The lookup budget limits discovery, not how long the consumer may spend
// trying queued providers. The error function is valid after the channel closes.
func Candidates(ctx context.Context, discovery Finder, d digest.Digest, lookupBudget time.Duration) (<-chan ifaces.Provider, func() error) {
	if streamer, ok := discovery.(ifaces.ProviderStreamer); ok {
		return streamer.StreamProviders(ctx, d, lookupBudget)
	}

	out := make(chan ifaces.Provider)

	var lookupErr error

	go func() {
		defer close(out)

		lookupCtx, cancel := context.WithTimeout(ctx, lookupBudget)
		providers, err := discovery.FindProviders(lookupCtx, d)

		lookupErr = err
		if lookupErr == nil {
			lookupErr = lookupCtx.Err()
		}

		cancel()

		for _, provider := range providers {
			select {
			case out <- provider:
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, func() error { return lookupErr }
}

// FirstFromDiscovery checks candidates as they arrive, without allowing a
// fixed-size stale result to hide a usable peer. Partial lookup results remain
// usable even when discovery reaches its deadline.
func FirstFromDiscovery(ctx context.Context, discovery Finder, dialer ifaces.PeerMetadataDialer, ref ifaces.OriginRef, attempted Attempted, lookupBudget time.Duration) (ifaces.Provider, bool, error) {
	lookupCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	candidates, lookupErr := Candidates(lookupCtx, discovery, ref.Digest, lookupBudget)
	provider, ok := first(ctx, dialer, candidates, ref, attempted, DefaultConcurrency)

	cancel()

	for range candidates {
	}

	if ok {
		return provider, true, nil
	}

	return ifaces.Provider{}, false, lookupErr()
}

// First returns the first provider whose transfer endpoint answers HEAD for
// ref. Each previously unseen candidate is added to attempted before probing.
func First(ctx context.Context, dialer ifaces.PeerMetadataDialer, providers []ifaces.Provider, ref ifaces.OriginRef, attempted Attempted, concurrency int) (ifaces.Provider, bool) {
	candidates := make(chan ifaces.Provider, len(providers))
	for _, provider := range providers {
		candidates <- provider
	}

	close(candidates)

	return first(ctx, dialer, candidates, ref, attempted, concurrency)
}

func first(ctx context.Context, dialer ifaces.PeerMetadataDialer, candidates <-chan ifaces.Provider, ref ifaces.OriginRef, attempted Attempted, concurrency int) (ifaces.Provider, bool) {
	if dialer == nil {
		return ifaces.Provider{}, false
	}

	if attempted == nil {
		attempted = Attempted{}
	}

	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}

	type result struct {
		provider ifaces.Provider
		usable   bool
	}

	probeCtx, cancel := context.WithCancel(registryauth.WithoutAuthorization(ctx))
	results := make(chan result, concurrency)
	inFlight := 0

	// Each outstanding probe always has room to report its result, including
	// after cancellation, so no sender is stranded when a usable peer wins.
	defer func() {
		cancel()

		for range inFlight {
			<-results
		}
	}()

	for candidates != nil || inFlight > 0 {
		input := candidates
		if inFlight == concurrency {
			input = nil
		}

		select {
		case <-ctx.Done():
			return ifaces.Provider{}, false
		case provider, open := <-input:
			if !open {
				candidates = nil
				continue
			}

			if ctx.Err() != nil {
				return ifaces.Provider{}, false
			}

			if _, seen := attempted[provider]; seen {
				continue
			}

			attempted[provider] = struct{}{}
			inFlight++

			go func() {
				_, _, err := dialer.HeadFromPeer(probeCtx, provider.Addr, ref)
				results <- result{provider: provider, usable: err == nil}
			}()
		case probeResult := <-results:
			inFlight--

			if probeResult.usable && ctx.Err() == nil {
				return probeResult.provider, true
			}
		}
	}

	return ifaces.Provider{}, false
}
