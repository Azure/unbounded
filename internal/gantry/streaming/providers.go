// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package streaming

import (
	"errors"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

const (
	defaultStaleProviderTTL       = 3 * time.Minute
	defaultUnavailableProviderTTL = 30 * time.Second
	defaultSuspiciousProviderTTL  = 5 * time.Minute
)

type providerDigestKey struct {
	digest digest.Digest
	nodeID ifaces.NodeID
	addr   string
}

type providerFailures struct {
	mu          sync.Mutex
	stale       map[providerDigestKey]time.Time
	suspicious  map[providerDigestKey]time.Time
	unavailable map[string]time.Time
	now         func() time.Time
}

func newProviderFailures() *providerFailures {
	return &providerFailures{
		stale:       map[providerDigestKey]time.Time{},
		suspicious:  map[providerDigestKey]time.Time{},
		unavailable: map[string]time.Time{},
		now:         time.Now,
	}
}

func (f *providerFailures) filter(d digest.Digest, providers []ifaces.Provider, self ifaces.NodeID) []ifaces.Provider {
	f.mu.Lock()
	defer f.mu.Unlock()

	now := f.now()
	f.sweep(now)

	filtered := make([]ifaces.Provider, 0, len(providers))
	for _, provider := range providers {
		if provider.NodeID == self || provider.Addr == "" {
			continue
		}

		key := providerDigestKey{digest: d, nodeID: provider.NodeID, addr: provider.Addr}
		if now.Before(f.stale[key]) || now.Before(f.suspicious[key]) || now.Before(f.unavailable[provider.Addr]) {
			continue
		}

		filtered = append(filtered, provider)
	}

	rand.Shuffle(len(filtered), func(left, right int) {
		filtered[left], filtered[right] = filtered[right], filtered[left]
	})

	return filtered
}

func (f *providerFailures) record(d digest.Digest, provider ifaces.Provider, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	now := f.now()
	f.sweep(now)

	key := providerDigestKey{digest: d, nodeID: provider.NodeID, addr: provider.Addr}

	var notFound *ifaces.ErrNotFound
	if errors.As(err, &notFound) {
		f.stale[key] = now.Add(defaultStaleProviderTTL)

		return
	}

	var protocol *ifaces.ErrPeerProtocol
	if errors.As(err, &protocol) {
		f.suspicious[key] = now.Add(defaultSuspiciousProviderTTL)

		return
	}

	var status *ifaces.ErrPeerHTTPStatus
	if errors.As(err, &status) {
		switch {
		case status.StatusCode == http.StatusNotFound:
			f.stale[key] = now.Add(defaultStaleProviderTTL)
		case status.StatusCode == http.StatusTooManyRequests && status.RetryAfter > 0:
			f.unavailable[provider.Addr] = now.Add(status.RetryAfter)
		case status.StatusCode == http.StatusTooManyRequests || status.StatusCode >= http.StatusInternalServerError:
			f.unavailable[provider.Addr] = now.Add(defaultUnavailableProviderTTL)
		default:
			f.suspicious[key] = now.Add(defaultSuspiciousProviderTTL)
		}

		return
	}

	f.unavailable[provider.Addr] = now.Add(defaultUnavailableProviderTTL)
}

func (f *providerFailures) sweep(now time.Time) {
	for key, until := range f.stale {
		if !now.Before(until) {
			delete(f.stale, key)
		}
	}

	for key, until := range f.suspicious {
		if !now.Before(until) {
			delete(f.suspicious, key)
		}
	}

	for addr, until := range f.unavailable {
		if !now.Before(until) {
			delete(f.unavailable, addr)
		}
	}
}
