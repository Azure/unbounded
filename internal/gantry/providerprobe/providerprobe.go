// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package providerprobe verifies that DHT provider candidates currently serve
// a requested digest.
package providerprobe

import (
	"context"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

const DefaultConcurrency = 4

// Attempted records provider identities and addresses already probed during
// one resolution. A changed address for the same peer remains eligible.
type Attempted map[ifaces.Provider]struct{}

// First returns the first provider whose transfer endpoint answers HEAD for
// ref. Each previously unseen candidate is added to attempted before probing.
func First(ctx context.Context, dialer ifaces.PeerMetadataDialer, providers []ifaces.Provider, ref ifaces.OriginRef, attempted Attempted, concurrency int) (ifaces.Provider, bool) {
	if dialer == nil {
		return ifaces.Provider{}, false
	}

	if attempted == nil {
		attempted = Attempted{}
	}

	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}

	candidates := make([]ifaces.Provider, 0, len(providers))
	queued := Attempted{}

	for _, provider := range providers {
		if _, seen := attempted[provider]; seen {
			continue
		}

		if _, seen := queued[provider]; seen {
			continue
		}

		queued[provider] = struct{}{}
		candidates = append(candidates, provider)
	}

	type result struct {
		provider ifaces.Provider
		usable   bool
	}

	for start := 0; start < len(candidates); start += concurrency {
		end := min(start+concurrency, len(candidates))
		probeCtx, cancel := context.WithCancel(ctx)
		probeCtx = registryauth.WithoutAuthorization(probeCtx)
		results := make(chan result, end-start)

		for _, provider := range candidates[start:end] {
			attempted[provider] = struct{}{}

			go func() {
				_, _, err := dialer.HeadFromPeer(probeCtx, provider.Addr, ref)
				results <- result{provider: provider, usable: err == nil}
			}()
		}

		var usable ifaces.Provider

		found := false

		for range end - start {
			probeResult := <-results

			if probeResult.usable && !found {
				usable = probeResult.provider
				found = true

				cancel()
			}
		}

		cancel()

		if found {
			return usable, true
		}

		if ctx.Err() != nil {
			return ifaces.Provider{}, false
		}
	}

	return ifaces.Provider{}, false
}
